package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

// fakeConsole is a Console that writes what a row programs and remembers every request it saw.
type fakeConsole struct {
	status       int    // 0 means Write only, without WriteHeader
	body         string // written after the status; empty means nothing is written
	cacheControl string // set on the response when non-empty
	location     string // set on the response when non-empty
	mu           sync.Mutex
	requests     []*http.Request
}

func (f *fakeConsole) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.mu.Unlock()
	if f.cacheControl != "" {
		w.Header().Set("Cache-Control", f.cacheControl)
	}
	if f.location != "" {
		w.Header().Set("Location", f.location)
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	if f.body != "" {
		_, _ = w.Write([]byte(f.body))
	}
}

// seen is every request the fake was handed, in order.
func (f *fakeConsole) seen() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]*http.Request(nil), f.requests...)
}

// consoleHarness is the harness with a fake console wired as Deps.Console.
func consoleHarness(c *fakeConsole) *harness {
	h := newHarness(baseTarget())
	h.console = c

	return h
}

// expectUIMetric checks the single Recorder.Request call carries the ui labels and code.
func (h *harness) expectUIMetric(t *testing.T, code string) {
	t.Helper()

	h.expectMetric(t, metrics.EndpointUI, labelNone)
	h.expectMetricCode(t, code)
}

// expectNoAudit checks that no request record was written.
func (h *harness) expectNoAudit(t *testing.T) {
	t.Helper()

	if records := h.audits(t); len(records) != 0 {
		t.Errorf("audit records = %d, want 0: %s", len(records), h.logs.String())
	}
}

func TestConsoleNil(t *testing.T) {
	rows := []struct{ method, path string }{
		{http.MethodGet, "/ui/"},
		{http.MethodGet, "/ui/static/abc/app.js"},
		{http.MethodHead, "/"},
		{http.MethodGet, "/"},
	}
	for _, row := range rows {
		t.Run(row.method+" "+row.path, func(t *testing.T) {
			h := newHarness(baseTarget())
			rec := h.do(t, row.method, row.path)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			if code, _ := errorBodyOf(t, rec); code != "route_unknown" {
				t.Errorf("code = %q, want route_unknown", code)
			}
			h.expectUIMetric(t, "route_unknown")
			h.expectNoAudit(t)
		})
	}
}

func TestConsoleDispatch(t *testing.T) {
	t.Run("dispatch", func(t *testing.T) {
		c := &fakeConsole{status: http.StatusOK, body: "<html>"}
		h := consoleHarness(c)
		rec := h.do(t, http.MethodGet, "/ui/?ns=x")
		seen := c.seen()
		if len(seen) != 1 {
			t.Fatalf("console calls = %d, want 1", len(seen))
		}
		if got := seen[0].URL.String(); got != "/ui/?ns=x" {
			t.Errorf("console saw %q, want /ui/?ns=x", got)
		}
		if seen[0].Method != http.MethodGet {
			t.Errorf("console saw method %q, want GET", seen[0].Method)
		}
		if rec.Code != http.StatusOK || rec.Body.String() != "<html>" {
			t.Errorf("response = %d %q, want 200 <html>", rec.Code, rec.Body.String())
		}
		h.expectUIMetric(t, codeOK)
		h.expectNoAudit(t)
	})

	t.Run("root", func(t *testing.T) {
		c := &fakeConsole{status: http.StatusFound, location: "/ui/"}
		h := consoleHarness(c)
		rec := h.do(t, http.MethodGet, "/")
		if len(c.seen()) != 1 {
			t.Fatalf("console calls = %d, want 1", len(c.seen()))
		}
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/" {
			t.Errorf("response = %d Location %q, want 302 /ui/", rec.Code, rec.Header().Get("Location"))
		}
		h.expectUIMetric(t, codeOK)
		h.expectNoAudit(t)
	})

	t.Run("deeper paths", func(t *testing.T) {
		c := &fakeConsole{status: http.StatusOK}
		h := consoleHarness(c)
		h.do(t, http.MethodGet, "/ui/static/h/vendor/preact/preact.module.js")
		if len(c.seen()) != 1 {
			t.Errorf("console calls = %d, want 1", len(c.seen()))
		}
		h.expectUIMetric(t, codeOK)
	})

	t.Run("not the prefix", func(t *testing.T) {
		for _, path := range []string{"/ui", "/uix", "/v1/ui/"} {
			t.Run(path, func(t *testing.T) {
				c := &fakeConsole{status: http.StatusOK}
				h := consoleHarness(c)
				rec := h.do(t, http.MethodGet, path)
				if len(c.seen()) != 0 {
					t.Errorf("console calls = %d, want 0", len(c.seen()))
				}
				h.expectError(t, rec, http.StatusNotFound, "route_unknown")
				h.expectMetric(t, metrics.EndpointProfile, labelNone)
			})
		}
	})

	t.Run("before /auth/ and /v1", func(t *testing.T) {
		c := &fakeConsole{status: http.StatusOK}
		h := consoleHarness(c)
		a := refuse(&auth.Failure{Status: http.StatusUnauthorized, Reason: auth.ReasonMissing})
		h.auth = a
		routes := &fakeRoutes{outcome: auth.RouteOutcome{Status: http.StatusUnauthorized, Code: "unauthenticated"}}
		h.routes = routes
		h.configure(func(cfg *config.Config) {
			cfg.Auth.Mode = "basic"
			cfg.Auth.AnonymousRealm = ""
			cfg.Realms = map[string]config.Realm{"developer": {}}
		})
		rec := h.do(t, http.MethodGet, "/ui/")
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if len(c.seen()) != 1 {
			t.Errorf("console calls = %d, want 1", len(c.seen()))
		}
		if a.called() != 0 {
			t.Errorf("Authenticate calls = %d, want 0", a.called())
		}
		routes.mu.Lock()
		defer routes.mu.Unlock()
		if routes.calls != 0 {
			t.Errorf("ServeAuth calls = %d, want 0", routes.calls)
		}
	})

	t.Run("no readiness step", func(t *testing.T) {
		c := &fakeConsole{status: http.StatusOK}
		h := consoleHarness(c)
		h.disc.synced = false
		rec := h.do(t, http.MethodGet, "/ui/")
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if len(c.seen()) != 1 {
			t.Errorf("console calls = %d, want 1", len(c.seen()))
		}
	})

	t.Run("cache-control", func(t *testing.T) {
		const immutable = "public, max-age=31536000, immutable"
		c := &fakeConsole{status: http.StatusOK, cacheControl: immutable}
		h := consoleHarness(c)
		rec := httptest.NewRecorder()
		h.handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ui/static/h/app.js", nil))
		if got := rec.Header().Get("Cache-Control"); got != immutable {
			t.Errorf("Cache-Control = %q, want %q", got, immutable)
		}
		h.expectUIMetric(t, codeOK)
	})
}

func TestConsoleStatusCodes(t *testing.T) {
	rows := []struct {
		name   string
		status int
		code   string
	}{
		{"404 from the console", http.StatusNotFound, "route_unknown"},
		{"405 from the console", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"500", http.StatusInternalServerError, "internal_error"},
		{"503", http.StatusServiceUnavailable, "internal_error"},
		{"400", http.StatusBadRequest, "internal_error"},
		{"418", http.StatusTeapot, "internal_error"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := &fakeConsole{status: row.status, body: "x"}
			h := consoleHarness(c)
			rec := h.do(t, http.MethodGet, "/ui/")
			if rec.Code != row.status {
				t.Errorf("status = %d, want %d", rec.Code, row.status)
			}
			h.expectUIMetric(t, row.code)
			h.expectNoAudit(t)
		})
	}

	t.Run("body without WriteHeader", func(t *testing.T) {
		c := &fakeConsole{body: "x"}
		h := consoleHarness(c)
		rec := h.do(t, http.MethodGet, "/ui/")
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		h.expectUIMetric(t, codeOK)
	})
}

func TestConsoleCode(t *testing.T) {
	allowed := map[string]bool{codeOK: true, "route_unknown": true, "method_not_allowed": true, "internal_error": true}
	rows := []struct {
		status int
		want   string
	}{
		{200, codeOK}, {204, codeOK}, {302, codeOK}, {304, codeOK},
		{404, "route_unknown"}, {405, "method_not_allowed"},
		{400, "internal_error"}, {500, "internal_error"}, {503, "internal_error"},
	}
	for _, row := range rows {
		t.Run(strconv.Itoa(row.status), func(t *testing.T) {
			got := consoleCode(row.status)
			if got != row.want {
				t.Errorf("consoleCode(%d) = %q, want %q", row.status, got, row.want)
			}
			if !allowed[got] {
				t.Errorf("consoleCode(%d) = %q is outside the closed set", row.status, got)
			}
		})
	}
	t.Run("every status stays in the closed set", func(t *testing.T) {
		for status := 100; status < 600; status++ {
			if got := consoleCode(status); !allowed[got] {
				t.Errorf("consoleCode(%d) = %q is outside the closed set", status, got)
			}
		}
	})
}

func TestWriteErrorExported(t *testing.T) {
	exported := httptest.NewRecorder()
	WriteError(exported, http.StatusNotFound, "route_unknown", "no such route")

	failed := httptest.NewRecorder()
	q := &request{}
	q.fail(failed, &requestError{status: http.StatusNotFound, code: "route_unknown", message: "no such route"})

	if exported.Code != failed.Code {
		t.Errorf("status = %d, want %d", exported.Code, failed.Code)
	}
	if exported.Body.String() != failed.Body.String() {
		t.Errorf("body = %q, want %q", exported.Body.String(), failed.Body.String())
	}
	if len(exported.Header()) != len(failed.Header()) {
		t.Errorf("headers = %v, want %v", exported.Header(), failed.Header())
	}
	for name, values := range failed.Header() {
		if got := exported.Header()[name]; len(got) != len(values) || got[0] != values[0] {
			t.Errorf("header %s = %v, want %v", name, got, values)
		}
	}
}
