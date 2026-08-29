package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/proxy"
)

// generatedID is the shape of a value the gateway mints: 32 lowercase hexadecimal characters.
var generatedID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// clientID is a value the grammar admits, used wherever a row needs the client's own text back.
const clientID = "client-request-1"

// send runs one request through a fresh handler,
// adding one X-Request-Id header per value the row names.
// The request is built in process
// because a value carrying CR, LF, or a non-ASCII byte is one no HTTP client would agree to send.
func (h *harness) send(t *testing.T, method, target string, ids ...string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	for _, id := range ids {
		req.Header.Add(requestIDHeader, id)
	}
	rec := httptest.NewRecorder()
	h.handler().ServeHTTP(rec, req)

	return rec
}

// identifierOf returns the one identifier a response carries,
// and fails when it carries none or more than one.
func identifierOf(t *testing.T, header http.Header) string {
	t.Helper()

	values := header.Values(requestIDHeader)
	if len(values) != 1 {
		t.Fatalf("X-Request-Id = %v, want exactly one value", values)
	}

	return values[0]
}

// auditAttrs returns the attribute keys of every "request" record in the order they were written,
// with the keys log/slog writes ahead of them removed.
func (h *harness) auditAttrs(t *testing.T) [][]string {
	t.Helper()

	var records [][]string
	for line := range strings.SplitSeq(strings.TrimSpace(h.logs.String()), "\n") {
		if line == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		if _, err := dec.Token(); err != nil {
			t.Fatalf("log line %q is not a JSON object: %v", line, err)
		}
		var keys []string
		message := ""
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			key, ok := tok.(string)
			if !ok {
				t.Fatalf("log line %q: key %v is not a string", line, tok)
			}
			var value json.RawMessage
			if err := dec.Decode(&value); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			if key == "msg" {
				message = string(value)

				continue
			}
			if key == "time" || key == "level" {
				continue
			}
			keys = append(keys, key)
		}
		if message == `"request"` {
			records = append(records, keys)
		}
	}

	return records
}

// TestRequestIDGrammar is the grammar of *Request identifier*:
// exactly one header of 1 to 128 bytes drawn from [A-Za-z0-9._-] is echoed,
// and every other request is answered with a generated value rather than refused.
func TestRequestIDGrammar(t *testing.T) {
	// The set is spelled out so a row proves every character of it survives.
	everyCharacter := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

	rows := []struct {
		name string
		sent []string
		echo bool
	}{
		{"one byte", []string{"a"}, true},
		{"128 bytes", []string{strings.Repeat("a", 128)}, true},
		{"every character of the set", []string{everyCharacter}, true},
		{"no header", nil, false},
		{"an empty value", []string{""}, false},
		{"129 bytes", []string{strings.Repeat("a", 129)}, false},
		{"a space", []string{"one two"}, false},
		{"a colon", []string{"one:two"}, false},
		{"a carriage return", []string{"one\rtwo"}, false},
		{"a line feed", []string{"one\ntwo"}, false},
		{"a non-ASCII byte", []string{"one\xfftwo"}, false},
		{"two headers", []string{"one", "two"}, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(baseTarget())
			rec := h.send(t, http.MethodGet, targetsPath, row.sent...)
			got := identifierOf(t, rec.Header())
			if row.echo {
				if got != row.sent[0] {
					t.Errorf("X-Request-Id = %q, want the client's %q", got, row.sent[0])
				}

				return
			}
			if !generatedID.MatchString(got) {
				t.Errorf("X-Request-Id = %q, want 32 lowercase hexadecimal characters", got)
			}
			for _, sent := range row.sent {
				if sent != "" && strings.Contains(got, sent) {
					t.Errorf("X-Request-Id = %q reflects the refused value %q", got, sent)
				}
			}
		})
	}

	t.Run("two requests with no header differ", func(t *testing.T) {
		h := newHarness(baseTarget())
		first := identifierOf(t, h.send(t, http.MethodGet, targetsPath).Header())
		second := identifierOf(t, h.send(t, http.MethodGet, targetsPath).Header())
		if first == second {
			t.Errorf("two generated identifiers are both %q: each request names itself", first)
		}
	})
}

// TestRequestIDSurfaces drives the surfaces *Request identifier* lists:
// every answer the gateway writes carries the header, whoever wrote the body.
func TestRequestIDSurfaces(t *testing.T) {
	t.Run("a targets 200", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.send(t, http.MethodGet, targetsPath, clientID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if got := identifierOf(t, rec.Header()); got != clientID {
			t.Errorf("X-Request-Id = %q, want %q", got, clientID)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	})

	t.Run("an error envelope", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.send(t, http.MethodGet, "/v1/nowhere", clientID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if got := identifierOf(t, rec.Header()); got != clientID {
			t.Errorf("X-Request-Id = %q, want %q", got, clientID)
		}
	})

	t.Run("a 405 beside Allow", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.send(t, http.MethodDelete, targetsPath, clientID)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Errorf("Allow = %q, want GET", got)
		}
		if got := identifierOf(t, rec.Header()); got != clientID {
			t.Errorf("X-Request-Id = %q, want %q", got, clientID)
		}
	})

	t.Run("a console file keeps its own cache policy", func(t *testing.T) {
		const policy = "no-cache"
		h := consoleHarness(&fakeConsole{status: http.StatusOK, body: "console-bytes", cacheControl: policy})
		rec := h.send(t, http.MethodGet, "/ui/app.js", clientID)
		if got := identifierOf(t, rec.Header()); got != clientID {
			t.Errorf("X-Request-Id = %q, want %q", got, clientID)
		}
		if got := rec.Header().Get("Cache-Control"); got != policy {
			t.Errorf("Cache-Control = %q, want the console's own %q", got, policy)
		}
	})

	t.Run("a console 404", func(t *testing.T) {
		h := consoleHarness(&fakeConsole{status: http.StatusNotFound, body: string(ErrorEnvelope("route_unknown", "no such route"))})
		rec := h.send(t, http.MethodGet, "/ui/missing", clientID)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if got := identifierOf(t, rec.Header()); got != clientID {
			t.Errorf("X-Request-Id = %q, want %q", got, clientID)
		}
	})

	t.Run("an /auth/ 302", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) {
			cfg.Auth.Mode = "oidc"
			cfg.Auth.AnonymousRealm = ""
		})
		h.routes = &fakeRoutes{outcome: auth.RouteOutcome{Status: http.StatusFound, Code: codeAuthRedirect, Principal: noPrincipal}}
		rec := h.send(t, http.MethodGet, "/auth/login", clientID)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if got := identifierOf(t, rec.Header()); got != clientID {
			t.Errorf("X-Request-Id = %q, want %q", got, clientID)
		}
	})

	t.Run("a forwarded upstream response", func(t *testing.T) {
		// The upstream's own identifier is not one of the five headers the proxy forwards,
		// so the gateway's value is what the client reads and the upstream's reaches nothing.
		tr := newTrap(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(requestIDHeader, "upstream-value")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("trap-body"))
		})
		h := newHarness(tr.target())
		h.upstream = proxy.New(proxy.Options{})
		rec := h.send(t, http.MethodGet, profilePath+"heap", clientID)
		if rec.Code != http.StatusOK || rec.Body.String() != "trap-body" {
			t.Fatalf("status %d body %q, want 200 trap-body", rec.Code, rec.Body.String())
		}
		if got := identifierOf(t, rec.Header()); got != clientID {
			t.Errorf("X-Request-Id = %q, want the gateway's %q", got, clientID)
		}
		for name, values := range rec.Header() {
			for _, value := range values {
				if strings.Contains(value, "upstream-value") {
					t.Errorf("header %s carries the upstream's identifier: %q", name, value)
				}
			}
		}
		if strings.Contains(rec.Body.String(), "upstream-value") {
			t.Errorf("body carries the upstream's identifier: %q", rec.Body.String())
		}
	})
}

// TestRequestIDAudit is the record half of *Logging*:
// every audit record names the response's identifier first,
// and the responses that write no record still carry the header.
func TestRequestIDAudit(t *testing.T) {
	// expectFirst checks that the harness wrote exactly one record naming id first.
	expectFirst := func(t *testing.T, h *harness, id string) {
		t.Helper()

		attrs := h.auditAttrs(t)
		if len(attrs) != 1 {
			t.Fatalf("audit records = %d, want 1: %s", len(attrs), h.logs.String())
		}
		if attrs[0][0] != "requestId" {
			t.Errorf("audit attributes = %v, want requestId first", attrs[0])
		}
		records := h.audits(t)
		if got := records[0]["requestId"]; got != id {
			t.Errorf("audit requestId = %v, want the response's %q", got, id)
		}
	}

	t.Run("a targets request", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.send(t, http.MethodGet, targetsPath, clientID)
		expectFirst(t, h, identifierOf(t, rec.Header()))
	})

	t.Run("a generated identifier reaches the record", func(t *testing.T) {
		// A request that sent no header is where a second call to RequestID would show:
		// the response and the record would name two different requests.
		h := newHarness(baseTarget())
		rec := h.send(t, http.MethodGet, targetsPath)
		id := identifierOf(t, rec.Header())
		if !generatedID.MatchString(id) {
			t.Fatalf("X-Request-Id = %q, want a generated value", id)
		}
		expectFirst(t, h, id)
	})

	t.Run("a pgo request", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.send(t, http.MethodGet, pgoPath, clientID)
		expectFirst(t, h, identifierOf(t, rec.Header()))
		if records := h.audits(t); records[0]["collection"] == nil {
			t.Fatalf("record %v is not the pgo shape", records[0])
		}
	})

	t.Run("an /auth/ request", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.configure(func(cfg *config.Config) {
			cfg.Auth.Mode = "oidc"
			cfg.Auth.AnonymousRealm = ""
		})
		h.routes = &fakeRoutes{outcome: auth.RouteOutcome{Status: http.StatusFound, Code: codeAuthRedirect, Principal: noPrincipal}}
		rec := h.send(t, http.MethodGet, "/auth/login", clientID)
		expectFirst(t, h, identifierOf(t, rec.Header()))
		if records := h.audits(t); records[0]["route"] != "auth_login" {
			t.Fatalf("record %v is not the /auth/ shape", records[0])
		}
	})

	t.Run("a console request writes none", func(t *testing.T) {
		h := consoleHarness(&fakeConsole{status: http.StatusOK, body: "console-bytes"})
		rec := h.send(t, http.MethodGet, "/ui/", clientID)
		if got := identifierOf(t, rec.Header()); got != clientID {
			t.Errorf("X-Request-Id = %q, want %q", got, clientID)
		}
		h.expectNoAudit(t)
	})

	t.Run("/v1/auth writes none", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.send(t, http.MethodGet, "/v1/auth", clientID)
		if got := identifierOf(t, rec.Header()); got != clientID {
			t.Errorf("X-Request-Id = %q, want %q", got, clientID)
		}
		h.expectNoAudit(t)
	})

	t.Run("no metrics label is built from the identifier", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.send(t, http.MethodGet, targetsPath, clientID)
		requests, _, _ := h.rec.snapshot()
		if len(requests) != 1 {
			t.Fatalf("Recorder.Request calls = %d, want 1: %v", len(requests), requests)
		}
		for _, label := range []string{string(requests[0].endpoint), requests[0].profile, requests[0].code} {
			if strings.Contains(label, clientID) {
				t.Errorf("metrics label %q is built from the identifier: it differs on every request", label)
			}
		}
	})
}

// TestWithRequestIDContext pins what the middleware hands the handler it wraps:
// the value it set on the response, so a handler names the same one.
func TestWithRequestIDContext(t *testing.T) {
	var seen string
	handler := WithRequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = requestIDFrom(r.Context())
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	req.Header.Set(requestIDHeader, clientID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := identifierOf(t, rec.Header()); got != clientID {
		t.Fatalf("X-Request-Id = %q, want %q", got, clientID)
	}
	if seen != clientID {
		t.Errorf("context identifier = %q, want the response's %q", seen, clientID)
	}
}
