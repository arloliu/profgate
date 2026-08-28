package client

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// issuerServer serves handler over TLS and returns an Issuer that trusts it.
// The verbose buffer collects the request lines.
func issuerServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Issuer, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	var verbose bytes.Buffer
	iss, err := NewIssuer(IssuerOptions{Transport: srv.Client().Transport, Verbose: &verbose})
	if err != nil {
		t.Fatal(err)
	}
	return srv, iss, &verbose
}

// discoveryHandler answers the discovery path with a document naming srv as
// the issuer and the given endpoints; extra keys are merged verbatim.
func discoveryHandler(t *testing.T, base *string, extra map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		doc := map[string]any{
			"issuer":                        *base,
			"device_authorization_endpoint": *base + "/device",
			"token_endpoint":                *base + "/token",
			"revocation_endpoint":           *base + "/revoke",
		}
		for k, v := range extra {
			if v == nil {
				delete(doc, k)
				continue
			}
			doc[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, toJSON(t, doc))
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDiscoverRefusesHTTPIssuer(t *testing.T) {
	iss, err := NewIssuer(IssuerOptions{Transport: refusingRoundTripper{t}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = iss.Discover(context.Background(), "http://issuer.example")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "http://") || !strings.Contains(err.Error(), "https") {
		t.Fatalf("refusal names neither scheme: %v", err)
	}
}

func TestDiscoverDocumentRules(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		want  []string // substrings of the error; nil means success
		check func(t *testing.T, m Metadata)
	}{
		{
			name:  "an http:// endpoint",
			extra: map[string]any{"token_endpoint": "http://issuer.example/token"}, //nolint:gosec // G101: a URL, not a credential
			want:  []string{"token_endpoint", "http://issuer.example/token", "https"},
		},
		{
			name:  "an issuer differing from the configured value",
			extra: map[string]any{"issuer": "https://other.example"},
			want:  []string{"https://other.example", "configured"},
		},
		{
			name:  "no device authorization endpoint",
			extra: map[string]any{"device_authorization_endpoint": nil},
			check: func(t *testing.T, m Metadata) {
				if m.DeviceAuthorizationEndpoint != "" {
					t.Fatalf("device endpoint = %q, want empty", m.DeviceAuthorizationEndpoint)
				}
				if m.TokenEndpoint == "" || m.RevocationEndpoint == "" {
					t.Fatalf("the other endpoints were dropped: %+v", m)
				}
			},
		},
		{
			name:  "no revocation endpoint",
			extra: map[string]any{"revocation_endpoint": nil},
			check: func(t *testing.T, m Metadata) {
				if m.RevocationEndpoint != "" {
					t.Fatalf("revocation endpoint = %q, want empty", m.RevocationEndpoint)
				}
			},
		},
		{
			name:  "no token endpoint",
			extra: map[string]any{"token_endpoint": nil},
			want:  []string{"token_endpoint"},
		},
		{
			name:  "an endpoint carrying userinfo",
			extra: map[string]any{"device_authorization_endpoint": "https://alice:secret@issuer.example/device"}, //nolint:gosec // G101: the userinfo is what the rule refuses
			want:  []string{"device_authorization_endpoint", "userinfo"},
		},
		{
			name:  "an endpoint carrying a fragment",
			extra: map[string]any{"revocation_endpoint": "https://issuer.example/revoke#frag"},
			want:  []string{"revocation_endpoint", "fragment"},
		},
		{
			name:  "a relative endpoint",
			extra: map[string]any{"token_endpoint": "/token"},
			want:  []string{"token_endpoint", "absolute"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var base string
			srv, iss, _ := issuerServer(t, discoveryHandler(t, &base, tc.extra))
			base = srv.URL
			m, err := iss.Discover(context.Background(), srv.URL)
			if tc.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				if m.Issuer != srv.URL {
					t.Fatalf("Issuer = %q", m.Issuer)
				}
				tc.check(t, m)
				return
			}
			if err == nil {
				t.Fatalf("expected a refusal, got %+v", m)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("refusal %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestDiscoverRedirects(t *testing.T) {
	cases := []struct {
		name      string
		redirects int
		want      string // substring of the error; empty means followed
	}{
		{"a third redirect is followed", 3, ""},
		{"a fourth redirect is refused", 4, "redirect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var base string
			doc := discoveryHandler(t, &base, nil)
			srv, iss, _ := issuerServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/.well-known/openid-configuration" {
					http.Redirect(w, r, "/hop/1", http.StatusFound)
					return
				}
				var n int
				if _, err := fmt.Sscanf(r.URL.Path, "/hop/%d", &n); err == nil {
					if n < tc.redirects {
						http.Redirect(w, r, fmt.Sprintf("/hop/%d", n+1), http.StatusFound)
						return
					}
					r.URL.Path = "/.well-known/openid-configuration"
					doc(w, r)
					return
				}
				http.NotFound(w, r)
			})
			base = srv.URL
			m, err := iss.Discover(context.Background(), srv.URL)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if m.TokenEndpoint != srv.URL+"/token" {
					t.Fatalf("TokenEndpoint = %q", m.TokenEndpoint)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestDiscoverRefusesRedirectToHTTP(t *testing.T) {
	srv, iss, _ := issuerServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://issuer.example/.well-known/openid-configuration", http.StatusFound)
	})
	_, err := iss.Discover(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "http://") || !strings.Contains(err.Error(), "https") {
		t.Fatalf("refusal names neither scheme: %v", err)
	}
	var te *TransportError
	if errors.As(err, &te) && strings.Contains(te.Error(), "/.well-known") {
		t.Fatalf("the transport error carries a path: %v", err)
	}
}

func TestDiscoverBodyBounds(t *testing.T) {
	cases := []struct {
		name string
		body func(base string) []byte
		want string
	}{
		{
			name: "a body of 1 MiB + 1",
			body: func(string) []byte { return bytes.Repeat([]byte(" "), maxIssuerBodyBytes+1) },
			want: "exceeds",
		},
		{
			name: "a valid JSON value followed by a second one",
			body: func(base string) []byte {
				return []byte(fmt.Sprintf(`{"issuer":%q,"token_endpoint":%q} {}`, base, base+"/token"))
			},
			want: "after the JSON value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var base string
			srv, iss, _ := issuerServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(tc.body(base))
			})
			base = srv.URL
			_, err := iss.Discover(context.Background(), srv.URL)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestDiscoverTrailingWhitespacePasses(t *testing.T) {
	var base string
	srv, iss, _ := issuerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "{\"issuer\":%q,\"token_endpoint\":%q}\n\n", base, base+"/token")
	})
	base = srv.URL
	if _, err := iss.Discover(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverNon200(t *testing.T) {
	srv, iss, _ := issuerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<html>down</html>", http.StatusBadGateway)
	})
	_, err := iss.Discover(context.Background(), srv.URL)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway {
		t.Fatalf("err = %v, want a StatusError 502", err)
	}
	if strings.Contains(err.Error(), "down") {
		t.Fatalf("the error carries the body: %v", err)
	}
}

func TestIssuerCAFile(t *testing.T) {
	var base string
	srv := httptest.NewTLSServer(discoveryHandler(t, &base, nil))
	defer srv.Close()
	base = srv.URL
	caPath := filepath.Join(t.TempDir(), "issuer-ca.crt")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.TLS.Certificates[0].Certificate[0]})
	if err := os.WriteFile(caPath, block, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("without the file the handshake fails", func(t *testing.T) {
		iss, err := NewIssuer(IssuerOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = iss.Discover(context.Background(), srv.URL)
		var te *TransportError
		if !errors.As(err, &te) {
			t.Fatalf("err = %v, want a TransportError", err)
		}
	})
	t.Run("with the file the fetch verifies", func(t *testing.T) {
		iss, err := NewIssuer(IssuerOptions{IssuerCAFile: caPath})
		if err != nil {
			t.Fatal(err)
		}
		m, err := iss.Discover(context.Background(), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if m.TokenEndpoint != srv.URL+"/token" {
			t.Fatalf("TokenEndpoint = %q", m.TokenEndpoint)
		}
	})
}

func TestIssuerCAFileRefusals(t *testing.T) {
	cases := []struct {
		name  string
		write bool
		want  string
	}{
		{"a missing file", false, "read issuer ca file"},
		{"a file with no certificate", true, "holds no CERTIFICATE block"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ca.crt")
			if tc.write {
				if err := os.WriteFile(path, []byte("not a certificate\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := NewIssuer(IssuerOptions{IssuerCAFile: path})
			if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a usage error naming %q", err, tc.want)
			}
		})
	}
}

func TestIssuerTransportBounds(t *testing.T) {
	iss, err := NewIssuer(IssuerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := iss.get.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", iss.get.Transport)
	}
	if tr.TLSHandshakeTimeout != issuerStepTimeout || tr.ResponseHeaderTimeout != issuerStepTimeout {
		t.Fatalf("step timeouts = %s, %s", tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
	}
	if tr.Proxy != nil {
		t.Fatal("the transport reads a proxy from the environment")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("verification is skipped")
	}
	if iss.get.Timeout != issuerRequestTimeout || iss.post.Timeout != issuerRequestTimeout {
		t.Fatalf("request deadlines = %s, %s", iss.get.Timeout, iss.post.Timeout)
	}
	if iss.post.Transport != iss.get.Transport {
		t.Fatal("the two clients use different transports")
	}
}

func TestPostFormRefusals(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"an http:// endpoint", "http://issuer.example/token", "https"},
		{"an endpoint carrying userinfo", "https://alice:secret@issuer.example/token", "userinfo"},
		{"an endpoint carrying a fragment", "https://issuer.example/token#frag", "fragment"},
		{"a relative endpoint", "/token", "absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iss, err := NewIssuer(IssuerOptions{Transport: refusingRoundTripper{t}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = iss.postForm(context.Background(), "token", tc.endpoint, url.Values{"grant_type": {"x"}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("the refusal carries the userinfo: %v", err)
			}
		})
	}
}

func TestPostFormRefusesRedirect(t *testing.T) {
	var followed bool
	srv, iss, _ := issuerServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"x"}`))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	_, err := iss.postForm(context.Background(), "token", srv.URL+"/token", url.Values{"grant_type": {"x"}})
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v, want a refusal naming the redirect", err)
	}
	if followed {
		t.Fatal("the redirect was followed")
	}
}

func TestPostFormResponses(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		check       func(t *testing.T, tr TokenResponse, err error)
	}{
		{
			name:        "a 200 with the token shape",
			status:      http.StatusOK,
			contentType: "application/json",
			body:        `{"id_token":"i","access_token":"a","refresh_token":"r","expires_in":300,"refresh_expires_in":1800,"token_type":"Bearer"}`,
			check: func(t *testing.T, tr TokenResponse, err error) {
				if err != nil {
					t.Fatal(err)
				}
				want := TokenResponse{IDToken: "i", AccessToken: "a", RefreshToken: "r", ExpiresIn: 300, RefreshExpiresIn: 1800}
				if tr != want {
					t.Fatalf("TokenResponse = %+v, want %+v", tr, want)
				}
			},
		},
		{
			name:        "a 200 that is not JSON",
			status:      http.StatusOK,
			contentType: "text/html",
			body:        "<html>ok</html>",
			check: func(t *testing.T, _ TokenResponse, err error) {
				var se *StatusError
				if !errors.As(err, &se) || se.Status != http.StatusOK {
					t.Fatalf("err = %v, want a StatusError 200", err)
				}
			},
		},
		{
			name:        "a 400 with an RFC 6749 error",
			status:      http.StatusBadRequest,
			contentType: "application/json",
			body:        `{"error":"invalid_grant","error_description":"the refresh token was revoked"}`,
			check: func(t *testing.T, _ TokenResponse, err error) {
				var ie *IssuerError
				if !errors.As(err, &ie) {
					t.Fatalf("err = %v, want an IssuerError", err)
				}
				if ie.Status != http.StatusBadRequest || ie.Code != "invalid_grant" {
					t.Fatalf("IssuerError = %+v", ie)
				}
				if strings.Contains(err.Error(), "revoked") {
					t.Fatalf("the error carries the description: %v", err)
				}
			},
		},
		{
			name:        "a 400 with an HTML body",
			status:      http.StatusBadRequest,
			contentType: "text/html",
			body:        "<html>rejected by proxy</html>",
			check: func(t *testing.T, _ TokenResponse, err error) {
				var ie *IssuerError
				if !errors.As(err, &ie) {
					t.Fatalf("err = %v, want an IssuerError", err)
				}
				if ie.Status != http.StatusBadRequest || ie.Code != "" {
					t.Fatalf("IssuerError = %+v", ie)
				}
				if strings.Contains(err.Error(), "proxy") || strings.Contains(err.Error(), "<") {
					t.Fatalf("the error carries a byte of the body: %v", err)
				}
			},
		},
		{
			name:        "a 500",
			status:      http.StatusInternalServerError,
			contentType: "application/json",
			body:        `{"error":"server_error"}`,
			check: func(t *testing.T, _ TokenResponse, err error) {
				var se *StatusError
				if !errors.As(err, &se) || se.Status != http.StatusInternalServerError {
					t.Fatalf("err = %v, want a StatusError 500", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotContentType, gotBody string
			srv, iss, _ := issuerServer(t, func(w http.ResponseWriter, r *http.Request) {
				gotContentType = r.Header.Get("Content-Type")
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			tr, err := iss.postForm(context.Background(), "token", srv.URL+"/token", url.Values{"grant_type": {"refresh_token"}})
			tc.check(t, tr, err)
			if gotContentType != "application/x-www-form-urlencoded" {
				t.Fatalf("Content-Type = %q", gotContentType)
			}
			if gotBody != "grant_type=refresh_token" {
				t.Fatalf("form = %q", gotBody)
			}
		})
	}
}

func TestPostFormTransportFailure(t *testing.T) {
	srv, iss, _ := issuerServer(t, func(http.ResponseWriter, *http.Request) {})
	endpoint := srv.URL + "/token"
	srv.Close()
	_, err := iss.postForm(context.Background(), "token", endpoint, url.Values{})
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want a TransportError", err)
	}
	if strings.Contains(err.Error(), "/token") {
		t.Fatalf("the error carries the path: %v", err)
	}
}

func TestIssuerErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		err  *IssuerError
		want string
	}{
		{"with a code", &IssuerError{Status: 400, Code: "invalid_grant"}, "the issuer answered 400: invalid_grant"},
		{"without a code", &IssuerError{Status: 400}, "the issuer answered HTTP 400 Bad Request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIssuerVerbose(t *testing.T) {
	var base string
	doc := discoveryHandler(t, &base, nil)
	srv, iss, verbose := issuerServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"secret-token"}`))
			return
		}
		doc(w, r)
	})
	base = srv.URL
	if _, err := iss.Discover(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.postForm(context.Background(), "token", srv.URL+"/token", url.Values{"device_code": {"secret-code"}}); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "https://")
	lines := strings.Split(strings.TrimSpace(verbose.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one line per request, got %q", verbose.String())
	}
	if !strings.HasPrefix(lines[0], "GET "+host+" discovery 200 ") {
		t.Fatalf("discovery line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "POST "+host+" token 200 ") {
		t.Fatalf("token line = %q", lines[1])
	}
	for _, secret := range []string{"secret", ".well-known", "/token", "Content-Type", "Accept"} {
		if strings.Contains(verbose.String(), secret) {
			t.Fatalf("verbose output carries %q: %q", secret, verbose.String())
		}
	}
}

func TestIssuerVerboseNilPrintsNothing(t *testing.T) {
	iss, err := NewIssuer(IssuerOptions{Transport: &recordingRoundTripper{}})
	if err != nil {
		t.Fatal(err)
	}
	if iss.verbose != nil {
		t.Fatal("a nil Verbose was replaced")
	}
	// The recording transport answers {} with no issuer, so discovery is
	// refused after the request; the point is that logging a nil writer
	// does not panic.
	if _, err := iss.Discover(context.Background(), "https://issuer.example"); err == nil {
		t.Fatal("expected a refusal")
	}
}

func TestIssuerClockAndSleeperSeams(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	slept := false
	iss, err := NewIssuer(IssuerOptions{
		Transport: &recordingRoundTripper{},
		Now:       func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error {
			slept = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !iss.now().Equal(now) {
		t.Fatal("the clock was not injected")
	}
	if err := iss.sleep(context.Background(), time.Second); err != nil || !slept {
		t.Fatal("the sleeper was not injected")
	}
}
