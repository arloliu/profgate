package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/k8s"
)

const (
	// cpuPath is the upstream path every row requests; the proxy treats paths opaquely.
	cpuPath = "/debug/pprof/profile"
	// handlerWait bounds how long a blocking upstream handler waits for its request context.
	handlerWait = 10 * time.Second
	// raceRuns is how many times the "headers at the deadline" row races the timer.
	raceRuns = 200
	// raceJitter spreads the upstream's header time around the deadline in that row.
	raceJitter = 20 * time.Millisecond
)

// fixture is one row's isolated world: a Proxy over a transport that counts response bodies
// and their closes, and a trap server that must never be reached.
type fixture struct {
	t         *testing.T
	p         *Proxy
	trap      *httptest.Server
	trapHits  atomic.Int32
	responses atomic.Int32
	closes    atomic.Int32
}

// countingTransport counts the non-nil responses it hands out and the closes of their bodies.
type countingTransport struct {
	next http.RoundTripper
	f    *fixture
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(req)
	if resp != nil {
		c.f.responses.Add(1)
		resp.Body = &countingBody{ReadCloser: resp.Body, f: c.f}
	}

	return resp, err
}

type countingBody struct {
	io.ReadCloser
	f    *fixture
	once sync.Once
}

func (b *countingBody) Close() error {
	b.once.Do(func() { b.f.closes.Add(1) })

	return b.ReadCloser.Close()
}

// newFixture builds a Proxy for one row, wraps its transport to count body closes,
// starts the trap server, and asserts at cleanup
// that every response handed out was closed exactly once.
func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()

	f := &fixture{t: t, p: New(opts)}
	f.p.client.Transport = &countingTransport{next: f.p.client.Transport, f: f}
	f.trap = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.trapHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		f.trap.Close()
		if got, want := f.closes.Load(), f.responses.Load(); got != want {
			t.Errorf("body closes = %d, want one per response (%d)", got, want)
		}
		if hits := f.trapHits.Load(); hits != 0 {
			t.Errorf("trap server hits = %d, want 0", hits)
		}
	})

	return f
}

// upstream starts a row's upstream server and returns it with its Target.
func (f *fixture) upstream(h http.HandlerFunc) (*httptest.Server, k8s.Target) {
	f.t.Helper()

	srv := httptest.NewServer(h)
	f.t.Cleanup(srv.Close)

	return srv, targetOf(f.t, srv.URL)
}

// targetOf parses an httptest URL into the Target the proxy dials.
func targetOf(t *testing.T, rawURL string) k8s.Target {
	t.Helper()

	host, portText, err := net.SplitHostPort(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatalf("SplitHostPort(%q) error = %v", rawURL, err)
	}
	port, err := strconv.ParseInt(portText, 10, 32)
	if err != nil {
		t.Fatalf("ParseInt(%q) error = %v", portText, err)
	}

	return k8s.Target{
		Namespace: "payment", Service: "payment-api", Pod: "payment-api-1", Node: "worker-07",
		PodIP: host, Port: int32(port), Version: "1.42.3", UID: "u1",
	}
}

// request builds the Request every row sends:
// the cpu path with the given duration and the target headers the HTTP layer would attach.
func request(target k8s.Target, seconds int) Request {
	return Request{
		Target:  target,
		Path:    cpuPath,
		Seconds: seconds,
		TargetHeaders: map[string]string{
			"X-Pprof-Target-Pod":     target.Pod,
			"X-Pprof-Target-Node":    target.Node,
			"X-Pprof-Target-Version": target.Version,
		},
	}
}

// do runs one request against the row's upstream under a context with the stated deadline.
func (f *fixture) do(deadline time.Duration, target k8s.Target, seconds int) (Outcome, *httptest.ResponseRecorder) {
	f.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	rec := httptest.NewRecorder()
	out := f.p.Do(ctx, rec, request(target, seconds))

	return out, rec
}

// assertTargetHeaders checks the gateway-owned headers carry the request's values, not the upstream's.
func assertTargetHeaders(t *testing.T, rec *httptest.ResponseRecorder, target k8s.Target) {
	t.Helper()

	want := map[string]string{
		"X-Pprof-Target-Pod":     target.Pod,
		"X-Pprof-Target-Node":    target.Node,
		"X-Pprof-Target-Version": target.Version,
	}
	for name, value := range want {
		if values := rec.Header().Values(name); len(values) != 1 || values[0] != value {
			t.Errorf("header %s = %q, want [%q]", name, values, value)
		}
	}
}

// assertNotCommitted checks nothing reached the client: no header, no body, no status.
func assertNotCommitted(t *testing.T, out Outcome, rec *httptest.ResponseRecorder, code string, status int) {
	t.Helper()

	if out.Committed {
		t.Errorf("Committed = true, want false")
	}
	if out.Code != code || out.Status != status {
		t.Errorf("Outcome = {%q %d}, want {%q %d}", out.Code, out.Status, code, status)
	}
	if len(rec.Header()) != 0 {
		t.Errorf("headers written before a decision: %v", rec.Header())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body written before a decision: %q", rec.Body.String())
	}
}

// assertNoLeak checks no header value and no written byte names the upstream address.
func assertNoLeak(t *testing.T, rec *httptest.ResponseRecorder, target k8s.Target) {
	t.Helper()

	port := strconv.Itoa(int(target.Port))
	for name, values := range rec.Header() {
		for _, v := range values {
			if strings.Contains(v, target.PodIP) || strings.Contains(v, port) {
				t.Errorf("header %s leaks the upstream address: %q", name, v)
			}
		}
	}
	if body := rec.Body.String(); strings.Contains(body, target.PodIP) || strings.Contains(body, port) {
		t.Errorf("body leaks the upstream address: %q", body)
	}
}

// hijackAndClose takes the raw connection, writes prefix, and closes it.
func hijackAndClose(t *testing.T, w http.ResponseWriter, prefix string) {
	t.Helper()

	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("Hijack() error = %v", err)

		return
	}
	if prefix != "" {
		if _, err := io.WriteString(conn, prefix); err != nil {
			t.Errorf("write after hijack error = %v", err)
		}
	}
	_ = conn.Close()
}

// flush pushes buffered response bytes to the client so timing rows observe them promptly.
func flush(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Errorf("Flush() error = %v", err)
	}
}

// notifyingWriter signals once the first body byte reaches the client side.
type notifyingWriter struct {
	*httptest.ResponseRecorder
	once  sync.Once
	first chan struct{}
}

func (n *notifyingWriter) Write(p []byte) (int, error) {
	n.once.Do(func() { close(n.first) })

	return n.ResponseRecorder.Write(p)
}

func TestDo(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != cpuPath || r.URL.Query().Get("seconds") != "5" {
				t.Errorf("upstream got %s, want %s?seconds=5", r.URL.RequestURI(), cpuPath)
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "abc")
		})

		out, rec := f.do(2*time.Second, target, 5)

		if out.Code != "ok" || out.Status != http.StatusOK || !out.Committed {
			t.Errorf("Outcome = %+v, want ok/200/committed", out)
		}
		if rec.Code != http.StatusOK || rec.Body.String() != "abc" {
			t.Errorf("response = %d %q, want 200 \"abc\"", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("Content-Type = %q, want application/octet-stream", got)
		}
	})

	t.Run("target headers on 2xx", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

		out, rec := f.do(2*time.Second, target, 0)

		if !out.Committed {
			t.Fatalf("Outcome = %+v, want committed", out)
		}
		assertTargetHeaders(t, rec, target)
	})

	t.Run("gzip preserved", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte("profile bytes"))
		_ = zw.Close()
		want := buf.Bytes()

		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(want)
		})

		out, rec := f.do(2*time.Second, target, 0)

		if out.Code != "ok" {
			t.Errorf("Code = %q, want ok", out.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("body = %x, want the gzip bytes %x", rec.Body.Bytes(), want)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want gzip", got)
		}
	})

	t.Run("disposition preserved", func(t *testing.T) {
		const disposition = `attachment; filename="profile"`
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Disposition", disposition)
			w.WriteHeader(http.StatusOK)
		})

		_, rec := f.do(2*time.Second, target, 0)

		if got := rec.Header().Get("Content-Disposition"); got != disposition {
			t.Errorf("Content-Disposition = %q, want %q", got, disposition)
		}
	})

	t.Run("headers dropped", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) {
			h := w.Header()
			h.Set("Set-Cookie", "session=1")
			h.Set("Server", "upstream/1.0")
			h.Set("Connection", "keep-alive")
			h.Set("X-Pprof-Target-Pod", "forged")
			h.Set("Cache-Control", "public")
			w.WriteHeader(http.StatusOK)
		})

		_, rec := f.do(2*time.Second, target, 0)

		for _, name := range []string{"Set-Cookie", "Server", "Connection", "Cache-Control"} {
			if values := rec.Header().Values(name); len(values) != 0 {
				t.Errorf("header %s = %q, want dropped", name, values)
			}
		}
		assertTargetHeaders(t, rec, target)
	})

	t.Run("404 passthrough", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusNotFound)
		})

		out, rec := f.do(2*time.Second, target, 0)

		if out.Code != "upstream_404" || out.Status != http.StatusNotFound || !out.Committed {
			t.Errorf("Outcome = %+v, want upstream_404/404/committed", out)
		}
		if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != "nope" {
			t.Errorf("response = %d %q, want 404 \"nope\"", rec.Code, rec.Body.String())
		}
		assertTargetHeaders(t, rec, target)
		assertNoLeak(t, rec, target)
	})

	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("%d passthrough", status), func(t *testing.T) {
			f := newFixture(t, Options{})
			_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })

			out, rec := f.do(2*time.Second, target, 0)

			want := fmt.Sprintf("upstream_%d", status)
			if out.Code != want || out.Status != status || !out.Committed {
				t.Errorf("Outcome = %+v, want %s/%d/committed", out, want, status)
			}
			if rec.Code != status {
				t.Errorf("status written = %d, want %d", rec.Code, status)
			}
			assertNoLeak(t, rec, target)
		})
	}

	t.Run("absolute redirect", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, f.trap.URL+"/x", http.StatusFound)
		})

		out, rec := f.do(2*time.Second, target, 0)

		assertNotCommitted(t, out, rec, "upstream_redirect", http.StatusBadGateway)
		assertNoLeak(t, rec, target)
	})

	t.Run("relative redirect", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/x", http.StatusFound)
		})

		out, rec := f.do(2*time.Second, target, 0)

		assertNotCommitted(t, out, rec, "upstream_redirect", http.StatusBadGateway)
		assertNoLeak(t, rec, target)
	})

	t.Run("env proxy ignored", func(t *testing.T) {
		f := newFixture(t, Options{})
		t.Setenv("HTTP_PROXY", f.trap.URL)
		t.Setenv("http_proxy", f.trap.URL)
		var reached atomic.Int32
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) {
			reached.Add(1)
			w.WriteHeader(http.StatusOK)
		})

		out, _ := f.do(2*time.Second, target, 0)

		if out.Code != "ok" || reached.Load() != 1 {
			t.Errorf("Outcome = %+v, upstream hits = %d, want ok reached directly", out, reached.Load())
		}
	})

	t.Run("refused", func(t *testing.T) {
		f := newFixture(t, Options{})
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen() error = %v", err)
		}
		target := targetOf(t, "http://"+ln.Addr().String())
		_ = ln.Close()

		out, rec := f.do(2*time.Second, target, 0)

		assertNotCommitted(t, out, rec, "upstream_unreachable", http.StatusBadGateway)
		assertNoLeak(t, rec, target)
	})

	t.Run("reset before headers", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) { hijackAndClose(t, w, "") })

		out, rec := f.do(2*time.Second, target, 0)

		assertNotCommitted(t, out, rec, "upstream_unreachable", http.StatusBadGateway)
		assertNoLeak(t, rec, target)
	})

	t.Run("eof before headers", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) {
			hijackAndClose(t, w, "HTTP/1.1 200 OK\r\n")
		})

		out, rec := f.do(2*time.Second, target, 0)

		assertNotCommitted(t, out, rec, "upstream_unreachable", http.StatusBadGateway)
		assertNoLeak(t, rec, target)
	})

	t.Run("header deadline", func(t *testing.T) {
		f := newFixture(t, Options{HeaderDeadline: func(int) time.Duration { return 200 * time.Millisecond }})
		_, target := f.upstream(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })

		start := time.Now()
		out, rec := f.do(60*time.Second, target, 0)

		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("Do took %s, want under 1s", elapsed)
		}
		assertNotCommitted(t, out, rec, "upstream_timeout", http.StatusGatewayTimeout)
		assertNoLeak(t, rec, target)
	})

	t.Run("overall deadline", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })

		out, rec := f.do(200*time.Millisecond, target, 0)

		assertNotCommitted(t, out, rec, "upstream_timeout", http.StatusGatewayTimeout)
		assertNoLeak(t, rec, target)
	})

	t.Run("body outlives header deadline", func(t *testing.T) {
		f := newFixture(t, Options{HeaderDeadline: func(int) time.Duration { return 200 * time.Millisecond }})
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			flush(t, w)
			for i := range 6 {
				time.Sleep(100 * time.Millisecond)
				_, _ = fmt.Fprintf(w, "chunk%d", i)
				flush(t, w)
			}
		})

		out, rec := f.do(5*time.Second, target, 0)

		if out.Code != "ok" || !out.Committed {
			t.Errorf("Outcome = %+v, want ok/committed", out)
		}
		if want := "chunk0chunk1chunk2chunk3chunk4chunk5"; rec.Body.String() != want {
			t.Errorf("body = %q, want %q", rec.Body.String(), want)
		}
	})

	t.Run("headers at the deadline", func(t *testing.T) {
		const deadline = 50 * time.Millisecond
		f := newFixture(t, Options{HeaderDeadline: func(int) time.Duration { return deadline }})
		var arrivals atomic.Int64
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) {
			// Headers land in a narrow window around the deadline so both arms of the race occur.
			n := time.Duration(arrivals.Add(1))
			time.Sleep(deadline - raceJitter/2 + raceJitter*n/raceRuns)
			w.WriteHeader(http.StatusOK)
			// Timed-out runs close the connection under the handler, so flush errors are expected here.
			_ = http.NewResponseController(w).Flush()
			for i := range 3 {
				time.Sleep(100 * time.Millisecond)
				_, _ = fmt.Fprintf(w, "chunk%d", i)
				_ = http.NewResponseController(w).Flush()
			}
		})

		var wg sync.WaitGroup
		var timeouts, oks atomic.Int32
		for run := range raceRuns {
			wg.Go(func() {
				out, rec := f.do(5*time.Second, target, 0)
				switch {
				case out.Code == "upstream_timeout" && !out.Committed && rec.Body.Len() == 0 && len(rec.Header()) == 0:
					timeouts.Add(1)
				case out.Code == "ok" && out.Committed && rec.Body.String() == "chunk0chunk1chunk2":
					oks.Add(1)
				default:
					t.Errorf("run %d: Outcome = %+v with body %q, want a clean timeout or a full body", run, out, rec.Body.String())
				}
			})
		}
		wg.Wait()
		t.Logf("timeouts = %d, oks = %d", timeouts.Load(), oks.Load())
	})

	t.Run("stream failed", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "half-")
			flush(t, w)
			hijackAndClose(t, w, "")
		})

		out, rec := f.do(2*time.Second, target, 0)

		if out.Code != "upstream_stream_failed" || out.Status != http.StatusOK || !out.Committed {
			t.Errorf("Outcome = %+v, want upstream_stream_failed/200/committed", out)
		}
		if rec.Body.String() != "half-" {
			t.Errorf("body = %q, want the half that arrived", rec.Body.String())
		}
		assertNoLeak(t, rec, target)
	})

	t.Run("client gone", func(t *testing.T) {
		f := newFixture(t, Options{})
		upstreamCancelled := make(chan struct{})
		_, target := f.upstream(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "partial")
			flush(t, w)
			select {
			case <-r.Context().Done():
				close(upstreamCancelled)
			case <-time.After(handlerWait):
				t.Errorf("upstream request context did not cancel within %s", handlerWait)
			}
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := &notifyingWriter{ResponseRecorder: httptest.NewRecorder(), first: make(chan struct{})}
		go func() {
			<-w.first
			cancel()
		}()

		out := f.p.Do(ctx, w, request(target, 0))

		if out.Code != "client_gone" || !out.Committed {
			t.Errorf("Outcome = %+v, want client_gone/committed", out)
		}
		select {
		case <-upstreamCancelled:
		case <-time.After(handlerWait):
			t.Errorf("upstream handler did not observe the cancellation within %s", handlerWait)
		}
		assertNoLeak(t, w.ResponseRecorder, target)
	})

	t.Run("independent header deadlines", func(t *testing.T) {
		f := newFixture(t, Options{HeaderDeadline: func(seconds int) time.Duration {
			if seconds == 1 {
				return 100 * time.Millisecond
			}

			return 2 * time.Second
		}})
		_, target := f.upstream(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(500 * time.Millisecond):
				w.WriteHeader(http.StatusOK)
			case <-r.Context().Done():
			}
		})

		var wg sync.WaitGroup
		outcomes := make([]Outcome, 2)
		for i := range outcomes {
			wg.Go(func() {
				outcomes[i], _ = f.do(5*time.Second, target, i+1)
			})
		}
		wg.Wait()

		if outcomes[0].Code != "upstream_timeout" || outcomes[0].Committed {
			t.Errorf("Seconds=1 Outcome = %+v, want upstream_timeout", outcomes[0])
		}
		if outcomes[1].Code != "ok" || !outcomes[1].Committed {
			t.Errorf("Seconds=2 Outcome = %+v, want ok", outcomes[1])
		}
	})

	t.Run("an idle upstream connection is closed after its timeout", func(t *testing.T) {
		f := newFixture(t, Options{IdleConnTimeout: 100 * time.Millisecond})
		var mu sync.Mutex
		states := map[net.Conn][]http.ConnState{}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "abc")
		}))
		srv.Config.ConnState = func(c net.Conn, state http.ConnState) {
			mu.Lock()
			defer mu.Unlock()
			states[c] = append(states[c], state)
		}
		srv.Start()
		t.Cleanup(srv.Close)
		target := targetOf(t, srv.URL)

		out, rec := f.do(2*time.Second, target, 0)

		if out.Code != "ok" || rec.Body.String() != "abc" {
			t.Fatalf("Outcome = %+v body %q, want ok \"abc\"", out, rec.Body.String())
		}
		// The upstream ended the body and the proxy closed it, so the connection sits idle in the pool;
		// the transport's idle timeout is what closes it, and nothing else does.
		closed := func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, seen := range states {
				for _, state := range seen {
					if state == http.StateClosed {
						return true
					}
				}
			}

			return false
		}
		deadline := time.Now().Add(time.Second)
		for !closed() {
			if time.Now().After(deadline) {
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("idle upstream connection still open after 1s; states = %v", states)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("a client that stops reading is cut at the budget", func(t *testing.T) {
		f := newFixture(t, Options{})
		_, target := f.upstream(streamUntilGone(t))
		type result struct {
			out     Outcome
			elapsed time.Duration
			// end is the budget's end and armed the write deadline Do set on the client connection.
			end, armed time.Time
		}
		results := make(chan result, 1)
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
			defer cancel()
			end, _ := ctx.Deadline()
			rw := &deadlineRecorder{ResponseWriter: w}
			start := time.Now()
			out := f.p.Do(ctx, rw, request(target, 0))
			results <- result{out: out, elapsed: time.Since(start), end: end, armed: rw.armed}
		}))
		t.Cleanup(gateway.Close)

		// The client reads the headers and then holds the socket open without reading;
		// its close at cleanup is what would end the blocked write if the deadline did not.
		holdAfterHeaders(t, gateway.Listener.Addr().String(), cpuPath)

		select {
		case got := <-results:
			if got.out.Code != "upstream_stream_failed" || !got.out.Committed {
				t.Errorf("Outcome = %+v, want upstream_stream_failed committed", got.out)
			}
			if got.elapsed > 1500*time.Millisecond {
				t.Errorf("Do took %s, want under 1.5s: the budget is the bound", got.elapsed)
			}
			// The write blocked and then failed only because the deadline was armed at the budget's end;
			// the recorded deadline is what proves the failure was the deadline's and not the socket's.
			if !got.armed.Equal(got.end) {
				t.Errorf("write deadline armed at %v, want the budget's end %v", got.armed, got.end)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Do still blocked 3s after a 500ms budget: the write to a client that stopped reading has no deadline")
		}
	})

	t.Run("an HTTP/2 stream reset at the budget is upstream_stream_failed", func(t *testing.T) {
		f := newFixture(t, Options{})
		// The upstream commits enough body to carry the headers to the client, then holds without a byte,
		// so the proxy is waiting on the upstream, not on the client, when the budget ends.
		_, target := f.upstream(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte("x"), 64<<10))
			flush(t, w)
			<-r.Context().Done()
		})
		results := make(chan Outcome, 1)
		gateway := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			budget := budgetWithoutTimer{Context: ctx, end: time.Now().Add(300 * time.Millisecond)}
			results <- f.p.Do(budget, w, request(target, 0))
		}))
		gateway.EnableHTTP2 = true
		gateway.StartTLS()
		t.Cleanup(gateway.Close)

		// The client reads the headers and stops there: the body is never read and the stream stays open.
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, gateway.URL+cpuPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if resp.ProtoMajor != 2 {
			t.Fatalf("response proto = %s, want HTTP/2.0: the row is about the stream reset", resp.Proto)
		}

		select {
		case out := <-results:
			if out.Code != "upstream_stream_failed" || !out.Committed {
				t.Errorf("Outcome = %+v, want upstream_stream_failed committed: the budget ended, the client did not leave", out)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Do still blocked 3s after a 300ms budget")
		}
	})
}

// deadlineRecorder is the writer the stalled-client row hands to Do:
// it records the write deadline Do arms and forwards it to the connection behind it,
// so the row proves the deadline was set at the budget's end rather than inferring it from a write that failed.
// Do is the only writer on the connection, so the field needs no lock;
// the row reads it after Do returns.
type deadlineRecorder struct {
	http.ResponseWriter
	armed time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.armed = t

	return http.NewResponseController(d.ResponseWriter).SetWriteDeadline(t)
}

// Unwrap keeps http.ResponseController reaching the connection for everything else.
func (d *deadlineRecorder) Unwrap() http.ResponseWriter { return d.ResponseWriter }

// budgetWithoutTimer carries a budget's end and no timer that would end it.
// Under HTTP/2 the write deadline fires by resetting the stream, which cancels the request context;
// a production budget has its own timer at the same instant, and the two race.
// Dropping the timer forces the losing order every time:
// the reset is the only thing that fires, the context is cancelled rather than expired,
// and the copy ends in a cancellation that must still read as the budget's expiry.
type budgetWithoutTimer struct {
	context.Context
	end time.Time
}

func (b budgetWithoutTimer) Deadline() (time.Time, bool) { return b.end, true }

// streamUntilGone is an upstream that flushes 64 KiB chunks until its request context ends,
// so a client that stops reading fills the loopback buffers within milliseconds.
func streamUntilGone(t *testing.T) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
		}
	}
}

// holdAfterHeaders dials addr, sends a GET for path, reads the response headers, and then stops reading;
// the socket stays open until the test's cleanup closes it.
func holdAfterHeaders(t *testing.T, addr, path string) {
	t.Helper()

	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("Dial(%q) error = %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: gateway\r\n\r\n", path); err != nil {
		t.Fatalf("write request error = %v", err)
	}
	// The status line and the headers are read by hand: an http.Response's Close would drain the body,
	// and the body is what this client must never read.
	tp := textproto.NewReader(bufio.NewReader(conn))
	status, err := tp.ReadLine()
	if err != nil {
		t.Fatalf("read status line error = %v", err)
	}
	if !strings.HasPrefix(status, "HTTP/1.1 200 ") {
		t.Fatalf("status line = %q, want 200 before the client stops reading", status)
	}
	if _, err := tp.ReadMIMEHeader(); err != nil {
		t.Fatalf("read headers error = %v", err)
	}
}

func TestNew(t *testing.T) {
	t.Run("default header deadline follows the spec rule", func(t *testing.T) {
		p := New(Options{})
		cases := []struct {
			seconds int
			want    time.Duration
		}{
			{seconds: 0, want: 30 * time.Second},
			{seconds: 1, want: 11 * time.Second},
			{seconds: 30, want: 40 * time.Second},
		}
		for _, tc := range cases {
			if got := p.headerDeadline(tc.seconds); got != tc.want {
				t.Errorf("headerDeadline(%d) = %s, want %s", tc.seconds, got, tc.want)
			}
		}
	})

	t.Run("transport is pinned", func(t *testing.T) {
		p := New(Options{})
		tr, ok := p.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport is %T, want *http.Transport", p.client.Transport)
		}
		if tr.Proxy != nil {
			t.Errorf("Proxy is set, want nil so the environment is never consulted")
		}
		if !tr.DisableCompression {
			t.Errorf("DisableCompression = false, want true")
		}
		if tr.ResponseHeaderTimeout != 0 {
			t.Errorf("ResponseHeaderTimeout = %s, want unset", tr.ResponseHeaderTimeout)
		}
		// The pool's bounds are an inventory of the transport's settings;
		// the idle close itself is exercised under TestDo.
		if tr.MaxIdleConns != 100 {
			t.Errorf("MaxIdleConns = %d, want 100", tr.MaxIdleConns)
		}
		if tr.IdleConnTimeout != 90*time.Second {
			t.Errorf("IdleConnTimeout = %s, want 90s", tr.IdleConnTimeout)
		}
		if tr.MaxIdleConnsPerHost != 0 {
			t.Errorf("MaxIdleConnsPerHost = %d, want 0 so Go's default of two applies", tr.MaxIdleConnsPerHost)
		}
		if tr.DisableKeepAlives {
			t.Errorf("DisableKeepAlives = true, want false so a client fetching several profiles reuses its connection")
		}
	})
}
