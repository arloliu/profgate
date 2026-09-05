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
	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/httpapi"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/ops"
	"github.com/arloliu/profgate/internal/pgo"
	"github.com/arloliu/profgate/internal/proxy"
	"github.com/arloliu/profgate/internal/tlscert"
	"github.com/arloliu/profgate/internal/ui"
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
	// idleTimeout is how long either listener holds a keep-alive connection that sends nothing before closing it.
	idleTimeout = 120 * time.Second
	// syncedPollInterval is how often the lifecycle re-checks HasSynced after the informers start.
	syncedPollInterval = 50 * time.Millisecond
	// syncedReportInterval is how often an informer sync still waiting says so.
	syncedReportInterval = 15 * time.Second
	// instanceSuffixBytes is how much randomness separates two instances
	// that a Pod name reused across restarts would otherwise conflate.
	instanceSuffixBytes = 4
)

// listenFunc opens one of the two listeners.
type listenFunc func(ctx context.Context, network, address string) (net.Listener, error)

// shutdownMode says whether the endpoint-removal window is worth spending.
type shutdownMode int

const (
	// drainEndpoints spends server.drainDelay letting the EndpointSlice controllers stop routing here.
	drainEndpoints shutdownMode = iota
	// listenerFailed skips that window.
	// The process is ending because a listener it cannot serve without has failed,
	// so there is nothing left for the window to protect.
	listenerFailed
)

// natsPreflightFunc is the NATS preflight the lifecycle retries; production is natskv.Preflight.
type natsPreflightFunc func(ctx context.Context, opts natskv.Options, instanceID string, log *slog.Logger) (natskv.Client, error)

// collectionWorker is what the lifecycle needs from the PGO worker:
// claiming until the stop request, and a drain of its own.
type collectionWorker interface {
	Run(ctx context.Context)
	Drain(ctx context.Context) error
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
	tlsRefresh    time.Duration                          // production: 0, so tlscert re-reads on its own interval
	idleTimeout   time.Duration                          // production: 0, so both listeners close an idle connection after idleTimeout
	authPoll      time.Duration                          // production: 0, so the users file and cookie key file are polled every 30 seconds
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
	var cfgPtr atomic.Pointer[config.Config]
	cfgPtr.Store(cfg)

	// The authenticator is built from the startup snapshot before anything
	// binds: a users file or cookie key file that cannot be read ends startup
	// here rather than answering every request 503.
	// basic and oidc are the two modes with state of their own; each keeps its
	// concrete handle so serve can start its pollers and, under oidc, drive
	// discovery before the preflight.
	var (
		authenticator auth.Authenticator
		basic         *auth.Basic
		oidc          *auth.OIDC
	)
	switch cfg.Auth.Mode {
	case config.ModeBasic:
		basic, err = auth.NewBasic(cfg, auth.BasicOptions{Logger: logger, Recorder: deps.recorder, PollInterval: deps.authPoll})
		if err != nil {
			logger.Error("basic authentication", "error", err)

			return 1
		}
		authenticator = basic
		if cfg.Auth.Basic.AllowPlaintext && !cfg.Server.TLS.Enabled() {
			logger.Warn("basic authentication over plaintext HTTP; passwords cross the network in the clear")
		}
	case config.ModeOIDC:
		oidc, err = auth.NewOIDC(cfg, auth.OIDCOptions{Logger: logger, Recorder: deps.recorder, PollInterval: deps.authPoll})
		if err != nil {
			logger.Error("oidc authentication", "error", err)

			return 1
		}
		authenticator = oidc
	default:
		logger.Warn("authentication disabled; access is controlled only by network boundary and static realm policy")
	}

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
	// issuerReady is the oidc readiness gate: discovery and the first key
	// fetch have succeeded. It starts true in the other modes, which have no
	// issuer to wait for.
	var issuerReady atomic.Bool
	issuerReady.Store(oidc == nil)
	ready := func() bool {
		return !draining.Load() && issuerReady.Load() && cluster.HasSynced() && (!cfg.PGO.Enabled || natsReady.Load())
	}
	// The handlers gate on less than /readyz does: the drain delay exists to
	// keep serving requests already routed here, and a replica whose NATS
	// preflight is pending still serves interactive requests.
	handlersReady := func() bool {
		return issuerReady.Load() && cluster.HasSynced()
	}
	// The interactive admission gate, sized from the configuration loaded now:
	// limits.maxConcurrentProfiles is a restart-only field.
	// Collection sampling does not pass through it.
	gate := admit.New(cfg.Limits.MaxConcurrentProfiles)
	// The handlers' late-bound view of the PGO machinery:
	// the HTTP server starts before the NATS preflight has passed,
	// so every PGO route answers 503 through an unbound runtime until it does.
	var pgoRuntime *pgo.Runtime
	if cfg.PGO.Enabled {
		pgoRuntime = pgo.NewRuntime()
	}
	// The signal a request held open by a wait ends on.
	// It closes with readiness, before the endpoint-removal window,
	// so no poll outlasts the drain the deployment sized.
	drainSignal := make(chan struct{})
	apiDeps := httpapi.Deps{
		Discovery: cluster,
		Upstream:  deps.upstream,
		Config:    &cfgPtr,
		Recorder:  deps.recorder,
		Gate:      gate,
		PGO:       pgoRuntime,
		Auth:      authenticator,
		Ready:     handlersReady,
		Drain:     drainSignal,
		Logger:    logger,
	}
	if oidc != nil {
		apiDeps.AuthRoutes = oidc.Routes()
	}
	// The console is built from files already in the binary, so the only way
	// it fails is a tree that cannot be read or a shell that cannot be
	// rendered; that ends startup here, before anything binds, as an
	// authenticator error does.
	if cfg.UI.Enabled {
		console, err := ui.New()
		if err != nil {
			logger.Error("console", "error", err)

			return 1
		}
		apiDeps.Console = console
		logger.Info("console enabled", "path", ui.Prefix)
	}
	api := httpapi.New(apiDeps)
	// inFlightRequests is how many API requests are being served right now,
	// so a drain that runs out of time reports how many it cut.
	var inFlightRequests atomic.Int64
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlightRequests.Add(1)
		defer inFlightRequests.Add(-1)
		api.ServeHTTP(w, r)
	})
	// A keep-alive connection that sends nothing is closed after idle,
	// and every line net/http writes on its own is an ERROR record on stdout under the process's handler,
	// a TLS handshake failure and a recovered handler panic among them.
	idle := idleTimeout
	if deps.idleTimeout > 0 {
		idle = deps.idleTimeout
	}
	errorLog := slog.NewLogLogger(logger.Handler(), slog.LevelError)
	apiServer := &http.Server{
		Handler:           counted,
		ReadHeaderTimeout: httpapi.RequestReadTimeout,
		IdleTimeout:       idle,
		ErrorLog:          errorLog,
	}
	opsServer := &http.Server{
		Handler:           ops.New(ready, deps.registry),
		ReadHeaderTimeout: httpapi.RequestReadTimeout,
		IdleTimeout:       idle,
		ErrorLog:          errorLog,
	}

	// The API listener serves HTTPS when the configuration names a certificate,
	// and the ops listener never does: its readers are the kubelet and the
	// scraper, both of which reach it by Pod address and would skip
	// verification anyway.
	// A certificate that cannot be read or parsed ends startup here, before
	// anything binds, rather than answering every handshake with an error.
	var certs *tlscert.Loader
	if cfg.Server.TLS.Enabled() {
		certs, err = tlscert.New(tlscert.Options{
			CertFile:   cfg.Server.TLS.CertFile,
			KeyFile:    cfg.Server.TLS.KeyFile,
			MinVersion: cfg.Server.TLS.MinVersion,
			Interval:   deps.tlsRefresh,
			Logger:     logger,
			Recorder:   deps.recorder,
		})
		if err != nil {
			logger.Error("tls certificate", "error", err)

			return 1
		}
		apiServer.TLSConfig = certs.TLSConfig()
	}

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
	scheme := "http"
	if certs != nil {
		scheme = "https"
	}
	logger.Info("listening",
		"api", apiListener.Addr().String(), "scheme", scheme, "ops", opsListener.Addr().String())

	// errCh holds one result per Serve; both goroutines send exactly once and never block.
	errCh := make(chan error, 2)
	// serving counts the two Serve calls, which the drain waits for.
	// Serve closes its listener as it returns,
	// and a Serve the scheduler has not entered yet holds no listener for Shutdown to close.
	// Without this wait serve returns while a socket still accepts connections.
	var serving sync.WaitGroup
	serving.Add(2)
	go func() {
		defer serving.Done()
		// ServeTLS with no file names wraps the listener with the TLSConfig
		// already set, which is where GetCertificate lives, and sets up ALPN
		// the way a plain tls.NewListener would not.
		if certs != nil {
			errCh <- apiServer.ServeTLS(apiListener, "", "")

			return
		}
		errCh <- apiServer.Serve(apiListener)
	}()
	go func() {
		defer serving.Done()
		errCh <- opsServer.Serve(opsListener)
	}()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	// The informers run under a context of their own, cancelled once both
	// drains have ended, because discovery is what the drain still needs:
	// an in-flight Collection re-resolves its targets every round, and a
	// profile request confirms its Pod before it dials.
	informerCtx, cancelInformers := context.WithCancel(ctx)
	defer cancelInformers()
	// The certificate is re-read under runCtx, so it stops with the other
	// loops at the top of the drain.
	// Nothing waits for it: the pair already loaded serves every connection
	// the drain is still finishing.
	if certs != nil {
		go certs.Run(runCtx)
	}
	if basic != nil {
		go basic.Run(runCtx)
	}
	preflightCh := make(chan error, 1)
	startPreflight := func() { go func() { preflightCh <- preflight(runCtx, rt, logger) }() }
	// Under oidc the Kubernetes preflight waits for discovery: a gateway that
	// cannot reach its issuer cannot authenticate anyone, and exiting is
	// better than serving 503 to every request while looking healthy.
	// discoverCh stays nil in the other modes, so its case never fires.
	var discoverCh chan error
	if oidc != nil {
		discoverCh = make(chan error, 1)
		go func() { discoverCh <- discoverIssuer(runCtx, oidc, cfg.Auth.OIDC.DiscoveryTimeout, logger) }()
	} else {
		startPreflight()
	}
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
			client, err := natsPreflight(runCtx, deps, cfg.NATS, owner.Instance, pgoRuntime, logger)
			natsCh <- natsResult{client: client, err: err}
		}()
	}

	shutdown := func(mode shutdownMode) {
		start := time.Now()
		draining.Store(true)
		// Every arm of the loop below returns once it has drained,
		// so this runs exactly once.
		close(drainSignal)
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
		if delay := cfg.Server.DrainDelay; delay > 0 && mode == drainEndpoints {
			logger.Info("draining; waiting for endpoint removal", "delay", delay.String())
			time.Sleep(delay)
		}

		// apiOutcome and pgoOutcome are written by the goroutines below and
		// read after wg.Wait, which is what orders them.
		apiOutcome := "completed"
		pgoOutcome := "drained"

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
		if worker != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A context of its own, and deliberately an unbounded one:
				// Drain stops the owner loops renewing
				// and waits no longer than the lease each of them last committed,
				// which is the bound another replica already honours before it reclaims the record.
				// That bound is unrelated to the interactive drain above,
				// so the two waits run side by side.
				if err := worker.Drain(context.Background()); err != nil {
					logger.Warn("pgo drain incomplete", "error", err)
					pgoOutcome = "incomplete"
				}
			}()
		}
		wg.Wait()
		cancelInformers()

		opsCtx, cancelOps := context.WithTimeout(context.Background(), opsDrainTimeout)
		defer cancelOps()
		_ = opsServer.Shutdown(opsCtx)
		// Shutdown closes the listeners its Serve has registered,
		// and returns without waiting for Serve itself to return.
		// This wait is what makes a returned serve mean both sockets refuse.
		serving.Wait()

		fields := []any{"elapsed", time.Since(start).Round(time.Millisecond).String(), "api", apiOutcome}
		if worker != nil {
			fields = append(fields, "pgo", pgoOutcome)
		}
		logger.Info("drain complete", fields...)
	}

	for {
		select {
		case err := <-discoverCh:
			if err != nil {
				logger.Error("issuer discovery failed", "error", err)
				shutdown(drainEndpoints)

				return 1
			}
			issuerReady.Store(true)
			logger.Info("issuer discovered; starting preflight")
			go oidc.Run(runCtx)
			startPreflight()
		case err := <-preflightCh:
			var fb k8s.ErrForbidden
			if errors.As(err, &fb) {
				logger.Error("preflight forbidden; the ClusterRole lacks a tuple", "resource", fb.Resource, "verb", fb.Verb)
				shutdown(drainEndpoints)

				return 1
			}
			if err != nil {
				logger.Error("preflight cancelled", "error", err)
				shutdown(drainEndpoints)

				return 1
			}
			logger.Info("preflight passed; starting informers")
			go cluster.Run(informerCtx)
			go waitSynced(runCtx, cluster.HasSynced, syncedCh, syncedReportInterval, logger)
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
				shutdown(drainEndpoints)

				return 1
			}
			natsReady.Store(true)
			logger.Info("nats preflight passed; starting pgo loops")
			w, err := startPGO(runCtx, res.client, cfg, pgoRuntime, cluster, owner, deps, logger)
			if err != nil {
				logger.Error("pgo runtime", "error", err)
				shutdown(drainEndpoints)

				return 1
			}
			worker = w
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				continue
			}
			logger.Error("listener failed", "error", err)
			shutdown(listenerFailed)

			return 1
		case <-deps.stop:
			logger.Info("stop requested; draining")
			shutdown(drainEndpoints)

			return 0
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

// discoverIssuer retries o.Discover with the preflight backoff until it
// succeeds or timeout passes; the returned error is nil, the last failure,
// or ctx.Err() when the caller ended first.
// Discovery has a bound where the Kubernetes preflight has none because the
// issuer is outside the cluster: waiting forever would hide an issuer that
// is down from a rollout that would otherwise stop.
func discoverIssuer(ctx context.Context, o *auth.OIDC, timeout time.Duration, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	backoff := preflightBackoffFirst
	for {
		err := o.Discover(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w (giving up after %s)", err, timeout)
		}
		logger.Warn("issuer discovery attempt", "error", err, "retry_in", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return fmt.Errorf("%w (giving up after %s)", err, timeout)
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
	ctx context.Context, deps serveDeps, nats config.NATSConfig, instance string,
	runtime *pgo.Runtime, logger *slog.Logger,
) (natskv.Client, error) {
	opts := natskv.Options{
		URL:            nats.URL,
		CredsFile:      nats.CredsFile,
		ConnectTimeout: nats.ConnectTimeout,
		// The connection callback drives profgate_nats_connected and nothing else.
		// It is what moves the gauge off the zero written below,
		// through the report Preflight makes on its first connection.
		OnConnectionChange: deps.recorder.NATSConnected,
		// Either move reaches this callback,
		// the disconnect and the watch cut under a live connection alike,
		// and the seam has already moved the generation by the time it runs,
		// so a request ended here always sees a generation that has moved.
		OnGenerationMove: runtime.MoveGeneration,
	}
	// The gauge reads NaN on a process that makes no connection.
	// This process makes one, so the transport is down until the callback above reports otherwise,
	// and a rule over the gauge fires through an outage that never reaches the callback at all.
	deps.recorder.NATSConnected(false)
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

// cacheBarrier is the watched-cache half of the PGO replay barrier,
// which *pgo.Caches satisfies.
type cacheBarrier interface {
	Synced(gen uint64) bool
}

// genBarrier is the seam half of the same barrier,
// and holds the store generation both halves are read under;
// natskv.Client satisfies it.
type genBarrier interface {
	cacheBarrier
	Generation() uint64
}

// pgoSynced answers the profgate_pgo_synced gauge for one scrape.
// It reads the store generation on every call and holds nothing between calls,
// so the answer turns false the moment a disconnect or a watch cut moves that generation,
// and turns true again only once every watch has replayed under the new one
// and every cache has applied that replay.
func pgoSynced(client genBarrier, caches cacheBarrier) bool {
	gen := client.Generation()

	return client.Synced(gen) && caches.Synced(gen)
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
	// The series exists only when pgo.enabled,
	// and this is the one path a PGO start reaches, from a channel one goroutine writes once,
	// so the gauge is registered here and exactly once.
	deps.recorder.PGOSyncedFrom(func() bool { return pgoSynced(client, caches) })

	publisher := pgo.NewPublisher(caches, clock, cfg.PGO.Limits.MaxLiveCollections, owner.Instance, logger)
	rounds := pgo.NewRounds(pgo.RoundsDeps{
		Discovery:    cluster,
		Proxy:        deps.sampler,
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

// waitSynced sends once on syncedCh when hasSynced reports true, or returns when ctx ends.
// A wait that outlives reportEvery says so at that cadence,
// so a running Pod that is not Ready names this wait in its own logs
// the way the retrying preflights name theirs;
// a sync that lands before the first report logs nothing.
// Production passes syncedReportInterval; tests shorten it.
func waitSynced(ctx context.Context, hasSynced func() bool, syncedCh chan<- struct{}, reportEvery time.Duration, logger *slog.Logger) {
	start := time.Now()
	ticker := time.NewTicker(syncedPollInterval)
	defer ticker.Stop()
	report := time.NewTicker(reportEvery)
	defer report.Stop()
	for {
		if hasSynced() {
			syncedCh <- struct{}{}

			return
		}
		select {
		case <-ticker.C:
		case <-report.C:
			logger.Warn("still waiting for informer caches to sync",
				"elapsed", time.Since(start).Round(time.Second).String())
		case <-ctx.Done():
			return
		}
	}
}
