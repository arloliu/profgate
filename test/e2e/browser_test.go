//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const (
	// browserMajorFloor and browserMajorCeiling bound the Chromium major version the console scenarios accept.
	// The floor is old enough that every platform feature the page uses is long established —
	// ES modules, fetch, URL, URLSearchParams, crypto.getRandomValues, and history.replaceState —
	// and new enough for every DevTools Protocol command this file sends.
	// The ceiling is the major the workflow pins and moves with that pin.
	// A version outside either bound fails the scenario rather than skipping it:
	// a browser too old for the page is a red test and not a mystery,
	// and one newer than anything this suite has run against is a claim nobody checked.
	browserMajorFloor   = 120
	browserMajorCeiling = 152

	// browserEnv names an executable explicitly, ahead of everything on PATH.
	// A value that names nothing resolvable fails the console scenarios;
	// searching PATH instead would run them against a browser nobody pinned.
	browserEnv = "PROFGATE_E2E_BROWSER"

	// browserDisabledFeatures is what the console sessions turn off in Chromium.
	// The first three are chromedp's own default value, repeated because a second
	// --disable-features replaces that value rather than adding to it;
	// a chromedp upgrade that changes its default has to be carried across to here.
	// NetworkTimeServiceQuerying is this suite's own, and it is what keeps the page loads whole.
	// Chromium asks Google for the current time as it starts,
	// and installing the answer, some tens of milliseconds later, replaces the certificate verifier;
	// every verification in flight at that moment is abandoned with ERR_CERT_VERIFIER_CHANGED.
	// The page opens several connections to the gateway at once for its modules,
	// so a load that overlaps the replacement loses one or two of them,
	// and app.js is left with imports that never resolve and a page that never runs.
	// Ignoring certificate errors does not cover it: the verification was abandoned, not failed.
	browserDisabledFeatures = "site-per-process,Translate,BlinkGenPropertyTrees,NetworkTimeServiceQuerying"

	// browserDeadline bounds one browser action: a navigation, a click, or an evaluation.
	browserDeadline = 60 * time.Second
	// downloadDeadline bounds the wait for a started download to finish.
	downloadDeadline = 60 * time.Second
)

// browserCandidates are the names Chromium and Chrome ship under, in search order.
var browserCandidates = []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"}

// majorRe reads a major version as the first run of digits in what --version printed,
// so "Chromium 141.0.7390.54" and "Google Chrome 141.0.7390.54" both give 141.
var majorRe = regexp.MustCompile(`[0-9]+`)

// browser is the Chromium the console scenarios drive, discovered once in TestMain.
// A machine with none leaves path empty and skip filled, and both scenarios skip with that reason.
// An executable whose --version printed no digits leaves major zero,
// which is outside the accepted range, so the scenario fails rather than skips:
// that is a broken pin and not an absent browser.
type browser struct {
	path    string // the executable, from PROFGATE_E2E_BROWSER or from PATH
	version string // what --version printed, whole
	major   int    // the major version parsed out of it
	skip    string // why no executable was found, naming everything that was looked for
}

// discoverBrowser finds the executable the console scenarios drive and reads its version.
// It runs once, in TestMain, because every field of the lane matrix describes a cluster
// and a browser is a property of the machine running the test.
func discoverBrowser(ctx context.Context, log *slog.Logger) browser {
	path := ""
	if named := os.Getenv(browserEnv); named != "" {
		found, err := exec.LookPath(named)
		if err != nil {
			// A name given explicitly and not resolvable is a broken pin, not a reason to search PATH:
			// the version stays unread and the major stays zero, which fails the scenario rather than skipping it.
			return browser{path: named, version: fmt.Sprintf("%s names %s, which does not resolve: %v", browserEnv, named, err)}
		}
		path = found
	}
	for _, name := range browserCandidates {
		if path != "" {
			break
		}
		if found, err := exec.LookPath(name); err == nil {
			path = found
		}
	}
	if path == "" {
		return browser{skip: fmt.Sprintf(
			"no browser found: %s names none, and none of %s is on PATH; the console scenarios need one",
			browserEnv, strings.Join(browserCandidates, ", "))}
	}

	b := browser{path: path}
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput() //nolint:gosec // the path came from PATH or from the environment
	if err != nil {
		b.version = fmt.Sprintf("%s --version failed: %v", path, err)
		log.Error("browser", "path", path, "error", err)

		return b
	}
	b.version = strings.TrimSpace(string(out))
	if m := majorRe.FindString(b.version); m != "" {
		b.major, _ = strconv.Atoi(m)
	}
	log.Info("browser", "path", b.path, "version", b.version, "major", b.major)

	return b
}

// requireBrowser is what a console scenario starts with:
// a machine with no executable skips the scenario by name with the reason discovery recorded,
// and one whose version falls outside the pinned range fails it.
func requireBrowser(t *testing.T, h *Harness) browser {
	t.Helper()
	b := h.Browser
	if b.skip != "" {
		t.Log(b.skip)
		t.Skip(b.skip)
	}
	if b.major < browserMajorFloor || b.major > browserMajorCeiling {
		t.Fatalf("%s reports %q, major %d, outside the %d to %d this suite is pinned to",
			b.path, b.version, b.major, browserMajorFloor, browserMajorCeiling)
	}
	t.Logf("driving %s (%s)", b.path, b.version)

	return b
}

// sentRequest is one request as the page sent it, read from the browser's own network events.
// The headers are the only place the start POST's media type and idempotency key can be observed
// as the page wrote them.
type sentRequest struct {
	method string
	url    string
	header map[string][]string
}

// headerValues returns every value the request carried under name, matched case-insensitively,
// because a header name on the wire has no case.
// A repeated header reaches the protocol as one entry whose values are newline separated,
// so the split is what makes a count of them a count of what went out.
func (r sentRequest) headerValues(name string) []string {
	for k, v := range r.header {
		if !strings.EqualFold(k, name) {
			continue
		}
		out := make([]string, 0, len(v))
		for _, one := range v {
			out = append(out, strings.Split(one, "\n")...)
		}

		return out
	}

	return nil
}

// session is one headless Chromium driving the console.
// The Log and Runtime domains are enabled and their events collected before the first navigation,
// so "the browser reported no Content Security Policy violation and no uncaught exception"
// is proven by an observer rather than by an absence;
// Network carries the requests the page sent, and Fetch, when credentials are given,
// answers the HTTP authentication challenge.
type session struct {
	ctx      context.Context
	download string // the directory downloads land in

	mu         sync.Mutex
	entries    []string
	violations []string
	exceptions []string
	requests   []sentRequest
	challenges int
	finished   chan string
}

// sessionOptions is what varies between the two console sessions.
type sessionOptions struct {
	// MapTo is the local host and port the name the gateway's certificate carries resolves to.
	MapTo string
	// User and Password, when set, answer the HTTP authentication challenge,
	// which needs the Fetch domain and the request interception it turns on.
	User     string
	Password string
}

// newSession launches the browser, enables the protocol domains before any navigation,
// and returns the session both scenarios drive.
// Every request is paused once Fetch is enabled, so the listener continues each one;
// a paused request nobody continues is a page that never loads.
func newSession(t *testing.T, b browser, o sessionOptions) *session {
	t.Helper()
	dir := t.TempDir()
	opts := append([]chromedp.ExecAllocatorOption{},
		chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.ExecPath(b.path),
		chromedp.NoSandbox,
		chromedp.IgnoreCertErrors,
		// The certificate certifies the name the gateway is reached by and the callback names it too,
		// while the forward is a loopback port: the browser has no dialer, so the resolver carries the mapping.
		// The port is part of the rule because the name is reached on 443 and the forward is not.
		chromedp.Flag("host-resolver-rules", "MAP "+tlsHost+":443 "+o.MapTo),
		chromedp.Flag("disable-features", browserDisabledFeatures),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(t.Context(), opts...)
	t.Cleanup(cancelAlloc)
	ctx, cancel := chromedp.NewContext(allocCtx)
	t.Cleanup(cancel)

	s := &session{ctx: ctx, download: filepath.Join(dir, "downloads"), finished: make(chan string, 16)}
	if err := os.MkdirAll(s.download, 0o750); err != nil {
		t.Fatal(err)
	}
	chromedp.ListenTarget(ctx, func(ev any) { s.observe(ev, o) })
	chromedp.ListenBrowser(ctx, func(ev any) { s.observe(ev, o) })

	// The browser is started on the session's own context and not on a deadline derived from it:
	// chromedp binds the target's event loop to the context of the first run,
	// so a run under a deadline would take the whole session down when that deadline's cancel fired.
	if err := chromedp.Run(ctx); err != nil {
		t.Fatalf("start %s: %v", b.path, err)
	}

	// The domains come first and the navigation second.
	// Enabling them after the first navigation would satisfy the sentence while observing nothing.
	actions := []chromedp.Action{
		cdplog.Enable(),
		runtime.Enable(),
		network.Enable(),
		cdpbrowser.SetDownloadBehavior(cdpbrowser.SetDownloadBehaviorBehaviorAllow).
			WithDownloadPath(s.download).WithEventsEnabled(true),
	}
	if o.User != "" {
		actions = append(actions, fetch.Enable().WithHandleAuthRequests(true))
	}
	s.run(t, "enable the protocol domains", actions...)

	return s
}

// observe records what the four domains report and answers what they ask.
// A protocol command sent from inside a listener would deadlock the event loop, so each answer runs in its own goroutine.
func (s *session) observe(ev any, o sessionOptions) {
	switch e := ev.(type) {
	case *cdplog.EventEntryAdded:
		s.mu.Lock()
		line := fmt.Sprintf("%s %s: %s (%s)", e.Entry.Source, e.Entry.Level, e.Entry.Text, e.Entry.URL)
		s.entries = append(s.entries, line)
		if e.Entry.Source == cdplog.SourceSecurity || strings.Contains(strings.ToLower(e.Entry.Text), "content security policy") {
			s.violations = append(s.violations, line)
		}
		s.mu.Unlock()
	case *runtime.EventExceptionThrown:
		s.mu.Lock()
		s.exceptions = append(s.exceptions, e.ExceptionDetails.Error())
		s.mu.Unlock()
	case *network.EventRequestWillBeSent:
		header := map[string][]string{}
		for k, v := range e.Request.Headers {
			header[k] = append(header[k], fmt.Sprint(v))
		}
		s.mu.Lock()
		s.requests = append(s.requests, sentRequest{method: e.Request.Method, url: e.Request.URL, header: header})
		s.mu.Unlock()
	case *cdpbrowser.EventDownloadProgress:
		if e.State == cdpbrowser.DownloadProgressStateCompleted {
			select {
			case s.finished <- e.GUID:
			default:
			}
		}
	case *fetch.EventAuthRequired:
		s.mu.Lock()
		s.challenges++
		s.mu.Unlock()
		go func() {
			answer := &fetch.AuthChallengeResponse{Response: fetch.AuthChallengeResponseResponseProvideCredentials,
				Username: o.User, Password: o.Password}
			_ = fetch.ContinueWithAuth(e.RequestID, answer).Do(s.executor())
		}()
	case *fetch.EventRequestPaused:
		go func() { _ = fetch.ContinueRequest(e.RequestID).Do(s.executor()) }()
	}
}

// executor is the protocol executor for the session's own target.
func (s *session) executor() context.Context {
	return cdp.WithExecutor(s.ctx, chromedp.FromContext(s.ctx).Target)
}

// run performs one group of actions under the action deadline and fails the test with what it was doing.
func (s *session) run(t *testing.T, what string, actions ...chromedp.Action) {
	t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, browserDeadline)
	defer cancel()
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatalf("%s: %v\n%s", what, err, s.report())
	}
}

// eval evaluates an expression in the page and decodes the result into out.
// The expression is wrapped so its value is awaited, which is what an evaluation of a promise needs.
func (s *session) eval(t *testing.T, what, expr string, out any) {
	t.Helper()
	s.run(t, what, chromedp.Evaluate(expr, out, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}))
}

// location is the page's current URL.
func (s *session) location(t *testing.T) string {
	t.Helper()
	var got string
	s.eval(t, "read the location", "window.location.href", &got)

	return got
}

// waitFor polls an expression in the page until it is true,
// so a wait is on the page's own state rather than on a sleep long enough to usually work.
func (s *session) waitFor(t *testing.T, what, expr string) {
	t.Helper()
	err := poll(s.ctx, browserDeadline, func(ctx context.Context) (bool, error) {
		var ok bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &ok)); err != nil {
			return false, err
		}

		return ok, nil
	})
	if err != nil {
		t.Fatalf("%s: %v\n%s", what, err, s.report())
	}
}

// assertClean fails the scenario when a load reported a Content Security Policy violation
// or an uncaught exception.
// It is called after every load,
// because a check only at the end would be blind to every load before it.
func (s *session) assertClean(t *testing.T, what string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.violations) > 0 {
		t.Fatalf("%s: the browser reported a Content Security Policy violation: %v", what, s.violations)
	}
	if len(s.exceptions) > 0 {
		t.Fatalf("%s: the browser reported an uncaught exception: %v", what, s.exceptions)
	}
}

// report is what a failure prints: everything the four domains have delivered so far.
func (s *session) report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "log entries: %v\nexceptions: %v\nrequests: %d\n", s.entries, s.exceptions, len(s.requests))
	for _, r := range s.requests {
		fmt.Fprintf(&b, "  %s %s\n", r.method, r.url)
	}

	return b.String()
}

// sentTo returns every request the page sent to a URL, as the browser recorded it.
func (s *session) sentTo(method, url string) []sentRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sentRequest
	for _, r := range s.requests {
		if r.method == method && r.url == url {
			out = append(out, r)
		}
	}

	return out
}

// challengeCount is how many HTTP authentication challenges the browser was asked to answer.
func (s *session) challengeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.challenges
}

// awaitDownload waits for a download to complete and returns the bytes that landed.
// The wait is on the completion event rather than on the click returning,
// because a click starts a download and says nothing about it finishing.
func (s *session) awaitDownload(t *testing.T, what string) []byte {
	t.Helper()
	select {
	case <-s.finished:
	case <-time.After(downloadDeadline):
		t.Fatalf("%s: no download completed within %v\n%s", what, downloadDeadline, s.report())
	case <-s.ctx.Done():
		t.Fatalf("%s: the browser went away: %v", what, s.ctx.Err())
	}
	entries, err := os.ReadDir(s.download)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".crdownload") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.download, e.Name())) //nolint:gosec // a path under the test's temporary directory
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(s.download, e.Name())); err != nil {
			t.Fatal(err)
		}

		return b
	}
	t.Fatalf("%s: the download completed but %s holds no file", what, s.download)

	return nil
}

// assertGzipFramed fails unless b is the gzip framing the profile endpoint streams.
// That those bytes parse as a profile is proven by the profiles scenario and is not repeated here.
func assertGzipFramed(t *testing.T, what string, b []byte) {
	t.Helper()
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatalf("%s: %d bytes that are not gzip framed: % x", what, len(b), b[:min(len(b), 16)])
	}
}
