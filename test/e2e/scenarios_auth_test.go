//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// oidcGatewayName and basicGatewayName are the Deployment, ServiceAccount,
	// ConfigMap, and ClusterRoleBinding of the oidc-gateway and basic-gateway
	// overlays.
	oidcGatewayName  = "profgate-oidc"
	basicGatewayName = "profgate-basic"

	// dexName is the Deployment, Service, and label value in dex.yaml;
	// dexHost is the name the gateway and the client reach it by, and the
	// name its certificate is issued for.
	dexName     = "dex"
	dexHost     = "dex"
	dexPort     = "5556"
	dexManifest = "test/e2e/dex.yaml"
	// dexConfigMap holds Dex's configuration, dexTLSSecret its certificate.
	dexConfigMap = "dex-config"
	dexTLSSecret = "dex-tls" //nolint:gosec // the Secret's name, not its contents
	// dexIssuer is what Dex publishes as its issuer and what the gateway is
	// configured to trust; the two must match byte for byte.
	dexIssuer = "https://" + dexHost + ":" + dexPort
	// dexClientID is the static client the gateway logs in as; the gateway's
	// audience is the same value.
	dexClientID = "profgate"
	// dexUser is the static password the scenario logs in with; its email is
	// the username claim the gateway maps to a realm.
	dexUser = "alice@example.com"

	// gatewayOrigin is the origin the browser walk reaches the gateway at;
	// the leaf certifies tlsHost and the client dials the forward for it.
	gatewayOrigin = "https://" + tlsHost
	// callbackURL is the redirect Dex is told about and the gateway serves.
	callbackURL = gatewayOrigin + "/auth/callback"

	// usersFileKey is the Secret key the basic gateway's usersFile names.
	usersFileKey = "users.yaml"
	// issuerCAKey and cookieKeyKey are the Secret keys the oidc gateway's
	// caFile and cookieKeyFile name.
	issuerCAKey  = "issuer-ca.crt"
	cookieKeyKey = "cookie.key"

	// The cookie names the browser flow sets.
	sessionCookie = "__Host-profgate_session"
	txnCookie     = "__Host-profgate_txn"

	// keyRotationDeadline bounds the wait for a token signed with a rotated
	// key to verify: the gateway fetches keys on an unknown kid, at most once
	// per jwksRefreshMin, which the scenario sets to a second.
	keyRotationDeadline = 15 * time.Second
	// basicCost is the bcrypt cost every hash the basic scenario mints carries;
	// a set must share one cost, and 10 is the lowest the configuration admits.
	basicCost = 10
	// maxHops bounds a walk through the issuer's redirects.
	maxHops = 10
)

// dexServer is a running Dex Deployment and the port-forward the client
// reaches it through; a restart replaces both the Pod and the forward.
type dexServer struct {
	h    *Harness
	ns   string
	pod  string
	stop func()

	local string // 127.0.0.1:<local port> of the current forward
}

// dexConfig renders Dex's static configuration: HTTPS on 5556 with the
// mounted pair, the approval screen skipped, the password connector on so a
// password grant mints an ID token, one public client with the gateway's
// callback, and one static password.
func dexConfig(clientRedirect, passwordHash string) string {
	return fmt.Sprintf(`issuer: %s
storage:
  type: memory
web:
  https: 0.0.0.0:%s
  tlsCert: /etc/dex/tls/tls.crt
  tlsKey: /etc/dex/tls/tls.key
oauth2:
  skipApprovalScreen: true
  passwordConnector: local
staticClients:
  - id: %s
    name: %s
    public: true
    redirectURIs:
      - %s
enablePasswordDB: true
staticPasswords:
  - email: %s
    hash: %q
    username: alice
    userID: "08a8684b-db88-4b73-90a9-3cd1661f5466"
`, dexIssuer, dexPort, dexClientID, dexClientID, clientRedirect, dexUser, passwordHash)
}

// deployDex writes Dex's configuration and certificate, applies dex.yaml into
// ns, waits for the rollout, and opens a port-forward to it.
// clientRedirect is the callback Dex accepts; passwordHash is the bcrypt hash
// of the static user's password.
func (h *Harness) deployDex(ctx context.Context, ns string, ca authority, clientRedirect, passwordHash string) (*dexServer, error) {
	if err := h.applyNamedTLSSecret(ctx, ns, dexTLSSecret, ca.certPEM, ca.keyPEM); err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dexConfigMap, Namespace: ns},
		Data:       map[string]string{"config.yaml": dexConfig(clientRedirect, passwordHash)},
	}
	if err := h.applyConfigMap(ctx, ns, cm); err != nil {
		return nil, err
	}
	if err := h.kubectl(ctx, "apply", "-n", ns, "-f", dexManifest); err != nil {
		return nil, err
	}
	d := &dexServer{h: h, ns: ns}
	if err := d.await(ctx); err != nil {
		return nil, err
	}

	return d, nil
}

// await waits for the rollout, finds the one ready Pod, and forwards to it.
func (d *dexServer) await(ctx context.Context) error {
	h := d.h
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+dexName, "-n", d.ns,
		"--timeout="+rolloutTimeout.String()); err != nil {
		_ = h.kubectl(ctx, "describe", "pods", "-n", d.ns, "-l", "app.kubernetes.io/name="+dexName)
		_ = h.kubectl(ctx, "logs", "-n", d.ns, "-l", "app.kubernetes.io/name="+dexName, "--tail=50")

		return err
	}
	pod, err := h.waitOnePod(ctx, d.ns, "app.kubernetes.io/name="+dexName)
	if err != nil {
		return err
	}
	ports, stop, err := h.forward(ctx, d.ns, pod, []string{"0:" + dexPort})
	if err != nil {
		return fmt.Errorf("port-forward %s: %w", pod, err)
	}
	d.pod = pod
	d.stop = stop
	d.local = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0])))
	h.log.Info("dex", "namespace", d.ns, "pod", pod, "local", d.local)

	return nil
}

// restart replaces the Dex Pod and re-forwards to the new one.
// Storage is in memory, so the new Pod mints a new signing key: this is how
// the scenario rotates the issuer's key.
func (d *dexServer) restart(ctx context.Context) error {
	d.stop()
	if err := d.h.kubectl(ctx, "rollout", "restart", "deployment/"+dexName, "-n", d.ns); err != nil {
		return err
	}

	return d.await(ctx)
}

// close ends the current forward.
func (d *dexServer) close() {
	d.stop()
}

// authClient walks the browser flow by hand: a cookie jar, no redirect
// following so every Location header is asserted, keep-alives off, and a
// dialer that maps the gateway's and Dex's real hostnames to their forwards,
// so the walk follows the Location headers the two servers write with the
// hostnames their certificates carry.
// dexLocal is read on every dial, because a Dex restart moves the forward.
func authClient(gwLocal string, dexLocal func() string, pool *x509.CertPool) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}

	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				var d net.Dialer
				switch addr {
				case tlsHost + ":443":
					return d.DialContext(ctx, network, gwLocal)
				case dexHost + ":" + dexPort:
					return d.DialContext(ctx, network, dexLocal())
				default:
					return nil, fmt.Errorf("the browser walk was sent to %s, which is neither the gateway nor dex", addr)
				}
			},
		},
	}
}

// send performs one request with the headers given and returns the response
// with its body read and closed.
func send(t *testing.T, c *http.Client, method, rawURL string, headers http.Header, body io.Reader) response {
	t.Helper()
	resp, err := try(t.Context(), c, method, rawURL, headers, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}

	return resp
}

// try is send for callers that poll and want the error instead of a failure.
func try(ctx context.Context, c *http.Client, method, rawURL string, headers http.Header, body io.Reader) (response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return response{}, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		return response{}, err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return response{}, err
	}

	return response{Status: resp.StatusCode, Header: resp.Header, Body: b}, nil
}

// location resolves a response's Location header against the URL it answered.
func location(t *testing.T, resp response, from string) *url.URL {
	t.Helper()
	base, err := url.Parse(from)
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("GET %s: status %d without a Location header: %s", from, resp.Status, resp.Body)
	}
	ref, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("GET %s: Location %q: %v", from, loc, err)
	}

	return base.ResolveReference(ref)
}

// navigationHeaders are what a browser sends on a top-level navigation.
func navigationHeaders() http.Header {
	return http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}}
}

// cookieNames lists the names the jar would send to origin, sorted.
func cookieNames(t *testing.T, c *http.Client, origin string) []string {
	t.Helper()
	u, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ck := range c.Jar.Cookies(u) {
		names = append(names, ck.Name)
	}

	return names
}

// bcryptHash mints a hash of password at basicCost.
func bcryptHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), basicCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}

	return string(h)
}

// usersFile renders a users file naming each user in names with hash, all in
// the developer realm.
func usersFile(hash string, names ...string) []byte {
	var b strings.Builder
	b.WriteString("users:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  - name: %s\n    passwordHash: %q\n    realm: developer\n", n, hash)
	}

	return []byte(b.String())
}

// currentLogs returns the logs of the Pod's running container.
func currentLogs(t *testing.T, h *Harness, ns, name string) string {
	t.Helper()
	out, err := h.Client.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(t.Context())
	if err != nil {
		t.Fatalf("logs of %s: %v", name, err)
	}

	return string(out)
}

// tokenKID reads the kid of a JWT's header, which is what says which signing
// key the issuer used.
func tokenKID(t *testing.T, token string) string {
	t.Helper()
	head, _, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token %q is not a JWT", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		t.Fatalf("token header: %v", err)
	}
	var hdr struct {
		KID string `json:"kid"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		t.Fatalf("token header: %v", err)
	}

	return hdr.KID
}

// passwordGrant obtains an ID token from Dex's password connector.
func passwordGrant(t *testing.T, c *http.Client, password string) string {
	t.Helper()
	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {dexClientID},
		"username":   {dexUser},
		"password":   {password},
		"scope":      {"openid email"},
	}
	var token string
	err := poll(t.Context(), settleDeadline, func(ctx context.Context) (bool, error) {
		resp, err := try(ctx, c, http.MethodPost, dexIssuer+"/token",
			http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, strings.NewReader(form.Encode()))
		if err != nil || resp.Status != http.StatusOK {
			return false, nil //nolint:nilerr // Dex answers once it is up; the poll bounds the wait
		}
		var body struct {
			IDToken string `json:"id_token"`
		}
		if err := json.Unmarshal(resp.Body, &body); err != nil || body.IDToken == "" {
			return false, fmt.Errorf("token response %s holds no id_token", resp.Body)
		}
		token = body.IDToken

		return true, nil
	})
	if err != nil {
		t.Fatalf("Dex never answered the password grant: %v", err)
	}

	return token
}

// oidcAuthBlock is the oidc gateway's auth block: Dex as the issuer behind
// the mounted CA, the email claim mapped to the developer realm, the browser
// block with the gateway's callback, and a one-second refresh floor so a key
// rotation is observed inside the scenario.
func oidcAuthBlock() string {
	return fmt.Sprintf(`auth:
  mode: oidc
  oidc:
    issuer: %s
    audience: %s
    usernameClaim: email
    caFile: %s/%s
    jwksRefreshMin: 1s
    mapping:
      users:
        - name: %s
          realm: developer
    browser:
      clientID: %s
      redirectURL: %s
      scopes: [openid, email]
      cookieKeyFile: %s/%s
`, dexIssuer, dexClientID, authMountPath, issuerCAKey, dexUser, dexClientID, callbackURL, authMountPath, cookieKeyKey)
}

// basicAuthBlock is the basic gateway's auth block: one user inline and the
// users file the auth Secret holds.
func basicAuthBlock(aliceHash string) string {
	return fmt.Sprintf(`auth:
  mode: basic
  basic:
    users:
      - name: alice
        passwordHash: %q
        realm: developer
    usersFile: %s/%s
`, aliceHash, authMountPath, usersFileKey)
}

// scenarioAuthOIDCBrowser proves oidc mode against Dex: the browser flow from
// a navigation through Dex's password form to a session cookie, the session's
// origin check, a bearer ID token, the refusal of a token in the query, a
// signing-key rotation the gateway follows without a restart, logout, and the
// audit lines that make each step attributable.
func scenarioAuthOIDCBrowser(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	ctx := t.Context()

	password := rand.Text()
	dexCA := newAuthority(t, dexHost)
	dex, err := h.deployDex(ctx, ns, dexCA, callbackURL, bcryptHash(t, password))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dex.close)

	// The gateway trusts Dex through the mounted CA and seals cookies with
	// the mounted key; both are in place before the Pod starts.
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
	gwCA := newAuthority(t, tlsHost)
	cfg := gatewayConfig(gatewayConfigOptions{TLSMount: tlsMountPath, AuthBlock: oidcAuthBlock()})
	local, pod := deployHTTPSGateway(t, h, ns, "oidc-gateway", oidcGatewayName, gwCA, cfg)

	// Readiness followed discovery: the log shows the issuer discovered
	// before the informers synced, which is the state the probe reports.
	logs := currentLogs(t, h, ns, pod)
	discovered := strings.Index(logs, "issuer discovered; starting preflight")
	synced := strings.Index(logs, "discovery synced; ready")
	if discovered < 0 || synced < 0 || discovered > synced {
		t.Fatalf("the gateway log does not show issuer discovery before readiness:\n%s", logs)
	}

	pool := x509.NewCertPool()
	pool.AddCert(gwCA.ca)
	pool.AddCert(dexCA.ca)
	client := authClient(local, func() string { return dex.local }, pool)
	profilePath := fmt.Sprintf("/v1/namespaces/%s/services/%s/profiles/heap?pod=%s", ns, testAppName, pods[0].Name)
	profileURL := gatewayOrigin + profilePath

	// A bare request is refused with the bearer challenge.
	// The first request is polled, as the TLS gateway polls its first one:
	// the port-forward is open before the connection through it is usable.
	var bare response
	err = poll(ctx, settleDeadline, func(ctx context.Context) (bool, error) {
		resp, err := try(ctx, client, http.MethodGet, profileURL, nil, nil)
		if err != nil {
			return false, nil //nolint:nilerr // the forward settles; the poll bounds the wait
		}
		bare = resp

		return true, nil
	})
	if err != nil {
		t.Fatalf("the gateway never answered over HTTPS: %v", err)
	}
	if bare.Status != http.StatusUnauthorized || bare.Header.Get("WWW-Authenticate") != `Bearer realm="profgate"` {
		t.Fatalf("GET without credentials: status %d, WWW-Authenticate %q, want 401 with the bearer challenge",
			bare.Status, bare.Header.Get("WWW-Authenticate"))
	}

	// A navigation is sent to login with the path and query to return to.
	nav := send(t, client, http.MethodGet, profileURL, navigationHeaders(), nil)
	if nav.Status != http.StatusFound {
		t.Fatalf("navigation without credentials: status %d, want 302", nav.Status)
	}
	loginURL := location(t, nav, profileURL)
	if loginURL.Path != "/auth/login" || loginURL.Query().Get("return") != profilePath {
		t.Fatalf("navigation redirected to %s, want /auth/login?return=%s", loginURL, profilePath)
	}

	// Login seals the transaction and sends the browser to Dex.
	login := send(t, client, http.MethodGet, loginURL.String(), navigationHeaders(), nil)
	if login.Status != http.StatusFound {
		t.Fatalf("GET %s: status %d, want 302: %s", loginURL, login.Status, login.Body)
	}
	authorize := location(t, login, loginURL.String())
	if authorize.Scheme != "https" || authorize.Host != dexHost+":"+dexPort || authorize.Path != "/auth" {
		t.Fatalf("login redirected to %s, want %s/auth", authorize, dexIssuer)
	}
	if names := cookieNames(t, client, gatewayOrigin); len(names) != 1 || names[0] != txnCookie {
		t.Fatalf("after login the jar holds %v, want exactly %s", names, txnCookie)
	}

	// Dex: follow its redirects to the password form, submit it, and follow
	// what comes back until Dex sends the browser to the gateway's callback.
	form := walkDex(t, client, authorize.String())
	action := formAction(t, form)
	credentials := url.Values{"login": {dexUser}, "password": {password}}
	submitted := send(t, client, http.MethodPost, action, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		strings.NewReader(credentials.Encode()))
	callback := followToGateway(t, client, submitted, action)
	if callback.Path != "/auth/callback" || callback.Query().Get("code") == "" || callback.Query().Get("state") == "" {
		t.Fatalf("Dex sent the browser to %s, want the callback with code and state", callback)
	}

	// The callback mints the session and answers a page that sends the
	// browser where it started. A browser reports the chain that began at
	// Dex as cross-site, which is what this request sends.
	cbHeaders := navigationHeaders()
	cbHeaders.Set("Sec-Fetch-Site", "cross-site")
	cb := send(t, client, http.MethodGet, callback.String(), cbHeaders, nil)
	if cb.Status != http.StatusOK || !strings.HasPrefix(cb.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("callback: status %d, Content-Type %q, want 200 text/html: %s", cb.Status, cb.Header.Get("Content-Type"), cb.Body)
	}
	if !strings.Contains(string(cb.Body), html.EscapeString(profilePath)) {
		t.Fatalf("the landing page does not name %s:\n%s", profilePath, cb.Body)
	}
	if names := cookieNames(t, client, gatewayOrigin); len(names) != 1 || names[0] != sessionCookie {
		t.Fatalf("after the callback the jar holds %v, want exactly %s", names, sessionCookie)
	}

	// The session pulls a profile from the navigation the landing page
	// starts, which comes from the gateway's own document.
	var withSession response
	err = poll(ctx, settleDeadline, func(ctx context.Context) (bool, error) {
		resp, err := try(ctx, client, http.MethodGet, profileURL, http.Header{"Sec-Fetch-Site": {"same-origin"}}, nil)
		if err != nil {
			return false, err
		}
		withSession = resp

		return resp.Status == http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("the session never pulled a profile: %v (last status %d: %s)", err, withSession.Status, withSession.Body)
	}
	if _, err := profile.ParseData(withSession.Body); err != nil {
		t.Fatalf("heap with the session: profile.Parse: %v", err)
	}

	// The same cookie from another site is refused.
	csrf := send(t, client, http.MethodGet, profileURL, http.Header{"Sec-Fetch-Site": {"cross-site"}}, nil)
	if csrf.Status != http.StatusUnauthorized {
		t.Fatalf("cross-site request with the session: status %d, want 401", csrf.Status)
	}

	// A bearer ID token from the password connector is accepted.
	idToken := passwordGrant(t, client, password)
	bearer := http.Header{"Authorization": {"Bearer " + idToken}}
	if resp := send(t, client, http.MethodGet, profileURL, bearer, nil); resp.Status != http.StatusOK {
		t.Fatalf("GET with a bearer token: status %d: %s", resp.Status, resp.Body)
	}

	// The token in the query is refused before the header is read.
	queryURL := profileURL + "&access_token=" + url.QueryEscape(idToken)
	if resp := send(t, client, http.MethodGet, queryURL, bearer, nil); resp.Status != http.StatusBadRequest ||
		!strings.Contains(string(resp.Body), `"invalid_parameter"`) {
		t.Fatalf("GET with access_token in the query: status %d: %s, want 400 invalid_parameter", resp.Status, resp.Body)
	}

	// Rotation: a restarted Dex signs with a new key, and the gateway learns
	// it from the unknown kid without being restarted.
	before := podState(t, h, ns, pod)
	if err := dex.restart(ctx); err != nil {
		t.Fatal(err)
	}
	rotated := passwordGrant(t, client, password)
	if tokenKID(t, rotated) == tokenKID(t, idToken) {
		t.Fatal("the restarted Dex signed with the same kid; the key was not rotated")
	}
	rotatedBearer := http.Header{"Authorization": {"Bearer " + rotated}}
	start := time.Now()
	var last response
	err = poll(ctx, keyRotationDeadline, func(ctx context.Context) (bool, error) {
		resp, err := try(ctx, client, http.MethodGet, profileURL, rotatedBearer, nil)
		if err != nil {
			return false, err
		}
		last = resp

		return resp.Status == http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("a token signed with the rotated key never verified within %v: %v (last status %d: %s)",
			keyRotationDeadline, err, last.Status, last.Body)
	}
	t.Logf("the rotated key verified %v after Dex restarted", time.Since(start).Round(time.Millisecond))
	after := podState(t, h, ns, pod)
	if after.uid != before.uid {
		t.Errorf("the Pod's UID changed from %s to %s; the key was picked up by replacing the Pod", before.uid, after.uid)
	}
	if after.restarts != before.restarts {
		t.Errorf("the container restart count went from %d to %d; the gateway restarted to pick the key up",
			before.restarts, after.restarts)
	}

	// Logout drops the session; Dex publishes no end_session_endpoint, so the
	// browser goes to /.
	logout := send(t, client, http.MethodGet, gatewayOrigin+"/auth/logout", navigationHeaders(), nil)
	if logout.Status != http.StatusFound || logout.Header.Get("Location") != "/" {
		t.Fatalf("logout: status %d, Location %q, want 302 to /", logout.Status, logout.Header.Get("Location"))
	}
	if names := cookieNames(t, client, gatewayOrigin); len(names) != 0 {
		t.Fatalf("after logout the jar holds %v, want nothing", names)
	}

	// Audit: the redirect, the refusal, and the login are each attributable.
	logs = currentLogs(t, h, ns, pod)
	for _, want := range []string{`"code":"auth_redirect"`, `"auth_reason":"csrf"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the gateway log has no record with %s", want)
		}
	}
	var callbackLogged bool
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"route":"auth_callback"`) && strings.Contains(line, `"principal":"`+dexUser+`"`) {
			callbackLogged = true
		}
	}
	if !callbackLogged {
		t.Errorf("the gateway log has no auth_callback record naming %s", dexUser)
	}
}

// walkDex follows Dex's redirects from the authorization endpoint to the
// page that holds its password form and returns that page.
func walkDex(t *testing.T, c *http.Client, rawURL string) string {
	t.Helper()
	current := rawURL
	for range maxHops {
		resp := send(t, c, http.MethodGet, current, navigationHeaders(), nil)
		switch {
		case resp.Status == http.StatusOK:
			return string(resp.Body)
		case resp.Status/100 == 3:
			current = location(t, resp, current).String()
		default:
			t.Fatalf("GET %s: status %d: %s", current, resp.Status, resp.Body)
		}
	}
	t.Fatalf("Dex redirected %d times without serving its password form", maxHops)

	return ""
}

// formAction reads the action of the first form on a Dex page, resolved
// against the issuer.
func formAction(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`<form[^>]*\saction="([^"]*)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("Dex served a page without a form:\n%s", page)
	}
	base, err := url.Parse(dexIssuer + "/")
	if err != nil {
		t.Fatal(err)
	}
	// The attribute is HTML: its ampersands are written as entities.
	action := html.UnescapeString(m[1])
	ref, err := url.Parse(action)
	if err != nil {
		t.Fatalf("form action %q: %v", action, err)
	}

	return base.ResolveReference(ref).String()
}

// followToGateway follows the redirects after the form was submitted, through
// the approval step when Dex returns one, until the Location names the
// gateway, and returns that URL without requesting it.
func followToGateway(t *testing.T, c *http.Client, resp response, from string) *url.URL {
	t.Helper()
	current := from
	for range maxHops {
		if resp.Status/100 != 3 {
			t.Fatalf("%s: status %d, want a redirect: %s", current, resp.Status, resp.Body)
		}
		next := location(t, resp, current)
		if next.Host == tlsHost {
			return next
		}
		current = next.String()
		resp = send(t, c, http.MethodGet, current, navigationHeaders(), nil)
	}
	t.Fatalf("Dex redirected %d times without reaching the gateway", maxHops)

	return nil
}

// scenarioAuthBasic proves basic mode over TLS: the challenge, a wrong
// password, an inline user, a user from the mounted file, go tool pprof with
// a userinfo URL, and a users-file rotation the gateway follows without the
// Pod being replaced.
func scenarioAuthBasic(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	ctx := t.Context()

	password := rand.Text()
	hash := bcryptHash(t, password)
	if err := h.applyAuthSecret(ctx, ns, map[string][]byte{usersFileKey: usersFile(hash, "bob")}); err != nil {
		t.Fatal(err)
	}
	// The leaf certifies the loopback address as well as the hostname, so
	// go tool pprof can dial the forward by address with no hostname mapping.
	ca := newAuthority(t, tlsHost, "127.0.0.1")
	cfg := gatewayConfig(gatewayConfigOptions{TLSMount: tlsMountPath, AuthBlock: basicAuthBlock(hash)})
	local, pod := deployHTTPSGateway(t, h, ns, "basic-gateway", basicGatewayName, ca, cfg)

	client := tlsClient(local, ca.pool)
	targetsURL := tlsTargetsURL(ns, testAppName)
	basic := func(user, pass string) http.Header {
		return http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))}}
	}
	status := func(ctx context.Context, headers http.Header) (int, error) {
		resp, err := try(ctx, client, http.MethodGet, targetsURL, headers, nil)

		return resp.Status, err
	}

	// No credential is the challenge.
	// The first request is polled, as the TLS gateway polls its first one:
	// the port-forward is open before the connection through it is usable.
	var bare response
	err := poll(ctx, settleDeadline, func(ctx context.Context) (bool, error) {
		resp, err := try(ctx, client, http.MethodGet, targetsURL, nil, nil)
		if err != nil {
			return false, nil //nolint:nilerr // the forward settles; the poll bounds the wait
		}
		bare = resp

		return true, nil
	})
	if err != nil {
		t.Fatalf("the gateway never answered over HTTPS: %v", err)
	}
	if bare.Status != http.StatusUnauthorized || bare.Header.Get("WWW-Authenticate") != `Basic realm="profgate"` {
		t.Fatalf("GET without credentials: status %d, WWW-Authenticate %q, want 401 with the basic challenge",
			bare.Status, bare.Header.Get("WWW-Authenticate"))
	}

	// The credential cases run in order against the one deployed gateway;
	// each names itself in its failure rather than as a subtest, because a
	// subtest would have to deploy a gateway of its own.
	expect := func(what, user, pass string, want int) {
		t.Helper()
		if got, err := status(ctx, basic(user, pass)); err != nil || got != want {
			t.Fatalf("%s: status %d, error %v, want %d", what, got, err, want)
		}
	}
	expect("wrong password", "alice", "wrong", http.StatusUnauthorized)
	expect("inline user", "alice", password, http.StatusOK)
	expect("file user", "bob", password, http.StatusOK)

	// go tool pprof with the credentials in the URL, trusting the authority
	// through SSL_CERT_FILE and dialing the forward directly.
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, ca.caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "heap.pb.gz")
	pprofURL := fmt.Sprintf("https://alice:%s@%s/v1/namespaces/%s/services/%s/profiles/heap?pod=%s",
		url.QueryEscape(password), local, ns, testAppName, pods[0].Name)
	cmd := exec.CommandContext(ctx, "go", "tool", "pprof", "-proto", "-output", out, pprofURL) //nolint:gosec // the arguments are composed by the test
	cmd.Dir = h.root
	cmd.Env = append(withoutProxy(os.Environ()), "SSL_CERT_FILE="+caFile, "PPROF_TMPDIR="+dir)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go tool pprof: %v\n%s", err, b)
	}
	fetched, err := os.ReadFile(out) //nolint:gosec // the path is in the test's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ParseData(fetched); err != nil {
		t.Fatalf("the profile go tool pprof wrote does not parse: %v", err)
	}

	// Rotation: the users file now names carol instead of bob, and the
	// running process picks it up.
	before := podState(t, h, ns, pod)
	start := time.Now()
	if err := h.applyAuthSecret(ctx, ns, map[string][]byte{usersFileKey: usersFile(hash, "carol")}); err != nil {
		t.Fatal(err)
	}
	err = poll(ctx, rotationDeadline, func(ctx context.Context) (bool, error) {
		got, err := status(ctx, basic("carol", password))

		return err == nil && got == http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("carol was never admitted within %v: %v", rotationDeadline, err)
	}
	t.Logf("the replaced users file was in effect %v after the Secret was updated", time.Since(start).Round(time.Second))
	if got, err := status(ctx, basic("bob", password)); err != nil || got != http.StatusUnauthorized {
		t.Errorf("bob after the file was replaced: status %d, error %v, want 401; the sets were merged rather than swapped", got, err)
	}
	after := podState(t, h, ns, pod)
	if after.uid != before.uid {
		t.Errorf("the Pod's UID changed from %s to %s; the users file was picked up by replacing the Pod", before.uid, after.uid)
	}
	if after.restarts != before.restarts {
		t.Errorf("the container restart count went from %d to %d; the gateway restarted to pick the users file up",
			before.restarts, after.restarts)
	}
}

// withoutProxy drops every proxy variable from env, so go tool pprof dials
// the loopback forward itself.
func withoutProxy(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		}
		out = append(out, kv)
	}

	return out
}
