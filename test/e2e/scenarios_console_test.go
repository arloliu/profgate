//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// consoleQueryPayload and consolePrincipalPayload are the two hostile values the console scenario carries.
	// A Service name, a Pod name, and a version label cannot hold one: the API server refuses `<` in all three.
	// What can is the page's own query string, which holds whatever the link someone followed contains,
	// and an OpenID Connect claim, which holds whatever the issuer put in it.
	// Each payload sets a global of its own, so a script that ran is attributable to the value it came from,
	// and neither uses a quote, which keeps both readable inside YAML and inside an HTML attribute.
	consoleQueryPayload     = "<img src=x onerror=window.profgateQueryRan=1>"
	consolePrincipalPayload = "<img src=x onerror=window.profgatePrincipalRan=1>"
	// consoleQuerySentinel and consolePrincipalSentinel are the globals those payloads would set.
	// "No script ran" is proven by asking for them after every load:
	// no img element and no logged error together would not prove it.
	consoleQuerySentinel     = "profgateQueryRan"
	consolePrincipalSentinel = "profgatePrincipalRan"

	// consoleUsernameClaim is the claim the console gateway reads as the principal.
	// It is not email, because the login has to succeed with a well formed address
	// while the principal carries the payload.
	consoleUsernameClaim = "name"

	// consoleGrantScopes are the scopes the setup probe asks for, which are the browser block's:
	// which claims an issuer mints depends on what the grant asked for,
	// so a probe under other scopes would prove nothing about the login the browser makes.
	consoleGrantScopes = "openid email profile"

	// consoleBasicUser is the user the browser answers the HTTP authentication challenge with.
	consoleBasicUser = "alice"

	// confirmWindow is a wait past the page's half second,
	// inside which a second press on an armed control is refused rather than sent.
	// Two presses the test makes in two runs can land inside that half second on a fast machine,
	// so every deliberate confirm waits this long first.
	confirmWindow = 600 * time.Millisecond

	// consoleSamplingDuration is what the Service policy raises sampling to before the browser starts a Collection.
	// The gateway's own default is two seconds, which can finish before a browser has pressed Cancel twice,
	// and a cancel on a terminal Collection is refused: that is a green-looking scenario proving the wrong thing.
	// It sits just under the ceiling the lane's configuration puts on a sampling duration.
	consoleSamplingDuration = "55s"

	// consoleCancelInterval and consoleCancelBudget bound the wait for a Collection the browser can cancel.
	// A Collection created while the Service's Pods are still being sampled ends at once with no samples,
	// so the case that needs a cancellable one starts again after this long, for at most this long in all,
	// which is longer than the sampling above the Pods are busy with.
	consoleCancelInterval = 10 * time.Second
	consoleCancelBudget   = 90 * time.Second
)

// scenarioConsoleOIDC drives the console in a browser against a gateway in oidc mode with PGO on.
// It is the first thing that executes app.js: every proof below is the page running,
// not the wire the page is supposed to write.
func scenarioConsoleOIDC(t *testing.T, h *Harness) {
	b := requireBrowser(t, h)
	ns := h.Namespace(t)
	deployTestApp(t, h, ns)
	// The second Service is created before the page loads the Service list it keeps,
	// because the page fetches that list on a namespace change and on a service_not_found and on nothing else,
	// and every fixture between here and that load is time for the informer to deliver it.
	nobodyService(t, h, ns)
	ctx := t.Context()

	password := rand.Text()
	nodeIP, err := h.nodeIP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dexCA := newAuthority(t, nodeIP)
	// The static user keeps a well formed email so the login succeeds,
	// and its name claim, which this gateway reads as the principal, carries the payload.
	dex, err := h.deployDex(ctx, ns, dexCA, nodeIP, callbackURL, bcryptHash(t, password), consolePrincipalPayload)
	if err != nil {
		t.Fatal(err)
	}

	cookieKey := make([]byte, 32)
	if _, err := rand.Read(cookieKey); err != nil {
		t.Fatal(err)
	}
	if err := h.applyAuthSecret(ctx, ns, map[string][]byte{
		issuerCAKey:  dexCA.caPEM,
		cookieKeyKey: []byte(base64.StdEncoding.EncodeToString(cookieKey) + "\n"),
	}); err != nil {
		t.Fatal(err)
	}

	// The Collections proofs need PGO, which needs a credential the HTTPS overlay writes no mount for,
	// so the overlay gains the one deploy/base carries.
	pub, sub, err := gatewayPermissions(h.root)
	if err != nil {
		t.Fatal(err)
	}
	user, err := h.NATS.ID.user("profgate", pub, sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.applyCredsSecret(ctx, ns, user.Creds); err != nil {
		t.Fatal(err)
	}

	gwCA := newAuthority(t, tlsHost)
	cfg := gatewayConfig(gatewayConfigOptions{
		NATSURL:   natsURL(gatewayNamespace),
		RealmPGO:  true,
		TLSMount:  tlsMountPath,
		AuthBlock: consoleAuthBlock(dex.issuer, consolePrincipalPayload),
		UIEnabled: true,
	})
	local, gateway := deployHTTPSGateway(t, h, ns, "oidc-gateway", oidcGatewayName, gwCA, cfg,
		credsMountPatch(oidcGatewayName))
	reportAuthFailures(t, h, ns, gateway)

	pool := x509.NewCertPool()
	pool.AddCert(gwCA.ca)
	pool.AddCert(dexCA.ca)
	client := authClient(local, dex.addr, pool)

	// Setup fails if the issuer cannot be configured with that principal,
	// rather than the scenario running and proving half of what it says it proves.
	idToken := passwordGrantScoped(t, client, dex.issuer, password, consoleGrantScopes)
	assertClaim(t, idToken, consoleUsernameClaim, consolePrincipalPayload)
	bearer := http.Header{"Authorization": {"Bearer " + idToken}}

	// A gateway with PGO on answers every PGO route 503 until its watches have replayed,
	// which is later than the readiness the rollout waited for.
	awaitPGORoutes(t, client, bearer, gatewayOrigin+pgoPath(ns, testAppName))

	// Two Collections, deliberately different.
	// This one has finished, which is what the list and the detail are read against.
	seeded := seedCompletedCollection(t, client, bearer, ns)
	// And the one the browser starts must not,
	// so the Service's sampling is raised well past the time a browser needs to press Cancel twice,
	// through the policy route the realm's configure flag admits.
	raiseSampling(t, client, bearer, ns)

	s := newSession(t, b, sessionOptions{MapTo: local})

	// The first load carries the hostile query and no session at all.
	// The whole login round trip is where a return path built by joining strings would show.
	hostile := url.Values{"ns": {consoleQueryPayload}, "svc": {consoleQueryPayload}}
	s.run(t, "open the console with no session",
		chromedp.Navigate(gatewayOrigin+uiPath+"?"+hostile.Encode()))
	signInThroughDex(t, s, dexUser, password)
	s.waitFor(t, "the console renders after the login", `document.querySelector(".panels") !== null`)

	// The page navigated to the login of its own accord, with the selection in its return path,
	// and the landing page brought the browser back to that selection.
	// A return path built by joining strings instead of by URLSearchParams is what would show here.
	assertNavigatedToLogin(t, s, consoleQueryPayload)
	assertSelection(t, s.location(t), consoleQueryPayload)

	// The identity disclosure's summary names the issuer's user and the realm it mapped to,
	// and the hostile query and the hostile principal are both rendered as text.
	// The summary is read on its own rather than through the disclosure:
	// a closed details carries its body in textContent,
	// so a read of the whole element passes whether or not the summary names either value.
	summary := s.textOf(t, "details.identity > summary")
	for _, want := range []string{consolePrincipalPayload, "developer"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("the identity disclosure's summary does not name %q:\n%s", want, summary)
		}
	}
	// The authentication mode is one of the seven facts the body holds and is not in the summary.
	if identity := s.textOf(t, "details.identity"); !strings.Contains(identity, "oidc") {
		t.Fatalf("the identity disclosure does not name the authentication mode oidc:\n%s", identity)
	}
	// The unlisted text is checked only once the namespace list has arrived.
	// nsListed is false while that list is still empty exactly as it is when the selection is absent from a list that arrived,
	// so the text renders in both states,
	// and a check made before the list lands cannot tell which one it is looking at.
	// Options beyond the placeholder are what say the list landed.
	s.waitFor(t, "the namespace list arrives",
		`(() => {
  const l = [...document.querySelectorAll("label")].find((l) => l.querySelector("select") && l.textContent.trim().startsWith("Namespace"));
  return Boolean(l) && l.querySelector("select").options.length > 1;
})()`)
	if listed := s.textOf(t, ".panels"); !strings.Contains(listed, consoleQueryPayload+" is not listed") {
		t.Fatalf("the page does not show the unlisted selection as text against a namespace list that arrived:\n%s",
			listed)
	}
	assertRenderedAsText(t, s, "the load with the hostile query", consolePrincipalPayload, consoleQueryPayload)
	s.assertClean(t, "the login round trip")

	// The second load is the working one: a listed namespace and a listed Service.
	s.run(t, "open the console on the test app",
		chromedp.Navigate(gatewayOrigin+uiPath+"?"+url.Values{"ns": {ns}, "svc": {testAppName}}.Encode()))
	s.waitFor(t, "the Service list answers", `document.querySelector(".panels") !== null`)
	s.waitFor(t, "the profile URL is built", `(document.querySelector("input.url") || {}).value !== ""`)

	// The disclosure is closed on load: what a realm admits is read when something asks for it.
	// The property is read and not the attribute, because open is the element's own state
	// and the template never writes the attribute.
	var openOnLoad bool
	s.eval(t, "read the identity disclosure on load",
		`(document.querySelector("details.identity") || {}).open === true`, &openOnLoad)
	if openOnLoad {
		t.Fatalf("the identity disclosure is open on load, want closed until something opens it:\n%s",
			s.textOf(t, "details.identity > summary"))
	}

	// Choosing a profile fills the field with the URL Flow describes.
	s.chooseOption(t, "Profile", "heap")
	wantURL := gatewayOrigin + "/v1/namespaces/" + ns + "/services/" + testAppName + "/profiles/heap"
	s.waitFor(t, "the profile URL follows the profile chosen",
		fmt.Sprintf(`(document.querySelector("input.url") || {}).value === %q`, wantURL))

	// Download fetches the profile and saves the body the profile endpoint streams.
	// The request is a Fetch and not a Document, and the file comes from an object URL,
	// which is what tells a fetch followed by a save apart from a navigation to the same URL.
	s.run(t, "press Download", chromedp.Click(control("Download"), chromedp.BySearch))
	began, body := s.awaitDownload(t, "the profile download")
	assertGzipFramed(t, "the file the browser saved", body)
	if began.suggested != "heap" {
		t.Fatalf("the browser suggested the name %q for the download, want heap, the profile chosen,"+
			" which the pprof handler names in Content-Disposition", began.suggested)
	}
	if !strings.HasPrefix(began.url, "blob:") {
		t.Fatalf("the download's URL is %q, want a blob: URL; the page navigated instead of fetching", began.url)
	}
	if sent := s.sentTo(http.MethodGet, wantURL); len(sent) == 0 || sent[len(sent)-1].resourceType != network.ResourceTypeFetch {
		t.Fatalf("the page sent %d requests to %s and the last was a %q, want a %q; the download was a navigation\n%s",
			len(sent), wantURL, lastResourceType(sent), network.ResourceTypeFetch, s.report())
	}
	s.assertClean(t, "the profile download")

	// The Collections table lists the Service's Collections and a row's detail shows its record.
	s.awaitRow(t, seeded, "the seeded Collection")
	s.run(t, "open the seeded Collection", chromedp.Click(control(seeded), chromedp.BySearch))
	// The Collection detail is selected inside the Collections panel:
	// the identity disclosure is the document's first details, so a bare selector would read it instead.
	s.waitFor(t, "the detail shows the seeded record",
		fmt.Sprintf(`(document.querySelector(".collections details summary") || {}).textContent === "Collection %s"`,
			seeded))
	if detail := s.textOf(t, ".collections details"); !strings.Contains(detail, "completed") {
		t.Fatalf("the detail of %s does not show its state:\n%s", seeded, detail)
	}

	// Start collection, pressed twice through its inline confirmation:
	// first as a double-click, which arms the control and sends nothing,
	// then once more past the window, which sends the one POST.
	route := gatewayOrigin + "/v1/namespaces/" + ns + "/services/" + testAppName + "/collections"
	pressTwiceInsideTheWindow(t, s, route)
	s.run(t, "confirm the start", chromedp.Click(control("Confirm start"), chromedp.BySearch))
	s.awaitRequest(t, http.MethodPost, route)
	// The page selects the Collection the start created, which is what names it in the detail,
	// so the detail is where the answer being applied is read,
	// and it is read before the test fetches anything of its own.
	started := s.awaitStartedDetail(t, seeded)
	assertStartRequest(t, s, route)
	t.Logf("the browser started collection %s", started)
	s.awaitRow(t, started, "the Collection the browser started")

	// Cancel on that row, pressed twice the same way, with the wait past the window between the two.
	s.run(t, "arm the cancel control", chromedp.Click(control("Cancel"), chromedp.BySearch))
	// Refresh on the Collections table, pressed while that row's Cancel is armed,
	// sends one GET of the list and nothing else, and leaves the armed row standing.
	// The GET is held at the browser while the control is read disabled and pressed again to no effect,
	// so the in-flight state is observed rather than inferred from a control that was enabled before and after.
	// The control is enabled again once the answer is applied, which is when what the answer left is read;
	// the armed cancel disarms itself after ten seconds, and one GET of the list settles well inside that.
	s.waitFor(t, "the Collections Refresh control is idle", refreshEnabled("Collections"))
	n := s.requestCount()
	release := s.holdRequest(t, "the Refresh of the Collections table", route,
		func(u string) bool { return u == route },
		func() {
			s.run(t, "press Refresh on the Collections table", chromedp.Click(refreshButton("Collections"), chromedp.BySearch))
		})
	s.awaitRequestSince(t, n, "the Refresh of the Collections table", func(r sentRequest) bool {
		return r.method == http.MethodGet && r.url == route
	})
	pressRefreshWhileHeld(t, s, "Collections")
	release()
	s.waitFor(t, "the Collections Refresh control is idle again", refreshEnabled("Collections"))
	if got := s.requestCount(); got != n+1 || s.heldCount() != 0 {
		t.Fatalf("Refresh on the Collections table sent %d requests, want exactly the one GET of the list\n%s",
			got-n, s.report())
	}
	var armedStanding bool
	s.eval(t, "read the cancel control", fmt.Sprintf(`((r) => Boolean(r) && `+
		`[...r.cells[8].querySelectorAll("button")].map((b) => b.textContent.trim()).join(",") === "Confirm cancel,Keep")(`+
		`[...document.querySelectorAll(".table table tbody tr")].find((r) => r.cells[0].textContent.trim() === %q))`, started),
		&armedStanding)
	if !armedStanding {
		t.Fatalf("Refresh on the Collections table did not leave Confirm cancel and Keep standing on %s:\n%s",
			started, s.textOf(t, ".panels"))
	}
	s.run(t, "wait past the window", chromedp.Sleep(confirmWindow))
	s.run(t, "confirm the cancel", chromedp.Click(control("Confirm cancel"), chromedp.BySearch))
	s.waitFor(t, "the row moves to cancelled",
		fmt.Sprintf(`%s === "cancelled"`, rowCell(started, 2)))
	s.waitFor(t, "the cancel control goes with the state",
		fmt.Sprintf(`%s === ""`, rowCell(started, 8)))

	// A Service whose selector matches no Pod disables Download, with the selector sentence beside it.
	const noSelector = "the Service's selector matches no Pod"
	s.chooseOption(t, "Service", nobodyServiceName)
	s.waitFor(t, "the Profile panel says the selector matches no Pod",
		fmt.Sprintf(`(%s || { textContent: "" }).textContent.includes(%q)`, profilePanel, noSelector))
	var disabledWithNote bool
	s.eval(t, "read the Download control", fmt.Sprintf(`(() => {
  const b = [...document.querySelectorAll(".actions button")].find((b) => b.textContent.trim() === "Download");
  return Boolean(b) && b.disabled && [...b.parentElement.querySelectorAll("small")].some((n) => n.textContent.includes(%q));
})()`, noSelector), &disabledWithNote)
	if !disabledWithNote {
		t.Fatalf("Download is not a disabled button with %q beside it while the Service matches no Pod:\n%s",
			noSelector, s.textOf(t, ".panels"))
	}

	// Back on the test app, the page's Pod menu lists a Pod, which says the targets fetch has landed;
	// the URL field cannot say that, because the URL is built from the selection alone.
	s.chooseOption(t, "Service", testAppName)
	s.waitFor(t, "the Pod control lists a Pod", podListed)

	// The identity disclosure's opening rule, on the four cases an answer the page classifies decides.
	// Each answer is written into a request the test paused, so the realm the gateway serves never changes.
	// They run here because every press below needs a Service the page can build a live request for,
	// and the scale-down that follows leaves the Service with no eligible Pod.
	targetsRoute := gatewayOrigin + "/v1/namespaces/" + ns + "/services/" + testAppName + "/targets"
	isTargetsGET := func(u string) bool { return strings.HasPrefix(u, targetsRoute) && strings.Contains(u, "explain=true") }
	profileRoute := gatewayOrigin + "/v1/namespaces/" + ns + "/services/" + testAppName + "/profiles/heap"
	servicesRoute := gatewayOrigin + "/v1/namespaces/" + ns + "/services"
	whoamiRoute := gatewayOrigin + "/v1/whoami"
	denied := writtenResponse{status: http.StatusForbidden, code: "realm_denied",
		message: "the realm does not admit this request"}
	isWhoamiGET := func(r sentRequest) bool { return r.method == http.MethodGet && r.url == whoamiRoute }
	// closeIdentity puts the disclosure back to closed, which is where a person's click leaves it,
	// so that the next denial's opening is a change and not a state the case found.
	closeIdentity := func(what string) {
		s.run(t, "close the identity disclosure before "+what,
			chromedp.Click("details.identity > summary", chromedp.ByQuery))
		s.waitFor(t, "the identity disclosure is closed before "+what,
			`(document.querySelector("details.identity") || {}).open === false`)
	}
	// denyTargets presses Refresh on the targets list and answers that one request 403 realm_denied.
	// It returns once the page has sent the /v1/whoami refetch that answer asks for,
	// which is the page having classified the answer: the opening runs before the refetch is sent.
	denyTargets := func(what string) {
		s.waitFor(t, "the targets Refresh control is idle before "+what, refreshEnabled("Profile"))
		at := s.requestCount()
		answer := s.answerRequest(t, what, targetsRoute+"*", isTargetsGET, func() {
			s.run(t, "press Refresh on the targets list", chromedp.Click(refreshButton("Profile"), chromedp.BySearch))
		}, denied)
		answer()
		s.awaitRequestSince(t, at, "the identity refetch "+what+" asks for", isWhoamiGET)
	}

	// A qualifying denial opens the disclosure, on each of the three paths that classify one:
	// a listing, a profile download, and the current start attempt.
	assertIdentityOpen(t, s, "the load, before any denial", false)
	denyTargets("the targets listing refused")
	assertIdentityOpen(t, s, "a targets listing answered 403 realm_denied", true)
	// The panel that holds the listing shows the refusal as the code, the message, and the hint,
	// which is the sentence that sends a reader to the identity.
	profile := s.textOf(t, ".request")
	for _, want := range []string{"realm_denied", denied.message,
		"your realm does not admit this; the identity shows what it does"} {
		if !strings.Contains(profile, want) {
			t.Fatalf("the Profile panel does not show %q on a refused targets listing:\n%s", want, profile)
		}
	}

	closeIdentity("the refused download")
	at := s.requestCount()
	answerDownload := s.answerRequest(t, "the profile download refused", profileRoute+"*",
		func(u string) bool { return strings.HasPrefix(u, profileRoute) },
		func() { s.run(t, "press Download", chromedp.Click(control("Download"), chromedp.BySearch)) }, denied)
	answerDownload()
	s.awaitRequestSince(t, at, "the identity refetch the refused download asks for", isWhoamiGET)
	assertIdentityOpen(t, s, "a profile download answered 403 realm_denied", true)

	closeIdentity("the refused start")
	s.run(t, "arm the start control", chromedp.Click(control("Start collection"), chromedp.BySearch))
	s.run(t, "wait past the window", chromedp.Sleep(confirmWindow))
	at = s.requestCount()
	answerStart := s.answerRequest(t, "the start refused", route, func(u string) bool { return u == route },
		func() { s.run(t, "confirm the start", chromedp.Click(control("Confirm start"), chromedp.BySearch)) }, denied)
	answerStart()
	s.awaitRequestSince(t, at, "the identity refetch the refused start asks for", isWhoamiGET)
	assertIdentityOpen(t, s, "a start answered 403 realm_denied", true)

	// A second identical denial opens it again: the opening is once per qualifying answer
	// and not once per page.
	closeIdentity("the second refused targets listing")
	denyTargets("the targets listing refused a second time")
	assertIdentityOpen(t, s, "a second identical denial", true)

	// A person's closing stands: a late answer to the identity refetch and an unrelated render leave it closed.
	// The targets denial is written only once the handler that catches the refetch is live,
	// because a refetch that reached the gateway would answer before anything could catch it.
	closeIdentity("the denial whose refetch is held")
	s.waitFor(t, "the targets Refresh control is idle", refreshEnabled("Profile"))
	releaseTargets := s.answerRequest(t, "the targets listing refused with its refetch held",
		targetsRoute+"*", isTargetsGET, func() {
			s.run(t, "press Refresh on the targets list", chromedp.Click(refreshButton("Profile"), chromedp.BySearch))
		}, denied)
	releaseWhoami := s.answerRequest(t, "the identity refetch that failed", whoamiRoute,
		func(u string) bool { return u == whoamiRoute }, releaseTargets,
		writtenResponse{status: http.StatusServiceUnavailable, code: "discovery_unavailable",
			message: "the gateway could not read its cache"})
	assertIdentityOpen(t, s, "the denial whose refetch is held", true)
	closeIdentity("the answer to the held refetch")
	releaseWhoami()
	s.waitFor(t, "the failed identity refetch offers its recovery", identityRecovery)
	assertIdentityOpen(t, s, "the identity refetch answering an error", false)
	s.waitFor(t, "the Collections Refresh control is idle", refreshEnabled("Collections"))
	s.run(t, "press Refresh on the Collections table",
		chromedp.Click(refreshButton("Collections"), chromedp.BySearch))
	s.waitFor(t, "the Collections Refresh control is idle again", refreshEnabled("Collections"))
	assertIdentityOpen(t, s, "a render the disclosure took no part in", false)
	// The recovery is read where a closed disclosure cannot hide it:
	// outside the disclosure and outside the panels, which is where the page renders it.
	// Pressing it is what clears the error the case wrote, so the page is left as the case found it.
	var retried bool
	s.eval(t, "press Retry on the failed identity refetch", pressIdentityRetry, &retried)
	if !retried {
		t.Fatalf("the failed identity refetch offers no Retry control outside the disclosure:\n%s", s.report())
	}
	s.waitFor(t, "the identity refetch succeeds and takes its error with it",
		"!("+identityRecovery+")")

	// A stale Service listing's denial is discarded before it is recorded, so it opens nothing.
	// The namespace is chosen, left for the placeholder, and chosen again,
	// so the page is back on the namespace the parked request asked for
	// and no comparison of the namespace can tell that answer from a current one.
	answerServices := s.answerRequest(t, "the Service listing for a namespace the page leaves",
		servicesRoute, func(u string) bool { return u == servicesRoute },
		func() { s.chooseOption(t, "Namespace", ns) }, denied)
	s.chooseOption(t, "Namespace", "")
	s.chooseOption(t, "Namespace", ns)
	// The second Service listing reaches the gateway and answers, which the Service menu offering the Service says.
	// It is awaited before the parked answer is written,
	// because a success landing after the denial would clear the error under the key
	// and leave the case green against a page that recorded it.
	s.waitFor(t, "the Service list answers for the namespace chosen again", serviceOffered(testAppName))
	answerServices()
	s.chooseOption(t, "Service", testAppName)
	s.waitFor(t, "the Pod control lists a Pod again", podListed)
	assertIdentityOpen(t, s, "a Service listing answered for a namespace the page had left", false)
	if selection := s.textOf(t, ".selection"); strings.Contains(selection, "realm_denied") {
		t.Fatalf("the Service panel shows a denial the page asked for before it left the namespace:\n%s", selection)
	}

	// A cancel's 404 collection_not_found refetches the identity and opens nothing:
	// it names no realm that refused.
	// The Collection is the case's own, because the one the scenario started earlier is terminal
	// and a terminal record offers no Cancel.
	// A Collection created while the Service's Pods are still being sampled ends at once,
	// reason no_samples, and a record that has ended offers no cancel control,
	// so the case starts one and starts another until a row carries that control.
	// The Pods stay busy for the sampling the scenario's policy override raised the Service to,
	// counted from the Collection the scenario started and cancelled above.
	var cancelled, ended string
	previous := started
	for waited := time.Duration(0); ; waited += consoleCancelInterval {
		s.waitFor(t, "the Collections Refresh control is idle", refreshEnabled("Collections"))
		s.run(t, "arm the start control", chromedp.Click(control("Start collection"), chromedp.BySearch))
		s.run(t, "wait past the window", chromedp.Sleep(confirmWindow))
		at = s.requestCount()
		s.run(t, "confirm the start", chromedp.Click(control("Confirm start"), chromedp.BySearch))
		s.awaitRequestSince(t, at, "the start of the Collection the cancel case ends", func(r sentRequest) bool {
			return r.method == http.MethodPost && r.url == route
		})
		cancelled = s.awaitStartedDetail(t, previous)
		previous = cancelled
		s.awaitRow(t, cancelled, "the Collection the cancel case started")
		// The list does not poll, so it is asked for again until the row carries its control or the record ends.
		var offered bool
		_ = poll(s.ctx, settleDeadline, func(context.Context) (bool, error) {
			s.eval(t, "read the cancel control of the Collection the cancel case started",
				fmt.Sprintf(`%s === "Cancel"`, rowCell(cancelled, 8)), &offered)
			s.eval(t, "read the state of that Collection",
				fmt.Sprintf(`String(%s)`, rowCell(cancelled, 2)), &ended)
			if offered || terminal(ended) {
				return true, nil
			}
			s.refetchCollections(t)

			return false, nil
		})
		if offered {
			break
		}
		if waited >= consoleCancelBudget {
			t.Fatalf("no Collection the browser started offered Cancel within %v; the last one ended %s\n%s",
				consoleCancelBudget, ended, s.report())
		}
		s.run(t, "wait for the sampling in flight to end", chromedp.Sleep(consoleCancelInterval))
	}
	cancelRoute := gatewayOrigin + "/v1/collections/" + cancelled + "/cancel"
	// Only the cancel route is paused: the Collections refetch that outcome causes answers normally,
	// and a 403 on that refetch would open the disclosure correctly and make the case say the opposite.
	s.run(t, "arm the cancel control", chromedp.Click(control("Cancel"), chromedp.BySearch))
	s.run(t, "wait past the window", chromedp.Sleep(confirmWindow))
	at = s.requestCount()
	answerCancel := s.answerRequest(t, "the cancel of a record the store no longer holds", cancelRoute,
		func(u string) bool { return u == cancelRoute },
		func() { s.run(t, "confirm the cancel", chromedp.Click(control("Confirm cancel"), chromedp.BySearch)) },
		writtenResponse{status: http.StatusNotFound, code: "collection_not_found", message: "no such Collection"})
	answerCancel()
	s.awaitRequestSince(t, at, "the identity refetch a cancel's 404 asks for", isWhoamiGET)
	assertIdentityOpen(t, s, "a cancel answered 404 collection_not_found", false)

	// The scenario leaves no Collection running.
	// The record the 404 refetched is either still cancellable or already ended,
	// and both leave nothing running, which is all the scale-down below needs.
	// The loop above hands over a Collection at the end of the sampling that kept it cancellable,
	// because a Collection created while the Pods are still busy ends at once,
	// so the one it hands over has the shortest cancellable life any of them has
	// and can reach a terminal state between that hand-over and this press.
	// Waiting for the control to come back is waiting for something that never returns:
	// a terminal record offers no cancel control, and the list is not asked for again on its own.
	// That a cancel works is the earlier cancel's to prove, not this one's.
	s.waitFor(t, "the Collections Refresh control is idle", refreshEnabled("Collections"))
	var offeredAgain bool
	s.eval(t, "read the cancel control of the Collection the cancel case started",
		fmt.Sprintf(`%s === "Cancel"`, rowCell(cancelled, 8)), &offeredAgain)
	if offeredAgain {
		s.run(t, "arm the cancel control", chromedp.Click(control("Cancel"), chromedp.BySearch))
		s.run(t, "wait past the window", chromedp.Sleep(confirmWindow))
		s.run(t, "confirm the cancel", chromedp.Click(control("Confirm cancel"), chromedp.BySearch))
		// A record that ended between the read above and the press answers 409 collection_terminal,
		// which the page takes as a Collections refetch and no error,
		// so the wait is for the end the press asked for or the end that beat it.
		s.waitFor(t, "the Collection the cancel case started ends",
			fmt.Sprintf(`["cancelled","completed","failed","expired"].indexOf(String(%s)) >= 0`, rowCell(cancelled, 2)))
	} else {
		s.eval(t, "read the state of the Collection the cancel case started",
			fmt.Sprintf(`String(%s)`, rowCell(cancelled, 2)), &ended)
		if !terminal(ended) {
			t.Fatalf("the Collection the cancel case started offers no Cancel and reads %q, which is neither cancellable nor ended\n%s",
				ended, s.report())
		}
	}

	// The app is scaled to zero as the scenario's last step against it, because it does not come back.
	// The test awaits the empty targets answer through a request of its own under the same credential,
	// and the page's own list is left as it is:
	// a Pod the page still lists is confirmed against the API server on the download and refused no_targets.
	body = fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, 0)
	if _, err := h.Client.AppsV1().Deployments(ns).Patch(ctx, testAppName, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		t.Fatalf("scale %s to 0: %v", testAppName, err)
	}
	awaitTargetsEmpty(t, client, bearer, targetsRoute)
	var podStillListed bool
	s.eval(t, "read the Pod control", podListed, &podStillListed)
	if !podStillListed {
		t.Fatalf("the page's Pod control emptied without a fetch of its own:\n%s", s.report())
	}
	before := s.downloadsBegun()
	// The panel is read for the hint and not for the code alone:
	// a bare no_targets is a word the person who pressed Download cannot act on.
	const noTargetsHint = "Refresh on the targets list updates the available Pods and the empty state"
	s.run(t, "press Download with no eligible Pod", chromedp.Click(control("Download"), chromedp.BySearch))
	s.waitFor(t, "the Profile panel explains no_targets",
		fmt.Sprintf(`(%s || { textContent: "" }).textContent.includes(%q)`, profilePanel, noTargetsHint))
	if n := s.downloadsBegun(); n != before {
		t.Fatalf("a download began on an answer that was not a profile: %d downloads, %d before", n, before)
	}

	// Refresh on the targets list sends one targets GET carrying explain=true and nothing else,
	// and the answer, listing no Pod, disables Download with the empty state's wording beside it.
	// The deleted Pods stay Terminating for the app's preStop sleep,
	// so the answer excludes them as pod_terminating first and reports a selector matching no Pod once they are gone;
	// either wording is the empty state.
	// The GET is held at the browser while the control is read disabled and pressed again to no effect, as above.
	// The disclosure is opened by hand first, so that the refresh below is a render it has to survive:
	// the page sets open once per qualifying answer rather than binding it in the template,
	// and a bound open would close again on the next render.
	s.run(t, "open the identity disclosure",
		chromedp.Click("details.identity > summary", chromedp.ByQuery))
	s.waitFor(t, "the identity disclosure is open",
		`(document.querySelector("details.identity") || {}).open === true`)
	s.waitFor(t, "the targets Refresh control is idle", refreshEnabled("Profile"))
	n = s.requestCount()
	release = s.holdRequest(t, "the Refresh of the targets list", targetsRoute+"*", isTargetsGET, func() {
		s.run(t, "press Refresh on the targets list", chromedp.Click(refreshButton("Profile"), chromedp.BySearch))
	})
	s.awaitRequestSince(t, n, "the Refresh of the targets list", func(r sentRequest) bool {
		return r.method == http.MethodGet && isTargetsGET(r.url)
	})
	pressRefreshWhileHeld(t, s, "Profile")
	release()
	s.waitFor(t, "the targets Refresh control is idle again", refreshEnabled("Profile"))
	if got := s.requestCount(); got != n+1 || s.heldCount() != 0 {
		t.Fatalf("Refresh on the targets list sent %d requests, want exactly the one targets GET\n%s", got-n, s.report())
	}
	// The refresh answered and the panel re-rendered, and the disclosure a person opened is still open.
	var openAfterRender bool
	s.eval(t, "read the identity disclosure after the refresh",
		`(document.querySelector("details.identity") || {}).open === true`, &openAfterRender)
	if !openAfterRender {
		t.Fatalf("the identity disclosure closed on a render, want a person's opening to stand:\n%s", s.report())
	}
	var emptyNote string
	s.eval(t, "read the Download control after the refresh", `(() => {
  const b = [...document.querySelectorAll(".actions button")].find((b) => b.textContent.trim() === "Download");
  if (!b || !b.disabled) { return ""; }
  return [...b.parentElement.querySelectorAll("small")].map((n) => n.textContent.trim()).find((t) => t !== "") || "";
})()`, &emptyNote)
	if !strings.Contains(emptyNote, "Pods being deleted") && !strings.Contains(emptyNote, noSelector) {
		t.Fatalf("after the refresh Download is not disabled with the empty state's wording beside it; the line reads %q:\n%s",
			emptyNote, s.textOf(t, ".panels"))
	}

	// A selection the browser flow would refuse is never sent as the return path.
	// A second session with no cookie opens the console on an ns of 1100 characters,
	// the page answers its 401 by navigating to /auth/login of its own accord,
	// and the return it carries is the marker alone, because the encoded selection would cross the 1024-byte bound.
	// The login is not completed; the request the page sent is the whole proof.
	assertLongSelectionDropped(t, newSession(t, b, sessionOptions{MapTo: local}))

	// Every load of the scenario, once more at the end: the observers ran through all of them.
	assertRenderedAsText(t, s, "the working load", consolePrincipalPayload, "")
	s.assertClean(t, "the console scenario")
}

// scenarioConsoleBasic drives the console in a browser against a gateway in basic mode.
// It proves the HTTP authentication challenge being answered and the page continuing,
// which the wire proof cannot reach.
// Whether Chromium drew a native dialog is not observed and is not claimed.
// Nothing in it reaches a Collection, so it needs neither NATS nor PGO.
func scenarioConsoleBasic(t *testing.T, h *Harness) {
	b := requireBrowser(t, h)
	ns := h.Namespace(t)
	deployTestApp(t, h, ns)
	ctx := t.Context()

	password := rand.Text()
	hash := bcryptHash(t, password)
	if err := h.applyAuthSecret(ctx, ns, map[string][]byte{usersFileKey: usersFile(hash, "bob")}); err != nil {
		t.Fatal(err)
	}
	ca := newAuthority(t, tlsHost)
	cfg := gatewayConfig(gatewayConfigOptions{TLSMount: tlsMountPath, AuthBlock: basicAuthBlock(hash), UIEnabled: true})
	local, gateway := deployHTTPSGateway(t, h, ns, "basic-gateway", basicGatewayName, ca, cfg)
	reportAuthFailures(t, h, ns, gateway)

	// The gateway is answering before the browser is pointed at it:
	// a port-forward is open before a connection through it is usable,
	// and a page that loads into a refused connection proves nothing about a challenge.
	client := tlsClient(local, ca.pool)
	awaitGateway(t, client, gatewayOrigin+uiPath)

	s := newSession(t, b, sessionOptions{MapTo: local, User: consoleBasicUser, Password: password})
	s.run(t, "open the console",
		chromedp.Navigate(gatewayOrigin+uiPath+"?"+url.Values{"ns": {ns}, "svc": {testAppName}}.Encode()))
	s.waitFor(t, "the page continues past the challenge", `document.querySelector(".panels") !== null`)

	// The identity disclosure is the page continuing:
	// the first fetch was answered 401 with the basic challenge, the browser raised it,
	// and the test answered it over the protocol's own handling.
	// The summary is read on its own for the reason the oidc scenario states.
	summary := s.textOf(t, "details.identity > summary")
	for _, want := range []string{consoleBasicUser, "developer"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("the identity disclosure's summary does not name %q:\n%s", want, summary)
		}
	}
	// The authentication mode is one of the seven facts the body holds and is not in the summary.
	if identity := s.textOf(t, "details.identity"); !strings.Contains(identity, "basic") {
		t.Fatalf("the identity disclosure does not name the authentication mode basic:\n%s", identity)
	}
	if n := s.challengeCount(); n != 1 {
		t.Fatalf("the browser was asked to answer %d authentication challenges, want exactly 1;"+
			" the page continued with a second prompt", n)
	}

	// And on to a completed profile download, which is a navigation carrying the same credential.
	s.waitFor(t, "the profile URL is built", `(document.querySelector("input.url") || {}).value !== ""`)
	s.chooseOption(t, "Profile", "heap")
	wantURL := gatewayOrigin + "/v1/namespaces/" + ns + "/services/" + testAppName + "/profiles/heap"
	s.waitFor(t, "the profile URL follows the profile chosen",
		fmt.Sprintf(`(document.querySelector("input.url") || {}).value === %q`, wantURL))
	s.run(t, "press Download", chromedp.Click(control("Download"), chromedp.BySearch))
	began, body := s.awaitDownload(t, "the profile download")
	assertGzipFramed(t, "the file the browser saved", body)
	if began.suggested != "heap" {
		t.Fatalf("the browser suggested the name %q for the download, want heap, the profile chosen", began.suggested)
	}
	if n := s.challengeCount(); n != 1 {
		t.Fatalf("the download raised another challenge: %d in all, want 1", n)
	}
	s.assertClean(t, "the console scenario")
}

// consoleAuthBlock is the console gateway's auth block.
// It differs from the one the authentication scenarios run in exactly two ways,
// both of which the hostile principal needs:
// the principal comes from the name claim rather than from the email,
// so the login keeps an address Dex can log in with,
// and the browser asks for the profile scope, which is what carries that claim.
func consoleAuthBlock(issuer, principal string) string {
	return fmt.Sprintf(`auth:
  mode: oidc
  oidc:
    issuer: %s
    audience: %s
    usernameClaim: %s
    caFile: %s/%s
    mapping:
      users:
        - name: %q
          realm: developer
    browser:
      clientID: %s
      redirectURL: %s
      scopes: [openid, email, profile]
      cookieKeyFile: %s/%s
`, issuer, dexClientID, consoleUsernameClaim, authMountPath, issuerCAKey, principal,
		dexClientID, callbackURL, authMountPath, cookieKeyKey)
}

// nobodyServiceName is the Service whose selector matches no Pod.
const nobodyServiceName = "nobody"

// nobodyService creates a Service in ns whose selector names a label no Pod carries.
// The catalog lists a Service with a selector whatever it matches,
// and the eligibility read answers a selectorMatched of 0 for it,
// which is the one targets answer that disables Download with the selector sentence beside it.
// It is created through the clientset the way the harness creates its ConfigMaps and Secrets,
// because a manifest would be a second fixture for one selector.
func nobodyService(t *testing.T, h *Harness, ns string) {
	t.Helper()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: nobodyServiceName},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{testAppLabel: nobodyServiceName},
			Ports:    []corev1.ServicePort{{Name: "pprof", Port: 6060, Protocol: corev1.ProtocolTCP}},
		},
	}
	if _, err := h.Client.CoreV1().Services(ns).Create(t.Context(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the Service %s/%s: %v", ns, nobodyServiceName, err)
	}
}

// awaitTargetsEmpty polls the targets route until the gateway answers 200 listing no target.
// The request is the test's own, under the scenario's credential and against the gateway it deployed,
// so the page's list is left as it is.
func awaitTargetsEmpty(t *testing.T, c *http.Client, header http.Header, rawURL string) {
	t.Helper()
	var last response
	err := poll(t.Context(), settleDeadline, func(ctx context.Context) (bool, error) {
		resp, err := try(ctx, c, http.MethodGet, rawURL, header, nil)
		if err != nil {
			return false, nil //nolint:nilerr // the forward settles; the poll bounds the wait
		}
		last = resp
		if resp.Status != http.StatusOK {
			return false, nil
		}
		var targets targetsResponse
		if err := json.Unmarshal(resp.Body, &targets); err != nil {
			return false, fmt.Errorf("decode the targets answer: %w: %s", err, resp.Body)
		}

		return len(targets.Targets) == 0, nil
	})
	if err != nil {
		t.Fatalf("the gateway never answered %s with no target: %v (last %d: %s)", rawURL, err, last.Status, last.Body)
	}
}

// profilePanel is the expression that reads the Profile panel's article.
const profilePanel = `[...document.querySelectorAll(".panels article")]` +
	`.find((a) => (a.querySelector("header") || { textContent: "" }).textContent.trim() === "Profile")`

// podListed is the expression that is true while the Pod control offers a Pod beyond the placeholder,
// which is the one option an empty menu has.
const podListed = `(() => {
  const l = [...document.querySelectorAll("label")].find((l) => l.querySelector("select") && l.textContent.trim().startsWith("Pod"));
  return Boolean(l) && l.querySelector("select").options.length > 1;
})()`

// lastResourceType names what the browser was fetching the last of the requests for, or nothing for none.
func lastResourceType(sent []sentRequest) network.ResourceType {
	if len(sent) == 0 {
		return ""
	}

	return sent[len(sent)-1].resourceType
}

// pgoPath is the Service's policy route, which is also the route a gateway answers 503 on
// until its watches have replayed.
func pgoPath(ns, service string) string {
	return "/v1/namespaces/" + ns + "/services/" + service + "/pgo"
}

// assertClaim fails unless the issuer put want in the named claim of the token it minted.
// It reads the payload the way tokenKID reads the header.
func assertClaim(t *testing.T, token, claim, want string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q is not a JWT", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("token payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("token payload: %v", err)
	}
	if got, _ := claims[claim].(string); got != want {
		t.Fatalf("the issuer put %q in the %s claim, want %q; the claims it minted are %v",
			got, claim, want, claims)
	}
}

// awaitGateway polls one URL until the gateway behind the forward answers it.
func awaitGateway(t *testing.T, c *http.Client, rawURL string) {
	t.Helper()
	var last response
	err := poll(t.Context(), settleDeadline, func(ctx context.Context) (bool, error) {
		resp, err := try(ctx, c, http.MethodGet, rawURL, navigationHeaders(), nil)
		if err != nil {
			return false, nil //nolint:nilerr // the forward settles; the poll bounds the wait
		}
		last = resp

		return resp.Status == http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("the gateway never answered %s: %v (last %d: %s)", rawURL, err, last.Status, last.Body)
	}
}

// awaitPGORoutes polls the policy route until the gateway answers past the replay barrier.
// A 503 from the first read is that barrier and not a bug.
func awaitPGORoutes(t *testing.T, c *http.Client, header http.Header, rawURL string) {
	t.Helper()
	var last response
	err := poll(t.Context(), barrierDeadline, func(ctx context.Context) (bool, error) {
		resp, err := try(ctx, c, http.MethodGet, rawURL, header, nil)
		if err != nil {
			return false, nil //nolint:nilerr // the forward settles; the poll bounds the wait
		}
		last = resp

		return resp.Status == http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("the gateway never answered %s past the replay barrier: %v (last %d: %s)",
			rawURL, err, last.Status, last.Body)
	}
}

// seedCompletedCollection creates one Collection over the API and follows it to completion,
// so the list and the detail the browser reads have a finished record to show.
func seedCompletedCollection(t *testing.T, c *http.Client, header http.Header, ns string) string {
	t.Helper()
	body := `{"sampling":{"duration":"2s","rounds":1,"roundInterval":"0s","replicas":"all"}}`
	post := header.Clone()
	post.Set("Content-Type", "application/json")
	resp := send(t, c, http.MethodPost, gatewayOrigin+"/v1/namespaces/"+ns+"/services/"+testAppName+"/collections",
		post, strings.NewReader(body))
	if resp.Status != http.StatusAccepted {
		t.Fatalf("seed a Collection: status %d: %s", resp.Status, resp.Body)
	}
	var accepted acceptedCollection
	decode(t, "seed a Collection", resp.Body, &accepted)

	var rec collectionRecord
	err := poll(t.Context(), collectionDeadline, func(context.Context) (bool, error) {
		read := send(t, c, http.MethodGet, gatewayOrigin+"/v1/collections/"+accepted.ID, header, nil)
		if read.Status != http.StatusOK {
			return false, fmt.Errorf("GET the seeded Collection: status %d: %s", read.Status, read.Body)
		}
		decode(t, "the seeded Collection", read.Body, &rec)

		return rec.State == "completed" || terminal(rec.State), nil
	})
	if err != nil {
		t.Fatalf("the seeded Collection never completed: %v", err)
	}
	if rec.State != "completed" {
		t.Fatalf("the seeded Collection ended %s (%s), want completed", rec.State, rec.Reason)
	}

	return accepted.ID
}

// raiseSampling puts a Service policy override that lengthens sampling,
// and removes it when the scenario ends.
// Nothing in the override enables scheduling, so the gateway manufactures no Collection under it.
func raiseSampling(t *testing.T, c *http.Client, header http.Header, ns string) {
	t.Helper()
	put := header.Clone()
	put.Set("Content-Type", "application/json")
	body := fmt.Sprintf(`{"sampling":{"duration":%q,"rounds":1,"roundInterval":"0s","replicas":"all"}}`,
		consoleSamplingDuration)
	rawURL := gatewayOrigin + pgoPath(ns, testAppName)
	resp := send(t, c, http.MethodPut, rawURL, put, strings.NewReader(body))
	if resp.Status != http.StatusCreated {
		t.Fatalf("PUT the policy override: status %d: %s", resp.Status, resp.Body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("PUT the policy override returned no ETag")
	}
	// The cleanup runs after the test's own context is cancelled, so it carries one of its own.
	t.Cleanup(func() {
		drop := header.Clone()
		drop.Set("If-Match", etag)
		del, err := try(context.Background(), c, http.MethodDelete, rawURL, drop, nil)
		if err != nil || del.Status != http.StatusNoContent {
			t.Errorf("DELETE the policy override: status %d, error %v: %s", del.Status, err, del.Body)
		}
	})
}

// assertNavigatedToLogin fails unless the browser asked for the login route itself,
// carrying the page's selection and the return marker in the path it sealed.
func assertNavigatedToLogin(t *testing.T, s *session, selection string) {
	t.Helper()
	for _, r := range s.sent(http.MethodGet) {
		if !strings.HasPrefix(r.url, gatewayOrigin+"/auth/login?") {
			continue
		}
		u, err := url.Parse(r.url)
		if err != nil {
			t.Fatal(err)
		}
		ret, err := url.Parse(u.Query().Get("return"))
		if err != nil {
			t.Fatalf("the login's return path %q: %v", u.Query().Get("return"), err)
		}
		q := ret.Query()
		if ret.Path != uiPath || q.Get("ns") != selection || q.Get("svc") != selection || q.Get("returned") != "1" {
			t.Fatalf("the page navigated to the login with return %q, want %s carrying the selection and the marker",
				u.Query().Get("return"), uiPath)
		}

		return
	}
	t.Fatalf("the page never navigated to /auth/login of its own accord\n%s", s.report())
}

// assertLongSelectionDropped opens the console in s, a session with no cookie,
// on an ns of 1100 characters and no svc,
// and fails unless the login the page navigated to carries `/ui/?returned=1` as its return and nothing more.
// The encoded selection would cross the 1024-byte bound the browser flow applies to the value as received,
// so the page leaves it out rather than sending a return the flow would refuse.
func assertLongSelectionDropped(t *testing.T, s *session) {
	t.Helper()
	long := url.Values{"ns": {strings.Repeat("a", 1100)}}
	s.run(t, "open the console on a selection longer than the return path bound",
		chromedp.Navigate(gatewayOrigin+uiPath+"?"+long.Encode()))
	r := s.awaitRequestSince(t, 0, "the second session's login", func(r sentRequest) bool {
		return r.method == http.MethodGet && strings.HasPrefix(r.url, gatewayOrigin+"/auth/login?")
	})
	u, err := url.Parse(r.url)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("return"); got != uiPath+"?returned=1" {
		t.Fatalf("the page navigated to the login with return %q for a selection past the bound, want %q alone",
			got, uiPath+"?returned=1")
	}
}

// assertSelection fails unless the browser is on the console carrying the selection,
// with the return marker already dropped from the address bar.
func assertSelection(t *testing.T, rawURL, selection string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("the browser is at %q: %v", rawURL, err)
	}
	q := u.Query()
	if u.Path != uiPath || q.Get("ns") != selection || q.Get("svc") != selection || q.Get("returned") != "" {
		t.Fatalf("after the login the browser is at %s, want %s carrying the selection and no marker", rawURL, uiPath)
	}
}

// pressTwiceInsideTheWindow presses **Start collection** and, fifty milliseconds later,
// the **Confirm start** that first press rendered, and holds the page to having sent nothing for the pair.
// Both clicks come from one evaluation in the page, timed by the page's own clock,
// so nothing between two chromedp runs can widen the gap past the half second the page refuses a second press in;
// the step fails when the pair took 400 milliseconds or more, so the two clicks are inside the window by construction.
// chromedp's DoubleClick is one press with a click count of two, which the page receives as one click,
// while a physical double-click is two clicks inside the platform's interval, which is what this dispatches.
// After the pair the control is still armed, with **Confirm start** and **Keep** standing,
// and the caller presses **Confirm start** once more after waiting past the window.
func pressTwiceInsideTheWindow(t *testing.T, s *session, route string) {
	t.Helper()
	var elapsed float64
	s.eval(t, "press Start collection twice inside the window", `(async () => {
  const press = (label) => {
    const b = [...document.querySelectorAll("button")].find((b) => b.textContent.trim() === label);
    if (!b) { throw new Error("no button is labelled " + label); }
    b.click();
  };
  const began = performance.now();
  press("Start collection");
  await new Promise((resolve) => setTimeout(resolve, 50));
  press("Confirm start");
  return performance.now() - began;
})()`, &elapsed)
	if elapsed >= 400 {
		t.Fatalf("the two presses were %.0f ms apart, want under 400 so both land inside the page's window", elapsed)
	}
	var standing []string
	s.eval(t, "read the start control",
		`[...document.querySelectorAll("button")].map((b) => b.textContent.trim()).filter((l) => l === "Confirm start" || l === "Keep")`,
		&standing)
	if len(standing) != 2 {
		t.Fatalf("after two presses inside the window the page shows %v, want Confirm start and Keep still standing\n%s",
			standing, s.report())
	}
	if sent := s.sentTo(http.MethodPost, route); len(sent) != 0 {
		t.Fatalf("two presses inside the window sent %d POSTs to %s, want none; a double-click must not start a Collection\n%s",
			len(sent), route, s.report())
	}
	s.run(t, "wait past the window", chromedp.Sleep(confirmWindow))
}

// assertStartRequest holds the start POST to what the page sent:
// one request to the route, the JSON media type, and exactly one idempotency key.
// The browser's own network events are the only place either header can be read as the page wrote it.
func assertStartRequest(t *testing.T, s *session, route string) {
	t.Helper()
	sent := s.sentTo(http.MethodPost, route)
	if len(sent) != 1 {
		t.Fatalf("the page sent %d POSTs to %s, want exactly 1; two presses are one request\n%s",
			len(sent), route, s.report())
	}
	if ct := sent[0].headerValues("Content-Type"); len(ct) != 1 || !strings.HasPrefix(ct[0], "application/json") {
		t.Fatalf("the start POST carried Content-Type %v, want application/json", ct)
	}
	if keys := sent[0].headerValues("Idempotency-Key"); len(keys) != 1 || keys[0] == "" {
		t.Fatalf("the start POST carried %d Idempotency-Key values (%v), want exactly one", len(keys), keys)
	}
}

// assertRenderedAsText fails unless every payload reached the document as text:
// no element either string names anywhere in the document,
// the escaped form in the markup of the container that renders it,
// and the sentinel each payload would set still undefined.
// The principal and the selection are named apart because the page renders them in containers of its own:
// the principal in the identity disclosure, the namespace and Service values in the Service panel.
// A single read of the document would pass and stop proving which container each value reached.
// An empty selection is a load carrying no hostile selection, and makes no Service-panel assertion.
func assertRenderedAsText(t *testing.T, s *session, what, principal, selection string) {
	t.Helper()
	var images int
	s.eval(t, "count the img elements", `document.querySelectorAll("img").length`, &images)
	if images != 0 {
		t.Fatalf("%s: the document holds %d img elements; a payload was parsed as markup", what, images)
	}
	containers := []struct {
		what     string
		selector string
		payload  string
	}{
		{"the identity disclosure", "details.identity", principal},
		{"the Service panel", ".selection", selection},
	}
	for _, c := range containers {
		if c.payload == "" {
			continue
		}
		var markup string
		s.eval(t, "read the markup of "+c.selector,
			fmt.Sprintf(`(document.querySelector(%q) || { innerHTML: "" }).innerHTML`, c.selector), &markup)
		if escaped := escapeMarkup(c.payload); !strings.Contains(markup, escaped) {
			t.Fatalf("%s: the markup of %s does not hold %q:\n%s", what, c.what, escaped, markup)
		}
	}
	for _, sentinel := range []string{consoleQuerySentinel, consolePrincipalSentinel} {
		var defined bool
		s.eval(t, "read the sentinel", fmt.Sprintf(`typeof window.%s !== "undefined"`, sentinel), &defined)
		if defined {
			t.Fatalf("%s: window.%s is set, so a payload's script ran", what, sentinel)
		}
	}
}

// escapeMarkup is how a browser serializes a text node back into markup.
func escapeMarkup(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// signInThroughDex completes the issuer's login form the page navigated to,
// and waits until the browser is back on the console.
func signInThroughDex(t *testing.T, s *session, user, password string) {
	t.Helper()
	s.waitFor(t, "the issuer serves its login form",
		`document.querySelector('input[name="password"]') !== null`)
	s.run(t, "complete the issuer's login form",
		chromedp.SendKeys(`input[name="login"]`, user, chromedp.ByQuery),
		chromedp.SendKeys(`input[name="password"]`, password, chromedp.ByQuery),
		chromedp.Submit(`input[name="password"]`, chromedp.ByQuery),
	)
	s.waitFor(t, "the landing page returns the browser to the console",
		`location.pathname === "`+uiPath+`"`)
}

// assertIdentityOpen reads the identity disclosure's own state, once the answer that decides it has landed.
// The property is read and not the attribute, because open is the element's state
// and the template never writes the attribute.
func assertIdentityOpen(t *testing.T, s *session, after string, want bool) {
	t.Helper()
	var open bool
	s.eval(t, "read the identity disclosure after "+after,
		`(document.querySelector("details.identity") || {}).open === true`, &open)
	if open != want {
		t.Fatalf("the identity disclosure is %s after %s, want %s:\n%s",
			openState(open), after, openState(want), s.report())
	}
}

// openState names a disclosure's state the way a reader of the failure would.
func openState(open bool) string {
	if open {
		return "open"
	}

	return "closed"
}

// identityErrorBox is the expression that finds the error a failed refetch of the identity produced,
// or null: the box that names the code the test wrote, outside the disclosure and outside the panels.
const identityErrorBox = `[...document.querySelectorAll("div.error")]` +
	`.filter((e) => !e.closest("details.identity") && !e.closest(".panels"))` +
	`.find((e) => e.textContent.includes("discovery_unavailable")) || null`

// identityRecovery is true while that error stands and carries the Retry control it offers,
// which is the way out a closed disclosure must not hide.
const identityRecovery = `((e) => Boolean(e) && ` +
	`[...e.querySelectorAll("button")].some((b) => b.textContent.trim() === "Retry"))(` + identityErrorBox + `)`

// pressIdentityRetry presses that Retry control and reports whether it pressed.
const pressIdentityRetry = `((e) => { const b = e && [...e.querySelectorAll("button")]` +
	`.find((b) => b.textContent.trim() === "Retry"); if (!b) { return false; } b.click(); return true; })(` +
	identityErrorBox + `)`

// serviceOffered is the expression that is true while the Service menu offers the Service named,
// which is a Service listing's answer having been applied.
func serviceOffered(svc string) string {
	return fmt.Sprintf(`(() => {
  const l = [...document.querySelectorAll("label")].find((l) => l.querySelector("select") && l.textContent.trim().startsWith("Service"));
  return Boolean(l) && [...l.querySelector("select").options].some((o) => o.value === %q);
})()`, svc)
}

// control is the search expression for the button with the exact label given.
// Every control the page draws is a button, Download included;
// the anchor the download is saved through is created for the click and never stands in the document.
// The labels sit inside a template that puts newlines around them, so the text is normalized.
func control(label string) string {
	return fmt.Sprintf(`//button[normalize-space()=%s]`, xpathLiteral(label))
}

// refreshButton is the search expression for the Refresh button inside the article whose title reads panel,
// Profile for the targets list and Collections for the table.
// The title is the strong element of the article's header,
// because the Collections header holds its Refresh beside the title.
func refreshButton(panel string) string {
	return fmt.Sprintf(`//article[header/strong[normalize-space()=%s]]//button[normalize-space()='Refresh']`,
		xpathLiteral(panel))
}

// refreshControl is the expression that reads the Refresh button of the panel refreshButton names,
// or null when it is absent.
func refreshControl(panel string) string {
	return fmt.Sprintf(`((a) => a ? [...a.querySelectorAll("button")].find((b) => b.textContent.trim() === "Refresh") || null : null)(`+
		`[...document.querySelectorAll(".panels article")]`+
		`.find((a) => (a.querySelector("header strong") || { textContent: "" }).textContent.trim() === %q))`, panel)
}

// refreshEnabled is the expression that is true while the panel's Refresh control stands and is enabled,
// which is the page idle on that list: the control is disabled from the press until the answer is applied or dropped.
func refreshEnabled(panel string) string {
	return fmt.Sprintf(`((b) => Boolean(b) && !b.disabled)(%s)`, refreshControl(panel))
}

// refreshDisabled is the expression that is true while the panel's Refresh control stands disabled,
// which is the page waiting on that list's request.
func refreshDisabled(panel string) string {
	return fmt.Sprintf(`((b) => Boolean(b) && b.disabled)(%s)`, refreshControl(panel))
}

// pressRefreshWhileHeld reads the panel's Refresh control disabled while its request is held at the browser,
// and clicks it once more through the element's own click, which a disabled button turns into nothing.
// Whether that second click sent a request is read by the caller once the held request is released,
// because a request the page sends is recorded by an event that arrives after the click has returned.
func pressRefreshWhileHeld(t *testing.T, s *session, panel string) {
	t.Helper()
	s.waitFor(t, "the "+panel+" Refresh control is disabled while its request is held", refreshDisabled(panel))
	var stood bool
	s.eval(t, "press the disabled "+panel+" Refresh control",
		fmt.Sprintf(`((b) => { if (!b) { return false; } b.click(); return true; })(%s)`, refreshControl(panel)), &stood)
	if !stood {
		t.Fatalf("the %s Refresh control went away while its request was held:\n%s", panel, s.textOf(t, ".panels"))
	}
}

// xpathLiteral quotes a string for XPath, which has no escape and needs concat for a value holding both quotes.
func xpathLiteral(s string) string {
	if !strings.Contains(s, `'`) {
		return `'` + s + `'`
	}
	if !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	parts := strings.Split(s, `'`)
	for i, p := range parts {
		parts[i] = `'` + p + `'`
	}

	return `concat(` + strings.Join(parts, `,"'",`) + `)`
}

// rowCell is the expression that reads one cell of the Collections row an identifier names.
// The Collections table is the one inside the panel's table container,
// so an empty target listing's reasons table is never read by mistake.
func rowCell(id string, cell int) string {
	return fmt.Sprintf(`((r) => r ? r.cells[%d].textContent.trim() : null)(`+
		`[...document.querySelectorAll(".table table tbody tr")].find((r) => r.cells[0].textContent.trim() === %q))`,
		cell, id)
}

// rowIDs lists the identifiers the Collections table shows.
func (s *session) rowIDs(t *testing.T) []string {
	t.Helper()
	var ids []string
	s.eval(t, "read the Collections table",
		`[...document.querySelectorAll(".table table tbody tr")].map((r) => r.cells[0].textContent.trim())`, &ids)

	return ids
}

// awaitRequest waits until the browser has recorded a request the page sent.
func (s *session) awaitRequest(t *testing.T, method, route string) {
	t.Helper()
	err := poll(s.ctx, settleDeadline, func(context.Context) (bool, error) {
		return len(s.sentTo(method, route)) > 0, nil
	})
	if err != nil {
		t.Fatalf("the page never sent %s %s: %v\n%s", method, route, err, s.report())
	}
}

// awaitRow waits until the Collections table shows an identifier.
// The page fetches the list once per selection and does not poll,
// so a list that has not caught up is refetched by pressing Refresh on the Collections table,
// which is the control an operator would use.
func (s *session) awaitRow(t *testing.T, id, what string) {
	t.Helper()
	var asked bool
	err := poll(s.ctx, settleDeadline, func(context.Context) (bool, error) {
		for _, got := range s.rowIDs(t) {
			if got == id {
				return true, nil
			}
		}
		asked = s.refetchCollections(t) || asked

		return false, nil
	})
	if err != nil {
		why := "the Collections Refresh control never became available, so the list was never asked for again"
		if asked {
			why = "the list was asked for again and never carried it"
		}
		t.Fatalf("%s never appeared in the Collections table: %v (%s)\n%s", what, err, why, s.report())
	}
}

// awaitStartedDetail waits until the detail names a Collection other than the one given, and returns its identifier.
// Only the start attempt's own outcome selects the record it created,
// so a detail naming an identifier no press of this scenario has opened is proof the page applied that outcome.
// Nothing here fetches on the page's behalf:
// a test that forced the list would find the row whether the answer was applied or discarded.
func (s *session) awaitStartedDetail(t *testing.T, opened string) string {
	t.Helper()
	const prefix = "Collection "
	var started string
	err := poll(s.ctx, settleDeadline, func(context.Context) (bool, error) {
		named, ok := strings.CutPrefix(strings.TrimSpace(s.textOf(t, ".collections details summary")), prefix)
		if !ok {
			return false, nil
		}
		if named = strings.TrimSpace(named); named == "" || named == opened {
			return false, nil
		}
		started = named

		return true, nil
	})
	if err != nil {
		t.Fatalf("the detail never named the Collection the browser started: %v\n%s", err, s.report())
	}

	return started
}

// refetchCollections asks the page for the Collections list again by pressing Refresh on the Collections table.
// It reports whether it pressed at all:
// a control that is absent is a page that has not rendered the table yet,
// and one that is disabled is a fetch still in flight,
// both for the caller's poll to wait out rather than for this to decide,
// and a caller that never once asked failed for a different reason than one whose asking went unanswered.
func (s *session) refetchCollections(t *testing.T) bool {
	t.Helper()
	var pressed bool
	s.eval(t, "press Refresh on the Collections table",
		fmt.Sprintf(`((b) => { if (!b || b.disabled) { return false; } b.click(); return true; })(%s)`,
			refreshControl("Collections")), &pressed)

	return pressed
}

// chooseOption picks a value in the select the label names,
// and dispatches the change event the page listens for, which setting the property alone would not.
// It polls the way waitFor polls rather than reading once,
// so a control the page has not finished rendering is waited for instead of reported as absent.
// Every early return in the expression comes before the assignment,
// so evaluating it a second time still sets the value at most once.
// The failure carries the session report, which every other browser helper's failure already carries:
// the request list names where the page went,
// and a page that navigated away from the console is indistinguishable from an unrendered control
// in a message that only says a label is missing.
func (s *session) chooseOption(t *testing.T, label, value string) {
	t.Helper()
	expr := fmt.Sprintf(`(() => {
  const l = [...document.querySelectorAll("label")].find((l) => l.querySelector("select") && l.textContent.trim().startsWith(%q));
  if (!l) { return "no select is labelled " + %q; }
  const sel = l.querySelector("select");
  if (![...sel.options].some((o) => o.value === %q)) {
    return %q + " is not offered; the options are " + [...sel.options].map((o) => o.value).join(", ");
  }
  sel.value = %q;
  sel.dispatchEvent(new Event("change", { bubbles: true }));
  return "";
})()`, label, label, value, value, value)
	var why string
	err := poll(s.ctx, browserDeadline, func(ctx context.Context) (bool, error) {
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &why)); err != nil {
			return false, err
		}

		return why == "", nil
	})
	if err != nil {
		// A transport error leaves no reason behind, so the error itself is the reason.
		if why == "" {
			why = err.Error()
		}
		t.Fatalf("choose %s = %q: %s\n%s", label, value, why, s.report())
	}
}

// authFailureMetric counts every request the authentication layer refused,
// labelled by the mode and by the reason auth.Reasons names.
const authFailureMetric = "profgate_auth_failures_total"

// reportAuthFailures logs what the gateway refused, and why, when the scenario fails.
// A console scenario that fails on a control it cannot find has usually left the console for the login,
// and the page goes there because something answered one of its fetches 401.
// The reason for that answer is recorded in this counter and nowhere else,
// so a failure that does not carry it leaves the cause to another run.
func reportAuthFailures(t *testing.T, h *Harness, ns, pod string) {
	t.Helper()
	ports, stop, err := h.forward(t.Context(), ns, pod, []string{"0:" + gatewayOpsPort})
	if err != nil {
		t.Logf("the authentication counters are unavailable: the ops port would not forward: %v", err)

		return
	}
	// The stop is registered first so that it runs last:
	// cleanups run in reverse, and the scrape below needs the forward still open.
	t.Cleanup(stop)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Log(authFailures(ports[0]))
	})
}

// authFailures reads the gateway's ops port and returns the failures it counted.
// The scrape carries a context of its own because a cleanup runs after the test's is cancelled.
func authFailures(port uint16) string {
	ctx, cancel := context.WithTimeout(context.Background(), settleDeadline)
	defer cancel()
	rawURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))) + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "the authentication counters could not be requested: " + err.Error()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "the authentication counters could not be read: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "the authentication counters could not be read: " + err.Error()
	}
	var b strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		// A counter the gateway has never incremented is still exported, as zero.
		if strings.HasPrefix(line, authFailureMetric) && !strings.HasSuffix(line, " 0") {
			b.WriteString("\n  " + line)
		}
	}
	if b.Len() == 0 {
		return "the gateway refused nothing: every " + authFailureMetric + " is zero"
	}

	return "the gateway refused these requests:" + b.String()
}

// textOf is the rendered text of the first element matching a selector.
func (s *session) textOf(t *testing.T, selector string) string {
	t.Helper()
	var out string
	s.eval(t, "read "+selector,
		fmt.Sprintf(`(document.querySelector(%q) || { textContent: "" }).textContent`, selector), &out)

	return out
}

// sent returns every request of one method the page sent, as the browser recorded it.
func (s *session) sent(method string) []sentRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentRequest, 0, len(s.requests))
	for _, r := range s.requests {
		if r.method == method {
			out = append(out, r)
		}
	}

	return out
}
