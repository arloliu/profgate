package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// connectTimeout bounds the dial and the TLS handshake.
// Nothing bounds the response: a profile takes as long as its seconds
// parameter says, and the context is the caller's deadline.
const connectTimeout = 10 * time.Second

// NewTransport builds the gateway transport from the resolved settings: the
// system pool plus the certificates in CAFile when given, ServerName and
// nothing else in the TLS configuration, and TLS 1.2 as the minimum.
// There is no field, flag, or option that skips verification.
func NewTransport(s Settings) (*http.Transport, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("system certificate pool: %w", err)
	}
	if s.CAFile != "" {
		pem, err := os.ReadFile(s.CAFile) //nolint:gosec // the user names the file; reading it is the purpose
		if err != nil {
			return nil, fmt.Errorf("%w: read ca file: %w", ErrUsage, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%w: ca file %s holds no CERTIFICATE block", ErrUsage, s.CAFile)
		}
	}
	return &http.Transport{
		Proxy:               nil, // never from the environment
		DialContext:         (&net.Dialer{Timeout: connectTimeout}).DialContext,
		TLSHandshakeTimeout: connectTimeout,
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			ServerName: s.ServerName,
			MinVersion: tls.VersionTLS12,
		},
	}, nil
}

// loopbackHosts are the hosts a credential may reach over http://: the
// port-forward case, where the bytes cross a loopback interface and no network.
// The check reads the host as the URL spells it and resolves nothing.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// checkPlaintext applies the plaintext rule to a URL a credential is about
// to be attached to.
// It returns whether the send is the loopback exception, which the caller
// warns about, or the refusal.
func checkPlaintext(u *url.URL) (loopback bool, err error) {
	switch {
	case strings.EqualFold(u.Scheme, "https"):
		return false, nil
	case isLoopbackHost(u.Hostname()):
		return true, nil
	default:
		return false, fmt.Errorf("%w: refusing to send a credential to %s: a credential travels only to an https:// URL, or over http:// to 127.0.0.1, ::1, or localhost", ErrUsage, u)
	}
}
