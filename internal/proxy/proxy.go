// Package proxy forwards one pprof request to a confirmed Pod over plain HTTP through a pinned transport,
// streams the upstream response to the client, and classifies every outcome by its cause
// so the HTTP layer can record it and, when nothing was forwarded, write the matching error envelope.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/arloliu/profgate/internal/k8s"
)

const (
	// dialTimeout bounds the TCP connect to a Pod;
	// a Pod that cannot be reached in this time is reported as unreachable rather than waited for.
	dialTimeout = 5 * time.Second
	// headerDeadlineGrace is added to the effective duration for profiles that take one.
	headerDeadlineGrace = 10 * time.Second
	// defaultHeaderDeadline applies to profiles without a duration.
	defaultHeaderDeadline = 30 * time.Second
	// maxIdleConns caps the transport's idle pool across every Pod, the standard transport's value;
	// the per-host cap stays at Go's default of two.
	maxIdleConns = 100
	// defaultIdleConnTimeout is how long an idle upstream connection is kept, the standard transport's value.
	defaultIdleConnTimeout = 90 * time.Second
	// redirectDrainLimit caps how much of a redirect body is read before the connection is released;
	// pprof handlers never redirect, so the body is never interesting.
	redirectDrainLimit = 64 << 10

	// Outcome codes, as the spec's proxy-behavior table names them.
	codeOK           = "ok"
	codeUnreachable  = "upstream_unreachable"
	codeTimeout      = "upstream_timeout"
	codeRedirect     = "upstream_redirect"
	codeStreamFailed = "upstream_stream_failed"
	codeClientGone   = "client_gone"
)

// errHeaderDeadline is the cancellation cause when response headers miss the header deadline.
// It is distinct from the overall budget's cause so classification never depends on which timer fired first.
var errHeaderDeadline = errors.New("upstream response headers missed the header deadline")

// allowedHeaders are the only upstream response headers forwarded to the client.
func allowedHeaders() []string {
	return []string{"Content-Type", "Content-Length", "Content-Encoding", "Content-Disposition", "X-Content-Type-Options"}
}

// Request describes one upstream fetch against a confirmed Target.
type Request struct {
	Target        k8s.Target
	Path          string            // upstream path from the spec's profile table
	Seconds       int               // effective duration; 0 for profiles without one
	TargetHeaders map[string]string // X-Pprof-Target-*; written only on forwarded responses
}

// Outcome reports how one upstream fetch ended.
type Outcome struct {
	// Code is "ok", "upstream_<status>", "upstream_unreachable", "upstream_timeout",
	// "upstream_redirect", "upstream_stream_failed", or "client_gone".
	Code string
	// Status is the status written, or the status the caller should write when !Committed.
	// It is 0 for a client that disconnected before anything was committed: there is nobody to write to.
	Status int
	// Committed reports that response headers were sent to the client.
	Committed bool
}

// Options tunes a Proxy; the zero value follows the spec.
type Options struct {
	// HeaderDeadline maps the effective duration to the header deadline; nil means the spec's rule:
	// seconds+10s, or 30s when seconds is 0.
	// Tests inject short values.
	HeaderDeadline func(seconds int) time.Duration
	// IdleConnTimeout is how long an idle upstream connection is kept; zero means the spec's 90 seconds.
	// Tests inject short values.
	IdleConnTimeout time.Duration
}

// Proxy forwards pprof requests through one shared, immutable transport.
type Proxy struct {
	client         *http.Client
	headerDeadline func(int) time.Duration
}

// New builds the one shared immutable transport: Proxy nil, DisableCompression true,
// 5s dial timeout, an idle pool of at most 100 connections each kept 90s,
// and a client whose CheckRedirect returns http.ErrUseLastResponse.
func New(opts Options) *Proxy {
	idle := opts.IdleConnTimeout
	if idle == 0 {
		idle = defaultIdleConnTimeout
	}
	transport := &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		DialContext:        (&net.Dialer{Timeout: dialTimeout}).DialContext,
		MaxIdleConns:       maxIdleConns,
		IdleConnTimeout:    idle,
	}
	headerDeadline := opts.HeaderDeadline
	if headerDeadline == nil {
		headerDeadline = specHeaderDeadline
	}

	return &Proxy{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		headerDeadline: headerDeadline,
	}
}

// specHeaderDeadline is the spec's rule: effective duration + 10s, or 30s without a duration.
func specHeaderDeadline(seconds int) time.Duration {
	if seconds == 0 {
		return defaultHeaderDeadline
	}

	return time.Duration(seconds)*time.Second + headerDeadlineGrace
}

// Do issues the upstream request under ctx (the caller's overall budget) plus its own header deadline,
// streams forwarded responses to w, and never writes a JSON body itself:
// when Outcome.Committed is false the caller writes the error envelope for Outcome.Code.
func (p *Proxy) Do(ctx context.Context, w http.ResponseWriter, req Request) Outcome {
	// The header deadline must not live on the request context:
	// Go applies a request's context to body reads too, and a body may legitimately stream past the header deadline.
	reqCtx, cancelReq := context.WithCancelCause(ctx)
	defer cancelReq(nil)

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, upstreamURL(req), nil)
	if err != nil {
		return Outcome{Code: codeUnreachable, Status: http.StatusBadGateway}
	}

	// The header deadline is a timer that cancels reqCtx; nothing else cancels it.
	// Stop's answer after the round trip settles the race with no goroutine and no channel:
	// true means the timer never fired and never will,
	// so headers that arrived can never be followed by a late cancel,
	// and the body then streams until the overall budget ctx expires.
	timer := time.AfterFunc(p.headerDeadline(req.Seconds), func() { cancelReq(errHeaderDeadline) })
	resp, err := p.client.Do(httpReq)
	if !timer.Stop() {
		// The deadline fired.
		// Whatever the client returned, nothing has been committed, so the outcome is a timeout;
		// a response that slipped through is released unread.
		if resp != nil {
			_ = resp.Body.Close()
		}

		return Outcome{Code: codeTimeout, Status: http.StatusGatewayTimeout}
	}
	if err != nil {
		return classifyBeforeHeaders(ctx, reqCtx)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, redirectDrainLimit))

		return Outcome{Code: codeRedirect, Status: http.StatusBadGateway}
	}

	header := w.Header()
	for _, name := range allowedHeaders() {
		for _, value := range resp.Header.Values(name) {
			header.Add(name, value)
		}
	}
	for name, value := range req.TargetHeaders {
		header.Set(name, value)
	}
	w.WriteHeader(resp.StatusCode)

	code := codeOK
	if resp.StatusCode >= 400 {
		code = fmt.Sprintf("upstream_%d", resp.StatusCode)
	}
	if _, err := io.Copy(w, contextReader{ctx: reqCtx, r: resp.Body}); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			code = codeClientGone
		} else {
			code = codeStreamFailed
		}
	}

	return Outcome{Code: code, Status: resp.StatusCode, Committed: true}
}

// classifyBeforeHeaders maps a failure before any response headers to its outcome.
// A deadline cause always wins; a client cancellation is reported as such;
// everything else (refused, reset, EOF, malformed response) is an unreachable upstream.
func classifyBeforeHeaders(ctx, reqCtx context.Context) Outcome {
	switch {
	case errors.Is(context.Cause(reqCtx), errHeaderDeadline), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return Outcome{Code: codeTimeout, Status: http.StatusGatewayTimeout}
	case errors.Is(ctx.Err(), context.Canceled):
		return Outcome{Code: codeClientGone}
	default:
		return Outcome{Code: codeUnreachable, Status: http.StatusBadGateway}
	}
}

// upstreamURL builds the plain-HTTP URL of the Pod's pprof handler.
func upstreamURL(req Request) string {
	u := "http://" + net.JoinHostPort(req.Target.PodIP, strconv.Itoa(int(req.Target.Port))) + req.Path
	if req.Seconds > 0 {
		u += "?seconds=" + strconv.Itoa(req.Seconds)
	}

	return u
}

// contextReader stops a body copy once ctx is done,
// so a cancelled or expired request is reported as such rather than read to the end.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(p)
}
