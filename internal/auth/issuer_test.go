package auth

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const wellKnown = "/.well-known/openid-configuration"

// discoveryServer serves one discovery document, built by edit from a valid
// default, and hands back a client that trusts the server through the
// transport seam.
func discoveryServer(t *testing.T, edit func(doc map[string]any)) (*httptest.Server, *issuerClient) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wellKnown {
			http.NotFound(w, r)

			return
		}
		doc := map[string]any{
			"issuer":                 srv.URL,
			"jwks_uri":               srv.URL + "/keys",
			"authorization_endpoint": srv.URL + "/auth",
			"token_endpoint":         srv.URL + "/token",
			"end_session_endpoint":   srv.URL + "/logout",
		}
		if edit != nil {
			edit(doc)
		}
		writeJSON(t, w, doc)
	}))
	t.Cleanup(srv.Close)

	return srv, seamClient(t, srv)
}

// seamClient builds an issuer client over the server's own transport.
func seamClient(t *testing.T, srv *httptest.Server) *issuerClient {
	t.Helper()
	c, err := newIssuerClient(issuerOptions{Transport: srv.Client().Transport})
	if err != nil {
		t.Fatalf("newIssuerClient: %v", err)
	}

	return c
}

// caFile writes the server's certificate to a file the CA option can name.
func caFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	type row struct {
		name    string
		edit    func(doc map[string]any)
		browser bool
		wantErr []string // substrings the error must name; nil means success
		check   func(t *testing.T, doc discoveryDocument)
	}
	rows := []row{
		{
			name:    "issuer mismatch",
			edit:    func(doc map[string]any) { doc["issuer"] = doc["issuer"].(string) + "/" },
			wantErr: []string{"issuer"},
			check:   nil,
		},
		{
			name:    "jwks_uri plaintext",
			edit:    func(doc map[string]any) { doc["jwks_uri"] = "http://keys.example/keys" },
			wantErr: []string{"jwks_uri"},
		},
		{
			name:    "token_endpoint userinfo",
			edit:    func(doc map[string]any) { doc["token_endpoint"] = "https://u@host/token" },
			browser: true,
			wantErr: []string{"token_endpoint"},
		},
		{
			name:    "endpoint fragment",
			edit:    func(doc map[string]any) { doc["authorization_endpoint"] = "https://host/auth#x" },
			browser: true,
			wantErr: []string{"authorization_endpoint"},
		},
		{
			name:    "endpoint relative",
			edit:    func(doc map[string]any) { doc["jwks_uri"] = "/keys" },
			wantErr: []string{"jwks_uri"},
		},
		{
			name: "browser endpoints optional",
			edit: func(doc map[string]any) {
				delete(doc, "authorization_endpoint")
				delete(doc, "token_endpoint")
				delete(doc, "end_session_endpoint")
			},
		},
		{
			name:    "browser endpoints required",
			edit:    func(doc map[string]any) { delete(doc, "token_endpoint") },
			browser: true,
			wantErr: []string{"token_endpoint"},
		},
		{
			name:    "end_session optional",
			edit:    func(doc map[string]any) { delete(doc, "end_session_endpoint") },
			browser: true,
			check: func(t *testing.T, doc discoveryDocument) {
				if doc.EndSessionEndpoint != "" {
					t.Fatalf("EndSessionEndpoint = %q, want empty", doc.EndSessionEndpoint)
				}
				if doc.TokenEndpoint == "" || doc.AuthorizationEndpoint == "" || doc.JWKSURI == "" {
					t.Fatalf("endpoints not recorded: %+v", doc)
				}
			},
		},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			srv, c := discoveryServer(t, tc.edit)
			doc, err := discover(context.Background(), c, srv.URL, tc.browser)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("discover: %v", err)
				}
				if tc.check != nil {
					tc.check(t, doc)
				}

				return
			}
			if err == nil {
				t.Fatalf("discover succeeded, want an error naming %v", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
		})
	}

	t.Run("issuer mismatch names both values", func(t *testing.T) {
		srv, c := discoveryServer(t, func(doc map[string]any) { doc["issuer"] = doc["issuer"].(string) + "/" })
		_, err := discover(context.Background(), c, srv.URL, false)
		if err == nil || !strings.Contains(err.Error(), srv.URL+"/") || !strings.Contains(err.Error(), srv.URL+`"`) {
			t.Fatalf("error %v, want it to name %q and %q", err, srv.URL, srv.URL+"/")
		}
	})

	t.Run("plaintext issuer", func(t *testing.T) {
		srv, c := discoveryServer(t, nil)
		_, err := discover(context.Background(), c, "http://"+strings.TrimPrefix(srv.URL, "https://"), false)
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("error %v, want one naming https", err)
		}
	})
}

// redirectServer answers the discovery path with hops redirects, then the
// document; a hop to plaintext rewrites the scheme.
func redirectServer(t *testing.T, hops int, plaintext bool) (*httptest.Server, *issuerClient) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hop := 0
		if strings.HasPrefix(r.URL.Path, "/hop/") {
			hop = int(r.URL.Path[len("/hop/")] - '0')
		}
		if hop < hops {
			next := srv.URL + "/hop/" + string(rune('0'+hop+1))
			if plaintext {
				next = "http://" + strings.TrimPrefix(next, "https://")
			}
			http.Redirect(w, r, next, http.StatusFound)

			return
		}
		writeJSON(t, w, map[string]any{"issuer": srv.URL, "jwks_uri": srv.URL + "/keys"})
	}))
	t.Cleanup(srv.Close)

	return srv, seamClient(t, srv)
}

func TestIssuerClientRedirects(t *testing.T) {
	t.Run("redirect to plaintext", func(t *testing.T) {
		srv, c := redirectServer(t, 1, true)
		if _, err := discover(context.Background(), c, srv.URL, false); err == nil {
			t.Fatal("a redirect to http:// was followed")
		}
	})
	t.Run("three redirects", func(t *testing.T) {
		srv, c := redirectServer(t, 3, false)
		if _, err := discover(context.Background(), c, srv.URL, false); err != nil {
			t.Fatalf("three redirects must be followed: %v", err)
		}
	})
	t.Run("fourth redirect", func(t *testing.T) {
		srv, c := redirectServer(t, 4, false)
		if _, err := discover(context.Background(), c, srv.URL, false); err == nil {
			t.Fatal("a fourth redirect was followed")
		}
	})
	t.Run("token endpoint redirect", func(t *testing.T) {
		var second atomic.Int32
		var srv *httptest.Server
		srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/token":
				http.Redirect(w, r, srv.URL+"/second", http.StatusTemporaryRedirect)
			case "/second":
				second.Add(1)
				w.WriteHeader(http.StatusOK)
			}
		}))
		t.Cleanup(srv.Close)
		c := seamClient(t, srv)
		form := url.Values{"client_secret": {"s3cret"}}
		if _, _, err := c.postForm(context.Background(), srv.URL+"/token", form); err == nil {
			t.Fatal("a token endpoint redirect was followed")
		}
		if n := second.Load(); n != 0 {
			t.Fatalf("the redirect target saw %d requests; the secret was replayed", n)
		}
	})
}

// bodyServer answers every path with body.
func bodyServer(t *testing.T, body string) (*httptest.Server, *issuerClient) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	return srv, seamClient(t, srv)
}

// jsonOfSize builds a valid JSON object of exactly n bytes.
func jsonOfSize(n int) string {
	const frame = `{"a":""}`

	return `{"a":"` + strings.Repeat("x", n-len(frame)) + `"}`
}

func TestIssuerClientBody(t *testing.T) {
	rows := []struct {
		name string
		body string
		ok   bool
	}{
		{"body limit", jsonOfSize(1<<20 + 1), false},
		{"body at limit", jsonOfSize(1 << 20), true},
		{"trailing value", `{} {}`, false},
		{"trailing whitespace", "{}\n", true},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			srv, c := bodyServer(t, tc.body)
			var into map[string]any
			err := c.getJSON(context.Background(), srv.URL+"/x", &into)
			if tc.ok && err != nil {
				t.Fatalf("getJSON: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("getJSON accepted the body")
			}
		})
	}
}

func TestIssuerClientTransport(t *testing.T) {
	t.Run("CA file", func(t *testing.T) {
		srv, _ := bodyServer(t, `{}`)
		c, err := newIssuerClient(issuerOptions{CAFile: caFile(t, srv)})
		if err != nil {
			t.Fatal(err)
		}
		var into map[string]any
		if err := c.getJSON(context.Background(), srv.URL+"/x", &into); err != nil {
			t.Fatalf("with the server certificate in CAFile: %v", err)
		}
		bare, err := newIssuerClient(issuerOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := bare.getJSON(context.Background(), srv.URL+"/x", &into); err == nil {
			t.Fatal("a certificate outside the pool was accepted")
		}
	})

	t.Run("CA file without a certificate", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := newIssuerClient(issuerOptions{CAFile: path}); err == nil {
			t.Fatal("a CA file holding no certificate was accepted")
		}
	})

	t.Run("timeouts", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(6 * time.Second):
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(srv.Close)
		c, err := newIssuerClient(issuerOptions{CAFile: caFile(t, srv)})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		var into map[string]any
		err = c.getJSON(context.Background(), srv.URL+"/x", &into)
		if err == nil {
			t.Fatal("a server that never sends headers was waited for")
		}
		if d := time.Since(start); d > 5500*time.Millisecond {
			t.Fatalf("gave up after %v, want within 5.5s", d)
		}
	})

	t.Run("no environment proxy", func(t *testing.T) {
		t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
		srv, _ := bodyServer(t, `{}`)
		c, err := newIssuerClient(issuerOptions{CAFile: caFile(t, srv)})
		if err != nil {
			t.Fatal(err)
		}
		var into map[string]any
		if err := c.getJSON(context.Background(), srv.URL+"/x", &into); err != nil {
			t.Fatalf("the environment proxy was used: %v", err)
		}
		// The transport itself must carry no proxy function: Go's
		// environment proxy skips loopback addresses, so the request above
		// alone cannot tell a nil Proxy from ProxyFromEnvironment.
		tr, ok := c.get.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport is %T, want *http.Transport", c.get.Transport)
		}
		if tr.Proxy != nil {
			t.Fatal("transport reads a proxy from the environment")
		}
	})

	t.Run("configured proxy", func(t *testing.T) {
		srv, _ := bodyServer(t, `{}`)
		var connects atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect {
				http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)

				return
			}
			connects.Add(1)
			tunnel(t, w, r.Host)
		}))
		t.Cleanup(proxy.Close)
		c, err := newIssuerClient(issuerOptions{CAFile: caFile(t, srv), HTTPProxy: proxy.URL})
		if err != nil {
			t.Fatal(err)
		}
		var into map[string]any
		if err := c.getJSON(context.Background(), srv.URL+"/x", &into); err != nil {
			t.Fatalf("through the configured proxy: %v", err)
		}
		if n := connects.Load(); n != 1 {
			t.Fatalf("proxy saw %d CONNECT requests, want 1", n)
		}
	})
}

// tunnel hijacks the CONNECT request and copies bytes to and from target.
func tunnel(t *testing.T, w http.ResponseWriter, target string) {
	t.Helper()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	upstream, err := dialer.DialContext(context.Background(), "tcp", target) //nolint:gosec // the test proxy dials the test server
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)

		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("response writer cannot hijack")

		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)

		return
	}
	_, _ = buf.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
	_ = buf.Flush()
	go func() {
		_, _ = io.Copy(upstream, bufio.NewReader(client))
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
}

func TestHTTPKeyFetcher(t *testing.T) {
	keys := testKeys(t)
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &keys.rsa2048.PublicKey, KeyID: "a", Use: "sig"},
		{Key: &keys.p256.PublicKey, KeyID: "b"},
	}}
	rows := []struct {
		name  string
		serve func(w http.ResponseWriter)
		want  int // keys returned; -1 means an error
	}{
		{"parsed set", func(w http.ResponseWriter) { writeJSON(t, w, set) }, 2},
		{"404", func(w http.ResponseWriter) { http.Error(w, "gone", http.StatusNotFound) }, -1},
		{"trailing value", func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{} {}`) }, -1},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tc.serve(w) }))
			t.Cleanup(srv.Close)
			f := &httpKeyFetcher{client: seamClient(t, srv), url: srv.URL + "/keys"}
			got, err := f.fetch(context.Background())
			if tc.want < 0 {
				if err == nil {
					t.Fatal("fetch succeeded, want an error")
				}

				return
			}
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if len(got.Keys) != tc.want {
				t.Fatalf("fetched %d keys, want %d", len(got.Keys), tc.want)
			}
		})
	}
}

func TestIssuerClientRefusesPlaintext(t *testing.T) {
	srv, c := bodyServer(t, `{}`)
	plain := "http://" + strings.TrimPrefix(srv.URL, "https://")
	var into map[string]any
	if err := c.getJSON(context.Background(), plain+"/x", &into); err == nil {
		t.Fatal("getJSON sent a plaintext request")
	}
	if _, _, err := c.postForm(context.Background(), plain+"/token", url.Values{}); err == nil {
		t.Fatal("postForm sent a plaintext request")
	}
}

func TestIssuerClientContext(t *testing.T) {
	srv, c := bodyServer(t, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var into map[string]any
	err := c.getJSON(ctx, srv.URL+"/x", &into)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the caller's cancellation to surface", err)
	}
}
