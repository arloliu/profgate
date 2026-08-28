package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusingRoundTripper fails the test when a request reaches it: it proves a
// refusal happened before any request was built.
type refusingRoundTripper struct{ t *testing.T }

func (r refusingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Helper()
	r.t.Fatalf("a request reached the transport: %s %s", req.Method, req.URL)
	return nil, nil
}

// recordingRoundTripper answers 200 with an empty JSON object and keeps the last request it saw.
type recordingRoundTripper struct {
	last *http.Request
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.last = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

func settingsFor(t *testing.T, server string) Settings {
	t.Helper()
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	return Settings{Server: u, Origin: CanonicalOrigin(u)}
}

func mustToken(t *testing.T, s string) Credential {
	t.Helper()
	c, err := TokenCredential(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustBasic(t *testing.T, user, password string) Credential {
	t.Helper()
	c, err := BasicCredential(user, password)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPlaintextRefusal(t *testing.T) {
	cases := []struct {
		name       string
		server     string
		credential func(t *testing.T) Credential
	}{
		{"bearer token", "http://gateway.example", func(t *testing.T) Credential { return mustToken(t, "tok") }},
		{"PROFGATE_TOKEN", "http://gateway.example", func(t *testing.T) Credential { return mustToken(t, "env-tok") }},
		{"basic password", "http://gateway.example", func(t *testing.T) Credential { return mustBasic(t, "alice", "pw") }},
		// The rule reads the host as written and resolves nothing, so a name
		// that resolves to 127.0.0.1 is refused like any other name.
		{"a name resolving to loopback", "http://localhost.localdomain:8443", func(t *testing.T) Credential { return mustToken(t, "tok") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var warn bytes.Buffer
			c, err := New(Options{
				Settings:   settingsFor(t, tc.server),
				Credential: tc.credential(t),
				Transport:  refusingRoundTripper{t},
				Warn:       &warn,
			})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"})
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("expected a refusal")
			}
			if !errors.Is(err, ErrUsage) {
				t.Fatalf("refusal is not a usage error: %v", err)
			}
			if !strings.Contains(err.Error(), tc.server) || !strings.Contains(err.Error(), "https://") {
				t.Fatalf("refusal names neither the URL nor https://: %v", err)
			}
			if warn.Len() != 0 {
				t.Fatalf("a refusal warned: %q", warn.String())
			}
		})
	}
}

func TestPlaintextLoopbackWarns(t *testing.T) {
	for _, server := range []string{"http://127.0.0.1:8443", "http://[::1]:8443", "http://localhost:8443"} {
		t.Run(server, func(t *testing.T) {
			var warn bytes.Buffer
			rt := &recordingRoundTripper{}
			c, err := New(Options{
				Settings:   settingsFor(t, server),
				Credential: mustToken(t, "tok"),
				Transport:  rt,
				Warn:       &warn,
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"})
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			if got := rt.last.Header.Get("Authorization"); got != "Bearer tok" {
				t.Fatalf("Authorization = %q", got)
			}
			lines := strings.Split(strings.TrimSpace(warn.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("expected one warning per credential sent, got %q", warn.String())
			}
			for _, line := range lines {
				if !strings.Contains(line, server+"/v1/whoami") {
					t.Fatalf("warning does not name the URL: %q", line)
				}
				if strings.Contains(line, "tok") {
					t.Fatalf("warning carries the token: %q", line)
				}
			}
		})
	}
}

func TestPlaintextWithoutCredential(t *testing.T) {
	rt := &recordingRoundTripper{}
	c, err := New(Options{Settings: settingsFor(t, "http://gateway.example"), Transport: rt})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if rt.last == nil {
		t.Fatal("no request was made")
	}
	if _, ok := rt.last.Header["Authorization"]; ok {
		t.Fatal("a request with no credential carried an Authorization header")
	}
}

func TestHTTPSSendsWithoutWarning(t *testing.T) {
	var warn bytes.Buffer
	rt := &recordingRoundTripper{}
	c, err := New(Options{
		Settings:   settingsFor(t, "https://gateway.example"),
		Credential: mustToken(t, "tok"),
		Transport:  rt,
		Warn:       &warn,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := rt.last.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
	if warn.Len() != 0 {
		t.Fatalf("an https:// request warned: %q", warn.String())
	}
}

// TestServerNameAndCAFile verifies a port-forward shape: the URL names
// 127.0.0.1, the certificate names example.com,
// and --server-name bridges them while the Host header stays the URL's authority.
func TestServerNameAndCAFile(t *testing.T) {
	var gotHost string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	srv.StartTLS()
	defer srv.Close()

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.TLS.Certificates[0].Certificate[0]})
	if err := os.WriteFile(caPath, block, 0o600); err != nil {
		t.Fatal(err)
	}

	s := settingsFor(t, srv.URL)
	s.CAFile = caPath
	s.ServerName = "example.com"
	tr, err := NewTransport(s)
	if err != nil {
		t.Fatal(err)
	}
	if tr.TLSClientConfig.ServerName != "example.com" {
		t.Fatalf("ServerName = %q", tr.TLSClientConfig.ServerName)
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x", tr.TLSClientConfig.MinVersion)
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("verification is skipped")
	}

	c, err := New(Options{Settings: s, Credential: mustToken(t, "tok"), Transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.JSON(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"}); err != nil {
		t.Fatalf("request with server name and ca file: %v", err)
	}
	if gotHost != s.Server.Host {
		t.Fatalf("Host header %q, want the URL's authority %q", gotHost, s.Server.Host)
	}

	// Without the extra certificate the same server is not trusted.
	s.CAFile = ""
	plain, err := NewTransport(s)
	if err != nil {
		t.Fatal(err)
	}
	c, err = New(Options{Settings: s, Credential: mustToken(t, "tok"), Transport: plain})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.JSON(context.Background(), Request{Method: http.MethodGet, Path: "/v1/whoami"})
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("expected a transport error without the ca file, got %v", err)
	}
}

func TestCAFileWithoutCertificate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.crt")
	if err := os.WriteFile(path, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := settingsFor(t, "https://gateway.example")
	s.CAFile = path
	_, err := NewTransport(s)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("expected an error naming %s, got %v", path, err)
	}
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("a bad ca file is not a usage error: %v", err)
	}
}
