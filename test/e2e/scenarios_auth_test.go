//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
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
	// dexPort is its listener, and dexNodePort the Service's NodePort.
	// The issuer is https://<node IP>:<dexNodePort>: the gateway inside the
	// cluster and the profgate client on the host must reach one issuer at
	// one address, an issuer is compared byte for byte, and the node's address is the one both sides can dial,
	// so the certificate is issued for that address and no name is resolved on either side.
	dexName     = "dex"
	dexPort     = "5556"
	dexNodePort = "30556"
	dexManifest = "test/e2e/dex.yaml"
	// dexConfigMap holds Dex's configuration, dexTLSSecret its certificate.
	dexConfigMap = "dex-config"
	dexTLSSecret = "dex-tls" //nolint:gosec // the Secret's name, not its contents
	// deviceCallback is the redirect the device flow ends at inside Dex; a
	// client listing any redirect URI is validated against its list alone.
	deviceCallback = "/device/callback"
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

	// logoutRoute is the value the audit record of a logout names its route with.
	logoutRoute = "auth_logout"

	// keyRotationDeadline bounds the wait for a token signed with a rotated
	// key to verify: the gateway fetches keys on an unknown kid, at most once
	// per jwksRefreshMin, which the scenario sets to a second.
	keyRotationDeadline = 15 * time.Second
	// basicCost is the bcrypt cost every hash the basic scenario mints carries;
	// a set must share one cost, and 10 is the lowest the configuration admits.
	basicCost = 10
	// maxHops bounds a walk through the issuer's redirects.
	maxHops = 10
	// dialTimeout bounds one connection attempt of the browser walk.
	dialTimeout = 5 * time.Second
	// forwardHealDeadline is how long a request is retried while a port-forward heals.
	// A session the Pod ended is reopened on the same local port,
	// and the reopen rebinds that port before its stream to the Pod works:
	// the port accepts a connection and resets it until the stream is up,
	// and client-go gives up on a stream it cannot create only after 30 seconds.
	// A budget sized for a dial gives up while the port is still accepting and resetting,
	// which is a scenario failing on a forward that was about to heal.
	forwardHealDeadline = 60 * time.Second
)

// dexServer is a running Dex Deployment, reached at its NodePort by the gateway and by the host alike;
// a restart replaces the Pod.
type dexServer struct {
	h   *Harness
	ns  string
	pod string

	addr   string // <node IP>:<dexNodePort>
	issuer string // https://<addr>, what Dex publishes and the gateway trusts
}

// dexConfig renders Dex's static configuration: HTTPS on 5556 with the
// mounted pair, the approval screen skipped, the password connector on so a
// password grant mints an ID token, one public client with the gateway's
// callback and the device flow's internal callback, and one static password.
// username is what the static user's name claim carries.
// The email stays well formed whatever that claim holds,
// so a login succeeds while the claim a gateway reads as the principal carries whatever a scenario needs.
func dexConfig(issuer, clientRedirect, passwordHash, username string) string {
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
      - %s
enablePasswordDB: true
staticPasswords:
  - email: %s
    hash: %q
    username: %q
    userID: "08a8684b-db88-4b73-90a9-3cd1661f5466"
`, issuer, dexPort, dexClientID, dexClientID, clientRedirect, deviceCallback, dexUser, passwordHash, username)
}

// deployDex writes Dex's configuration and certificate, applies dex.yaml into ns, and waits for the rollout.
// nodeIP is the address the issuer is published at, which ca must certify;
// clientRedirect is the callback Dex accepts;
// passwordHash is the bcrypt hash of the static user's password;
// username is what the name claim carries.
func (h *Harness) deployDex(
	ctx context.Context, ns string, ca authority, nodeIP, clientRedirect, passwordHash, username string,
) (*dexServer, error) {
	if err := h.applyNamedTLSSecret(ctx, ns, dexTLSSecret, ca.certPEM, ca.keyPEM); err != nil {
		return nil, err
	}
	d := &dexServer{h: h, ns: ns, addr: net.JoinHostPort(nodeIP, dexNodePort)}
	d.issuer = "https://" + d.addr
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dexConfigMap, Namespace: ns},
		Data:       map[string]string{"config.yaml": dexConfig(d.issuer, clientRedirect, passwordHash, username)},
	}
	if err := h.applyConfigMap(ctx, ns, cm); err != nil {
		return nil, err
	}
	if err := h.kubectl(ctx, "apply", "-n", ns, "-f", dexManifest); err != nil {
		return nil, err
	}
	if err := d.await(ctx); err != nil {
		return nil, err
	}

	return d, nil
}

// nodeIP is the internal address of one node, the address a NodePort is
// reached at from a Pod and from the host running kind.
func (h *Harness) nodeIP(ctx context.Context) (string, error) {
	nodes, err := h.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				return a.Address, nil
			}
		}
	}

	return "", errors.New("no node reports an internal address")
}

// await waits for the rollout and finds the one ready Pod.
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
	d.pod = pod
	h.log.Info("dex", "namespace", d.ns, "pod", pod, "issuer", d.issuer)

	return nil
}

// restart replaces the Dex Pod.
// Storage is in memory, so the new Pod mints a new signing key: this is how
// the scenario rotates the issuer's key.
func (d *dexServer) restart(ctx context.Context) error {
	if err := d.h.kubectl(ctx, "rollout", "restart", "deployment/"+dexName, "-n", d.ns); err != nil {
		return err
	}

	return d.await(ctx)
}

// authClient walks the browser flow by hand: a cookie jar, no redirect
// following so every Location header is asserted, keep-alives off, and a
// dialer that maps the gateway's real hostname to its forward,
// so the walk follows the Location headers the gateway writes with the hostname its certificate carries;
// Dex is dialed at the address it publishes.
func authClient(gwLocal, dexAddr string, pool *x509.CertPool) *http.Client {
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
				// A connection attempt to the NodePort while Dex's Pod is being replaced is dropped rather than refused; the timeout turns
				// that into a failed attempt the polls retry.
				d := net.Dialer{Timeout: dialTimeout}
				switch addr {
				case tlsHost + ":443":
					return dialForward(ctx, &d, network, gwLocal)
				case dexAddr:
					return d.DialContext(ctx, network, addr)
				default:
					return nil, fmt.Errorf("the browser walk was sent to %s, which is neither the gateway nor dex", addr)
				}
			},
		},
	}
}

// dialForward dials a port-forward's local address, retrying a refused connection while the forward heals:
// the harness reopens a forward whose session the Pod ended, and a dial inside that window finds no listener.
// The window is the reopen's, not a dial's,
// so the budget is the one send retries by rather than the one a single connection attempt is given.
func dialForward(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
	deadline := time.Now().Add(forwardHealDeadline)
	for {
		conn, err := d.DialContext(ctx, network, addr)
		if !errors.Is(err, syscall.ECONNREFUSED) || time.Now().After(deadline) {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// send performs one request with the headers given and returns the response
// with its body read and closed.
// A request that reaches no answer is sent again while the forward heals,
// for as long as sending it again is something the gateway cannot observe.
func send(t *testing.T, c *http.Client, method, rawURL string, headers http.Header, body io.Reader) response {
	t.Helper()
	// The body is read once and replayed from memory,
	// because a reader a first attempt drained has nothing left for a second.
	var payload []byte
	if body != nil {
		var err error
		payload, err = io.ReadAll(body)
		if err != nil {
			t.Fatalf("%s %s: read the body to send: %v", method, rawURL, err)
		}
	}
	deadline := time.Now().Add(forwardHealDeadline)
	for {
		var attempt io.Reader
		if body != nil {
			attempt = bytes.NewReader(payload)
		}
		resp, err := try(t.Context(), c, method, rawURL, headers, attempt)
		if err == nil {
			return resp
		}
		// A forward whose session the Pod ended answers a request in flight with EOF or a reset rather than refusing the dial,
		// so dialForward's retry never sees it and the caller is left with no answer at all.
		if !repeatable(method, headers) || time.Now().After(deadline) {
			t.Fatalf("%s %s: %v", method, rawURL, err)
		}
		time.Sleep(pollInterval)
	}
}

// repeatable says whether the gateway could tell that a request was sent twice.
// A GET and a HEAD change nothing, so a second one is invisible.
// Any other method is repeated only when it carries an idempotency key,
// which the gateway answers from the receipt it wrote for the first instead of acting a second time.
func repeatable(method string, headers http.Header) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return true
	}

	return headers.Get("Idempotency-Key") != ""
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

// logsWithAudit returns the Pod's log once it holds at least count records for the audit route named.
// A request's audit record is written after the request has been answered,
// so a log read the instant an answer arrives can be missing that answer's own record,
// and a count taken from it is one a later read grows without the gateway having written anything since.
// Waiting for the record of the last request answered is waiting for the log to hold the ones before it.
func logsWithAudit(t *testing.T, h *Harness, ns, pod, route string, count int) string {
	t.Helper()
	want := fmt.Sprintf(`"route":%q`, route)
	var logs string
	err := poll(t.Context(), settleDeadline, func(context.Context) (bool, error) {
		logs = currentLogs(t, h, ns, pod)

		return strings.Count(logs, want) >= count, nil
	})
	if err != nil {
		t.Fatalf("the gateway log never held %d %s records: %v\n%s", count, route, err, logs)
	}

	return logs
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
func passwordGrant(t *testing.T, c *http.Client, issuer, password string) string {
	t.Helper()

	return passwordGrantScoped(t, c, issuer, password, "openid email")
}

// passwordGrantScoped is passwordGrant for a caller that needs a claim another scope carries,
// because which claims an issuer mints depends on the scopes the grant asked for.
func passwordGrantScoped(t *testing.T, c *http.Client, issuer, password, scope string) string {
	t.Helper()
	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {dexClientID},
		"username":   {dexUser},
		"password":   {password},
		"scope":      {scope},
	}
	var token string
	var last error
	err := poll(t.Context(), settleDeadline, func(ctx context.Context) (bool, error) {
		resp, err := try(ctx, c, http.MethodPost, issuer+"/token",
			http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, strings.NewReader(form.Encode()))
		if err != nil {
			last = err

			return false, nil //nolint:nilerr // Dex answers once it is up; the poll bounds the wait
		}
		if resp.Status != http.StatusOK {
			last = fmt.Errorf("status %d: %s", resp.Status, resp.Body)

			return false, nil
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
		t.Fatalf("Dex never answered the password grant: %v (last: %v)", err, last)
	}

	return token
}

// oidcAuthBlock is the oidc gateway's auth block: Dex as the issuer behind
// the mounted CA, the email claim mapped to the developer realm, the browser
// block with the gateway's callback, the cli block that makes /v1/auth report
// the device login with a PKCE challenge and the scopes that carry the email claim and a refresh token, and a one-second refresh floor so a key rotation is observed inside the scenario.
func oidcAuthBlock(issuer string) string {
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
    cli:
      scopes: [openid, email, offline_access]
      pkce: true
`, issuer, dexClientID, authMountPath, issuerCAKey, dexUser, dexClientID, callbackURL, authMountPath, cookieKeyKey)
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
// signing-key rotation the gateway follows without a restart, logout, the
// console's shell, assets, and listing routes under the same session, and the
// audit lines that make each step attributable.
func scenarioAuthOIDCBrowser(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	ctx := t.Context()

	password := rand.Text()
	nodeIP, err := h.nodeIP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dexCA := newAuthority(t, nodeIP)
	dex, err := h.deployDex(ctx, ns, dexCA, nodeIP, callbackURL, bcryptHash(t, password), "alice")
	if err != nil {
		t.Fatal(err)
	}

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
	cfg := gatewayConfig(gatewayConfigOptions{TLSMount: tlsMountPath, AuthBlock: oidcAuthBlock(dex.issuer), UIEnabled: true})
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
	client := authClient(local, dex.addr, pool)
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
	if authorize.Scheme != "https" || authorize.Host != dex.addr || authorize.Path != "/auth" {
		t.Fatalf("login redirected to %s, want %s/auth", authorize, dex.issuer)
	}
	if names := cookieNames(t, client, gatewayOrigin); len(names) != 1 || names[0] != txnCookie {
		t.Fatalf("after login the jar holds %v, want exactly %s", names, txnCookie)
	}

	// Dex: follow its redirects to the password form, submit it, and follow
	// what comes back until Dex sends the browser to the gateway's callback.
	form := walkIssuer(t, client, authorize.String())
	action := formAction(t, dex.issuer, form)
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
	idToken := passwordGrant(t, client, dex.issuer, password)
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
	// The issuer is a NodePort,
	// and the node's forwarding rules name the new Pod some time after the rollout reports the old one gone,
	// so a grant taken the instant the rollout finishes can still be signed by the Pod that is leaving.
	// The assertion is unchanged, the key must rotate;
	// the wait only lets the routing catch up with the rollout.
	var rotated string
	kidDeadline := time.Now().Add(keyRotationDeadline)
	for {
		rotated = passwordGrant(t, client, dex.issuer, password)
		if tokenKID(t, rotated) != tokenKID(t, idToken) {
			break
		}
		if time.Now().After(kidDeadline) {
			t.Fatal("the restarted Dex signed with the same kid; the key was not rotated")
		}
		time.Sleep(pollInterval)
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

	// The console: the jar is empty, as a browser's is on its first visit.
	// The shell and its assets need no session and write no audit record.
	// The logout just answered is the last request that writes a record before this baseline,
	// so the baseline is taken once the log holds it.
	recordsBefore := auditRecords(logsWithAudit(t, h, ns, pod, logoutRoute, 1))
	shell := send(t, client, http.MethodGet, gatewayOrigin+uiPath, navigationHeaders(), nil)
	assertShell(t, "GET /ui/ without a cookie", shell)
	if !strings.Contains(string(shell.Body), "<main") {
		t.Fatalf("GET /ui/ without a cookie: the body is not the shell:\n%s", shell.Body)
	}
	head := send(t, client, http.MethodHead, gatewayOrigin+uiPath, navigationHeaders(), nil)
	assertShell(t, "HEAD /ui/", head)
	if len(head.Body) != 0 {
		t.Fatalf("HEAD /ui/: body of %d bytes, want none", len(head.Body))
	}
	for _, asset := range assetPaths(t, shell.Body) {
		resp := send(t, client, http.MethodGet, gatewayOrigin+asset, nil, nil)
		tag := assertAsset(t, asset, resp)
		// The browser holds the file under no-cache and asks again on every use,
		// so the same request carrying that tag is what a second page load sends.
		again := send(t, client, http.MethodGet, gatewayOrigin+asset, http.Header{"If-None-Match": {tag}}, nil)
		assertNotModified(t, asset, again, tag)
	}
	if n := auditRecords(currentLogs(t, h, ns, pod)); n != recordsBefore {
		t.Errorf("the shell and asset requests grew the audit log from %d to %d records; /ui/ writes none", recordsBefore, n)
	}

	// A fetch from the page without a session is refused, never redirected:
	// the page reads the 401 and starts the login itself.
	fetched := send(t, client, http.MethodGet, gatewayOrigin+"/v1/whoami", fetchHeaders(), nil)
	if fetched.Status != http.StatusUnauthorized || fetched.Header.Get("WWW-Authenticate") != `Bearer realm="profgate"` {
		t.Fatalf("fetch-shaped GET /v1/whoami without a cookie: status %d, WWW-Authenticate %q, want 401 with the bearer challenge",
			fetched.Status, fetched.Header.Get("WWW-Authenticate"))
	}

	// The login the page starts returns to the page's own path and query.
	pageLogin := gatewayOrigin + "/auth/login?return=" + url.QueryEscape(consoleReturn)
	login = send(t, client, http.MethodGet, pageLogin, navigationHeaders(), nil)
	if login.Status != http.StatusFound {
		t.Fatalf("GET %s: status %d, want 302: %s", pageLogin, login.Status, login.Body)
	}
	authorize = location(t, login, pageLogin)
	form = walkIssuer(t, client, authorize.String())
	action = formAction(t, dex.issuer, form)
	submitted = send(t, client, http.MethodPost, action, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		strings.NewReader(credentials.Encode()))
	callback = followToGateway(t, client, submitted, action)
	cb = send(t, client, http.MethodGet, callback.String(), cbHeaders, nil)
	if cb.Status != http.StatusOK || !strings.HasPrefix(cb.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("callback: status %d, Content-Type %q, want 200 text/html: %s", cb.Status, cb.Header.Get("Content-Type"), cb.Body)
	}
	escapedReturn := html.EscapeString(consoleReturn)
	for _, want := range []string{`content="0;url=` + escapedReturn + `"`, `href="` + escapedReturn + `"`} {
		if !strings.Contains(string(cb.Body), want) {
			t.Fatalf("the landing page does not carry %s:\n%s", want, cb.Body)
		}
	}
	if names := cookieNames(t, client, gatewayOrigin); len(names) != 1 || names[0] != sessionCookie {
		t.Fatalf("after the console login the jar holds %v, want exactly %s", names, sessionCookie)
	}

	// The shell has no authentication step, so the navigation the landing page
	// starts is answered whatever site the browser reports.
	crossHeaders := navigationHeaders()
	crossHeaders.Set("Sec-Fetch-Site", "cross-site")
	if resp := send(t, client, http.MethodGet, gatewayOrigin+consoleReturn, crossHeaders, nil); resp.Status != http.StatusOK {
		t.Fatalf("GET %s with the cookie: status %d, want 200: %s", consoleReturn, resp.Status, resp.Body)
	}

	// The page's fetches carry the cookie and are same-origin; each listing
	// route answers, filtered by the realm, and none names an address.
	listing := func(path string) []byte {
		t.Helper()
		resp := send(t, client, http.MethodGet, gatewayOrigin+path, fetchHeaders(), nil)
		if resp.Status != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("GET %s with the cookie: status %d, Content-Type %q, want 200 application/json: %s",
				path, resp.Status, resp.Header.Get("Content-Type"), resp.Body)
		}
		if m := ipv4Re.Find(resp.Body); m != nil {
			t.Fatalf("GET %s: the body names the address %s:\n%s", path, m, resp.Body)
		}

		return resp.Body
	}
	var whoami struct {
		Principal string `json:"principal"`
		Realm     struct {
			Name string `json:"name"`
		} `json:"realm"`
		Auth struct {
			Mode   string `json:"mode"`
			Logout string `json:"logout"`
		} `json:"auth"`
	}
	decode(t, "/v1/whoami", listing("/v1/whoami"), &whoami)
	if whoami.Principal != dexUser || whoami.Realm.Name != "developer" || whoami.Auth.Mode != "oidc" || whoami.Auth.Logout != "/auth/logout" {
		t.Fatalf("whoami: %+v, want principal %s in realm developer under oidc with logout /auth/logout", whoami, dexUser)
	}
	var limits struct {
		CPUSeconds int `json:"cpuSeconds"`
	}
	decode(t, "/v1/limits", listing("/v1/limits"), &limits)
	if limits.CPUSeconds != 60 {
		t.Fatalf("limits.cpuSeconds is %d, want 60", limits.CPUSeconds)
	}
	assertNamespaces(t, listing("/v1/namespaces"), ns)
	var services struct {
		Namespace string   `json:"namespace"`
		Services  []string `json:"services"`
	}
	decode(t, "services", listing("/v1/namespaces/"+ns+"/services"), &services)
	if services.Namespace != ns || !slices.Contains(services.Services, testAppName) {
		t.Fatalf("services of %s: %+v, want %s among them", ns, services, testAppName)
	}

	// Logout from the console lands on a signed-out console: / is the
	// fallback when the issuer publishes no end_session_endpoint, and / is
	// the console's own redirect.
	logout = send(t, client, http.MethodGet, gatewayOrigin+"/auth/logout", navigationHeaders(), nil)
	if logout.Status != http.StatusFound || logout.Header.Get("Location") != "/" {
		t.Fatalf("logout from the console: status %d, Location %q, want 302 to /", logout.Status, logout.Header.Get("Location"))
	}
	if names := cookieNames(t, client, gatewayOrigin); len(names) != 0 {
		t.Fatalf("after logout the jar holds %v, want nothing", names)
	}
	root := send(t, client, http.MethodGet, gatewayOrigin+"/", navigationHeaders(), nil)
	if root.Status != http.StatusFound || root.Header.Get("Location") != uiPath {
		t.Fatalf("GET /: status %d, Location %q, want 302 to %s", root.Status, root.Header.Get("Location"), uiPath)
	}

	// Audit: the redirect, the refusal, the login, and each listing route are
	// attributable; the Service list names its namespace.
	// The second logout is the last request that writes a record,
	// and the four listing records this counts were written before it.
	logs = logsWithAudit(t, h, ns, pod, logoutRoute, 2)
	for _, want := range []string{`"code":"auth_redirect"`, `"auth_reason":"csrf"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the gateway log has no record with %s", want)
		}
	}
	var callbackLogged, listings, serviceLists int
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"principal":"`+dexUser+`"`) {
			continue
		}
		switch {
		case strings.Contains(line, `"route":"auth_callback"`):
			callbackLogged++
		case strings.Contains(line, `"route":"`):
		case strings.Contains(line, `"profile":""`):
			// A listing route: the interactive shape with no target selected.
			listings++
			if strings.Contains(line, `"namespace":"`+ns+`"`) {
				serviceLists++
			}
		}
	}
	if callbackLogged == 0 {
		t.Errorf("the gateway log has no auth_callback record naming %s", dexUser)
	}
	if listings != 4 || serviceLists != 1 {
		t.Errorf("the gateway log has %d listing records naming %s, %d of them with namespace %s; want 4 and 1:\n%s",
			listings, dexUser, serviceLists, ns, logs)
	}

	clientOIDCSteps(t, h, local, gwCA, dexCA, dex, client, password)
}

// walkIssuer follows the issuer's redirects from rawURL to the page that holds a form and returns that page.
func walkIssuer(t *testing.T, c *http.Client, rawURL string) string {
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
	t.Fatalf("the issuer redirected %d times without serving a page", maxHops)

	return ""
}

// formAction reads the action of the first form on an issuer's page,
// resolved against the issuer.
func formAction(t *testing.T, issuer, page string) string {
	t.Helper()
	m := regexp.MustCompile(`<form[^>]*\saction="([^"]*)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the issuer served a page without a form:\n%s", page)
	}
	base, err := url.Parse(issuer + "/")
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
// a userinfo URL, a users-file rotation the gateway follows without the Pod
// being replaced, and the console's shell and listing routes under the
// challenge a browser's dialog reacts to.
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
	cfg := gatewayConfig(gatewayConfigOptions{TLSMount: tlsMountPath, AuthBlock: basicAuthBlock(hash), UIEnabled: true})
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

	// The console: the shell needs no credential, the listing routes answer
	// the challenge the browser's dialog reacts to, and with a credential they
	// answer the realm's view and the configured limits.
	shell := send(t, client, http.MethodGet, gatewayOrigin+uiPath, navigationHeaders(), nil)
	assertShell(t, "GET /ui/ without a credential", shell)
	namespacesURL := gatewayOrigin + "/v1/namespaces"
	challenge := send(t, client, http.MethodGet, namespacesURL, fetchHeaders(), nil)
	if challenge.Status != http.StatusUnauthorized || challenge.Header.Get("WWW-Authenticate") != `Basic realm="profgate"` {
		t.Fatalf("GET /v1/namespaces without a credential: status %d, WWW-Authenticate %q, want 401 with the basic challenge",
			challenge.Status, challenge.Header.Get("WWW-Authenticate"))
	}
	asAlice := func(path string) []byte {
		t.Helper()
		headers := basic("alice", password)
		for k, vs := range fetchHeaders() {
			headers[k] = vs
		}
		resp := send(t, client, http.MethodGet, gatewayOrigin+path, headers, nil)
		if resp.Status != http.StatusOK {
			t.Fatalf("GET %s as alice: status %d, want 200: %s", path, resp.Status, resp.Body)
		}

		return resp.Body
	}
	assertNamespaces(t, asAlice("/v1/namespaces"), ns)
	var limits struct {
		CPUSeconds   int `json:"cpuSeconds"`
		TraceSeconds int `json:"traceSeconds"`
		Pprof        struct {
			Default           json.RawMessage   `json:"default"`
			AllowedSelections []json.RawMessage `json:"allowedSelections"`
		} `json:"pprof"`
		PGO struct {
			Enabled bool `json:"enabled"`
		} `json:"pgo"`
	}
	body := asAlice("/v1/limits")
	decode(t, "/v1/limits", body, &limits)
	var selections []string
	for _, s := range limits.Pprof.AllowedSelections {
		selections = append(selections, string(s))
	}
	wantSelections := []string{`{"port":6061}`, `{"portName":"pprof-alt"}`}
	if limits.CPUSeconds != 60 || limits.TraceSeconds != 60 || string(limits.Pprof.Default) != `{"port":6060}` ||
		!slices.Equal(selections, wantSelections) || limits.PGO.Enabled {
		t.Fatalf("limits: %s, want cpuSeconds 60, traceSeconds 60, pprof.default {\"port\":6060}, allowedSelections %v in that order, and pgo.enabled false", body, wantSelections)
	}

	clientBasicSteps(t, h, ns, pods, local, caFile, password)
}

// clientBasicSteps runs the profgate client against the basic gateway's port-forward:
// the reading verbs and a profile fetch as alice, a login that verifies the pair and stores nothing, and the three refusals that prove the
// variables are required, the certificate is verified, and no password travels over plaintext.
func clientBasicSteps(t *testing.T, h *Harness, ns string, pods []corev1.Pod, local, caFile, password string) {
	t.Helper()
	bin := buildClient(t, h)
	home := t.TempDir()
	env := clientEnv(home, "PROFGATE_USER=alice", "PROFGATE_PASSWORD="+password)
	// The forward is dialed by address and the certificate is verified for
	// the name it was issued to.
	server := []string{"--server", "https://" + local, "--server-name", tlsHost, "--ca-file", caFile}
	client := func(what string, env []string, args ...string) (stdout, stderr string) {
		t.Helper()
		stdout, stderr, code := runClient(t, bin, env, append(args, server...)...)
		if code != 0 {
			t.Fatalf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", what, code, stdout, stderr)
		}

		return stdout, stderr
	}
	expectRow := func(what, out, key, value string) {
		t.Helper()
		if !slices.Contains(strings.Split(out, "\n"), key+"\t"+value) {
			t.Fatalf("%s: no row %q %q in:\n%s", what, key, value, out)
		}
	}
	expectLine := func(what, out, line string) {
		t.Helper()
		if !slices.Contains(strings.Split(out, "\n"), line) {
			t.Fatalf("%s: no line %q in:\n%s", what, line, out)
		}
	}

	out, _ := client("whoami", env, "whoami")
	expectRow("whoami", out, "principal", "alice")
	expectRow("whoami", out, "realm", "developer")

	out, _ = client("namespaces", env, "namespaces")
	expectLine("namespaces", out, ns)
	out, _ = client("services", env, "services", ns)
	expectLine("services", out, testAppName)

	out, _ = client("targets", env, "targets", ns+"/"+testAppName)
	for _, p := range pods {
		if !strings.Contains(out, p.Name+"\t") {
			t.Fatalf("targets: no row for %s in:\n%s", p.Name, out)
		}
	}

	profileOut := filepath.Join(home, "client-heap.pprof")
	client("profile", env, "profile", ns+"/"+testAppName, "heap", "-o", profileOut)
	fetched, err := os.ReadFile(profileOut) //nolint:gosec // the path is in the test's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ParseData(fetched); err != nil {
		t.Fatalf("the profile the client wrote does not parse: %v", err)
	}

	// login verifies the pair, prints the principal and realm as key:
	// value lines, names the two variables, and stores nothing.
	out, errOut := client("login", env, "login")
	expectLine("login", out, "principal: alice")
	expectLine("login", out, "realm: developer")
	if !strings.Contains(errOut, "PROFGATE_USER") || !strings.Contains(errOut, "PROFGATE_PASSWORD") {
		t.Fatalf("login: stderr does not name the two variables:\n%s", errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "profgate")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("login under basic left state under %s (stat error %v); a basic pair is never cached", home, err)
	}

	// Without the two variables there is no pair, and stdin is not a
	// terminal to prompt on.
	_, errOut, code := runClient(t, bin, clientEnv(home), append([]string{"login"}, server...)...)
	if code != exitUsage {
		t.Fatalf("login without the variables: exit %d, want %d:\n%s", code, exitUsage, errOut)
	}

	// A certificate authority that did not issue the gateway's leaf is a TLS failure,
	// which is what proves the client verifies the certificate.
	other := newAuthority(t, tlsHost)
	otherCA := filepath.Join(home, "other-ca.pem")
	if err := os.WriteFile(otherCA, other.caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = runClient(t, bin, env, "login", "--server", "https://"+local, "--server-name", tlsHost, "--ca-file", otherCA)
	if code != exitRefused || !strings.Contains(errOut, "certificate") {
		t.Fatalf("login with another authority: exit %d, want %d on a certificate failure:\n%s", code, exitRefused, errOut)
	}

	// The password travels only over https://: the client refuses the
	// plaintext address before it builds the request, so the name is never dialed and nothing is written.
	_, port, err := net.SplitHostPort(local)
	if err != nil {
		t.Fatal(err)
	}
	plaintextOut := filepath.Join(home, "plaintext.pprof")
	_, errOut, code = runClient(t, bin, env, "profile", ns+"/"+testAppName, "heap", "-o", plaintextOut,
		"--server", "http://"+net.JoinHostPort(tlsHost, port))
	if code != exitUsage || !strings.Contains(errOut, "refusing to send a credential") {
		t.Fatalf("profile over http://: exit %d, want %d refusing the credential:\n%s", code, exitUsage, errOut)
	}
	if _, err := os.Stat(plaintextOut); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile over http:// left %s (stat error %v)", plaintextOut, err)
	}
}

const (
	// exitRefused and exitUsage are the client's exit codes for a refusal
	// or transport failure and for a usage error.
	exitRefused = 1
	exitUsage   = 2
)

// buildClient builds the profgate binary under the e2e Go build tag,
// which compiles in the test-only seams the client scenarios drive, into the
// test's temporary directory and returns its path.
// The gateway image is built without it: ko's --tags names an image tag,
// not a Go build tag.
func buildClient(t *testing.T, h *Harness) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "profgate")
	if err := h.run(t.Context(), nil, "go", "build", "-tags", "e2e", "-o", bin, "./cmd/profgate"); err != nil {
		t.Fatalf("build the client: %v", err)
	}

	return bin
}

// clientEnv is the environment one client invocation runs with: a PATH, a
// home and the two XDG directories under home, so no invocation reads the
// developer's contexts file or token cache, and extra on top.
func clientEnv(home string, extra ...string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
	}

	return append(env, extra...)
}

// runClient runs the client binary with exactly env, stdin closed, and returns its stdout, stderr, and exit code.
func runClient(t *testing.T, bin string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // the test built the binary and composes its arguments
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("run %s %v: %v", bin, args, err)
	}
	t.Logf("profgate %s: exit %d", strings.Join(args, " "), code)

	return out.String(), errOut.String(), code
}

const (
	// clientContext is the context name the oidc lane logs in under; the
	// login creates the entry, and the cache entry carries the same name.
	clientContext = "e2e"
	// cacheDir and contextsFile are where the client keeps its state under the two XDG directories clientEnv points into home.
	cacheDir     = "state/profgate/tokens"
	contextsFile = "config/profgate/config.yaml"
)

// clientOIDCSteps runs the profgate client against the oidc gateway's
// port-forward: /v1/auth reports the device login, a login completes once
// the lane approves the user code at Dex, whoami answers from the cache,
// the cache and context files carry what the login recorded, logout removes the cache,
// and a login polling with a verifier that does not match its challenge is refused by Dex,
// which is what proves PKCE is enforced.
func clientOIDCSteps(t *testing.T, h *Harness, local string, gwCA, dexCA authority, dex *dexServer, browser *http.Client, password string) {
	t.Helper()

	// /v1/auth needs no credential and names the device login.
	var info struct {
		Mode string `json:"mode"`
		OIDC struct {
			Issuer    string   `json:"issuer"`
			ClientID  string   `json:"clientID"`
			TokenType string   `json:"tokenType"`
			Scopes    []string `json:"scopes"`
			PKCE      bool     `json:"pkce"`
		} `json:"oidc"`
	}
	resp := send(t, browser, http.MethodGet, gatewayOrigin+"/v1/auth", nil, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET /v1/auth: status %d, want 200: %s", resp.Status, resp.Body)
	}
	decode(t, "/v1/auth", resp.Body, &info)
	if info.Mode != "oidc" || info.OIDC.Issuer != dex.issuer || info.OIDC.ClientID != dexClientID ||
		info.OIDC.TokenType != "id" || !info.OIDC.PKCE {
		t.Fatalf("/v1/auth: %s, want mode oidc, issuer %s, clientID %s, tokenType id, and pkce true", resp.Body, dex.issuer, dexClientID)
	}

	bin := buildClient(t, h)
	home := t.TempDir()
	gwCAFile := filepath.Join(home, "gateway-ca.pem")
	dexCAFile := filepath.Join(home, "issuer-ca.pem")
	for path, pem := range map[string][]byte{gwCAFile: gwCA.caPEM, dexCAFile: dexCA.caPEM} {
		if err := os.WriteFile(path, pem, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := clientEnv(home)
	// The context names the entry the login creates;
	// the forward is dialed by address and the certificate is verified for the name it was issued to;
	// the issuer is trusted through its own authority.
	server := []string{"--context", clientContext, "--server", "https://" + local, "--server-name", tlsHost,
		"--ca-file", gwCAFile, "--issuer-ca-file", dexCAFile}
	cacheFile := filepath.Join(home, cacheDir, clientContext+".json")

	// The login prints the user code, the verification URI, and the complete
	// URI on stderr, in that order, and polls until the lane approves the code.
	run := startClient(t, bin, env, append([]string{"login"}, server...)...)
	code, verification, complete := deviceLines(t, run, dex.issuer)
	approveDevice(t, browser, dex.issuer, complete, code, password)
	stdout, stderr, exit := run.wait(t)
	if exit != 0 {
		t.Fatalf("login: exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	t.Logf("login: the user code was approved at %s", verification)
	for _, line := range []string{"principal: " + dexUser, "realm: developer"} {
		if !slices.Contains(strings.Split(stdout, "\n"), line) {
			t.Fatalf("login: no line %q in:\n%s", line, stdout)
		}
	}

	// whoami carries no credential of its own: the cached token answers.
	stdout, stderr, exit = runClient(t, bin, env, append([]string{"whoami"}, server...)...)
	if exit != 0 {
		t.Fatalf("whoami after login: exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	for _, row := range []string{"principal\t" + dexUser, "realm\tdeveloper"} {
		if !slices.Contains(strings.Split(stdout, "\n"), row) {
			t.Fatalf("whoami after login: no row %q in:\n%s", row, stdout)
		}
	}

	// The cache entry is private, bound to the gateway's origin, and holds the refresh token offline_access asked for.
	var entry struct {
		Origin       string `json:"origin"`
		Issuer       string `json:"issuer"`
		RefreshToken string `json:"refreshToken"`
	}
	decode(t, cacheFile, readPrivate(t, cacheFile), &entry)
	if entry.Origin != "https://"+local || entry.Issuer != dex.issuer || entry.RefreshToken == "" {
		t.Fatalf("cache entry: origin %q, issuer %q, refresh token of %d bytes; want origin https://%s, issuer %s, and a refresh token",
			entry.Origin, entry.Issuer, len(entry.RefreshToken), local, dex.issuer)
	}

	// The contexts file is private and its entry carries the snapshot
	// /v1/auth reported.
	var file struct {
		Contexts map[string]struct {
			Server string `yaml:"server"`
			Auth   struct {
				Mode      string   `yaml:"mode"`
				Issuer    string   `yaml:"issuer"`
				ClientID  string   `yaml:"clientID"`
				TokenType string   `yaml:"tokenType"`
				Scopes    []string `yaml:"scopes"`
				PKCE      bool     `yaml:"pkce"`
			} `yaml:"auth"`
		} `yaml:"contexts"`
	}
	contextsPath := filepath.Join(home, contextsFile)
	if err := yaml.Unmarshal(readPrivate(t, contextsPath), &file); err != nil {
		t.Fatalf("%s: %v", contextsPath, err)
	}
	c, ok := file.Contexts[clientContext]
	if !ok {
		t.Fatalf("%s names no context %s: %+v", contextsPath, clientContext, file)
	}
	if c.Server != "https://"+local || c.Auth.Mode != "oidc" || c.Auth.Issuer != dex.issuer || c.Auth.ClientID != dexClientID ||
		c.Auth.TokenType != "id" || !slices.Equal(c.Auth.Scopes, info.OIDC.Scopes) || !c.Auth.PKCE {
		t.Fatalf("context %s: %+v, want server https://%s and the snapshot /v1/auth reported: %+v", clientContext, c, local, info.OIDC)
	}

	// logout removes the entry.
	stdout, stderr, exit = runClient(t, bin, env, append([]string{"logout"}, server...)...)
	if exit != 0 {
		t.Fatalf("logout: exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	if _, err := os.Stat(cacheFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after logout %s remains (stat error %v)", cacheFile, err)
	}

	// PKCE is proven negatively: the device request carries the challenge of
	// the verifier the client generated, the poll sends another valid verifier,
	// and Dex refuses the approved code with invalid_grant,
	// the value its device token handler sends for a wrong code_verifier.
	// The code is approved first, because a refusal of a pending code would prove only that it was pending.
	mismatched := append(clientEnv(home), "PROFGATE_E2E_PKCE_VERIFIER_OVERRIDE="+otherVerifier(t))
	run = startClient(t, bin, mismatched, append([]string{"login"}, server...)...)
	code, _, complete = deviceLines(t, run, dex.issuer)
	approveDevice(t, browser, dex.issuer, complete, code, password)
	stdout, stderr, exit = run.wait(t)
	if exit != exitRefused || !strings.Contains(stderr, "the issuer answered invalid_grant") {
		t.Fatalf("login with a mismatched verifier: exit %d, want %d printing the issuer's invalid_grant\nstdout:\n%s\nstderr:\n%s",
			exit, exitRefused, stdout, stderr)
	}
	if _, err := os.Stat(cacheFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("login with a mismatched verifier wrote %s (stat error %v)", cacheFile, err)
	}
}

// deviceLines waits for the three lines login prints before it polls and returns the user code, the verification URI, and the complete URI, checking their order and that both URIs are the issuer's.
func deviceLines(t *testing.T, run *clientRun, issuer string) (code, verification, complete string) {
	t.Helper()
	var lines []string
	err := poll(t.Context(), settleDeadline, func(context.Context) (bool, error) {
		if run.done() {
			return false, fmt.Errorf("login exited before printing the code:\n%s", run.stderr.String())
		}
		lines = strings.Split(strings.TrimRight(run.stderr.String(), "\n"), "\n")

		return len(lines) >= 3, nil
	})
	if err != nil {
		t.Fatalf("login never printed the user code: %v\nstderr:\n%s", err, run.stderr.String())
	}
	code = strings.TrimPrefix(lines[0], "Enter the code ")
	verification = strings.TrimPrefix(lines[1], "at ")
	complete = strings.TrimPrefix(lines[2], "or open ")
	if code == lines[0] || verification == lines[1] || complete == lines[2] || code == "" {
		t.Fatalf("login: stderr is not the code, the verification URI, and the complete URI in that order:\n%s", run.stderr.String())
	}
	if !strings.HasPrefix(verification, issuer+"/") || !strings.HasPrefix(complete, issuer+"/") || !strings.Contains(complete, code) {
		t.Fatalf("login: the URIs %s and %s are not the issuer's, or the complete one does not carry the code %s", verification, complete, code)
	}

	return code, verification, complete
}

// approveDevice approves a user code at Dex the way a browser would: the
// complete verification URI is opened, its form is submitted with the code,
// the password form that follows is submitted, and the redirects are followed to the page that confirms the device.
// The client and the headers are the ones the browser walk sends to Dex's password form.
func approveDevice(t *testing.T, c *http.Client, issuer, complete, code, password string) {
	t.Helper()
	formHeaders := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	page := walkIssuer(t, c, complete)
	if !strings.Contains(page, `name="user_code"`) {
		t.Fatalf("GET %s did not serve the user code form:\n%s", complete, page)
	}
	action := formAction(t, issuer, page)
	submitted := send(t, c, http.MethodPost, action, formHeaders, strings.NewReader(url.Values{"user_code": {code}}.Encode()))
	login := followIssuer(t, c, submitted, action)
	action = formAction(t, issuer, login)
	credentials := url.Values{"login": {dexUser}, "password": {password}}
	submitted = send(t, c, http.MethodPost, action, formHeaders, strings.NewReader(credentials.Encode()))
	done := followIssuer(t, c, submitted, action)
	if strings.Contains(done, `name="password"`) || strings.Contains(done, `name="user_code"`) {
		t.Fatalf("Dex served a form again instead of confirming the device:\n%s", done)
	}
}

// followIssuer follows the redirects after a form was submitted until the
// issuer serves a page, and returns that page.
func followIssuer(t *testing.T, c *http.Client, resp response, from string) string {
	t.Helper()
	current := from
	for range maxHops {
		switch {
		case resp.Status == http.StatusOK:
			return string(resp.Body)
		case resp.Status/100 == 3:
			current = location(t, resp, current).String()
			resp = send(t, c, http.MethodGet, current, navigationHeaders(), nil)
		default:
			t.Fatalf("%s: status %d, want 200 or a redirect: %s", current, resp.Status, resp.Body)
		}
	}
	t.Fatalf("the issuer redirected %d times without serving a page", maxHops)

	return ""
}

// otherVerifier is a valid PKCE verifier no device request carried the
// challenge of.
func otherVerifier(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}

	return base64.RawURLEncoding.EncodeToString(b)
}

const (
	// keycloakName is the Deployment, Service, and label value in
	// keycloak.yaml; keycloakNodePort is the Service's NodePort, published for the same reason as Dex's.
	keycloakName     = "keycloak"
	keycloakNodePort = "30443"
	keycloakManifest = "test/e2e/keycloak.yaml"
	// keycloakConfigMap holds the realm to import and the hostname;
	// keycloakTLSSecret its certificate.
	keycloakConfigMap = "keycloak-config"
	keycloakTLSSecret = "keycloak-tls" //nolint:gosec // the Secret's name, not its contents
	// keycloakRealmFile is the realm export the scenario imports, the one docs/authentication.md records as verified by hand;
	// keycloakRealm, keycloakUser, keycloakPassword, and keycloakGroup are what it holds.
	keycloakRealmFile = "docs/keycloak-realm.json"
	keycloakRealm     = "profgate"
	keycloakUser      = "alice"
	keycloakPassword  = "secret"
	keycloakGroup     = "engineering"
	// keycloakTokenLifespan is the client's access.token.lifespan in the
	// lane's copy of the realm, in seconds: Keycloak issues its ID token for
	// the same span, so a login is stale within the scenario and the refresh is observed.
	// The committed export keeps Keycloak's default.
	keycloakTokenLifespan = 60
	// keycloakRefreshDeadline bounds the wait for that ID token to expire.
	keycloakRefreshDeadline = 2 * keycloakTokenLifespan * time.Second
)

// keycloakServer is a running Keycloak Deployment, reached at its NodePort by
// the gateway and by the host alike.
type keycloakServer struct {
	h   *Harness
	ns  string
	pod string

	addr   string // <node IP>:<keycloakNodePort>
	issuer string // https://<addr>/realms/<keycloakRealm>
}

// keycloakRealmJSON is the committed realm export with the client's token lifespan shortened to keycloakTokenLifespan.
func keycloakRealmJSON(t *testing.T, h *Harness) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.root, keycloakRealmFile))
	if err != nil {
		t.Fatal(err)
	}
	var realm map[string]json.RawMessage
	decode(t, keycloakRealmFile, raw, &realm)
	var clients []map[string]json.RawMessage
	decode(t, keycloakRealmFile, realm["clients"], &clients)
	if len(clients) != 1 {
		t.Fatalf("%s names %d clients, want the one public client", keycloakRealmFile, len(clients))
	}
	var attrs map[string]string
	decode(t, keycloakRealmFile, clients[0]["attributes"], &attrs)
	if attrs["oauth2.device.authorization.grant.enabled"] != "true" {
		t.Fatalf("%s does not enable the device grant on its client: %v", keycloakRealmFile, attrs)
	}
	attrs["access.token.lifespan"] = strconv.Itoa(keycloakTokenLifespan)
	marshal := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}

		return b
	}
	clients[0]["attributes"] = marshal(attrs)
	realm["clients"] = marshal(clients)

	return string(marshal(realm))
}

// deployKeycloak writes Keycloak's realm, hostname, and certificate, applies keycloak.yaml into ns, and waits for the rollout.
// nodeIP is the address the issuer is published at, which ca must certify.
func (h *Harness) deployKeycloak(ctx context.Context, ns string, ca authority, nodeIP, realmJSON string) (*keycloakServer, error) {
	if err := h.applyNamedTLSSecret(ctx, ns, keycloakTLSSecret, ca.certPEM, ca.keyPEM); err != nil {
		return nil, err
	}
	k := &keycloakServer{h: h, ns: ns, addr: net.JoinHostPort(nodeIP, keycloakNodePort)}
	k.issuer = "https://" + k.addr + "/realms/" + keycloakRealm
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: keycloakConfigMap, Namespace: ns},
		Data:       map[string]string{"realm.json": realmJSON, "hostname": "https://" + k.addr},
	}
	if err := h.applyConfigMap(ctx, ns, cm); err != nil {
		return nil, err
	}
	if err := h.kubectl(ctx, "apply", "-n", ns, "-f", keycloakManifest); err != nil {
		return nil, err
	}
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+keycloakName, "-n", ns,
		"--timeout="+rolloutTimeout.String()); err != nil {
		_ = h.kubectl(ctx, "describe", "pods", "-n", ns, "-l", "app.kubernetes.io/name="+keycloakName)
		_ = h.kubectl(ctx, "logs", "-n", ns, "-l", "app.kubernetes.io/name="+keycloakName, "--tail=50")

		return nil, err
	}
	pod, err := h.waitOnePod(ctx, ns, "app.kubernetes.io/name="+keycloakName)
	if err != nil {
		return nil, err
	}
	k.pod = pod
	h.log.Info("keycloak", "namespace", ns, "pod", pod, "issuer", k.issuer)

	return k, nil
}

// keycloakAuthBlock is the Keycloak gateway's auth block: Keycloak's realm as
// the issuer behind the mounted CA, preferred_username as the principal, the
// engineering group mapped to the developer realm, and the cli block with a
// PKCE challenge.
// The scope list is openid alone: Keycloak issues a refresh token bound to
// the SSO session without offline_access, and with it would issue an offline
// token instead, which needs the offline_access realm role on the user and carries no expiry.
func keycloakAuthBlock(issuer string) string {
	return fmt.Sprintf(`auth:
  mode: oidc
  oidc:
    issuer: %s
    audience: %s
    usernameClaim: preferred_username
    caFile: %s/%s
    mapping:
      groups:
        - name: %s
          realm: developer
    cli:
      scopes: [openid]
      pkce: true
`, issuer, dexClientID, authMountPath, issuerCAKey, keycloakGroup)
}

// scenarioAuthOIDCKeycloak proves the device login against Keycloak, the
// issuer docs/keycloak-realm.json was verified against: the imported realm
// publishes the device endpoint, /v1/auth reports the login, a login
// completes once the lane approves the user code through Keycloak's login
// and grant forms, whoami answers from the cache, the cache carries the
// refresh token and its expiry, a profile is fetched after the ID token
// expired because the refresh ran, logout revokes the refresh token, and a
// login polling with a verifier that does not match its challenge is refused.
func scenarioAuthOIDCKeycloak(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	deployTestApp(t, h, ns)
	ctx := t.Context()

	nodeIP, err := h.nodeIP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kcCA := newAuthority(t, nodeIP)
	kc, err := h.deployKeycloak(ctx, ns, kcCA, nodeIP, keycloakRealmJSON(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.applyAuthSecret(ctx, ns, map[string][]byte{issuerCAKey: kcCA.caPEM}); err != nil {
		t.Fatal(err)
	}
	gwCA := newAuthority(t, tlsHost)
	cfg := gatewayConfig(gatewayConfigOptions{TLSMount: tlsMountPath, AuthBlock: keycloakAuthBlock(kc.issuer)})
	local, _ := deployHTTPSGateway(t, h, ns, "oidc-gateway", oidcGatewayName, gwCA, cfg)

	pool := x509.NewCertPool()
	pool.AddCert(gwCA.ca)
	pool.AddCert(kcCA.ca)
	browser := authClient(local, kc.addr, pool)

	// The imported realm publishes the device authorization endpoint.
	var doc struct {
		Issuer         string `json:"issuer"`
		DeviceEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint  string `json:"token_endpoint"`
	}
	discovery := send(t, browser, http.MethodGet, kc.issuer+"/.well-known/openid-configuration", nil, nil)
	if discovery.Status != http.StatusOK {
		t.Fatalf("discovery: status %d: %s", discovery.Status, discovery.Body)
	}
	decode(t, "discovery", discovery.Body, &doc)
	if doc.Issuer != kc.issuer || doc.DeviceEndpoint == "" {
		t.Fatalf("discovery: %s, want issuer %s and a device_authorization_endpoint", discovery.Body, kc.issuer)
	}

	// /v1/auth needs no credential and names the device login.
	// The first request is polled, as the TLS gateway polls its first one:
	// the port-forward is open before the connection through it is usable.
	var info struct {
		Mode string `json:"mode"`
		OIDC struct {
			Issuer    string `json:"issuer"`
			ClientID  string `json:"clientID"`
			TokenType string `json:"tokenType"`
			PKCE      bool   `json:"pkce"`
		} `json:"oidc"`
	}
	var resp response
	err = poll(ctx, settleDeadline, func(ctx context.Context) (bool, error) {
		r, err := try(ctx, browser, http.MethodGet, gatewayOrigin+"/v1/auth", nil, nil)
		if err != nil {
			return false, nil //nolint:nilerr // the forward settles; the poll bounds the wait
		}
		resp = r

		return true, nil
	})
	if err != nil {
		t.Fatalf("the gateway never answered over HTTPS: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("GET /v1/auth: status %d, want 200: %s", resp.Status, resp.Body)
	}
	decode(t, "/v1/auth", resp.Body, &info)
	if info.Mode != "oidc" || info.OIDC.Issuer != kc.issuer || info.OIDC.ClientID != dexClientID ||
		info.OIDC.TokenType != "id" || !info.OIDC.PKCE {
		t.Fatalf("/v1/auth: %s, want mode oidc, issuer %s, clientID %s, tokenType id, and pkce true", resp.Body, kc.issuer, dexClientID)
	}

	bin := buildClient(t, h)
	home := t.TempDir()
	gwCAFile := filepath.Join(home, "gateway-ca.pem")
	kcCAFile := filepath.Join(home, "issuer-ca.pem")
	for path, pem := range map[string][]byte{gwCAFile: gwCA.caPEM, kcCAFile: kcCA.caPEM} {
		if err := os.WriteFile(path, pem, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := clientEnv(home)
	server := []string{"--context", clientContext, "--server", "https://" + local, "--server-name", tlsHost,
		"--ca-file", gwCAFile, "--issuer-ca-file", kcCAFile}
	cacheFile := filepath.Join(home, cacheDir, clientContext+".json")

	// The login polls until the lane approves the code at Keycloak.
	run := startClient(t, bin, env, append([]string{"login"}, server...)...)
	code, verification, complete := deviceLines(t, run, kc.issuer)
	approveKeycloakDevice(t, browser, kc.issuer, complete)
	stdout, stderr, exit := run.wait(t)
	if exit != 0 {
		t.Fatalf("login: exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	t.Logf("login: the user code %s was approved at %s", code, verification)
	for _, line := range []string{"principal: " + keycloakUser, "realm: developer"} {
		if !slices.Contains(strings.Split(stdout, "\n"), line) {
			t.Fatalf("login: no line %q in:\n%s", line, stdout)
		}
	}

	// whoami carries no credential of its own: the cached token answers,
	// and the group mapping names the realm.
	stdout, stderr, exit = runClient(t, bin, env, append([]string{"whoami"}, server...)...)
	if exit != 0 {
		t.Fatalf("whoami after login: exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	for _, row := range []string{"principal\t" + keycloakUser, "realm\tdeveloper"} {
		if !slices.Contains(strings.Split(stdout, "\n"), row) {
			t.Fatalf("whoami after login: no row %q in:\n%s", row, stdout)
		}
	}

	// The cache entry is private, bound to the gateway's origin, and holds
	// the refresh token with the expiry Keycloak's refresh_expires_in gave it.
	type cacheEntry struct {
		Origin           string    `json:"origin"`
		Issuer           string    `json:"issuer"`
		Token            string    `json:"token"`
		ExpiresAt        time.Time `json:"expiresAt"`
		RefreshToken     string    `json:"refreshToken"`
		RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
	}
	var entry cacheEntry
	decode(t, cacheFile, readPrivate(t, cacheFile), &entry)
	if entry.Origin != "https://"+local || entry.Issuer != kc.issuer || entry.RefreshToken == "" || entry.RefreshExpiresAt.IsZero() {
		t.Fatalf("cache entry: origin %q, issuer %q, refresh token of %d bytes, refreshExpiresAt %v; want origin https://%s, issuer %s, a refresh token, and an expiry",
			entry.Origin, entry.Issuer, len(entry.RefreshToken), entry.RefreshExpiresAt, local, kc.issuer)
	}
	if lifespan := time.Until(entry.ExpiresAt); lifespan > keycloakTokenLifespan*time.Second {
		t.Fatalf("the cached token expires in %v; the realm's client issues one for %ds", lifespan.Round(time.Second), keycloakTokenLifespan)
	}

	// Once the ID token has expired, a profile is still fetched:
	// the refresh ran first and the cache holds the token it produced.
	err = poll(ctx, keycloakRefreshDeadline, func(context.Context) (bool, error) {
		return time.Now().After(entry.ExpiresAt), nil
	})
	if err != nil {
		t.Fatalf("the ID token never expired: %v", err)
	}
	profileOut := filepath.Join(home, "client-heap.pprof")
	stdout, stderr, exit = runClient(t, bin, env, append([]string{"profile", ns + "/" + testAppName, "heap", "-o", profileOut}, server...)...)
	if exit != 0 {
		t.Fatalf("profile after the token expired: exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	fetched, err := os.ReadFile(profileOut) //nolint:gosec // the path is in the test's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ParseData(fetched); err != nil {
		t.Fatalf("the profile the client wrote does not parse: %v", err)
	}
	var refreshed cacheEntry
	decode(t, cacheFile, readPrivate(t, cacheFile), &refreshed)
	if refreshed.Token == entry.Token || !refreshed.ExpiresAt.After(entry.ExpiresAt) || refreshed.RefreshToken == "" {
		t.Fatalf("after the profile the cache holds the same token, or one expiring at %v rather than after %v; the refresh did not run",
			refreshed.ExpiresAt, entry.ExpiresAt)
	}

	// logout revokes the refresh token at Keycloak and removes the entry:
	// the token the cache held no longer refreshes.
	stdout, stderr, exit = runClient(t, bin, env, append([]string{"logout"}, server...)...)
	if exit != 0 {
		t.Fatalf("logout: exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	if _, err := os.Stat(cacheFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after logout %s remains (stat error %v)", cacheFile, err)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {dexClientID}, "refresh_token": {refreshed.RefreshToken}}
	revoked := send(t, browser, http.MethodPost, doc.TokenEndpoint,
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, strings.NewReader(form.Encode()))
	if revoked.Status != http.StatusBadRequest || !strings.Contains(string(revoked.Body), `"invalid_grant"`) {
		t.Fatalf("the refresh token after logout: status %d: %s, want 400 invalid_grant", revoked.Status, revoked.Body)
	}

	// PKCE is proven negatively, as under Dex:
	// the code is approved, the poll sends another valid verifier,
	// and Keycloak refuses the approved code with invalid_grant, its value for a failed PKCE verification.
	mismatched := append(clientEnv(home), "PROFGATE_E2E_PKCE_VERIFIER_OVERRIDE="+otherVerifier(t))
	run = startClient(t, bin, mismatched, append([]string{"login"}, server...)...)
	_, _, complete = deviceLines(t, run, kc.issuer)
	approveKeycloakDevice(t, browser, kc.issuer, complete)
	stdout, stderr, exit = run.wait(t)
	if exit != exitRefused || !strings.Contains(stderr, "the issuer answered invalid_grant") {
		t.Fatalf("login with a mismatched verifier: exit %d, want %d printing the issuer's invalid_grant\nstdout:\n%s\nstderr:\n%s",
			exit, exitRefused, stdout, stderr)
	}
	if _, err := os.Stat(cacheFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("login with a mismatched verifier wrote %s (stat error %v)", cacheFile, err)
	}
}

// keycloakGrantCode matches the hidden code of Keycloak's grant form.
var keycloakGrantCode = regexp.MustCompile(`<input[^>]*name="code"[^>]*value="([^"]*)"`)

// approveKeycloakDevice approves a user code at Keycloak the way a browser would:
// the complete verification URI carries the code and lands on the login form, which is submitted with the realm's user;
// the grant form that follows, which Keycloak shows for every device login, is accepted;
// and the redirects are followed to the page that confirms the device.
// The client and the headers are the ones the browser walk sends to an
// issuer's forms.
func approveKeycloakDevice(t *testing.T, c *http.Client, issuer, complete string) {
	t.Helper()
	formHeaders := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	login := walkIssuer(t, c, complete)
	if !strings.Contains(login, `name="username"`) {
		t.Fatalf("GET %s did not land on the login form:\n%s", complete, login)
	}
	action := formAction(t, issuer, login)
	credentials := url.Values{"username": {keycloakUser}, "password": {keycloakPassword}}
	submitted := send(t, c, http.MethodPost, action, formHeaders, strings.NewReader(credentials.Encode()))
	grant := followIssuer(t, c, submitted, action)
	m := keycloakGrantCode.FindStringSubmatch(grant)
	if m == nil {
		t.Fatalf("Keycloak did not serve the grant form after the login:\n%s", grant)
	}
	action = formAction(t, issuer, grant)
	submitted = send(t, c, http.MethodPost, action, formHeaders, strings.NewReader(url.Values{"code": {m[1]}, "accept": {""}}.Encode()))
	done := followIssuer(t, c, submitted, action)
	if strings.Contains(done, `name="password"`) || strings.Contains(done, `name="code"`) {
		t.Fatalf("Keycloak served a form again instead of confirming the device:\n%s", done)
	}
}

// readPrivate reads a file the client wrote, failing when it is not 0600.
func readPrivate(t *testing.T, path string) []byte {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Fatalf("%s has mode %o, want 0600", path, mode)
	}
	b, err := os.ReadFile(path) //nolint:gosec // the path is in the test's temporary directory
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// clientRun is a client invocation that is still running: its output so far,
// and the error its exit will report.
type clientRun struct {
	cmd            *exec.Cmd
	stdout, stderr *lockedBuffer
	exited         chan error
}

// startClient starts the client binary with exactly env, stdin closed, and returns the running invocation;
// wait collects its result.
func startClient(t *testing.T, bin string, env []string, args ...string) *clientRun {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // the test built the binary and composes its arguments
	cmd.Env = env
	r := &clientRun{cmd: cmd, stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, exited: make(chan error, 1)}
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %v: %v", bin, args, err)
	}
	go func() { r.exited <- cmd.Wait() }()

	return r
}

// done reports whether the process has exited, without consuming the result.
func (r *clientRun) done() bool {
	select {
	case err := <-r.exited:
		r.exited <- err

		return true
	default:
		return false
	}
}

// wait returns the invocation's stdout, stderr, and exit code once it exits.
func (r *clientRun) wait(t *testing.T) (stdout, stderr string, code int) {
	t.Helper()
	err := <-r.exited
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("wait for %v: %v", r.cmd.Args, err)
	}
	t.Logf("profgate %s: exit %d", strings.Join(r.cmd.Args[1:], " "), code)

	return r.stdout.String(), r.stderr.String(), code
}

// lockedBuffer is a bytes.Buffer the process writes into while the test reads what it has printed so far.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}

const (
	// uiPath is the console's shell; / redirects to it while ui.enabled.
	uiPath = "/ui/"
	// consoleReturn is the path and query a login started from the page returns to.
	consoleReturn = "/ui/?ns=x"
)

// ipv4Re matches a dotted address; no listing body may carry one.
var ipv4Re = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

// assetRe matches the paths the shell references, which carry no hash segment.
var assetRe = regexp.MustCompile(`(?:href|src)="(/ui/[^"]+)"`)

// consoleHeaders returns what every response under /ui/ carries, Cache-Control
// and Content-Type aside.
func consoleHeaders() map[string]string {
	return map[string]string{
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; img-src data:; " +
			"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'",
		"X-Frame-Options":              "DENY",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
}

// fetchHeaders are what a browser sends on a fetch the page starts: same-origin,
// and never a navigation, so the gateway answers 401 rather than redirecting.
func fetchHeaders() http.Header {
	return http.Header{"Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Dest": {"empty"}}
}

// assertConsoleHeaders checks every header of the console's policy on resp.
func assertConsoleHeaders(t *testing.T, what string, resp response) {
	t.Helper()
	for name, want := range consoleHeaders() {
		if got := resp.Header.Get(name); got != want {
			t.Fatalf("%s: %s is %q, want %q", what, name, got, want)
		}
	}
}

// assertShell checks a 200 shell response: HTML, never stored, with the policy headers.
func assertShell(t *testing.T, what string, resp response) {
	t.Helper()
	if resp.Status != http.StatusOK {
		t.Fatalf("%s: status %d, want 200: %s", what, resp.Status, resp.Body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("%s: Content-Type %q, want text/html", what, ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("%s: Cache-Control %q, want no-store", what, cc)
	}
	// The shell is never stored, so it has nothing to revalidate against.
	if tag := resp.Header.Get("ETag"); tag != "" {
		t.Fatalf("%s: ETag %q, want none", what, tag)
	}
	assertConsoleHeaders(t, what, resp)
}

// tagRe matches the entity tag an asset carries: the whole SHA-256 of the file, quoted.
var tagRe = regexp.MustCompile(`^"[0-9a-f]{64}"$`)

// assertAsset checks an asset response: the type its extension names,
// revalidated on every use, with the policy headers, and returns its entity tag.
func assertAsset(t *testing.T, path string, resp response) string {
	t.Helper()
	if resp.Status != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200: %s", path, resp.Status, resp.Body)
	}
	want := "text/css; charset=utf-8"
	if strings.HasSuffix(path, ".js") {
		want = "text/javascript; charset=utf-8"
	}
	if ct := resp.Header.Get("Content-Type"); ct != want {
		t.Fatalf("GET %s: Content-Type %q, want %q", path, ct, want)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("GET %s: Cache-Control %q, want no-cache", path, cc)
	}
	tag := resp.Header.Get("ETag")
	if !tagRe.MatchString(tag) {
		t.Fatalf("GET %s: ETag %q, want the file's whole digest in quotes", path, tag)
	}
	if len(resp.Body) == 0 {
		t.Fatalf("GET %s: empty body", path)
	}
	assertConsoleHeaders(t, "GET "+path, resp)

	return tag
}

// assertNotModified checks the answer to a request repeating an asset's tag:
// the revalidation no-cache buys, which sends the bytes again only once they differ.
func assertNotModified(t *testing.T, path string, resp response, tag string) {
	t.Helper()
	if resp.Status != http.StatusNotModified {
		t.Fatalf("GET %s with If-None-Match %s: status %d, want 304: %s", path, tag, resp.Status, resp.Body)
	}
	if got := resp.Header.Get("ETag"); got != tag {
		t.Fatalf("GET %s with If-None-Match %s: ETag %q, want the same tag", path, tag, got)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("GET %s with If-None-Match %s: Cache-Control %q, want no-cache", path, tag, cc)
	}
	if len(resp.Body) != 0 {
		t.Fatalf("GET %s with If-None-Match %s: body of %d bytes, want none", path, tag, len(resp.Body))
	}
	assertConsoleHeaders(t, "GET "+path+" with If-None-Match", resp)
}

// assetPaths reads the asset paths the shell references,
// plus one vendored module the shell reaches through an import rather than a reference of its own.
// Every replica of one release serves the same paths, so they are known in advance
// and the shell is checked against them rather than read for whatever it happens to name.
func assetPaths(t *testing.T, shell []byte) []string {
	t.Helper()
	var paths []string
	for _, m := range assetRe.FindAllSubmatch(shell, -1) {
		paths = append(paths, string(m[1]))
	}
	want := []string{"/ui/app.css", "/ui/app.js"}
	if !slices.Equal(paths, want) {
		t.Fatalf("the shell references %v, want %v:\n%s", paths, want, shell)
	}

	return append(paths, "/ui/vendor/preact/preact.module.js")
}

// auditRecords counts the request records in a gateway log.
func auditRecords(logs string) int {
	return strings.Count(logs, `"msg":"request"`)
}

// decode parses body as JSON into v or fails naming what was fetched.
func decode(t *testing.T, what string, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("%s: %v:\n%s", what, err, body)
	}
}

// assertNamespaces checks that a namespace list names ns.
func assertNamespaces(t *testing.T, body []byte, ns string) {
	t.Helper()
	var list struct {
		Namespaces []string `json:"namespaces"`
	}
	decode(t, "/v1/namespaces", body, &list)
	if !slices.Contains(list.Namespaces, ns) {
		t.Fatalf("namespaces %v do not name %s", list.Namespaces, ns)
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
