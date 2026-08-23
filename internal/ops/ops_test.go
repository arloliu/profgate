package ops_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body) //nolint:bodyclose // a recorder's body needs no close
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return rec.Code, string(body)
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
