package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// issuerStepTimeout bounds each of connecting, the TLS handshake, and
	// waiting for response headers.
	issuerStepTimeout = 5 * time.Second
	// issuerRequestTimeout bounds one whole request to the issuer.
	issuerRequestTimeout = 10 * time.Second
	// maxIssuerBodyBytes is the largest body the gateway reads from the
	// issuer; one more byte than that fails the request.
	maxIssuerBodyBytes = 1 << 20
	// maxIssuerRedirects is how many redirects discovery and key fetches
	// follow; the token endpoint follows none.
	maxIssuerRedirects = 3
)

// issuerOptions is what the issuer client needs from the outside.
// Transport replaces the transport built here; a test hands in the one its
// httptest server trusts.
type issuerOptions struct {
	CAFile    string
	HTTPProxy string
	Transport http.RoundTripper
}

// issuerClient is the one dedicated http.Client every request to the issuer
// goes through: discovery, signing keys, and the browser flow's token
// exchange.
type issuerClient struct {
	get  *http.Client // discovery and keys: at most 3 redirects, each https
	post *http.Client // token endpoint: no redirects
}

// newIssuerClient builds the client over the configured CA and proxy.
func newIssuerClient(o issuerOptions) (*issuerClient, error) {
	rt := o.Transport
	if rt == nil {
		var err error
		if rt, err = newIssuerTransport(o.CAFile, o.HTTPProxy); err != nil {
			return nil, err
		}
	}

	return &issuerClient{
		get: &http.Client{
			Transport: rt,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// via holds the requests already sent, the original first,
				// so the fourth redirect is the first to see four.
				if len(via) > maxIssuerRedirects {
					return fmt.Errorf("auth: issuer: more than %d redirects", maxIssuerRedirects)
				}
				if req.URL.Scheme != "https" {
					return fmt.Errorf("auth: issuer: redirect to %s://, only https is followed", req.URL.Scheme)
				}

				return nil
			},
		},
		post: &http.Client{
			Transport: rt,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// A 307 or 308 would replay the client secret to whatever
				// host the redirect names.
				return errors.New("auth: issuer: token endpoint redirected; no redirect is followed")
			},
		},
	}, nil
}

// newIssuerTransport builds the transport: the system pool plus caFile,
// the proxy only when named, and a timeout on each step.
func newIssuerTransport(caFile, httpProxy string) (*http.Transport, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("auth: system certificate pool: %w", err)
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // the operator names the file; reading it is the purpose
		if err != nil {
			return nil, fmt.Errorf("auth: read caFile: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("auth: caFile %q holds no certificate", caFile)
		}
	}
	tr := &http.Transport{
		Proxy:                 nil, // never from the environment
		DialContext:           (&net.Dialer{Timeout: issuerStepTimeout}).DialContext,
		TLSHandshakeTimeout:   issuerStepTimeout,
		ResponseHeaderTimeout: issuerStepTimeout,
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	if httpProxy != "" {
		proxy, err := url.Parse(httpProxy)
		if err != nil {
			return nil, fmt.Errorf("auth: httpProxy: %w", err)
		}
		tr.Proxy = http.ProxyURL(proxy)
	}

	return tr, nil
}

// getJSON fetches rawURL, enforces the body limit, decodes one JSON value
// into into, and rejects bytes after it.
func (c *issuerClient) getJSON(ctx context.Context, rawURL string, into any) error {
	if err := requireHTTPS(rawURL); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, issuerRequestTimeout)
	defer cancel()
	// The URL is the issuer's discovered jwks_uri or its discovery path;
	// the taint the linter follows is the token whose kid asked for the
	// refresh, and no byte of it reaches the request.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil) //nolint:gosec // G704: the URL comes from discovery, not the request
	if err != nil {
		return fmt.Errorf("auth: issuer request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.get.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return fmt.Errorf("auth: issuer request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do with a close error on a read
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: issuer answered %d", resp.StatusCode)
	}
	body, err := readLimited(resp.Body)
	if err != nil {
		return err
	}

	return decodeOne(body, into)
}

// postForm posts form to rawURL without following redirects and returns the
// status and the limited body.
// The body is returned rather than decoded because a token endpoint's error
// response is read for its status first, and its shape second.
func (c *issuerClient) postForm(ctx context.Context, rawURL string, form url.Values) (int, []byte, error) {
	if err := requireHTTPS(rawURL); err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, issuerRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("auth: issuer request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.post.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("auth: issuer request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do with a close error on a read
	body, err := readLimited(resp.Body)
	if err != nil {
		return 0, nil, err
	}

	return resp.StatusCode, body, nil
}

// requireHTTPS refuses a URL the client must never send a request to.
func requireHTTPS(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("auth: issuer URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("auth: issuer URL %q: only https is requested", rawURL)
	}

	return nil
}

// readLimited reads at most the body limit; a body that fills the limit
// plus one byte is refused rather than truncated.
func readLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxIssuerBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("auth: issuer response: %w", err)
	}
	if len(body) > maxIssuerBodyBytes {
		return nil, fmt.Errorf("auth: issuer response exceeds %d bytes", maxIssuerBodyBytes)
	}

	return body, nil
}

// decodeOne decodes exactly one JSON value: trailing whitespace passes, a
// second value fails.
func decodeOne(body []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("auth: issuer response: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("auth: issuer response: bytes after the JSON value")
	}

	return nil
}
