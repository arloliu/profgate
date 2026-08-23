// Command testapp is the workload the end-to-end suite profiles through the gateway:
// the standard net/http/pprof handlers on :6060, a readiness probe the harness can
// flip, and a count of profile requests so a scenario can prove none arrived.
//
// "testapp sleep <seconds>" only sleeps; the Deployment's preStop hook runs it
// because the distroless image has no sleep binary.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	listenAddr        = ":6060"
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches the "sleep" helper or serves until SIGINT or SIGTERM, and returns the exit code.
func run(args []string) int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(args) > 0 && args[0] == "sleep" {
		return runSleep(args[1:], logger)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, logger); err != nil {
		logger.Error("serve", "error", err)
		return 1
	}
	return 0
}

// runSleep sleeps for the whole number of seconds in args[0].
func runSleep(args []string, logger *slog.Logger) int {
	if len(args) != 1 {
		logger.Error("usage: testapp sleep <seconds>")
		return 2
	}
	seconds, err := strconv.Atoi(args[0])
	if err != nil || seconds < 0 {
		logger.Error("usage: testapp sleep <seconds>", "got", args[0])
		return 2
	}
	time.Sleep(time.Duration(seconds) * time.Second)
	return 0
}

// app is the test application's state: the probe result and the profile request count.
type app struct {
	healthy atomic.Bool
	pprof   atomic.Int64
}

// handler builds the routes: pprof under /debug/pprof/ counted per request,
// the readiness probe and its switches, and the counter readout.
func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", a.counted(http.HandlerFunc(pprof.Index)))
	mux.Handle("/debug/pprof/cmdline", a.counted(http.HandlerFunc(pprof.Cmdline)))
	mux.Handle("/debug/pprof/profile", a.counted(http.HandlerFunc(pprof.Profile)))
	mux.Handle("/debug/pprof/symbol", a.counted(http.HandlerFunc(pprof.Symbol)))
	mux.Handle("/debug/pprof/trace", a.counted(http.HandlerFunc(pprof.Trace)))
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("POST /healthz/fail", a.setHealth(false))
	mux.HandleFunc("POST /healthz/pass", a.setHealth(true))
	mux.HandleFunc("GET /hits", a.hits)
	return mux
}

// counted increments the profile request counter before handing the request to next.
func (a *app) counted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.pprof.Add(1)
		next.ServeHTTP(w, r)
	})
}

func (a *app) healthz(w http.ResponseWriter, _ *http.Request) {
	if !a.healthy.Load() {
		http.Error(w, "failing", http.StatusServiceUnavailable)
		return
	}
	_, _ = fmt.Fprintln(w, "ok")
}

func (a *app) setHealth(healthy bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		a.healthy.Store(healthy)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *app) hits(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"pprof": a.pprof.Load()})
}

// serve runs the HTTP server until ctx is done, then drains it.
func serve(ctx context.Context, logger *slog.Logger) error {
	a := &app{}
	a.healthy.Store(true)
	srv := &http.Server{Addr: listenAddr, Handler: a.handler(), ReadHeaderTimeout: readHeaderTimeout}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("listening", "address", listenAddr)

	select {
	case err := <-errCh:
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	case <-ctx.Done():
	}
	logger.Info("stop requested; draining")
	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
