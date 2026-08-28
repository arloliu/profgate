package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// profileResponse is a 200 carrying profile bytes and the three target
// headers, the body served by whatever reader the test passes.
func profileResponse(body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":           {"application/octet-stream"},
			"X-Pprof-Target-Pod":     {"checkout-1"},
			"X-Pprof-Target-Node":    {"worker-07"},
			"X-Pprof-Target-Version": {"1.42.3"},
		},
		Body: io.NopCloser(body),
	}
}

const profileBytes = "\x1f\x8b profile bytes"

// profileTransport answers every request with profileBytes and records it.
type profileTransport struct {
	requests []*http.Request
}

func (pt *profileTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	pt.requests = append(pt.requests, r)
	return profileResponse(strings.NewReader(profileBytes)), nil
}

// runProfile runs the profile verb in a fresh working directory against pt
// and returns the exit code and that directory.
func runProfile(t *testing.T, te *testEnv, pt http.RoundTripper, args ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	te.env.transport = pt
	args = append([]string{"profile"}, args...)
	args = append(args, "--server", "https://g.example")
	return dispatch(context.Background(), te.env, clientVerbs(), args), dir
}

func TestProfileLocalRefusals(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStderr []string
	}{
		{name: "both port flags", args: []string{"payments/checkout", "cpu", "--port", "6060", "--port-name", "pprof"}, wantStderr: []string{"--port", "--port-name"}},
		{name: "seconds 0", args: []string{"payments/checkout", "cpu", "--seconds", "0"}, wantStderr: []string{"--seconds", "positive integer"}},
		{name: "seconds -1", args: []string{"payments/checkout", "cpu", "--seconds", "-1"}, wantStderr: []string{"--seconds", "positive integer"}},
		{name: "seconds abc", args: []string{"payments/checkout", "cpu", "--seconds", "abc"}, wantStderr: []string{"--seconds", "positive integer"}},
		{name: "unknown profile name", args: []string{"payments/checkout", "cpuu"}, wantStderr: []string{"cpuu", "cpu", "trace", "heap", "allocs", "goroutine", "mutex", "block", "threadcreate"}},
		{name: "output path not writable", args: []string{"payments/checkout", "heap", "-o", filepath.Join(t.TempDir(), "missing", "heap.pprof")}, wantStderr: []string{"missing"}},
		{name: "stdout and open together", args: []string{"payments/checkout", "heap", "-o", "-", "--open"}, wantStderr: []string{"-o -", "--open"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			code, dir := runProfile(t, te, refusingTransport(t), tc.args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(te.stderr.String(), want) {
					t.Fatalf("stderr = %q, want %q in it", te.stderr.String(), want)
				}
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("the working directory holds %d entries, want none", len(entries))
			}
		})
	}
}

func TestProfileSecondsAboveTheLimitIsSent(t *testing.T) {
	te := newTestEnv(t)
	var query string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		query = r.URL.RawQuery
		return jsonResponse(http.StatusBadRequest, `{"error":"effective duration 120s exceeds the limit of 60s","code":"seconds_exceeds_limit"}`), nil
	})
	code, dir := runProfile(t, te, rt, "payments/checkout", "cpu", "--seconds", "120")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	if query != "seconds=120" {
		t.Fatalf("query = %q, want seconds=120", query)
	}
	if !strings.Contains(te.stderr.String(), "seconds_exceeds_limit: effective duration 120s exceeds the limit of 60s") {
		t.Fatalf("stderr = %q, want the gateway's refusal", te.stderr.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("a refusal left %d entries in the working directory", len(entries))
	}
}

func TestProfileQuery(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		query string
	}{
		{name: "none", flags: nil, query: ""},
		{name: "seconds", flags: []string{"--seconds", "30"}, query: "seconds=30"},
		{name: "pod and version", flags: []string{"--pod", "checkout-1", "--version", "1.42.3"}, query: "pod=checkout-1&version=1.42.3"},
		{name: "port", flags: []string{"--port", "6061"}, query: "port=6061"},
		{name: "port name", flags: []string{"--port-name", "pprof-alt"}, query: "portName=pprof-alt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			pt := &profileTransport{}
			args := append([]string{"payments/checkout", "cpu"}, tc.flags...)
			code, _ := runProfile(t, te, pt, args...)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if len(pt.requests) != 1 {
				t.Fatalf("%d requests, want one", len(pt.requests))
			}
			r := pt.requests[0]
			if r.Method != http.MethodGet || r.URL.Path != "/v1/namespaces/payments/services/checkout/profiles/cpu" || r.URL.RawQuery != tc.query {
				t.Fatalf("request = %s %s?%s, want GET the profile route with %q", r.Method, r.URL.Path, r.URL.RawQuery, tc.query)
			}
		})
	}
}

func TestProfileDerivedFileName(t *testing.T) {
	te := newTestEnv(t)
	te.env.terminal = true
	code, dir := runProfile(t, te, &profileTransport{}, "payments/checkout", "cpu")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	const name = "payments-checkout-cpu-20260828T120000Z.pprof"
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a test reading the file it asked for
	if err != nil {
		t.Fatalf("the derived file: %v", err)
	}
	if string(data) != profileBytes {
		t.Fatalf("file = %q, want the profile bytes", data)
	}
	if !strings.Contains(te.stderr.String(), name) {
		t.Fatalf("stderr = %q, want the path named", te.stderr.String())
	}
	if te.stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing: a binary body never reaches a terminal by default", te.stdout.String())
	}
}

func TestProfileToStdout(t *testing.T) {
	te := newTestEnv(t)
	te.env.terminal = true
	code, dir := runProfile(t, te, &profileTransport{}, "payments/checkout", "heap", "-o", "-")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if te.stdout.String() != profileBytes {
		t.Fatalf("stdout = %q, want the profile bytes", te.stdout.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("-o - left %d entries in the working directory", len(entries))
	}
}

func TestProfileToNamedFile(t *testing.T) {
	te := newTestEnv(t)
	out := filepath.Join(t.TempDir(), "heap.pprof")
	code, _ := runProfile(t, te, &profileTransport{}, "payments/checkout", "heap", "-o", out)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	data, err := os.ReadFile(out) //nolint:gosec // a test reading the file it asked for
	if err != nil || string(data) != profileBytes {
		t.Fatalf("file = %q, %v; want the profile bytes", data, err)
	}
}

func TestProfileTargetHeaders(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		te := newTestEnv(t)
		code, _ := runProfile(t, te, &profileTransport{}, "payments/checkout", "heap")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		for _, want := range []string{"pod: checkout-1\n", "node: worker-07\n", "version: 1.42.3\n"} {
			if !strings.Contains(te.stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", te.stderr.String(), want)
			}
		}
	})
	t.Run("json", func(t *testing.T) {
		te := newTestEnv(t)
		code, dir := runProfile(t, te, &profileTransport{}, "payments/checkout", "heap", "--output", "json")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if !strings.Contains(te.stderr.String(), `{"pod":"checkout-1","node":"worker-07","version":"1.42.3"}`) {
			t.Fatalf("stderr = %q, want the three values as one JSON object", te.stderr.String())
		}
		if te.stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want nothing: the profile is bytes and goes to the file", te.stdout.String())
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("%d entries in the working directory, want the profile file", len(entries))
		}
	})
}

// cancellingBody yields some bytes and then the cancellation.
type cancellingBody struct {
	sent bool
}

func (b *cancellingBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, context.Canceled
	}
	b.sent = true
	return copy(p, "partial"), nil
}

func TestProfileCancelledMidBodyRemovesThePartialFile(t *testing.T) {
	te := newTestEnv(t)
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return profileResponse(&cancellingBody{}), nil
	})
	code, dir := runProfile(t, te, rt, "payments/checkout", "cpu")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("the partial file survived: %d entries", len(entries))
	}
}

// openRun is what the runner seam recorded: the name, the arguments, the
// context's error at the call, and what the file and its directory looked
// like while the viewer ran.
type openRun struct {
	name     string
	args     []string
	ctxErr   error
	fileMode os.FileMode
	dirMode  os.FileMode
	content  string
	path     string
}

// recordRunner records the call and inspects the file it was handed.
func recordRunner(t *testing.T, got *openRun) func(context.Context, string, ...string) error {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) error {
		got.name, got.args, got.ctxErr = name, args, ctx.Err()
		got.path = args[len(args)-1]
		if info, err := os.Stat(got.path); err == nil {
			got.fileMode = info.Mode().Perm()
		}
		if info, err := os.Stat(filepath.Dir(got.path)); err == nil {
			got.dirMode = info.Mode().Perm()
		}
		data, _ := os.ReadFile(got.path)
		got.content = string(data)
		return nil
	}
}

func TestProfileOpenNeedsGo(t *testing.T) {
	te := newTestEnv(t)
	te.env.lookPath = func(string) (string, error) { return "", errors.New("executable file not found") }
	te.env.run = func(context.Context, string, ...string) error {
		t.Fatal("the runner ran without go")
		return nil
	}
	code, dir := runProfile(t, te, refusingTransport(t), "payments/checkout", "heap", "--open")
	if code != 2 {
		t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
	}
	if !strings.Contains(te.stderr.String(), "go") || !strings.Contains(te.stderr.String(), "PATH") {
		t.Fatalf("stderr = %q, want go and PATH named", te.stderr.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("%d entries in the working directory, want none", len(entries))
	}
}

func TestProfileOpenRunsPprof(t *testing.T) {
	te := newTestEnv(t)
	var looked string
	te.env.lookPath = func(name string) (string, error) {
		looked = name
		return "/usr/local/go/bin/go", nil
	}
	var got openRun
	te.env.run = recordRunner(t, &got)
	code, dir := runProfile(t, te, &profileTransport{}, "payments/checkout", "heap", "--open")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if looked != "go" {
		t.Fatalf("looked up %q, want go", looked)
	}
	if got.name != "/usr/local/go/bin/go" {
		t.Fatalf("runner name = %q, want the resolved go", got.name)
	}
	if len(got.args) != 4 || got.args[0] != "tool" || got.args[1] != "pprof" || got.args[2] != "-http=:0" || got.args[3] != got.path {
		t.Fatalf("runner args = %q, want tool pprof -http=:0 <path>", got.args)
	}
	if got.ctxErr != nil {
		t.Fatalf("the runner's context was already done: %v", got.ctxErr)
	}
	if got.content != profileBytes {
		t.Fatalf("the viewer saw %q, want the profile bytes", got.content)
	}
	if got.fileMode != 0o600 || got.dirMode != 0o700 {
		t.Fatalf("file %o in directory %o, want 0600 in 0700", got.fileMode, got.dirMode)
	}
	if strings.HasPrefix(got.path, dir) {
		t.Fatalf("the temporary file %q is in the working directory", got.path)
	}
	if _, err := os.Stat(filepath.Dir(got.path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the temporary directory survived the viewer: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("--open left %d entries in the working directory", len(entries))
	}
}

func TestProfileOpenWithOutputPathKeepsTheFile(t *testing.T) {
	te := newTestEnv(t)
	te.env.lookPath = func(string) (string, error) { return "/usr/local/go/bin/go", nil }
	var got openRun
	te.env.run = recordRunner(t, &got)
	out := filepath.Join(t.TempDir(), "heap.pprof")
	code, _ := runProfile(t, te, &profileTransport{}, "payments/checkout", "heap", "-o", out, "--open")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if got.path != out {
		t.Fatalf("the viewer opened %q, want %q", got.path, out)
	}
	data, err := os.ReadFile(out) //nolint:gosec // a test reading the file it asked for
	if err != nil || string(data) != profileBytes {
		t.Fatalf("file = %q, %v; want the profile bytes to survive", data, err)
	}
}

func TestProfileOpenCancelledReachesTheRunnerBeforeCleanup(t *testing.T) {
	te := newTestEnv(t)
	te.env.lookPath = func(string) (string, error) { return "/usr/local/go/bin/go", nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	te.env.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return profileResponse(strings.NewReader(profileBytes)), nil
	})
	var got openRun
	var dirExisted bool
	te.env.run = func(runCtx context.Context, _ string, args ...string) error {
		got.ctxErr = runCtx.Err()
		_, err := os.Stat(filepath.Dir(args[len(args)-1]))
		dirExisted = err == nil
		return runCtx.Err()
	}
	t.Chdir(t.TempDir())
	code := dispatch(ctx, te.env, clientVerbs(), []string{"profile", "payments/checkout", "heap", "--open", "--server", "https://g.example"})
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	if !errors.Is(got.ctxErr, context.Canceled) {
		t.Fatalf("the runner saw %v, want the cancellation", got.ctxErr)
	}
	if !dirExisted {
		t.Fatal("the cleanup ran before the runner")
	}
}
