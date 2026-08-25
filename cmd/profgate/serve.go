package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arloliu/profgate/internal/admit"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/httpapi"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/ops"
	"github.com/arloliu/profgate/internal/pgo"
	"github.com/arloliu/profgate/internal/proxy"
)

const (
	// preflightBackoffFirst is the wait after the first failed preflight attempt.
	preflightBackoffFirst = time.Second
	// preflightBackoffCap caps the doubling wait between preflight attempts.
	preflightBackoffCap = 30 * time.Second
	// drainSlack is added to the longest profile duration to bound the API drain.
	drainSlack = 30 * time.Second
	// opsDrainTimeout bounds the ops listener's shutdown.
	opsDrainTimeout = 5 * time.Second
	// drainReportInterval is how often a drain that is still waiting for a
	// Collection says so.
	drainReportInterval = 30 * time.Second
	// readHeaderTimeout bounds how long a connection may take to send its request headers.
	readHeaderTimeout = 10 * time.Second
	// syncedPollInterval is how often the lifecycle re-checks HasSynced after the informers start.
	syncedPollInterval = 50 * time.Millisecond
	// instanceSuffixBytes is how much randomness separates two instances
	// that a Pod name reused across restarts would otherwise conflate.
	instanceSuffixBytes = 4
)

// listenFunc opens one of the two listeners.
type listenFunc func(ctx context.Context, network, address string) (net.Listener, error)

// shutdownMode says what the shutdown waits for.
type shutdownMode int

const (
	// drainAll waits for the interactive drain and the Collection drain.
	drainAll shutdownMode = iota
	// abandonCollections skips the Collection drain.
	// The process is ending because a listener it cannot serve without has
	// failed, and there is nothing left for a finished Collection to be
	// serving; a Collection left running stops renewing its lease, and
	// another replica reclaims it after leaseTTL.
	abandonCollections
)

// natsPreflightFunc is the NATS preflight the lifecycle retries; production is natskv.Preflight.
type natsPreflightFunc func(ctx context.Context, opts natskv.Options, instanceID string, log *slog.Logger) (natskv.Client, error)

// collectionWorker is what the lifecycle needs from the PGO worker:
// claiming until the stop request, a drain of its own,
// and the Collection ids the drain reports on.
type collectionWorker interface {
	Run(ctx context.Context)
	Drain(ctx context.Context) error
	InFlight() []string
}

// serveDeps is what serve needs from the outside; production fills it in runServe,
// tests fill it with fakes.
type serveDeps struct {
	namespaceFile string                                 // production: the projected token directory's namespace file
	runtime       func(k8s.Options) (k8s.Runtime, error) // production: k8s.NewRuntime
	upstream      httpapi.Upstream                       // production: proxy.New(proxy.Options{})
	sampler       *proxy.Proxy                           // production: the same proxy, as the Collection sampler
	registry      *prometheus.Registry                   // production: prometheus.NewRegistry()
	recorder      metrics.Recorder                       // production: metrics.NewPrometheus(registry)
	stop          <-chan struct{}                        // production: closed by the first SIGINT or SIGTERM
	natsPreflight natsPreflightFunc                      // production: natskv.Preflight
	pgoWorker     collectionWorker                       // production: nil, so serve builds a pgo.Worker
	listen        listenFunc                             // production: nil, so serve uses net.ListenConfig
}

// serve runs the gateway until stop is closed or a fatal event happens, and returns the exit code.
// ctx is the parent of everything the gateway runs; stop requests a drain.
func serve(ctx context.Context, cfgPath string, deps serveDeps, stdout, stderr io.Writer) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: cfg.Server.SlogLevel()}))
	logger.Warn("authentication disabled; access is controlled only by network boundary and static realm policy")
	var cfgPtr atomic.Pointer[config.Config]
	cfgPtr.Store(cfg)

	opts := k8s.Options{
		VersionLabel:  cfg.Discovery.VersionLabel,
		Port:          cfg.Discovery.Pprof.Port,
		PortName:      cfg.Discovery.Pprof.PortName,
		NamespaceFile: deps.namespaceFile,
		Logger:        logger,
	}
	rt, err := deps.runtime(opts)
	if err != nil {
		logger.Error("kubernetes runtime", "error", err)

		return 1
	}
	cluster := rt.Cluster()

	var draining atomic.Bool
	// natsReady holds the one PGO readiness gate: the NATS preflight has passed.
	// Readiness never waits for the replay barrier,
	// and never falls back to 503 when the connection drops later:
	// a replica whose watches are replaying, or whose connection is down,
	// serves interactive requests and answers the PGO routes 503,
	// which is correct behavior rather than a reason to leave the Service.
	var natsReady atomic.Bool
	ready := func() bool {
		return !draining.Load() && cluster.HasSynced() && (!cfg.PGO.Enabled || natsReady.Load())
	}
	// The one admission gate, sized from the configuration loaded now:
	// limits.maxConcurrentProfiles is a restart-only field.
	gate := admit.New(cfg.Limits.MaxConcurrentProfiles)
	// The handlers' late-bound view of the PGO machinery:
	// the HTTP server starts before the NATS preflight has passed,
	// so every PGO route answers 503 through an unbound runtime until it does.
	var pgoRuntime *pgo.Runtime
	if cfg.PGO.Enabled {
		pgoRuntime = pgo.NewRuntime()
	}
	api := httpapi.New(httpapi.Deps{
		Discovery: cluster,
		Upstream:  deps.upstream,
		Config:    &cfgPtr,
		Recorder:  deps.recorder,
		Gate:      gate,
		PGO:       pgoRuntime,
		Logger:    logger,
	})
	// inFlightRequests is how many API requests are being served right now,
	// so a drain that runs out of time reports how many it cut.
	var inFlightRequests atomic.Int64
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlightRequests.Add(1)
		defer inFlightRequests.Add(-1)
		api.ServeHTTP(w, r)
	})
	apiServer := &http.Server{Handler: counted, ReadHeaderTimeout: readHeaderTimeout}
	opsServer := &http.Server{Handler: ops.New(ready, deps.registry), ReadHeaderTimeout: readHeaderTimeout}

	listen := deps.listen
	if listen == nil {
		var lc net.ListenConfig
		listen = lc.Listen
	}
	apiListener, err := listen(ctx, "tcp", cfg.Server.Listen)
	if err != nil {
		logger.Error("listen", "address", cfg.Server.Listen, "error", err)

		return 1
	}
	opsListener, err := listen(ctx, "tcp", cfg.Server.OpsListen)
	if err != nil {
		logger.Error("listen", "address", cfg.Server.OpsListen, "error", err)
		_ = apiListener.Close()

		return 1
	}
	logger.Info("listening", "api", apiListener.Addr().String(), "ops", opsListener.Addr().String())

	// errCh holds one result per Serve; both goroutines send exactly once and never block.
	errCh := make(chan error, 2)
	go func() { errCh <- apiServer.Serve(apiListener) }()
	go func() { errCh <- opsServer.Serve(opsListener) }()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	// The informers run under a context of their own, cancelled once both
	// drains have ended, because discovery is what the drain still needs:
	// an in-flight Collection re-resolves its targets every round, and a
	// profile request confirms its Pod before it dials.
	informerCtx, cancelInformers := context.WithCancel(ctx)
	defer cancelInformers()
	preflightCh := make(chan error, 1)
	go func() { preflightCh <- preflight(runCtx, rt, logger) }()
	syncedCh := make(chan struct{}, 1)

	// natsCh carries the one result of the NATS preflight, and stays nil when
	// PGO is off so nothing NATS-related is ever constructed.
	var natsCh chan natsResult
	// worker is set when the PGO loops start, and read only by the loop below
	// and the shutdown it calls, both of which run on this goroutine.
	var worker collectionWorker
	owner := instanceOwner(logger)
	if cfg.PGO.Enabled {
		natsCh = make(chan natsResult, 1)
		go func() {
			client, err := natsPreflight(runCtx, deps, cfg.NATS, owner.Instance, logger)
			natsCh <- natsResult{client: client, err: err}
		}()
	}

	shutdown := func(mode shutdownMode) {
		start := time.Now()
		draining.Store(true)
		// Stops the scheduler, the sweeper, and the worker's claiming.
		// The informers are the one thing that outlives it, until the waits
		// below have ended: an in-flight Collection finishes when it can, and
		// is reclaimed by another replica when it cannot.
		cancelRun()

		// Readiness is 503 from here, and the API listener stays open for the
		// delay, which is the window the EndpointSlice controllers and every
		// kube-proxy get to stop routing new requests to this replica.
		// A preStop hook is where a deployment usually buys that window: the
		// image is distroless and has no shell to run one, and the lifecycle
		// "sleep" action is newer than the Kubernetes baseline this gateway
		// supports.
		// A listener that has failed receives nothing the window protects,
		// so the fatal path spends none of the grace period on it.
		if delay := cfg.Server.DrainDelay; delay > 0 && mode == drainAll {
			logger.Info("draining; waiting for endpoint removal", "delay", delay.String())
			time.Sleep(delay)
		}

		// apiOutcome and the two PGO variables are written by the goroutines
		// below and read after wg.Wait, which is what orders them.
		apiOutcome := "completed"
		pgoOutcome := "drained"
		var abandoned []string

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			drainBound := time.Duration(max(cfg.Limits.CPUSeconds, cfg.Limits.TraceSeconds))*time.Second + drainSlack
			drainCtx, cancel := context.WithTimeout(context.Background(), drainBound)
			defer cancel()
			err := apiServer.Shutdown(drainCtx)
			switch {
			case err == nil:
			case errors.Is(err, context.DeadlineExceeded):
				apiOutcome = "deadline_closed"
				logger.Warn("drain deadline passed; closing in-flight connections",
					"requests", inFlightRequests.Load())
				_ = apiServer.Close()
			default:
				apiOutcome = "failed"
				logger.Warn("api listener shutdown", "error", err)
			}
		}()
		switch {
		case worker == nil:
		case mode == abandonCollections:
			// Nothing waits: the Collections this replica owns stop renewing
			// when the process exits, and another replica reclaims them.
			abandoned = worker.InFlight()
			pgoOutcome = "abandoned"
			logger.Warn("exiting without the collection drain", "collections", abandoned)
		default:
			wg.Add(1)
			go func() {
				defer wg.Done()
				reporting := make(chan struct{})
				defer close(reporting)
				go reportDraining(reporting, worker, start, logger)
				// A context of its own, and deliberately an unbounded one:
				// Drain already waits per Collection no longer than that Collection's deadline,
				// which is the only bound that knows how long a merge may still legitimately run.
				// A Collection deadline can far exceed the interactive drain bound above,
				// so the two waits run side by side.
				if err := worker.Drain(context.Background()); err != nil {
					logger.Warn("pgo drain incomplete", "error", err)
					pgoOutcome = "abandoned"
					abandoned = worker.InFlight()
				}
			}()
		}
		wg.Wait()
		cancelInformers()

		opsCtx, cancelOps := context.WithTimeout(context.Background(), opsDrainTimeout)
		defer cancelOps()
		_ = opsServer.Shutdown(opsCtx)

		fields := []any{"elapsed", time.Since(start).Round(time.Millisecond).String(), "api", apiOutcome}
		if worker != nil {
			fields = append(fields, "pgo", pgoOutcome)
			if len(abandoned) > 0 {
				fields = append(fields, "collections", abandoned)
			}
		}
		logger.Info("drain complete", fields...)
	}

	for {
		select {
		case err := <-preflightCh:
			var fb k8s.ErrForbidden
			if errors.As(err, &fb) {
				logger.Error("preflight forbidden; the ClusterRole lacks a tuple", "resource", fb.Resource, "verb", fb.Verb)
				shutdown(drainAll)

				return 1
			}
			if err != nil {
				logger.Error("preflight cancelled", "error", err)
				shutdown(drainAll)

				return 1
			}
			logger.Info("preflight passed; starting informers")
			go cluster.Run(informerCtx)
			go waitSynced(runCtx, cluster, syncedCh)
		case <-syncedCh:
			deps.recorder.DiscoverySynced(true)
			logger.Info("discovery synced; ready")
		case res := <-natsCh:
			if res.err != nil {
				// Everything that reaches here is fatal by classification:
				// a missing bucket, a bucket of the wrong kind,
				// a configuration outside the contract, or a denied probe.
				// The error names the bucket and the operation or field.
				logger.Error("nats preflight failed", "error", res.err)
				shutdown(drainAll)

				return 1
			}
			natsReady.Store(true)
			logger.Info("nats preflight passed; starting pgo loops")
			w, err := startPGO(runCtx, res.client, cfg, pgoRuntime, gate, cluster, owner, deps, logger)
			if err != nil {
				logger.Error("pgo runtime", "error", err)
				shutdown(drainAll)

				return 1
			}
			worker = w
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				continue
			}
			logger.Error("listener failed", "error", err)
			shutdown(abandonCollections)

			return 1
		case <-deps.stop:
			logger.Info("stop requested; draining")
			shutdown(drainAll)

			return 0
		}
	}
}

// reportDraining says what the Collection drain is still waiting for, every
// drainReportInterval, until reporting is closed.
// A drain that outlasts the interactive one has nothing else to show for
// itself: the merge it waits for writes no record until it ends.
func reportDraining(reporting <-chan struct{}, worker collectionWorker, start time.Time, logger *slog.Logger) {
	ticker := time.NewTicker(drainReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-reporting:
			return
		case <-ticker.C:
			ids := worker.InFlight()
			logger.Info("still draining collections",
				"collections", len(ids), "ids", ids, "elapsed", time.Since(start).Round(time.Second).String())
		}
	}
}

// preflight retries rt.Preflight with a doubling backoff until it passes, a tuple is forbidden, or ctx ends;
// the returned error is nil, the ErrForbidden, or ctx.Err().
func preflight(ctx context.Context, rt k8s.Runtime, logger *slog.Logger) error {
	backoff := preflightBackoffFirst
	for {
		err := rt.Preflight(ctx)
		var fb k8s.ErrForbidden
		switch {
		case err == nil:
			return nil
		case errors.As(err, &fb):
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		}
		logger.Warn("preflight attempt", "error", err, "retry_in", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, preflightBackoffCap)
	}
}

// natsResult is what the NATS preflight goroutine reports once.
type natsResult struct {
	client natskv.Client
	err    error
}

// natsPreflight retries the NATS preflight with the same doubling backoff as the
// Kubernetes one, but only while the failure is a connection-level one.
// Everything else is returned for the caller to end startup with —
// a missing bucket, a bucket of the wrong kind, a configuration outside the contract, a denied probe —
// because no amount of waiting turns it into a pass.
func natsPreflight(
	ctx context.Context, deps serveDeps, nats config.NATSConfig, instance string, logger *slog.Logger,
) (natskv.Client, error) {
	opts := natskv.Options{
		URL:            nats.URL,
		CredsFile:      nats.CredsFile,
		ConnectTimeout: nats.ConnectTimeout,
		// Without the report Preflight makes on its first connection,
		// the gauge would read zero until the first reconnect.
		OnConnectionChange: deps.recorder.NATSConnected,
	}
	backoff := preflightBackoffFirst
	for {
		client, err := deps.natsPreflight(ctx, opts, instance, logger)
		switch {
		case err == nil:
			return client, nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case !errors.Is(err, natskv.ErrUnavailable):
			return nil, err
		}
		logger.Warn("nats preflight attempt", "error", err, "retry_in", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff = min(backoff*2, preflightBackoffCap)
	}
}

// startPGO constructs the PGO machinery the passed preflight made reachable,
// publishes it to the handlers, and starts the three loops.
// Each loop begins every pass behind the replay barrier,
// so starting them before the watches have replayed costs an idle tick and nothing else.
func startPGO(
	ctx context.Context,
	client natskv.Client,
	cfg *config.Config,
	runtime *pgo.Runtime,
	gate *admit.Gate,
	cluster *k8s.Cluster,
	owner pgo.Owner,
	deps serveDeps,
	logger *slog.Logger,
) (collectionWorker, error) {
	defaults, err := pgo.DefaultPolicy(cfg.PGO.Defaults)
	if err != nil {
		return nil, err
	}

	clock := pgo.SystemClock{}
	caches := pgo.NewCaches(logger)
	go runCaches(ctx, caches, client, logger)

	publisher := pgo.NewPublisher(caches, clock, cfg.PGO.Limits.MaxLiveCollections, owner.Instance, logger)
	rounds := pgo.NewRounds(pgo.RoundsDeps{
		Discovery:    cluster,
		Proxy:        deps.sampler,
		Gate:         gate,
		Limits:       cfg.PGO.Limits,
		Clock:        clock,
		Recorder:     deps.recorder,
		Log:          logger,
		VersionLabel: cfg.Discovery.VersionLabel,
		Gateway:      owner.Instance,
	})
	var worker collectionWorker = pgo.NewWorker(client, caches, cfg.PGO, owner, rounds, clock, deps.recorder, logger)
	if deps.pgoWorker != nil {
		worker = deps.pgoWorker
	}
	scheduler := pgo.NewScheduler(client, caches, publisher, defaults, cfg.PGO.Limits, clock, deps.recorder, logger)
	sweeper := pgo.NewSweeper(client, caches, cfg.PGO, owner, clock, deps.recorder, logger)

	runtime.Bind(pgo.Bundle{
		Client:    client,
		Caches:    caches,
		Publisher: publisher,
		Bucket:    pgo.NewTokenBucket(cfg.PGO.Limits.OnDemandPerMinute, clock),
		Defaults:  defaults,
		Limits:    cfg.PGO.Limits,
		Clock:     clock,
		Recorder:  deps.recorder,
		Instance:  owner.Instance,
		Log:       logger,
	})

	go worker.Run(ctx)
	go scheduler.Run(ctx)
	go sweeper.Run(ctx)

	return worker, nil
}

// runCaches keeps the four watched caches open until ctx ends.
// Caches.Run neither retries nor reconnects,
// and the replay barrier stays shut until its watches exist,
// so a failure to open them has one answer: open them again, with the same backoff the preflights use.
func runCaches(ctx context.Context, caches *pgo.Caches, client natskv.Client, logger *slog.Logger) {
	backoff := preflightBackoffFirst
	for {
		err := caches.Run(ctx, client)
		if ctx.Err() != nil {
			return
		}
		logger.Warn("pgo watched caches stopped; reopening", "error", err, "retry_in", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, preflightBackoffCap)
	}
}

// instanceOwner is this replica's identity in every Collection record it claims:
// the Pod name, plus a random suffix
// that separates two processes a reused Pod name would otherwise conflate.
func instanceOwner(logger *slog.Logger) pgo.Owner {
	pod, err := os.Hostname()
	if err != nil || pod == "" {
		logger.Warn("hostname unavailable; naming this instance after the binary", "error", err)
		pod = "profgate"
	}
	var suffix [instanceSuffixBytes]byte
	// crypto/rand.Read never returns an error and always fills its argument.
	_, _ = rand.Read(suffix[:])

	return pgo.Owner{Instance: pod + "/" + hex.EncodeToString(suffix[:]), Pod: pod}
}

// waitSynced sends once on syncedCh when the informers have synced, or returns when ctx ends.
func waitSynced(ctx context.Context, cluster *k8s.Cluster, syncedCh chan<- struct{}) {
	ticker := time.NewTicker(syncedPollInterval)
	defer ticker.Stop()
	for {
		if cluster.HasSynced() {
			syncedCh <- struct{}{}

			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}
