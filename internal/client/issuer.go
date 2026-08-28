package client

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
	"strconv"
	"strings"
	"time"
)

const (
	// issuerStepTimeout bounds each of connecting, the TLS handshake, and
	// waiting for response headers.
	issuerStepTimeout = 5 * time.Second
	// issuerRequestTimeout bounds one whole request to the issuer; it is a
	// setting of the two http.Clients, not a deadline computed from a clock.
	issuerRequestTimeout = 10 * time.Second
	// maxIssuerBodyBytes is the largest body the client reads from the
	// issuer; one more byte than that fails the request.
	maxIssuerBodyBytes = 1 << 20
	// maxIssuerRedirects is how many redirects discovery follows; the
	// device, token, and revocation endpoints follow none.
	maxIssuerRedirects = 3
	// discoveryPath is where an OpenID Connect issuer publishes its metadata.
	discoveryPath = "/.well-known/openid-configuration"
)

// Issuer is every request the client makes to the identity provider:
// discovery, the device endpoint, the token endpoint, and revocation.
// It verifies no signature and holds no key set.
type Issuer struct {
	get     *http.Client // discovery: at most 3 redirects, each https
	post    *http.Client // the endpoints: no redirects
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	verbose io.Writer
}

// IssuerOptions is everything an Issuer is built from; every field a test
// replaces is a seam.
type IssuerOptions struct {
	IssuerCAFile string
	Transport    http.RoundTripper // nil means one built from IssuerCAFile
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	Verbose      io.Writer // nil prints no request lines
}

// Metadata is what discovery published, each endpoint an absolute https URL
// with no userinfo and no fragment.
// The device and revocation endpoints are empty when the issuer publishes
// none; the token endpoint is required.
type Metadata struct {
	Issuer                      string
	DeviceAuthorizationEndpoint string
	TokenEndpoint               string
	RevocationEndpoint          string
}

// TokenResponse is the token endpoint's 200: the two tokens, the rotation,
// and the two lifetimes.
type TokenResponse struct {
	IDToken          string `json:"id_token"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
}

// IssuerError is a 4xx from an issuer endpoint: the status and the issuer's
// own error value, and nothing else from the body.
// Code is empty when the body is not the RFC 6749 error shape.
type IssuerError struct {
	Status int
	Code   string
}

func (e *IssuerError) Error() string {
	if e.Code != "" {
		return "the issuer answered " + strconv.Itoa(e.Status) + ": " + e.Code
	}
	return "the issuer answered " + (&StatusError{Status: e.Status}).Error()
}

// NewIssuer builds the Issuer; the transport comes from IssuerCAFile when
// none is given, and a nil clock or sleeper is the real one.
func NewIssuer(o IssuerOptions) (*Issuer, error) {
	rt := o.Transport
	if rt == nil {
		t, err := newIssuerTransport(o.IssuerCAFile)
		if err != nil {
			return nil, err
		}
		rt = t
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = realSleep
	}
	return &Issuer{
		get: &http.Client{
			Transport: rt,
			Timeout:   issuerRequestTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// via holds the requests already sent, the original first,
				// so the fourth redirect is the first to see four.
				if len(via) > maxIssuerRedirects {
					return fmt.Errorf("discovery: more than %d redirects", maxIssuerRedirects)
				}
				if req.URL.Scheme != "https" {
					return fmt.Errorf("discovery: redirect to %s://, only https is followed", req.URL.Scheme)
				}
				return nil
			},
		},
		post: &http.Client{
			Transport: rt,
			Timeout:   issuerRequestTimeout,
			// A 307 or 308 would replay the form, device code included, to
			// whatever host the redirect names.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:     now,
		sleep:   sleep,
		verbose: o.Verbose,
	}, nil
}

// newIssuerTransport builds the transport: the system pool plus caFile when
// given, never a proxy from the environment, and a timeout on each step.
func newIssuerTransport(caFile string) (*http.Transport, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("system certificate pool: %w", err)
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // the user names the file; reading it is the purpose
		if err != nil {
			return nil, fmt.Errorf("%w: read issuer ca file: %w", ErrUsage, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%w: issuer ca file %s holds no CERTIFICATE block", ErrUsage, caFile)
		}
	}
	return &http.Transport{
		Proxy:                 nil, // never from the environment
		DialContext:           (&net.Dialer{Timeout: issuerStepTimeout}).DialContext,
		TLSHandshakeTimeout:   issuerStepTimeout,
		ResponseHeaderTimeout: issuerStepTimeout,
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}, nil
}

// discoveryDocument is the part of the issuer's metadata the client reads.
type discoveryDocument struct {
	Issuer                      string `json:"issuer"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	RevocationEndpoint          string `json:"revocation_endpoint"`
}

// Discover fetches <issuer>/.well-known/openid-configuration and requires the
// document's issuer to equal the configured value byte for byte.
func (i *Issuer) Discover(ctx context.Context, issuer string) (Metadata, error) {
	base, err := checkEndpoint("issuer", issuer)
	if err != nil {
		return Metadata{}, err
	}
	target := strings.TrimSuffix(base.String(), "/") + discoveryPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: build discovery request: %w", ErrUsage, err)
	}
	req.Header.Set("Accept", "application/json")
	body, err := i.do(i.get, req, "discovery")
	if err != nil {
		return Metadata{}, err
	}
	var doc discoveryDocument
	if err := decodeOne(body, &doc); err != nil {
		return Metadata{}, fmt.Errorf("discovery: %w", err)
	}
	if doc.Issuer != issuer {
		return Metadata{}, fmt.Errorf("discovery: the document names issuer %q and the configured issuer is %q", doc.Issuer, issuer)
	}
	if doc.TokenEndpoint == "" {
		return Metadata{}, errors.New("discovery: the document publishes no token_endpoint")
	}
	endpoints := []struct {
		name string
		url  string
	}{
		{"token_endpoint", doc.TokenEndpoint},
		{"device_authorization_endpoint", doc.DeviceAuthorizationEndpoint},
		{"revocation_endpoint", doc.RevocationEndpoint},
	}
	for _, e := range endpoints {
		if e.url == "" {
			continue
		}
		if _, err := checkEndpoint(e.name, e.url); err != nil {
			return Metadata{}, fmt.Errorf("discovery: %w", err)
		}
	}
	return Metadata(doc), nil
}

// postForm posts one form to an endpoint and decodes the bounded response:
// a 200 into TokenResponse, a 4xx into *IssuerError, and a 5xx or a
// transport failure into the errors the gateway client already uses.
// name is what the --verbose line calls the endpoint: device, token, or
// revocation.
func (i *Issuer) postForm(ctx context.Context, name, endpoint string, form url.Values) (TokenResponse, error) {
	body, err := i.postFormBody(ctx, name, endpoint, form)
	if err != nil {
		return TokenResponse{}, err
	}
	var tr TokenResponse
	if err := decodeOne(body, &tr); err != nil {
		return TokenResponse{}, &StatusError{Status: http.StatusOK}
	}
	return tr, nil
}

// postFormBody posts one form to an endpoint and returns a 200's bounded
// body; the device endpoint decodes its own shape from it.
func (i *Issuer) postFormBody(ctx context.Context, name, endpoint string, form url.Values) ([]byte, error) {
	if _, err := checkEndpoint(name, endpoint); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: build %s request: %w", ErrUsage, name, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return i.do(i.post, req, name)
}

// do sends one request, prints the --verbose line, and returns a 200's
// bounded body; any other status becomes the error its class selects.
func (i *Issuer) do(c *http.Client, req *http.Request, name string) ([]byte, error) {
	start := i.now()
	resp, err := c.Do(req)
	if err != nil {
		i.logRequest(req, name, 0, start)
		return nil, newTransportError(CanonicalOrigin(req.URL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	i.logRequest(req, name, resp.StatusCode, start)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("%s: the endpoint answered a %d redirect; no redirect is followed", name, resp.StatusCode)
	}
	body, err := readIssuerBody(resp.Body)
	if resp.StatusCode == http.StatusOK {
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return body, nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, &IssuerError{Status: resp.StatusCode, Code: issuerCode(resp, body)}
	}
	return nil, &StatusError{Status: resp.StatusCode}
}

// issuerCode is the error member of an RFC 6749 error body, and the empty
// string when the body is anything else.
func issuerCode(resp *http.Response, body []byte) string {
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if strings.TrimSpace(strings.ToLower(mediaType)) != "application/json" {
		return ""
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	return e.Error
}

// logRequest prints the --verbose line: method, host, endpoint name, status,
// and duration, and never a path or a header.
// A status of 0 is a request that got no answer.
func (i *Issuer) logRequest(req *http.Request, name string, status int, start time.Time) {
	if i.verbose == nil {
		return
	}
	_, _ = fmt.Fprintf(i.verbose, "%s %s %s %d %s\n", req.Method, req.URL.Host, name, status, i.now().Sub(start))
}

// checkEndpoint admits an absolute https:// URL with no userinfo and no
// fragment, naming the endpoint and the rule in the refusal.
// The refusal prints the URL without its userinfo.
func checkEndpoint(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("%s %q is not an absolute URL", name, raw)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("%s %q uses %s://, and only https:// is requested", name, u.Redacted(), u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%s %q carries userinfo", name, u.Redacted())
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return nil, fmt.Errorf("%s %q carries a fragment", name, raw)
	}
	return u, nil
}

// readIssuerBody reads at most the body limit; a body that fills the limit
// plus one byte is refused rather than truncated.
func readIssuerBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxIssuerBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxIssuerBodyBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxIssuerBodyBytes)
	}
	return body, nil
}

// decodeOne decodes exactly one JSON value: trailing whitespace passes, a
// second value fails.
func decodeOne(body []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("response: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("response carries bytes after the JSON value")
	}
	return nil
}
