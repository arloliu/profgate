//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	// id is what the browser calls this request in every later event about it,
	// and is how a load that failed is matched back to the URL it was for.
	id     network.RequestID
	method string
	url    string
	header map[string][]string
	// resourceType is what the browser was fetching the request for:
	// Document for a navigation, Fetch for a call the page made,
	// which is how a download is told apart from a navigation to the same URL.
	resourceType network.ResourceType
}

// downloadBegan is one download as the browser announced it, before any byte landed:
// the URL it downloads from and the name it suggests, which the disk may not use.
type downloadBegan struct {
	guid      string
	url       string
	suggested string
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
// Network carries the requests the page sent and the loads that ended with no answer,
// and Fetch, when credentials are given,
// answers the HTTP authentication challenge.
type session struct {
	ctx      context.Context
	download string // the directory downloads land in

	mu         sync.Mutex
	entries    []string
	violations []string
	exceptions []string
	requests   []sentRequest
	began      []downloadBegan
	failures   map[network.RequestID]string
	challenges int
	finished   chan string

	// intercepting says the Fetch domain is on for the whole session, which answering a challenge needs,
	// so a step that holds a request neither enables it a second time nor disables it afterwards.
	intercepting bool
	// handlers is the ordered list of paused-request handlers the live steps have registered.
	// A paused request goes to the first entry whose match accepts it and that has not taken one already,
	// and every request no entry accepts is continued, which is what every request gets when the list is empty.
	// First match wins, so two entries on disjoint patterns never contend
	// and two on overlapping patterns resolve by registration order.
	handlers []*pausedHandler
}

// pausedHandler is one step's claim on one paused request.
// pattern is the Fetch domain's URL pattern the entry needs paused,
// match selects the request among the ones that pattern reaches,
// and held carries the identifier of the one request the entry takes.
// An entry takes exactly one request: once it has parked one the walk continues past it,
// so a second request on the same route reaches the gateway.
type pausedHandler struct {
	pattern string
	match   func(url string) bool
	held    chan fetch.RequestID
	// taken says the entry has caught its request; it is read and written under the session's mutex.
	taken bool
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

	s := &session{ctx: ctx, download: filepath.Join(dir, "downloads"),
		failures: map[network.RequestID]string{}, finished: make(chan string, 16),
		intercepting: o.User != ""}
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
	if s.intercepting {
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
		s.requests = append(s.requests,
			sentRequest{id: e.RequestID, method: e.Request.Method, url: e.Request.URL, header: header, resourceType: e.Type})
		s.mu.Unlock()
	case *network.EventLoadingFailed:
		// A fetch that never reaches a response reports here and nowhere else:
		// the page sees only that it has no answer,
		// so a failure that shows the requests sent cannot otherwise say which of them died.
		// Canceled separates the two ways a load ends without an answer,
		// because a page leaving for another document cancels every load still in flight,
		// and that is not the same event as a connection the network broke.
		reason := e.ErrorText
		if e.Canceled {
			reason = "canceled: " + reason
		}
		s.mu.Lock()
		s.failures[e.RequestID] = reason
		s.mu.Unlock()
	case *cdpbrowser.EventDownloadWillBegin:
		s.mu.Lock()
		s.began = append(s.began, downloadBegan{guid: e.GUID, url: e.URL, suggested: e.SuggestedFilename})
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
		s.mu.Lock()
		var taken *pausedHandler
		for _, h := range s.handlers {
			if !h.taken && h.match(e.Request.URL) {
				h.taken = true
				taken = h

				break
			}
		}
		s.mu.Unlock()
		if taken != nil {
			taken.held <- e.RequestID

			return
		}
		go func() { _ = fetch.ContinueRequest(e.RequestID).Do(s.executor()) }()
	}
}

// register adds a paused-request handler and pauses the routes every live handler needs.
// The Fetch domain replaces its patterns on each enable rather than adding to them,
// so the whole set is sent every time.
// A session that intercepts for the whole run, which answering an authentication challenge needs,
// pauses every request already and is left exactly as it is.
func (s *session) register(t *testing.T, what string, h *pausedHandler) {
	t.Helper()
	s.mu.Lock()
	s.handlers = append(s.handlers, h)
	patterns := s.patternsLocked()
	s.mu.Unlock()
	s.pause(t, "intercept "+what, patterns)
}

// deregister drops a paused-request handler and pauses what the handlers still live need,
// disabling the domain when nothing is left.
func (s *session) deregister(t *testing.T, what string, h *pausedHandler) {
	t.Helper()
	s.mu.Lock()
	kept := s.handlers[:0]
	for _, live := range s.handlers {
		if live != h {
			kept = append(kept, live)
		}
	}
	s.handlers = kept
	patterns := s.patternsLocked()
	s.mu.Unlock()
	s.pause(t, "stop intercepting "+what, patterns)
}

// patternsLocked is the URL patterns the live handlers need, in registration order.
// The caller holds the session's mutex.
func (s *session) patternsLocked() []string {
	var out []string
	for _, h := range s.handlers {
		out = append(out, h.pattern)
	}

	return out
}

// pause enables the Fetch domain over patterns, or disables it when there are none.
// A session that intercepts for the whole run keeps the interception newSession gave it:
// that enable carries the flag the authentication challenge needs and no pattern at all,
// and narrowing it to one step's route would break the challenge in the middle of a scenario.
func (s *session) pause(t *testing.T, what string, patterns []string) {
	t.Helper()
	if s.intercepting {
		return
	}
	if len(patterns) == 0 {
		s.run(t, what, fetch.Disable())

		return
	}
	ps := make([]*fetch.RequestPattern, 0, len(patterns))
	for _, p := range patterns {
		ps = append(ps, &fetch.RequestPattern{URLPattern: p})
	}
	s.run(t, what, fetch.Enable().WithPatterns(ps))
}

// await returns once the browser has paused a request the handler accepts,
// and fails the step with what it was waiting for when none arrives.
func (s *session) await(t *testing.T, what string, h *pausedHandler) fetch.RequestID {
	t.Helper()
	select {
	case id := <-h.held:
		return id
	case <-time.After(settleDeadline):
		t.Fatalf("%s: no request was paused within %v\n%s", what, settleDeadline, s.report())
	case <-s.ctx.Done():
		t.Fatalf("%s: the browser went away: %v", what, s.ctx.Err())
	}

	return ""
}

// holdRequest runs press and returns once the browser has paused a request to a URL match accepts,
// so a step can read the page while that request stands sent and unanswered.
// The request is paused before it leaves the browser: the gateway sees nothing until the returned release continues it.
// pattern is the Fetch domain's URL pattern the step intercepts, enabled for the step
// and dropped again by release; a session answering a challenge keeps its interception as it is.
// release runs its body at most once, and is also registered as a t.Cleanup right after the paused request arrives,
// so an assertion that fails before the caller's own release call still continues the request.
func (s *session) holdRequest(t *testing.T, what, pattern string, match func(url string) bool, press func()) (release func()) {
	t.Helper()
	h := &pausedHandler{pattern: pattern, match: match, held: make(chan fetch.RequestID, 1)}
	s.register(t, what, h)
	press()
	id := s.await(t, what, h)

	var once sync.Once
	release = func() {
		once.Do(func() {
			s.run(t, "release "+what, fetch.ContinueRequest(id))
			s.deregister(t, what, h)
		})
	}
	t.Cleanup(release)

	return release
}

// writtenResponse is the answer a step writes into a request it paused:
// a status and the envelope Errors reads, whose error is the message and whose code is the hint's key.
// The page reads a body as an envelope only when the response says application/json
// and the body decodes to an object with string error and code fields,
// so an answer without that header would show HTTP <status> <statusText> and nothing from the body.
type writtenResponse struct {
	status  int
	code    string
	message string
}

// answerRequest is holdRequest with an answer of the step's own where that one continues.
// It registers a handler, runs press, returns once the browser has paused a request match accepts,
// and hands back a release that writes the response and drops the handler.
// The release writes the answer rather than the handler writing it the moment the request pauses,
// because a step that wants the answer to land after the page has moved on needs that order,
// and a step that wants it at once calls the release at once.
// release runs its body at most once and is registered as a t.Cleanup of its own,
// so a failing assertion still answers the request the step parked.
func (s *session) answerRequest(t *testing.T, what, pattern string, match func(url string) bool,
	press func(), answer writtenResponse,
) (release func()) {
	t.Helper()
	h := &pausedHandler{pattern: pattern, match: match, held: make(chan fetch.RequestID, 1)}
	s.register(t, what, h)
	press()
	id := s.await(t, what, h)
	body, err := json.Marshal(map[string]string{"error": answer.message, "code": answer.code})
	if err != nil {
		t.Fatalf("%s: encode the answer: %v", what, err)
	}

	var once sync.Once
	release = func() {
		once.Do(func() {
			s.run(t, "answer "+what, fetch.FulfillRequest(id, int64(answer.status)).
				WithResponseHeaders([]*fetch.HeaderEntry{{Name: "Content-Type", Value: "application/json"}}).
				WithBody(base64.StdEncoding.EncodeToString(body)))
			s.deregister(t, what, h)
		})
	}
	t.Cleanup(release)

	return release
}

// heldCount is how many live handlers are holding a paused request nothing has answered or continued.
func (s *session) heldCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, h := range s.handlers {
		if h.taken {
			n++
		}
	}

	return n
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
	fmt.Fprintf(&b, "log entries: %v\nexceptions: %v\nrequests: %d (%d failed)\n",
		s.entries, s.exceptions, len(s.requests), len(s.failures))
	for _, r := range s.requests {
		fmt.Fprintf(&b, "  %s %s", r.method, r.url)
		if reason, failed := s.failures[r.id]; failed {
			fmt.Fprintf(&b, " -- no answer: %s", reason)
		}
		b.WriteString("\n")
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

// requestCount is how many requests the browser has recorded so far.
// A step records it before a press,
// so awaitRequestSince can ask what that press sent rather than whether a URL was ever sent.
func (s *session) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.requests)
}

// awaitRequestSince waits until a request recorded at index since or beyond satisfies match.
// It returns the first that does.
// A request is recorded when the browser is about to send it, and says nothing about its answer being applied;
// a step that reads what the answer did waits for that separately.
// The failure names what was pressed and prints the session report.
func (s *session) awaitRequestSince(t *testing.T, since int, what string, match func(sentRequest) bool) sentRequest {
	t.Helper()
	var found sentRequest
	err := poll(s.ctx, settleDeadline, func(context.Context) (bool, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := since; i < len(s.requests); i++ {
			if match(s.requests[i]) {
				found = s.requests[i]

				return true, nil
			}
		}

		return false, nil
	})
	if err != nil {
		t.Fatalf("%s sent no request that was waited for: %v\n%s", what, err, s.report())
	}

	return found
}

// challengeCount is how many HTTP authentication challenges the browser was asked to answer.
func (s *session) challengeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.challenges
}

// downloadsBegun is how many downloads the browser has announced so far.
func (s *session) downloadsBegun() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.began)
}

// awaitDownload waits for a download to complete and returns how the browser announced it and the bytes that landed.
// The wait is on the completion event rather than on the click returning,
// because a click starts a download and says nothing about it finishing.
// The completion names the download by its identifier, which selects the announcement;
// a completion for a download the browser never announced is a harness fault.
// The bytes are read from the directory under whatever name the browser wrote,
// because the suggested name is documented as one the disk may differ from.
func (s *session) awaitDownload(t *testing.T, what string) (downloadBegan, []byte) {
	t.Helper()
	var guid string
	select {
	case guid = <-s.finished:
	case <-time.After(downloadDeadline):
		t.Fatalf("%s: no download completed within %v\n%s", what, downloadDeadline, s.report())
	case <-s.ctx.Done():
		t.Fatalf("%s: the browser went away: %v", what, s.ctx.Err())
	}
	var began downloadBegan
	found := false
	s.mu.Lock()
	for _, b := range s.began {
		if b.guid == guid {
			began, found = b, true

			break
		}
	}
	s.mu.Unlock()
	if !found {
		t.Fatalf("%s: download %s completed but the browser never announced it\n%s", what, guid, s.report())
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

		return began, b
	}
	t.Fatalf("%s: the download completed but %s holds no file", what, s.download)

	return began, nil
}

// assertGzipFramed fails unless b is the gzip framing the profile endpoint streams.
// That those bytes parse as a profile is proven by the profiles scenario and is not repeated here.
func assertGzipFramed(t *testing.T, what string, b []byte) {
	t.Helper()
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatalf("%s: %d bytes that are not gzip framed: % x", what, len(b), b[:min(len(b), 16)])
	}
}
