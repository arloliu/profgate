//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
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

	// consoleSamplingDuration is what the Service policy raises sampling to before the browser starts a Collection.
	// The gateway's own default is two seconds, which can finish before a browser has pressed Cancel twice,
	// and a cancel on a terminal Collection is refused: that is a green-looking scenario proving the wrong thing.
	// It sits just under the ceiling the lane's configuration puts on a sampling duration.
	consoleSamplingDuration = "55s"
)

// scenarioConsoleOIDC drives the console in a browser against a gateway in oidc mode with PGO on.
// It is the first thing that executes app.js: every proof below is the page running,
// not the wire the page is supposed to write.
func scenarioConsoleOIDC(t *testing.T, h *Harness) {
	b := requireBrowser(t, h)
	ns := h.Namespace(t)
	deployTestApp(t, h, ns)
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
	local, _ := deployHTTPSGateway(t, h, ns, "oidc-gateway", oidcGatewayName, gwCA, cfg,
		credsMountPatch(oidcGatewayName))

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

	// The identity panel names the issuer's user and the realm it mapped to,
	// and the hostile query and the hostile principal are both rendered as text.
	panels := s.textOf(t, ".panels")
	for _, want := range []string{consolePrincipalPayload, "developer", "oidc"} {
		if !strings.Contains(panels, want) {
			t.Fatalf("the identity panel does not name %q:\n%s", want, panels)
		}
	}
	if !strings.Contains(panels, consoleQueryPayload+" is not listed") {
		t.Fatalf("the page does not show the unlisted selection as text:\n%s", panels)
	}
	assertRenderedAsText(t, s, "the load with the hostile query", consoleQueryPayload, consolePrincipalPayload)
	s.assertClean(t, "the login round trip")

	// The second load is the working one: a listed namespace and a listed Service.
	s.run(t, "open the console on the test app",
		chromedp.Navigate(gatewayOrigin+uiPath+"?"+url.Values{"ns": {ns}, "svc": {testAppName}}.Encode()))
	s.waitFor(t, "the Service list answers", `document.querySelector(".panels") !== null`)
	s.waitFor(t, "the profile URL is built", `(document.querySelector("input.url") || {}).value !== ""`)

	// Choosing a profile fills the field with the URL Flow describes.
	s.chooseOption(t, "Profile", "heap")
	wantURL := gatewayOrigin + "/v1/namespaces/" + ns + "/services/" + testAppName + "/profiles/heap"
	s.waitFor(t, "the profile URL follows the profile chosen",
		fmt.Sprintf(`(document.querySelector("input.url") || {}).value === %q`, wantURL))

	// Download saves the body the profile endpoint streams.
	s.run(t, "press Download", chromedp.Click(control("Download"), chromedp.BySearch))
	assertGzipFramed(t, "the file the browser saved", s.awaitDownload(t, "the profile download"))
	s.assertClean(t, "the profile download")

	// The Collections table lists the Service's Collections and a row's detail shows its record.
	s.awaitRow(t, seeded, "the seeded Collection")
	s.run(t, "open the seeded Collection", chromedp.Click(control(seeded), chromedp.BySearch))
	s.waitFor(t, "the detail shows the seeded record",
		fmt.Sprintf(`(document.querySelector("details summary") || {}).textContent === "Collection %s"`, seeded))
	if detail := s.textOf(t, "details"); !strings.Contains(detail, "completed") {
		t.Fatalf("the detail of %s does not show its state:\n%s", seeded, detail)
	}

	// Start collection, pressed twice through its inline confirmation.
	before := s.rowIDs(t)
	s.run(t, "arm the start control", chromedp.Click(control("Start collection"), chromedp.BySearch))
	s.run(t, "confirm the start", chromedp.Click(control("Confirm start"), chromedp.BySearch))
	// A press queues the request; the assertion is on what the browser then sent,
	// so the count is read once the outcome has landed and not while it is still in flight.
	route := gatewayOrigin + "/v1/namespaces/" + ns + "/services/" + testAppName + "/collections"
	s.awaitRequest(t, http.MethodPost, route)
	started := s.awaitNewRow(t, before)
	assertStartRequest(t, s, route)
	t.Logf("the browser started collection %s", started)

	// Cancel on that row, pressed twice the same way.
	s.run(t, "arm the cancel control", chromedp.Click(control("Cancel"), chromedp.BySearch))
	s.run(t, "confirm the cancel", chromedp.Click(control("Confirm cancel"), chromedp.BySearch))
	s.waitFor(t, "the row moves to cancelled",
		fmt.Sprintf(`%s === "cancelled"`, rowCell(started, 2)))
	s.waitFor(t, "the cancel control goes with the state",
		fmt.Sprintf(`%s === ""`, rowCell(started, 8)))

	// Every load of the scenario, once more at the end: the observers ran through all of them.
	assertRenderedAsText(t, s, "the working load", consolePrincipalPayload)
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
	local, _ := deployHTTPSGateway(t, h, ns, "basic-gateway", basicGatewayName, ca, cfg)

	// The gateway is answering before the browser is pointed at it:
	// a port-forward is open before a connection through it is usable,
	// and a page that loads into a refused connection proves nothing about a challenge.
	client := tlsClient(local, ca.pool)
	awaitGateway(t, client, gatewayOrigin+uiPath)

	s := newSession(t, b, sessionOptions{MapTo: local, User: consoleBasicUser, Password: password})
	s.run(t, "open the console",
		chromedp.Navigate(gatewayOrigin+uiPath+"?"+url.Values{"ns": {ns}, "svc": {testAppName}}.Encode()))
	s.waitFor(t, "the page continues past the challenge", `document.querySelector(".panels") !== null`)

	// The identity panel is the page continuing:
	// the first fetch was answered 401 with the basic challenge, the browser raised it,
	// and the test answered it over the protocol's own handling.
	panels := s.textOf(t, ".panels")
	for _, want := range []string{consoleBasicUser, "developer", "basic"} {
		if !strings.Contains(panels, want) {
			t.Fatalf("the identity panel does not name %q:\n%s", want, panels)
		}
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
	assertGzipFramed(t, "the file the browser saved", s.awaitDownload(t, "the profile download"))
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
// the escaped form in the container's markup,
// and the sentinel each payload would set still undefined.
func assertRenderedAsText(t *testing.T, s *session, what string, payloads ...string) {
	t.Helper()
	var images int
	s.eval(t, "count the img elements", `document.querySelectorAll("img").length`, &images)
	if images != 0 {
		t.Fatalf("%s: the document holds %d img elements; a payload was parsed as markup", what, images)
	}
	var markup string
	s.eval(t, "read the container's markup", `document.querySelector(".panels").innerHTML`, &markup)
	for _, payload := range payloads {
		if escaped := escapeMarkup(payload); !strings.Contains(markup, escaped) {
			t.Fatalf("%s: the container's markup does not hold %q:\n%s", what, escaped, markup)
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

// control is the search expression for the control with the exact label given.
// Both element names are matched because the page draws Download as a link with a button role
// and every other control as a button, and a caller presses a control rather than an element.
// The labels sit inside a template that puts newlines around them, so the text is normalized.
func control(label string) string {
	return fmt.Sprintf(`//*[self::button or self::a][normalize-space()=%s]`, xpathLiteral(label))
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
// so a list that has not caught up is refetched by choosing the Service again,
// which is the control an operator would use.
func (s *session) awaitRow(t *testing.T, id, what string) {
	t.Helper()
	err := poll(s.ctx, settleDeadline, func(context.Context) (bool, error) {
		for _, got := range s.rowIDs(t) {
			if got == id {
				return true, nil
			}
		}
		s.refetchCollections(t)

		return false, nil
	})
	if err != nil {
		t.Fatalf("%s never appeared in the Collections table: %v\n%s", what, err, s.report())
	}
}

// awaitNewRow waits for the one identifier the Collections table gained since before,
// and returns it.
func (s *session) awaitNewRow(t *testing.T, before []string) string {
	t.Helper()
	had := map[string]bool{}
	for _, id := range before {
		had[id] = true
	}
	var found string
	err := poll(s.ctx, settleDeadline, func(context.Context) (bool, error) {
		for _, got := range s.rowIDs(t) {
			if !had[got] {
				found = got

				return true, nil
			}
		}
		s.refetchCollections(t)

		return false, nil
	})
	if err != nil {
		t.Fatalf("no row appeared for the Collection the browser started: %v\n%s", err, s.report())
	}

	return found
}

// refetchCollections asks the page for the Collections list again by choosing the same Service,
// which is what the control does on every change.
func (s *session) refetchCollections(t *testing.T) {
	t.Helper()
	var svc string
	s.eval(t, "read the Service control",
		`(() => { const l = [...document.querySelectorAll("label")].find((l) => l.querySelector("select") &&`+
			` l.textContent.trim().startsWith("Service")); return l ? l.querySelector("select").value : ""; })()`, &svc)
	if svc == "" {
		return
	}
	s.chooseOption(t, "Service", svc)
}

// chooseOption picks a value in the select the label names,
// and dispatches the change event the page listens for, which setting the property alone would not.
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
	s.eval(t, "choose "+label, expr, &why)
	if why != "" {
		t.Fatalf("choose %s = %q: %s", label, value, why)
	}
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
