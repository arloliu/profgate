// Command testapp is the workload the end-to-end suite profiles through the gateway:
// the standard net/http/pprof handlers on :6060 and :6061, a readiness probe the harness can flip,
// and a count of profile requests, in total and per listener,
// so a scenario can prove none arrived or which port one arrived on.
//
// "testapp sleep <seconds>" only sleeps; the Deployment's preStop hook runs it
// because the distroless image has no sleep binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second

	// One shared handler is served on both addresses:
	// the container port pprof and, for client-selected ports, pprof-alt.
	pprofAddr    = ":6060"
	pprofAltAddr = ":6061"
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

// app is the test application's state: the probe result, the profile request total,
// and the same count per listen address so a scenario can tell which port a request arrived on.
type app struct {
	healthy atomic.Bool
	pprof   atomic.Int64
	mu      sync.Mutex
	perAddr map[string]int64
}

// hitsResponse is the body of GET /hits.
type hitsResponse struct {
	Pprof int64            `json:"pprof"`
	Hits  map[string]int64 `json:"hits"`
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

// counted increments the profile request total and the count of the listener
// the request arrived on, keyed ":<port>", before handing the request to next.
func (a *app) counted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.pprof.Add(1)
		if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			if _, port, err := net.SplitHostPort(addr.String()); err == nil {
				a.mu.Lock()
				a.perAddr[":"+port]++
				a.mu.Unlock()
			}
		}
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
	a.mu.Lock()
	perAddr := make(map[string]int64, len(a.perAddr))
	for addr, n := range a.perAddr {
		perAddr[addr] = n
	}
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(hitsResponse{Pprof: a.pprof.Load(), Hits: perAddr})
}

// serve runs one HTTP server per listen address, sharing the handler, until ctx is done
// or the first listener fails, then drains them all.
func serve(ctx context.Context, logger *slog.Logger) error {
	listenAddrs := []string{pprofAddr, pprofAltAddr}
	a := &app{perAddr: make(map[string]int64, len(listenAddrs))}
	a.healthy.Store(true)
	for _, addr := range listenAddrs {
		a.perAddr[addr] = 0
	}
	handler := a.handler()

	errCh := make(chan error, len(listenAddrs))
	servers := make([]*http.Server, 0, len(listenAddrs))
	for _, addr := range listenAddrs {
		srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
		servers = append(servers, srv)
		go func() {
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("listen %s: %w", addr, err)
			}
		}()
		logger.Info("listening", "address", addr)
	}

	var listenErr error
	select {
	case listenErr = <-errCh:
	case <-ctx.Done():
	}
	logger.Info("stop requested; draining")
	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(drainCtx); err != nil && listenErr == nil {
			listenErr = fmt.Errorf("shutdown %s: %w", srv.Addr, err)
		}
	}
	return listenErr
}
