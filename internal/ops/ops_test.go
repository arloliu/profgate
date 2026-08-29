package ops_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/ops"
)

// get runs one GET against h and returns the status and body.
func get(t *testing.T, h http.Handler, path string, headers map[string]string) (int, string) {
	t.Helper()

	rec := run(t, h, path, headers)
	body, err := io.ReadAll(rec.Result().Body) //nolint:bodyclose // a recorder's body needs no close
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return rec.Code, string(body)
}

// run drives one GET through h in process,
// so a header value no client would agree to send still reaches the handler.
func run(t *testing.T, h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// identifierOf returns the one X-Request-Id value a response carries,
// and fails when it carries none or more than one.
func identifierOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	values := rec.Header().Values("X-Request-Id")
	if len(values) != 1 {
		t.Fatalf("X-Request-Id = %v, want exactly one value", values)
	}

	return values[0]
}

func TestOps(t *testing.T) {
	t.Run("healthz is always 200", func(t *testing.T) {
		h := ops.New(func() bool { return false }, prometheus.NewRegistry())
		if code, _ := get(t, h, "/healthz", nil); code != http.StatusOK {
			t.Fatalf("/healthz = %d, want 200: liveness means the process serves HTTP, nothing more", code)
		}
	})

	t.Run("readyz follows the ready func", func(t *testing.T) {
		var ready atomic.Bool
		h := ops.New(ready.Load, prometheus.NewRegistry())
		if code, _ := get(t, h, "/readyz", nil); code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz before ready = %d, want 503", code)
		}
		ready.Store(true)
		if code, _ := get(t, h, "/readyz", nil); code != http.StatusOK {
			t.Fatalf("/readyz after ready = %d, want 200", code)
		}
	})

	t.Run("metrics exposes the registry", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		metrics.NewPrometheus(reg).DiscoverySynced(true)
		h := ops.New(func() bool { return true }, reg)
		code, body := get(t, h, "/metrics", nil)
		if code != http.StatusOK {
			t.Fatalf("/metrics = %d, want 200", code)
		}
		if !strings.Contains(body, "profgate_discovery_synced 1") {
			t.Fatalf("/metrics lacks profgate_discovery_synced 1:\n%s", body)
		}
	})

	t.Run("no request carries auth", func(t *testing.T) {
		h := ops.New(func() bool { return true }, prometheus.NewRegistry())
		for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
			bare, _ := get(t, h, path, nil)
			withAuth, _ := get(t, h, path, map[string]string{"Authorization": "Bearer garbage"})
			if bare != http.StatusOK || withAuth != http.StatusOK {
				t.Fatalf("%s = %d bare, %d with a bogus credential; want 200 for both: the ops listener checks no authentication", path, bare, withAuth)
			}
		}
	})
}

// TestOpsRequestID pins what the ops listener buys from the identifier:
// every probe and every scrape can be named in a report, and no path writes a record.
func TestOpsRequestID(t *testing.T) {
	// generatedID is the shape of a value the gateway mints.
	generatedID := regexp.MustCompile(`^[0-9a-f]{32}$`)

	t.Run("every path carries one identifier", func(t *testing.T) {
		var ready atomic.Bool
		reg := prometheus.NewRegistry()
		metrics.NewPrometheus(reg).DiscoverySynced(true)
		h := ops.New(ready.Load, reg)
		for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
			sent := identifierOf(t, run(t, h, path, map[string]string{"X-Request-Id": "probe-1"}))
			if sent != "probe-1" {
				t.Errorf("%s echoed %q, want probe-1", path, sent)
			}
			minted := identifierOf(t, run(t, h, path, nil))
			if !generatedID.MatchString(minted) {
				t.Errorf("%s minted %q, want 32 lowercase hexadecimal characters", path, minted)
			}
		}
		ready.Store(true)
		for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
			if got := identifierOf(t, run(t, h, path, nil)); !generatedID.MatchString(got) {
				t.Errorf("%s minted %q once ready, want 32 lowercase hexadecimal characters", path, got)
			}
		}
	})

	t.Run("a value the grammar refuses is replaced", func(t *testing.T) {
		h := ops.New(func() bool { return true }, prometheus.NewRegistry())
		rows := []struct{ name, sent string }{
			{"an empty value", ""},
			{"129 bytes", strings.Repeat("a", 129)},
			{"a space", "one two"},
			{"a carriage return", "one\rtwo"},
		}
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				got := identifierOf(t, run(t, h, "/healthz", map[string]string{"X-Request-Id": row.sent}))
				if !generatedID.MatchString(got) {
					t.Errorf("X-Request-Id = %q, want a generated value", got)
				}
			})
		}
	})

	t.Run("a failing probe is text/plain and named", func(t *testing.T) {
		h := ops.New(func() bool { return false }, prometheus.NewRegistry())
		rec := run(t, h, "/readyz", nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz = %d, want 503", rec.Code)
		}
		identifierOf(t, rec)
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain: the probe reads the status line, never a JSON body", got)
		}
	})

	t.Run("a passing probe is text/plain and named", func(t *testing.T) {
		// The passing bodies get their type from net/http's sniffing,
		// which only a real server performs.
		srv := httptest.NewServer(ops.New(func() bool { return true }, prometheus.NewRegistry()))
		t.Cleanup(srv.Close)
		for _, path := range []string{"/healthz", "/readyz"} {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s = %d, want 200", path, resp.StatusCode)
			}
			if values := resp.Header.Values("X-Request-Id"); len(values) != 1 {
				t.Errorf("%s X-Request-Id = %v, want exactly one value", path, values)
			}
			if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
				t.Errorf("%s Content-Type = %q, want text/plain", path, got)
			}
			if string(body) != "ok\n" {
				t.Errorf("%s body = %q, want ok", path, body)
			}
		}
	})

	t.Run("metrics answers text/plain", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		metrics.NewPrometheus(reg).DiscoverySynced(true)
		rec := run(t, ops.New(func() bool { return true }, reg), "/metrics", nil)
		identifierOf(t, rec)
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", got)
		}
	})
}
