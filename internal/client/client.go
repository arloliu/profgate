package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes bounds every JSON document the client reads; a body that fills the bound is refused rather than read.
// Profile bytes stream through Do and are not bounded here.
const maxResponseBytes = 1 << 20

// maxPasswordBytes is where bcrypt stops reading, so the gateway refuses a
// longer password; the client refuses it first because the gateway's 401 is deliberately uninformative.
const maxPasswordBytes = 72

// Credential supplies the Authorization header for one request.
// It is resolved once per command and may refresh a cached token before returning.
type Credential interface {
	Apply(ctx context.Context, r *http.Request) error
}

// Client talks to one gateway as one principal.
// Every method issues exactly one request, except where the design says otherwise.
type Client struct {
	settings   Settings
	credential Credential
	http       *http.Client
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
	verbose    io.Writer
	warn       io.Writer
}

// Options is everything a Client is built from;
// every field a test replaces is a seam rather than something New reaches for.
type Options struct {
	Settings   Settings
	Credential Credential
	Transport  http.RoundTripper // nil means one built from Settings
	Now        func() time.Time
	Sleep      func(context.Context, time.Duration) error // nil is the real one; the wait and the cancel retry pace themselves on it
	Verbose    io.Writer                                  // nil prints no request lines
	Warn       io.Writer                                  // the loopback warning
}

// Request is one call: a method, a /v1 path, a query, and an optional JSON body.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
	Header http.Header
}

// New builds a Client; the transport comes from Settings when none is given.
func New(o Options) (*Client, error) {
	if o.Settings.Server == nil {
		return nil, fmt.Errorf("%w: no gateway selected", ErrUsage)
	}
	tr := o.Transport
	if tr == nil {
		t, err := NewTransport(o.Settings)
		if err != nil {
			return nil, err
		}
		tr = t
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = realSleep
	}
	warn := o.Warn
	if warn == nil {
		warn = io.Discard
	}
	return &Client{
		settings:   o.Settings,
		credential: o.Credential,
		http: &http.Client{
			Transport: tr,
			// A redirect would carry the credential to a URL nobody checked.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:     now,
		sleep:   sleep,
		verbose: o.Verbose,
		warn:    warn,
	}, nil
}

// Do issues one request and returns the response with its body still open,
// or an *APIError, a *StatusError, or a *TransportError.
// A 2xx response is returned as it arrived; any other status is read,
// bounded, and turned into the error its body shape selects.
func (c *Client) Do(ctx context.Context, req Request) (*http.Response, error) {
	r, err := c.build(ctx, req)
	if err != nil {
		return nil, err
	}
	start := c.now()
	resp, err := c.http.Do(r)
	if err != nil {
		c.logRequest(r, 0, start)
		return nil, newTransportError(c.settings.Origin, err)
	}
	c.logRequest(r, resp.StatusCode, start)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var failure error = &StatusError{Status: resp.StatusCode}
	if body, err := readBounded(resp.Body); err == nil {
		if e := decodeEnvelope(resp, body); e != nil {
			failure = e
		}
	}
	if d, ok := c.credential.(diagnoser); ok && resp.StatusCode == http.StatusUnauthorized {
		failure = d.diagnose(failure)
	}
	return nil, failure
}

// diagnoser is a Credential that can say more about a 401 than the gateway does:
// the cached token knows the issuer and client it was obtained for.
type diagnoser interface {
	diagnose(err error) error
}

// withCredential is the same gateway client speaking as one principal.
func (c *Client) withCredential(cred Credential) *Client {
	d := *c
	d.credential = cred
	return &d
}

// JSON issues one request and returns the response body verbatim, which is
// what --output json prints and what the table renderers decode.
// A body that is not one JSON document is a *StatusError, whatever the status.
func (c *Client) JSON(ctx context.Context, req Request) ([]byte, http.Header, error) {
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readBounded(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", c.settings.Origin, err)
	}
	if !json.Valid(body) {
		return nil, nil, &StatusError{Status: resp.StatusCode}
	}
	return body, resp.Header, nil
}

// build assembles the request and attaches the credential under the
// plaintext rule: only an https:// URL, or a loopback http:// URL with one
// warning line each time.
func (c *Client) build(ctx context.Context, req Request) (*http.Request, error) {
	u := c.settings.Server.JoinPath(req.Path)
	u.RawQuery = req.Query.Encode()
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	r, err := http.NewRequestWithContext(ctx, req.Method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", ErrUsage, err)
	}
	for k, vs := range req.Header {
		r.Header[k] = append([]string(nil), vs...)
	}
	if c.credential == nil {
		return r, nil
	}
	loopback, err := checkPlaintext(u)
	if err != nil {
		return nil, err
	}
	if loopback {
		_, _ = fmt.Fprintf(c.warn, "profgate: warning: sending a credential over plaintext to %s\n", u)
	}
	if err := c.credential.Apply(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// logRequest prints the --verbose line: method, URL, status, and duration,
// and never a header.
// A status of 0 is a request that got no answer.
func (c *Client) logRequest(r *http.Request, status int, start time.Time) {
	if c.verbose == nil {
		return
	}
	_, _ = fmt.Fprintf(c.verbose, "%s %s %d %s\n", r.Method, r.URL, status, c.now().Sub(start))
}

// errResponseTooLarge marks a body that filled the bound.
// Those bytes arrived and this client refuses to read them,
// so the same request would meet the same body again.
var errResponseTooLarge = errors.New("response exceeds the bound")

// readBounded reads at most maxResponseBytes and refuses a body that fills
// the bound rather than returning a prefix of it.
func readBounded(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("%w of %d bytes", errResponseTooLarge, maxResponseBytes)
	}
	return data, nil
}

// envelope is the gateway's error body; details is present only on
// port_not_allowed and no verb reads it.
type envelope struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// decodeEnvelope returns the APIError a JSON error body carries, or nil when
// the body is not the envelope: another content type, invalid JSON, or a
// document without a code.
func decodeEnvelope(resp *http.Response, body []byte) *APIError {
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if strings.TrimSpace(strings.ToLower(mediaType)) != "application/json" {
		return nil
	}
	var e envelope
	if err := json.Unmarshal(body, &e); err != nil || e.Code == "" {
		return nil
	}
	return &APIError{Status: resp.StatusCode, Code: e.Code, Message: e.Error}
}

type tokenCredential string

func (t tokenCredential) Apply(_ context.Context, r *http.Request) error {
	r.Header.Set("Authorization", "Bearer "+string(t))
	return nil
}

// TokenCredential is a token the client did not obtain: from --token-file,
// --token-stdin, or PROFGATE_TOKEN. It is never written to the cache.
// Whitespace at both ends is trimmed; an empty token is a usage error.
func TokenCredential(token string) (Credential, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("%w: the token is empty", ErrUsage)
	}
	return tokenCredential(token), nil
}

type basicCredential struct {
	user, password string
}

func (b basicCredential) Apply(_ context.Context, r *http.Request) error {
	r.SetBasicAuth(b.user, b.password)
	return nil
}

// BasicCredential is a user name and password, refused locally when the name
// carries a colon or the password exceeds 72 bytes, because the gateway's
// 401 for either is deliberately uninformative.
func BasicCredential(user, password string) (Credential, error) {
	if strings.Contains(user, ":") {
		return nil, fmt.Errorf("%w: a basic user name cannot contain a colon", ErrUsage)
	}
	if len(password) > maxPasswordBytes {
		return nil, fmt.Errorf("%w: a basic password is at most %d bytes, found %d", ErrUsage, maxPasswordBytes, len(password))
	}
	return basicCredential{user: user, password: password}, nil
}
