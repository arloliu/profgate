package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/httpapi"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/ops"
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
	// readHeaderTimeout bounds how long a connection may take to send its request headers.
	readHeaderTimeout = 10 * time.Second
	// syncedPollInterval is how often the lifecycle re-checks HasSynced after the informers start.
	syncedPollInterval = 50 * time.Millisecond
)

// serveDeps is what serve needs from the outside; production fills it in runServe,
// tests fill it with fakes.
type serveDeps struct {
	namespaceFile string                                 // production: the projected token directory's namespace file
	runtime       func(k8s.Options) (k8s.Runtime, error) // production: k8s.NewRuntime
	upstream      httpapi.Upstream                       // production: proxy.New(proxy.Options{})
	registry      *prometheus.Registry                   // production: prometheus.NewRegistry()
	recorder      metrics.Recorder                       // production: metrics.NewPrometheus(registry)
	stop          <-chan struct{}                        // production: signal.NotifyContext(...).Done()
}

// serve runs the gateway until stop is closed or a fatal event happens, and returns the exit code.
// ctx is the parent of everything the gateway runs; stop requests a drain.
func serve(ctx context.Context, cfgPath string, deps serveDeps, stdout, stderr io.Writer) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
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
	ready := func() bool { return !draining.Load() && cluster.HasSynced() }
	api := httpapi.New(httpapi.Deps{
		Discovery: cluster,
		Upstream:  deps.upstream,
		Config:    &cfgPtr,
		Recorder:  deps.recorder,
		Logger:    logger,
	})
	apiServer := &http.Server{Handler: api, ReadHeaderTimeout: readHeaderTimeout}
	opsServer := &http.Server{Handler: ops.New(ready, deps.registry), ReadHeaderTimeout: readHeaderTimeout}

	var lc net.ListenConfig
	apiListener, err := lc.Listen(ctx, "tcp", cfg.Server.Listen)
	if err != nil {
		logger.Error("listen", "address", cfg.Server.Listen, "error", err)

		return 1
	}
	opsListener, err := lc.Listen(ctx, "tcp", cfg.Server.OpsListen)
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
	preflightCh := make(chan error, 1)
	go func() { preflightCh <- preflight(runCtx, rt, logger) }()
	syncedCh := make(chan struct{}, 1)

	shutdown := func() {
		draining.Store(true)
		cancelRun()
		drainBound := time.Duration(max(cfg.Limits.CPUSeconds, cfg.Limits.TraceSeconds))*time.Second + drainSlack
		drainCtx, cancel := context.WithTimeout(context.Background(), drainBound)
		defer cancel()
		if err := apiServer.Shutdown(drainCtx); errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("drain deadline passed; closing in-flight connections")
			_ = apiServer.Close()
		}
		opsCtx, cancelOps := context.WithTimeout(context.Background(), opsDrainTimeout)
		defer cancelOps()
		_ = opsServer.Shutdown(opsCtx)
	}

	for {
		select {
		case err := <-preflightCh:
			var fb k8s.ErrForbidden
			if errors.As(err, &fb) {
				logger.Error("preflight forbidden; the ClusterRole lacks a tuple", "resource", fb.Resource, "verb", fb.Verb)
				shutdown()

				return 1
			}
			if err != nil {
				logger.Error("preflight cancelled", "error", err)
				shutdown()

				return 1
			}
			logger.Info("preflight passed; starting informers")
			go cluster.Run(runCtx)
			go waitSynced(runCtx, cluster, syncedCh)
		case <-syncedCh:
			deps.recorder.DiscoverySynced(true)
			logger.Info("discovery synced; ready")
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				continue
			}
			logger.Error("listener failed", "error", err)
			shutdown()

			return 1
		case <-deps.stop:
			logger.Info("stop requested; draining")
			shutdown()

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
