package client

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, handler http.HandlerFunc, credential Credential, verbose *bytes.Buffer) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	o := Options{
		Settings:   settingsFor(t, srv.URL),
		Credential: credential,
		Now:        func() time.Time { now = now.Add(250 * time.Millisecond); return now },
	}
	if verbose != nil {
		o.Verbose = verbose
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestErrorShapes(t *testing.T) {
	envelope := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"effective duration 120s exceeds the limit of 60s","code":"seconds_exceeds_limit"}` + "\n"))
	}
	html := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html><body>upstream secret-host is down</body></html>`))
	}
	empty := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	truncated := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"name":"payments"`))
	}

	cases := []struct {
		name    string
		handler http.HandlerFunc
		message string
		check   func(t *testing.T, err error)
	}{
		{"400 envelope", envelope, "seconds_exceeds_limit: effective duration 120s exceeds the limit of 60s", func(t *testing.T, err error) {
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Fatalf("not an APIError: %v", err)
			}
			if ae.Status != 400 || ae.Code != "seconds_exceeds_limit" {
				t.Fatalf("APIError = %+v", ae)
			}
		}},
		{"502 html", html, "HTTP 502 Bad Gateway: body is not a profgate JSON document", func(t *testing.T, err error) {
			var se *StatusError
			if !errors.As(err, &se) || se.Status != 502 {
				t.Fatalf("not a StatusError 502: %v", err)
			}
			if strings.Contains(err.Error(), "secret-host") || strings.Contains(err.Error(), "<") {
				t.Fatalf("body text leaked: %v", err)
			}
		}},
		{"500 empty", empty, "HTTP 500 Internal Server Error: body is not a profgate JSON document", func(t *testing.T, err error) {
			var se *StatusError
			if !errors.As(err, &se) || se.Status != 500 {
				t.Fatalf("not a StatusError 500: %v", err)
			}
		}},
		{"200 truncated JSON", truncated, "HTTP 200 OK: body is not a profgate JSON document", func(t *testing.T, err error) {
			var se *StatusError
			if !errors.As(err, &se) || se.Status != 200 {
				t.Fatalf("not a StatusError 200: %v", err)
			}
			if strings.Contains(err.Error(), "payments") {
				t.Fatalf("body text leaked: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, tc.handler, nil, nil)
			_, _, err := c.JSON(context.Background(), Request{Method: http.MethodGet, Path: "/v1/namespaces"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tc.message {
				t.Fatalf("message %q, want %q", err.Error(), tc.message)
			}
			tc.check(t, err)
		})
	}
}

func TestTransportErrorNamesNoPath(t *testing.T) {
	// A listener that is closed before the request is made refuses the connection.
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	c, err := New(Options{Settings: settingsFor(t, "http://"+addr)})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/v1/secret-namespace/whoami"})
	if resp != nil {
		_ = resp.Body.Close()
	}
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("not a TransportError: %v", err)
	}
	if te.Origin != "http://"+addr {
		t.Fatalf("Origin = %q", te.Origin)
	}
	msg := err.Error()
	if !strings.Contains(msg, "http://"+addr) {
		t.Fatalf("message does not name scheme, host, and port: %q", msg)
	}
	if strings.Contains(msg, "/v1") || strings.Contains(msg, "secret-namespace") {
		t.Fatalf("message carries the path: %q", msg)
	}
}

func TestResponseBound(t *testing.T) {
	big := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"`))
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxResponseBytes))
		_, _ = w.Write([]byte(`"`))
	}
	c := serve(t, big, nil, nil)
	body, _, err := c.JSON(context.Background(), Request{Method: http.MethodGet, Path: "/v1/namespaces"})
	if err == nil {
		t.Fatalf("a body of %d bytes was read", len(body))
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected a bound error, got %v", err)
	}
}

func TestJSONReturnsBodyVerbatim(t *testing.T) {
	const doc = `{"items": [ {"name":"a"} ], "extra":  1}` + "\n"
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/namespaces" || r.URL.Query().Get("port") != "6060" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte(doc))
	}
	var verbose bytes.Buffer
	c := serve(t, handler, mustToken(t, "tok"), &verbose)
	body, header, err := c.JSON(context.Background(), Request{
		Method: http.MethodGet,
		Path:   "/v1/namespaces",
		Query:  map[string][]string{"port": {"6060"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != doc {
		t.Fatalf("body %q, want it verbatim", body)
	}
	if header.Get("ETag") != `"abc"` {
		t.Fatalf("ETag = %q", header.Get("ETag"))
	}
	line := strings.TrimSpace(verbose.String())
	if !strings.HasPrefix(line, "GET "+c.settings.Server.String()+"/v1/namespaces?port=6060 200 ") {
		t.Fatalf("verbose line = %q", line)
	}
	if strings.Contains(line, "tok") || strings.Contains(line, "Authorization") {
		t.Fatalf("verbose line carries a header: %q", line)
	}
	if !strings.HasSuffix(line, "250ms") {
		t.Fatalf("verbose line lacks the duration from the injected clock: %q", line)
	}
}

func TestDoSendsBodyAndHeaders(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("method %s content-type %q", r.Method, r.Header.Get("Content-Type"))
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		if b.String() != `{"seconds":5}` {
			t.Errorf("body %q", b.String())
		}
		if r.Header.Get("Idempotency-Key") != "k1" {
			t.Errorf("Idempotency-Key = %q", r.Header.Get("Idempotency-Key"))
		}
		w.WriteHeader(http.StatusAccepted)
	}
	c := serve(t, handler, nil, nil)
	resp, err := c.Do(context.Background(), Request{
		Method: http.MethodPost,
		Path:   "/v1/collections",
		Body:   []byte(`{"seconds":5}`),
		Header: http.Header{"Content-Type": {"application/json"}, "Idempotency-Key": {"k1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestTokenCredential(t *testing.T) {
	cases := []struct {
		name, in, want string
		usage          bool
	}{
		{"trimmed", "  tok\n", "Bearer tok", false},
		{"empty", "", "", true},
		{"whitespace only", " \n\t", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := TokenCredential(tc.in)
			if tc.usage {
				if !errors.Is(err, ErrUsage) {
					t.Fatalf("expected a usage error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://gateway.example/v1/whoami", nil)
			if err := c.Apply(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if got := r.Header.Get("Authorization"); got != tc.want {
				t.Fatalf("Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBasicCredentialRefusals(t *testing.T) {
	cases := []struct {
		name, user, password string
		ok                   bool
	}{
		{"plain", "alice", "pw", true},
		{"72-byte password", "alice", strings.Repeat("p", 72), true},
		{"colon in user name", "ali:ce", "pw", false},
		{"73-byte password", "alice", strings.Repeat("p", 73), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := BasicCredential(tc.user, tc.password)
			if !tc.ok {
				if !errors.Is(err, ErrUsage) {
					t.Fatalf("expected a usage error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://gateway.example/v1/whoami", nil)
			if err := c.Apply(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			user, password, ok := r.BasicAuth()
			if !ok || user != tc.user || password != tc.password {
				t.Fatalf("basic auth %q %q %v", user, password, ok)
			}
		})
	}
}

// TestEnvelopeShape pins the whole test the client applies to an error body:
// application/json, one JSON object, a non-empty code, and an error that is a string.
// Anything else is not the gateway's envelope, whatever it looks like.
func TestEnvelopeShape(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		want        *APIError // nil means the body is not envelope-shaped
	}{
		{
			name:        "a full envelope",
			contentType: "application/json",
			body:        `{"error":"the realm does not admit payments","code":"realm_denied"}`,
			want:        &APIError{Status: 403, Code: "realm_denied", Message: "the realm does not admit payments"},
		},
		{name: "no error key", contentType: "application/json", body: `{"code":"realm_denied"}`},
		{name: "a null error", contentType: "application/json", body: `{"code":"realm_denied","error":null}`},
		{name: "an error that is not a string", contentType: "application/json", body: `{"code":"realm_denied","error":5}`},
		{name: "a full envelope under text/plain", contentType: "text/plain", body: `{"error":"denied","code":"realm_denied"}`},
		{name: "a JSON array", contentType: "application/json", body: `[{"error":"denied","code":"realm_denied"}]`},
		{name: "no code key", contentType: "application/json", body: `{"error":"denied"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": {tc.contentType}}}
			got := decodeEnvelope(resp, []byte(tc.body))
			if tc.want == nil {
				if got != nil {
					t.Fatalf("decodeEnvelope(%s) = %+v, want nil", tc.body, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("decodeEnvelope(%s) = nil, want %+v", tc.body, tc.want)
			}
			if got.Status != tc.want.Status || got.Code != tc.want.Code || got.Message != tc.want.Message {
				t.Fatalf("decodeEnvelope = %+v, want %+v", got, tc.want)
			}
			if string(got.Body) != tc.body {
				t.Fatalf("Body = %q, want the bytes as they arrived, %q", got.Body, tc.body)
			}
		})
	}
}

// TestDoSurfacesTheBound proves a body that filled the response bound says so on both paths,
// rather than being discarded into a bare status line:
// those bytes were never read, so there is nothing to classify.
func TestDoSurfacesTheBound(t *testing.T) {
	oversized := strings.Repeat("x", maxResponseBytes+1)
	answer := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(oversized))
		}
	}
	const bound = "body exceeds the 1048576-byte bound this client reads"
	t.Run("Do", func(t *testing.T) {
		c := serve(t, answer(http.StatusBadGateway), nil, nil)
		resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/v1/namespaces"})
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected an error")
		}
		if err.Error() != "HTTP 502 Bad Gateway: "+bound {
			t.Fatalf("message %q, want the bound named", err.Error())
		}
	})
	t.Run("JSON", func(t *testing.T) {
		c := serve(t, answer(http.StatusOK), nil, nil)
		_, _, err := c.JSON(context.Background(), Request{Method: http.MethodGet, Path: "/v1/namespaces"})
		if err == nil {
			t.Fatal("expected an error")
		}
		if err.Error() != "HTTP 200 OK: "+bound {
			t.Fatalf("message %q, want the bound named", err.Error())
		}
	})
}

// TestPlaintextWarningFollowsTheCredential pins the order of the loopback warning
// and the credential it describes:
// a credential that is never obtained is never announced.
func TestPlaintextWarningFollowsTheCredential(t *testing.T) {
	obtained := time.Date(2026, 8, 28, 9, 30, 12, 0, time.UTC)
	now := func() time.Time { return obtained.Add(time.Hour) }

	// build serves one loopback http:// gateway, which is what httptest
	// listens on, with e as the only cached entry.
	build := func(t *testing.T, e Entry) (*Client, *bytes.Buffer, *int) {
		t.Helper()
		requests := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}\n"))
		}))
		t.Cleanup(srv.Close)
		store, _, _ := testStore(t)
		s := settingsFor(t, srv.URL)
		s.CacheName = "prod"
		e.Origin = s.Origin
		if err := store.Write("prod", e); err != nil {
			t.Fatal(err)
		}
		iss, err := NewIssuer(IssuerOptions{Now: now})
		if err != nil {
			t.Fatal(err)
		}
		var warn bytes.Buffer
		c, err := New(Options{Settings: s, Credential: CachedCredential(store, iss, s, now), Now: now, Warn: &warn})
		if err != nil {
			t.Fatal(err)
		}
		return c, &warn, &requests
	}

	t.Run("an expired entry with no refresh token warns about nothing", func(t *testing.T) {
		e := testEntry()
		e.ExpiresAt = obtained.Add(time.Minute)
		e.RefreshToken = ""
		c, warn, requests := build(t, e)
		_, _, err := c.JSON(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"})
		if !errors.Is(err, ErrLoginNeeded) {
			t.Fatalf("err = %v, want %v", err, ErrLoginNeeded)
		}
		if warn.Len() != 0 {
			t.Fatalf("warning = %q, want none: no credential was sent", warn.String())
		}
		if *requests != 0 {
			t.Fatalf("requests = %d, want 0", *requests)
		}
	})

	t.Run("a usable entry warns and sends the request", func(t *testing.T) {
		e := testEntry()
		e.ExpiresAt = obtained.Add(2 * time.Hour)
		c, warn, requests := build(t, e)
		if _, _, err := c.JSON(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"}); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(warn.String(), "profgate: warning: sending a credential over plaintext to http://127.0.0.1:") {
			t.Fatalf("warning = %q, want the loopback warning", warn.String())
		}
		if !strings.HasSuffix(warn.String(), "/v1/whoami\n") {
			t.Fatalf("warning = %q, want it to name the request's URL", warn.String())
		}
		if *requests != 1 {
			t.Fatalf("requests = %d, want 1", *requests)
		}
	})
}
