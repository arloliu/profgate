package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

const (
	// The three routes; the caller matches them and hands the request here.
	pathLogin    = "/auth/login"
	pathCallback = "/auth/callback"
	pathLogout   = "/auth/logout"

	// The codes a route outcome carries, as the audit line and the metrics
	// row name them.
	codeOK              = "ok"
	codeAuthRedirect    = "auth_redirect"
	codeUnauthenticated = "unauthenticated"
	codeAuthUnavailable = "auth_unavailable"
	codeNotReady        = "not_ready"
	codeRouteUnknown    = "route_unknown"

	// failureMessage is the one message every authentication failure carries.
	failureMessage = "authentication required"
	// noPrincipal is the principal an outcome names when nobody was resolved.
	noPrincipal = "-"
	// retryAfter is the Retry-After value of every 503 the routes write.
	retryAfter = "5"
	// maxClientSecretBytes bounds the trimmed client secret file.
	maxClientSecretBytes = 1024
)

// RouteOutcome is what one /auth/ route reports for the audit line and the
// metrics row.
type RouteOutcome struct {
	Status    int
	Code      string // ok, auth_redirect, unauthenticated, auth_unavailable, or not_ready
	Reason    string // an audit reason, or "" on success
	Principal string // the resolved principal on a successful callback, "-" otherwise
}

// AuthRoutes serves /auth/login, /auth/callback, and /auth/logout.
// The caller owns the route match, the method check, readiness,
// Cache-Control, the audit line, and the metrics row.
// The name reads as auth.AuthRoutes beside auth.Authenticator, the other
// seam internal/httpapi holds.
type AuthRoutes interface { //nolint:revive // named to pair with Authenticator in the caller's Deps

	ServeAuth(w http.ResponseWriter, r *http.Request, cfg *config.Config) RouteOutcome
}

// Routes returns the /auth/ handler, or nil when the browser block is not
// configured.
func (o *OIDC) Routes() AuthRoutes {
	if o.browser == nil {
		return nil
	}

	return o.browser
}

// browser is the relying party: it starts a login, completes it into a
// session cookie, and ends it.
// The endpoints are read from the issuer state per request, so a route is
// usable only once discovery and the first key fetch have both succeeded.
type browser struct {
	clientID, redirectURL, secret string
	// postLogoutURL is scheme://host of redirectURL plus "/", where the
	// issuer sends the browser after logout.
	postLogoutURL              string
	scopes                     []string
	sessionTTL, transactionTTL time.Duration
	state                      *atomic.Pointer[issuerState]
	client                     *issuerClient
	verifier                   *verifier
	sealer                     *sealer
	keyFile                    *filePoller
	rand                       io.Reader
	now                        func() time.Time
	log                        *slog.Logger
	rec                        metrics.Recorder
}

// newBrowser builds the relying party from the startup snapshot: the client
// secret and the cookie key file are read once here, and a file that cannot
// be read or parsed fails construction.
func newBrowser(o *OIDC, bc *config.OIDCBrowser) (*browser, error) {
	redirect, err := url.Parse(bc.RedirectURL)
	if err != nil {
		return nil, fmt.Errorf("auth: redirectURL: %w", err)
	}
	b := &browser{
		clientID:       bc.ClientID,
		redirectURL:    bc.RedirectURL,
		postLogoutURL:  redirect.Scheme + "://" + redirect.Host + "/",
		scopes:         bc.Scopes,
		sessionTTL:     bc.SessionTTL,
		transactionTTL: bc.TransactionTTL,
		state:          &o.state,
		client:         o.client,
		verifier:       o.verifier,
		sealer:         newSealer(),
		rand:           rand.Reader,
		now:            o.now,
		log:            o.log,
		rec:            o.rec,
	}
	if bc.ClientSecretFile != "" {
		raw, err := os.ReadFile(bc.ClientSecretFile) //nolint:gosec // the operator names the file; reading it is the purpose
		if err != nil {
			return nil, fmt.Errorf("auth: read clientSecretFile: %w", err)
		}
		b.secret = strings.TrimSpace(string(raw))
		if b.secret == "" || len(b.secret) > maxClientSecretBytes {
			return nil, fmt.Errorf("auth: clientSecretFile must hold 1 to %d bytes after trimming", maxClientSecretBytes)
		}
	}
	b.keyFile = newFilePoller(bc.CookieKeyFile, b.applyKeyFile, "cookie_key", o.pollInterval, o.rec, o.log)
	if err := b.keyFile.load(); err != nil {
		return nil, fmt.Errorf("auth: cookieKeyFile: %w", err)
	}

	return b, nil
}

// applyKeyFile is the key file poller's apply: the sealer takes the keys,
// then the recorder learns which fingerprints are loaded.
func (b *browser) applyKeyFile(raw []byte) error {
	if err := b.sealer.applyKeyFile(raw); err != nil {
		return err
	}
	b.rec.CookieKeys(b.sealer.keyInfo())

	return nil
}

// isNavigation reports a top-level browser navigation: Sec-Fetch-Mode
// navigate with Sec-Fetch-Dest document.
func isNavigation(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
}

// loginRedirect is /auth/login?return=<path and query of r>.
func loginRedirect(r *http.Request) string {
	ret := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		ret += "?" + r.URL.RawQuery
	}

	return pathLogin + "?return=" + url.QueryEscape(ret)
}

// redirect is the Redirect a Failure carries for r: the login URL for a
// navigation, "" for anything else.
func (b *browser) redirect(r *http.Request) string {
	if b == nil || !isNavigation(r) {
		return ""
	}

	return loginRedirect(r)
}

// session judges a request that carries the session cookie and no
// Authorization header.
// An unopenable or expired cookie is cleared and then treated as no
// credential; an opened one is admitted only from this origin or from no
// site at all, because the browser attaches the cookie to a cross-site
// top-level GET on its own.
func (b *browser) session(r *http.Request, value string) (Principal, error) {
	plain, ok := b.sealer.open(cookieSession, value)
	s, decoded := decodeSession(plain)
	if !ok || !decoded || !s.Exp.After(b.now()) {
		return Principal{}, &Failure{
			Status: http.StatusUnauthorized, Reason: ReasonSession, ClearSession: true, Redirect: b.redirect(r),
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "same-origin" && site != "none" {
		return Principal{}, &Failure{Status: http.StatusUnauthorized, Reason: ReasonCSRF}
	}

	return Principal{Name: s.Principal, Realm: s.Realm}, nil
}

// ServeAuth implements AuthRoutes.
// The issuer state is loaded once per request; without one every route is
// 503 not_ready, which the caller's readiness step already prevents.
func (b *browser) ServeAuth(w http.ResponseWriter, r *http.Request, cfg *config.Config) RouteOutcome {
	st := b.state.Load()
	if st == nil {
		writeEnvelope(w, http.StatusServiceUnavailable, codeNotReady, "issuer discovery has not completed")

		return RouteOutcome{Status: http.StatusServiceUnavailable, Code: codeNotReady, Principal: noPrincipal}
	}
	switch r.URL.Path {
	case pathLogin:
		return b.login(w, r, st)
	case pathCallback:
		return b.callback(w, r, st, cfg)
	case pathLogout:
		return b.logout(w, st)
	default:
		writeEnvelope(w, http.StatusNotFound, codeRouteUnknown, "no such route")

		return RouteOutcome{Status: http.StatusNotFound, Code: codeRouteUnknown, Principal: noPrincipal}
	}
}

// login seals the transaction and sends the browser to the authorization
// endpoint.
func (b *browser) login(w http.ResponseWriter, r *http.Request, st *issuerState) RouteOutcome {
	ret := canonicalReturn(r.URL.Query().Get("return"))
	var values [3]string
	for i := range values {
		v, fail := randomValue(b.rand)
		if fail != nil {
			return b.fail(w, fail)
		}
		values[i] = v
	}
	state, nonce, verifier := values[0], values[1], values[2]
	txn := transaction{State: state, Nonce: nonce, Verifier: verifier, Return: ret, Exp: b.now().Add(b.transactionTTL)}
	value, fail := b.sealer.seal(cookieTxn, txn.encode())
	if fail != nil {
		return b.fail(w, fail)
	}
	if err := setCookie(w, cookieTxn, value, b.transactionTTL); err != nil {
		return b.internal(w, err)
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {b.clientID},
		"redirect_uri":          {b.redirectURL},
		"scope":                 {strings.Join(b.scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	redirectTo(w, withQuery(st.doc.AuthorizationEndpoint, q))

	return RouteOutcome{Status: http.StatusFound, Code: codeAuthRedirect, Principal: noPrincipal}
}

// callback completes the login: it checks the transaction, exchanges the
// code, verifies the ID token, maps the realm, and mints the session.
// Success answers a landing page rather than a redirect: a browser computes
// Sec-Fetch-Site over the whole redirect chain, so a 302 from here would
// deliver the return path as cross-site and session would refuse it.
// Every failure deletes the transaction cookie and answers the envelope
// itself; nothing is redirected back to login.
func (b *browser) callback(w http.ResponseWriter, r *http.Request, st *issuerState, cfg *config.Config) RouteOutcome {
	q := r.URL.Query()
	txn, ok := b.openTransaction(r)
	if !ok {
		return b.failCallback(w, &Failure{Status: http.StatusUnauthorized, Reason: ReasonState})
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(txn.State)) != 1 {
		return b.failCallback(w, &Failure{Status: http.StatusUnauthorized, Reason: ReasonState})
	}
	if issuerErr := q.Get("error"); issuerErr != "" {
		b.log.Warn("issuer refused the login", "error", issuerErr)

		return b.failCallback(w, &Failure{Status: http.StatusUnauthorized, Reason: ReasonIssuerDenied})
	}
	idToken, fail := b.exchange(r, st, q.Get("code"), txn.Verifier)
	if fail != nil {
		return b.failCallback(w, fail)
	}
	c, fail := b.verifier.verify(r.Context(), idToken)
	if fail != nil {
		return b.failCallback(w, fail)
	}
	if subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(txn.Nonce)) != 1 {
		return b.failCallback(w, &Failure{Status: http.StatusUnauthorized, Reason: ReasonNonce})
	}
	realm, ok := mapRealm(cfg.Auth.OIDC.Mapping, c)
	if !ok {
		return b.failCallback(w, &Failure{Status: http.StatusUnauthorized, Reason: ReasonNoRealm})
	}
	s := session{Principal: c.Username, Realm: realm, Exp: b.now().Add(b.sessionTTL)}
	value, fail := b.sealer.seal(cookieSession, s.encode())
	if fail != nil {
		return b.failCallback(w, fail)
	}
	if err := setCookie(w, cookieSession, value, b.sessionTTL); err != nil {
		deleteCookie(w, cookieTxn)

		return b.internal(w, err)
	}
	deleteCookie(w, cookieTxn)
	b.rec.AuthSessionIssued()
	writeLanding(w, txn.Return)

	return RouteOutcome{Status: http.StatusOK, Code: codeOK, Principal: c.Username}
}

// writeLanding answers 200 with the page that sends the browser to ret, the
// canonical return path: a refresh to it and a link for a browser that does
// not follow one.
// The navigation the page starts comes from this origin, so the session
// cookie it carries passes the Sec-Fetch-Site check.
// The policy loads nothing; the page needs no script, style, or image.
func writeLanding(w http.ResponseWriter, ret string) {
	escaped := html.EscapeString(ret)
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<!doctype html>\n<meta charset=\"utf-8\">\n"+
		"<meta http-equiv=\"refresh\" content=\"0;url="+escaped+"\">\n"+
		"<title>Signed in</title>\n"+
		"<p>Signed in. Redirecting…</p>\n"+
		"<p><a href=\""+escaped+"\">Continue</a></p>\n")
}

// openTransaction reads the transaction cookie; false when it is absent,
// unopenable, malformed, or expired.
func (b *browser) openTransaction(r *http.Request) (transaction, bool) {
	c, err := r.Cookie(cookieTxn)
	if err != nil {
		return transaction{}, false
	}
	plain, ok := b.sealer.open(cookieTxn, c.Value)
	if !ok {
		return transaction{}, false
	}
	txn, ok := decodeTransaction(plain)
	if !ok || !txn.Exp.After(b.now()) {
		return transaction{}, false
	}

	return txn, true
}

// exchange posts the code to the token endpoint and returns the ID token.
// A transport failure or a server error is the issuer being unavailable; a
// refusal, or an answer without an ID token string, is the code being bad.
func (b *browser) exchange(r *http.Request, st *issuerState, code, verifier string) (string, *Failure) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {b.redirectURL},
		"client_id":     {b.clientID},
		"code_verifier": {verifier},
	}
	if b.secret != "" {
		form.Set("client_secret", b.secret)
	}
	status, body, err := b.client.postForm(r.Context(), st.doc.TokenEndpoint, form)
	if err != nil {
		b.log.Warn("token exchange failed", "error", err)

		return "", &Failure{Status: http.StatusServiceUnavailable, Reason: ReasonExchange}
	}
	if status >= http.StatusInternalServerError {
		b.log.Warn("token endpoint answered a server error", "status", status)

		return "", &Failure{Status: http.StatusServiceUnavailable, Reason: ReasonExchange}
	}
	denied := &Failure{Status: http.StatusUnauthorized, Reason: ReasonExchangeDenied}
	if status != http.StatusOK {
		return "", denied
	}
	var response map[string]json.RawMessage
	if err := decodeOne(body, &response); err != nil {
		return "", denied
	}
	idToken, ok := asString(response["id_token"])
	if !ok || idToken == "" {
		return "", denied
	}

	return idToken, nil
}

// logout deletes the session and sends the browser to the issuer's
// end_session_endpoint when discovery published one, or to / otherwise.
func (b *browser) logout(w http.ResponseWriter, st *issuerState) RouteOutcome {
	deleteCookie(w, cookieSession)
	target := "/"
	if st.doc.EndSessionEndpoint != "" {
		target = withQuery(st.doc.EndSessionEndpoint, url.Values{
			"post_logout_redirect_uri": {b.postLogoutURL},
			"client_id":                {b.clientID},
		})
	}
	redirectTo(w, target)

	return RouteOutcome{Status: http.StatusFound, Code: codeAuthRedirect, Principal: noPrincipal}
}

// failCallback deletes the transaction cookie, then answers the failure.
func (b *browser) failCallback(w http.ResponseWriter, fail *Failure) RouteOutcome {
	deleteCookie(w, cookieTxn)

	return b.fail(w, fail)
}

// fail writes the envelope for a Failure: 401 unauthenticated, or 503
// auth_unavailable with Retry-After.
func (b *browser) fail(w http.ResponseWriter, fail *Failure) RouteOutcome {
	code := codeUnauthenticated
	if fail.Status == http.StatusServiceUnavailable {
		code = codeAuthUnavailable
		w.Header().Set("Retry-After", retryAfter)
	}
	writeEnvelope(w, fail.Status, code, failureMessage)

	return RouteOutcome{Status: fail.Status, Code: code, Reason: fail.Reason, Principal: noPrincipal}
}

// internal answers a programming error — a cookie the bounds say cannot be
// too long was — as 503 with reason internal, logged at error.
func (b *browser) internal(w http.ResponseWriter, err error) RouteOutcome {
	b.log.Error("browser flow failed", "error", err)

	return b.fail(w, &Failure{Status: http.StatusServiceUnavailable, Reason: ReasonInternal})
}

// redirectTo answers 302 with an empty body.
func redirectTo(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}

// withQuery appends q to an endpoint that may already carry a query.
func withQuery(endpoint string, q url.Values) string {
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}

	return endpoint + sep + q.Encode()
}

// writeEnvelope writes the gateway's JSON error envelope, byte for byte what
// internal/httpapi writes; that package cannot be imported from here, and a
// test pins the two to the same bytes.
func writeEnvelope(w http.ResponseWriter, status int, code, message string) {
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	body, _ := json.Marshal(struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{Error: message, Code: code})
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
