// Package httpapi serves the /v1 API:
// it runs the spec's request algorithm against a Discovery, writes the targets listing and every gateway error itself,
// and hands one confirmed Target to an Upstream for the profile bytes.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/arloliu/profgate/internal/admit"
	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/pgo"
	"github.com/arloliu/profgate/internal/proxy"
)

const (
	// budgetGrace is added to the effective duration to form the overall request budget;
	// it is the whole budget for profiles without a duration.
	budgetGrace = 30 * time.Second
	// codeOK is the code of a request that was answered as asked.
	codeOK = "ok"
	// codeStreamFailed is the proxy outcome after which the connection is dropped.
	codeStreamFailed = "upstream_stream_failed"
	// labelNone is the metrics profile label when no profile applies.
	labelNone = "none"
	// labelCPU is the metrics profile label every Collection route carries:
	// a Collection profiles CPU and nothing else.
	labelCPU = "cpu"
)

var (
	// serviceRouteRE matches the four Service-scoped /v1 routes;
	// the namespace and Service segments are validated separately.
	serviceRouteRE = regexp.MustCompile(
		`^/v1/namespaces/([^/]+)/services/([^/]+)/(targets|pgo|collections|profiles/([^/]+))$`)
	// listingRouteRE matches the Service listing route; the namespace is validated as a DNS-1123 label.
	listingRouteRE = regexp.MustCompile(`^/v1/namespaces/([^/]+)/services$`)
	// collectionRouteRE matches the three Collection-scoped routes;
	// the identifier is validated against its own grammar, so a path carrying a
	// separator or a traversal segment is never read as one.
	collectionRouteRE = regexp.MustCompile(`^/v1/collections/([^/]+)(?:/(profile|cancel))?$`)
)

// Upstream is what the profile handler needs from internal/proxy.
type Upstream interface {
	Do(ctx context.Context, w http.ResponseWriter, req proxy.Request) proxy.Outcome
}

// Deps is everything the handler is built from.
type Deps struct {
	Discovery k8s.Discovery
	Upstream  Upstream
	Config    *atomic.Pointer[config.Config]
	Recorder  metrics.Recorder
	Gate      *admit.Gate
	// PGO is the late-bound handle to the PGO machinery.
	// The server starts before the NATS preflight has succeeded, so an unbound
	// runtime is the normal early state and every PGO route answers
	// 503 pgo_unavailable through it; nil means one that is never bound.
	PGO *pgo.Runtime
	// Auth resolves the principal; nil means auth.Disabled{}.
	Auth auth.Authenticator
	// AuthRoutes serves /auth/*; nil means the three routes are 404 route_unknown.
	AuthRoutes auth.AuthRoutes
	// Console serves /ui/ and /; nil means ui.enabled is false and both are 404 route_unknown.
	Console http.Handler
	// Ready gates the /v1 and /auth/ readiness steps: discovery synced and,
	// under oidc, the issuer discovered.
	// It is narrower than /readyz, which also turns 503 for the drain and for
	// a pending NATS preflight, because the drain delay exists to keep serving
	// requests already routed here, and a replica whose NATS is down still
	// serves interactive requests.
	// nil means Discovery.HasSynced alone.
	Ready  func() bool
	Logger *slog.Logger
	Choose func(n int) int // nil means math/rand/v2 IntN
}

// server is the handler's state.
type server struct {
	deps Deps
	// beforeAllowlist, when set, runs after the realm check and before the allowlist check.
	// Production leaves it nil; a test uses it to swap the configuration pointer mid-request.
	beforeAllowlist func()
}

// New builds the /v1 handler.
// Admission runs on the caller's Gate, which is shared with PGO collection and sized once at startup.
func New(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Recorder == nil {
		d.Recorder = metrics.Noop{}
	}
	if d.Choose == nil {
		d.Choose = rand.IntN
	}
	if d.PGO == nil {
		d.PGO = pgo.NewRuntime()
	}
	if d.Auth == nil {
		d.Auth = auth.Disabled{}
	}

	return &server{deps: d}
}

// routeKind is which of the /v1 routes a path matched.
type routeKind int

// The routes the gateway serves: the two interactive ones, the five PGO ones, and the four listing ones.
// Each classification below is an exhaustive switch that names every kind,
// so declaration order carries no meaning and a kind added later is classified by name.
const (
	kindTargets routeKind = iota
	kindProfile
	kindPGOPolicy
	kindCollections
	kindCollection
	kindCollectionProfile
	kindCollectionCancel
	kindNamespaces
	kindServices
	kindWhoami
	kindLimits
)

// isPGO reports whether the route reads or writes PGO state, which is what the
// pgo.enabled and replay-barrier steps of the request algorithm gate.
func (k routeKind) isPGO() bool {
	switch k {
	case kindPGOPolicy, kindCollections, kindCollection, kindCollectionProfile, kindCollectionCancel:
		return true
	case kindTargets, kindProfile, kindNamespaces, kindServices, kindWhoami, kindLimits:
		return false
	default:
		return false
	}
}

// isCollectionScoped reports whether the route names a Collection rather than a
// Service, so its namespace, Service, and realm come from the stored record.
func (k routeKind) isCollectionScoped() bool {
	switch k {
	case kindCollection, kindCollectionProfile, kindCollectionCancel:
		return true
	case kindTargets, kindProfile, kindPGOPolicy, kindCollections, kindNamespaces, kindServices, kindWhoami, kindLimits:
		return false
	default:
		return false
	}
}

// isListing reports whether the route is one of the four listing endpoints,
// which run the algorithm up to the realm step and then read the cache or the configuration.
func (k routeKind) isListing() bool {
	switch k {
	case kindNamespaces, kindServices, kindWhoami, kindLimits:
		return true
	case kindTargets, kindProfile, kindPGOPolicy, kindCollections, kindCollection, kindCollectionProfile, kindCollectionCancel:
		return false
	default:
		return false
	}
}

// methods lists what the route accepts, in the order the Allow header carries them.
func (k routeKind) methods() []string {
	switch k {
	case kindPGOPolicy:
		return []string{http.MethodGet, http.MethodPut, http.MethodDelete}
	case kindCollections:
		return []string{http.MethodGet, http.MethodPost}
	case kindCollectionCancel:
		return []string{http.MethodPost}
	case kindTargets, kindProfile, kindCollection, kindCollectionProfile,
		kindNamespaces, kindServices, kindWhoami, kindLimits:
		return []string{http.MethodGet}
	default:
		return []string{http.MethodGet}
	}
}

// route is a parsed /v1 path.
type route struct {
	kind       routeKind
	namespace  string
	service    string
	profile    string // empty for every route but the profile endpoint
	collection string // set for the three Collection-scoped routes
}

// parseRoute matches path against the eleven routes,
// validating the name segments as DNS-1123 labels and the identifier against its own grammar.
func parseRoute(path string) (route, bool) {
	switch path {
	case "/v1/namespaces":
		return route{kind: kindNamespaces}, true
	case "/v1/whoami":
		return route{kind: kindWhoami}, true
	case "/v1/limits":
		return route{kind: kindLimits}, true
	}
	if m := listingRouteRE.FindStringSubmatch(path); m != nil {
		if len(validation.IsDNS1123Label(m[1])) > 0 {
			return route{}, false
		}

		return route{kind: kindServices, namespace: m[1]}, true
	}
	if m := serviceRouteRE.FindStringSubmatch(path); m != nil {
		if len(validation.IsDNS1123Label(m[1])) > 0 || len(validation.IsDNS1123Label(m[2])) > 0 {
			return route{}, false
		}
		rt := route{namespace: m[1], service: m[2]}
		switch m[3] {
		case "targets":
			rt.kind = kindTargets
		case "pgo":
			rt.kind = kindPGOPolicy
		case "collections":
			rt.kind = kindCollections
		default:
			rt.kind = kindProfile
			rt.profile = m[4]
		}

		return rt, true
	}

	m := collectionRouteRE.FindStringSubmatch(path)
	if m == nil || !pgo.ValidID(m[1]) {
		return route{}, false
	}
	rt := route{collection: m[1]}
	switch m[2] {
	case "profile":
		rt.kind = kindCollectionProfile
	case "cancel":
		rt.kind = kindCollectionCancel
	default:
		rt.kind = kindCollection
	}

	return rt, true
}

// request is what one request accumulates for its audit record and its metrics labels.
type request struct {
	routed bool
	route  route
	// authRoute marks a request under /auth/, which has labels and an audit shape of its own.
	authRoute bool
	// console marks a request under /ui/ or to /: counted under EndpointUI, never narrated in the audit log.
	console bool
	port    portParams // the client's port selection; zero on PGO routes
	audit   auditRecord
}

// labels are the metrics endpoint and profile for this request:
// the resolved route when there is one, ("profile","none") before a route resolves
// or when the profile name is unknown.
func (q *request) labels() (metrics.Endpoint, string) {
	if q.console {
		return metrics.EndpointUI, labelNone
	}
	if q.authRoute {
		return metrics.EndpointAuth, labelNone
	}
	if !q.routed {
		return metrics.EndpointProfile, labelNone
	}
	switch q.route.kind {
	case kindTargets:
		return metrics.EndpointTargets, labelNone
	case kindPGOPolicy:
		return metrics.EndpointPGOPolicy, labelNone
	case kindCollections:
		return metrics.EndpointCollections, labelNone
	case kindCollection:
		return metrics.EndpointCollection, labelCPU
	case kindCollectionProfile:
		return metrics.EndpointCollectionProfile, labelCPU
	case kindCollectionCancel:
		return metrics.EndpointCollectionCancel, labelCPU
	case kindNamespaces:
		return metrics.EndpointNamespaces, labelNone
	case kindServices:
		return metrics.EndpointServices, labelNone
	case kindWhoami:
		return metrics.EndpointWhoami, labelNone
	case kindLimits:
		return metrics.EndpointLimits, labelNone
	case kindProfile:
		if config.IsProfile(q.route.profile) {
			return metrics.EndpointProfile, q.route.profile
		}

		return metrics.EndpointProfile, labelNone
	default:
		return metrics.EndpointProfile, labelNone
	}
}

// fail writes a gateway-generated error and records it.
func (q *request) fail(w http.ResponseWriter, e *requestError) {
	q.audit.status = e.status
	q.audit.code = e.code
	if e.auditCode != "" {
		q.audit.code = e.auditCode
	}
	writeError(w, e)
}

// ServeHTTP runs the request algorithm:
// route, method, readiness, credential placement, authentication, realm, parameters, discovery,
// filter and select, admit, confirm, proxy.
// The first failing step answers.
// The configuration is loaded once here and the request uses that snapshot throughout.
// A path under /auth/ is not a /v1 route and takes the shorter algorithm of serveAuthRoute;
// a path under /ui/ or exactly / is handed to the console, which runs its own.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cfg := s.deps.Config.Load()
	w.Header().Set("Cache-Control", "no-store")

	q := &request{}
	defer func() {
		q.audit.duration = time.Since(start)
		endpoint, profile := q.labels()
		s.deps.Recorder.Request(endpoint, profile, q.audit.code, q.audit.duration)
		// A console request is counted, not narrated: it carries no principal and names nothing a realm bounds.
		if !q.console {
			writeAudit(s.deps.Logger, q.audit)
		}
	}()

	if isConsolePath(r.URL.Path) {
		s.serveConsole(w, r, q)

		return
	}

	if strings.HasPrefix(r.URL.Path, authPrefix) {
		s.serveAuthRoute(w, r, q, cfg)

		return
	}

	rt, ok := parseRoute(r.URL.Path)
	if !ok {
		q.fail(w, &requestError{status: http.StatusNotFound, code: "route_unknown", message: "no such route"})

		return
	}
	q.routed = true
	q.route = rt
	q.audit.pgo = rt.kind.isPGO()
	q.audit.method = r.Method
	q.audit.namespace = rt.namespace
	q.audit.service = rt.service
	q.audit.profile = rt.profile
	q.audit.collection = rt.collection
	var spec profileSpec
	if rt.kind == kindProfile {
		spec, ok = lookupProfile(rt.profile)
		if !ok {
			q.fail(w, &requestError{status: http.StatusNotFound, code: "profile_unknown", message: fmt.Sprintf("unknown profile %q", rt.profile)})

			return
		}
	}

	if !slices.Contains(rt.kind.methods(), r.Method) {
		w.Header().Set("Allow", strings.Join(rt.kind.methods(), ", "))
		q.fail(w, &requestError{status: http.StatusMethodNotAllowed, code: "method_not_allowed", message: fmt.Sprintf("method %s not allowed", r.Method)})

		return
	}

	if !s.ready() {
		q.fail(w, &requestError{status: http.StatusServiceUnavailable, code: "not_ready", message: "the gateway is not ready"})

		return
	}

	// PGO collection is off, or its stores cannot be decided from yet.
	// The step sits between readiness and authentication, so a caller learns
	// the feature is absent before anything about the realm it asked through.
	var sess *pgo.Session
	if rt.kind.isPGO() {
		if !cfg.PGO.Enabled {
			q.fail(w, &requestError{status: http.StatusNotImplemented, code: "pgo_disabled", message: "pgo collection is not enabled"})

			return
		}
		var err error
		if sess, err = s.deps.PGO.Session(); err != nil {
			q.fail(w, errPGOUnavailable)

			return
		}
	}

	// A token in the URL is refused before any credential is read, even when
	// a valid one is also in the header: the URL form must never work.
	if hasAccessToken(r.URL.RawQuery) {
		q.fail(w, invalidParameter("access_token is not accepted as a query parameter"))

		return
	}

	p, realm, ok := s.authenticate(w, r, q, cfg)
	if !ok {
		return
	}
	principal := p.Name

	// A Collection-scoped route reads its record first: the realm is evaluated
	// against the record's namespace and Service, and a denied record and a
	// missing one answer alike.
	if rt.kind.isCollectionScoped() {
		s.servePGOCollection(w, r, q, cfg, sess, realm)

		return
	}

	if !realmAllows(realm, rt, r.Method) {
		// The denial names nothing: the same body whether or not the Service exists.
		q.fail(w, &requestError{status: http.StatusForbidden, code: "realm_denied", message: "access denied by realm"})

		return
	}

	if rt.kind.isListing() {
		s.serveListing(w, r, q, cfg, p, realm)

		return
	}

	if rt.kind.isPGO() {
		s.servePGOService(w, r, q, cfg, sess, principal)

		return
	}

	// Parameters, then the allowlist, then discovery: a refused port never reaches Targets.
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		q.fail(w, invalidParameter("the query string is malformed"))

		return
	}
	var (
		params profileParams
		perr   *requestError
	)
	if rt.kind == kindTargets {
		params.port, perr = parseTargetsParams(values)
	} else {
		params, perr = parseProfileParams(values, spec, cfg.Limits)
	}
	// The selection is recorded as sent even when another parameter fails;
	// a fault in the selection itself leaves it empty.
	q.port = params.port
	q.audit.port = q.port.sent
	if perr != nil {
		q.fail(w, perr)

		return
	}

	if s.beforeAllowlist != nil {
		s.beforeAllowlist()
	}
	// The allowlist reads cfg, the snapshot loaded at entry; a zero selection
	// resolves under the configured default, which is always permitted.
	if aerr := allowPort(cfg.Discovery.Pprof, q.port); aerr != nil {
		q.fail(w, aerr)

		return
	}

	if rt.kind == kindTargets {
		s.serveTargets(w, r, q)

		return
	}
	q.audit.seconds = params.seconds
	s.serveProfile(w, r, q, spec, params)
}

// allowPort evaluates the selection against the allowlists of one configuration snapshot.
func allowPort(pprof config.PprofConfig, port portParams) *requestError {
	sel := port.sel
	switch {
	case sel.Port != 0 && !pprof.AllowsPort(sel.Port):
		return portNotAllowed("port", port.sent)
	case sel.PortName != "" && !pprof.AllowsPortName(sel.PortName):
		return portNotAllowed("portName", port.sent)
	default:
		return nil
	}
}

// discover runs the discovery step and maps its errors.
func (s *server) discover(ctx context.Context, q *request) ([]k8s.Target, *requestError) {
	rt := q.route
	targets, err := s.deps.Discovery.Targets(ctx, rt.namespace, rt.service, q.port.sel)
	switch {
	case err == nil:
		return targets, nil
	case errors.Is(err, k8s.ErrServiceNotFound):
		return nil, &requestError{
			status:  http.StatusNotFound,
			code:    "service_not_found",
			message: fmt.Sprintf("service %s not found in namespace %s", rt.service, rt.namespace),
		}
	case errors.Is(err, k8s.ErrServiceSelectorless):
		return nil, &requestError{
			status:  http.StatusUnprocessableEntity,
			code:    "service_selectorless",
			message: fmt.Sprintf("service %s in namespace %s has no selector", rt.service, rt.namespace),
		}
	default:
		return nil, &requestError{
			status:  http.StatusServiceUnavailable,
			code:    "discovery_unavailable",
			message: fmt.Sprintf("discovery cannot resolve service %s in namespace %s", rt.service, rt.namespace),
		}
	}
}

// serveTargets answers the targets endpoint from the cache.
func (s *server) serveTargets(w http.ResponseWriter, r *http.Request, q *request) {
	targets, derr := s.discover(r.Context(), q)
	if derr != nil {
		q.fail(w, derr)

		return
	}
	q.audit.status = http.StatusOK
	q.audit.code = codeOK
	writeTargets(w, q.route.namespace, q.route.service, targets)
}

// serveProfile runs discovery, selection, admission, confirmation, and the proxy.
func (s *server) serveProfile(w http.ResponseWriter, r *http.Request, q *request, spec profileSpec, params profileParams) {
	targets, derr := s.discover(r.Context(), q)
	if derr != nil {
		q.fail(w, derr)

		return
	}
	target, serr := selectTarget(targets, params, s.deps.Choose)
	if serr != nil {
		q.fail(w, serr)

		return
	}
	q.audit.pod = target.Pod

	// Admission never waits: a slot is taken now or the request is refused now.
	release, ok := s.deps.Gate.TryAcquire()
	if !ok {
		q.fail(w, &requestError{status: http.StatusTooManyRequests, code: "too_many_profiles", message: "too many profiles in flight"})

		return
	}
	defer release()
	s.deps.Recorder.ProfilesInFlight(1)
	defer s.deps.Recorder.ProfilesInFlight(-1)

	// The overall budget starts here and bounds confirmation, dial, header wait, and streaming.
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(params.seconds)*time.Second+budgetGrace)
	defer cancel()

	if err := s.deps.Discovery.Confirm(ctx, target); err != nil {
		if errors.Is(err, k8s.ErrTargetChanged) {
			s.deps.Recorder.Confirm("changed")
			q.fail(w, &requestError{
				status:  http.StatusServiceUnavailable,
				code:    "target_changed",
				message: fmt.Sprintf("pod %s changed since it was selected; retry", target.Pod),
			})

			return
		}
		// Anything the gateway cannot classify is treated as the API server not vouching for the target.
		s.deps.Recorder.Confirm("unavailable")
		q.fail(w, &requestError{
			status:  http.StatusServiceUnavailable,
			code:    "discovery_unavailable",
			message: fmt.Sprintf("discovery cannot confirm pod %s", target.Pod),
		})

		return
	}
	s.deps.Recorder.Confirm("ok")

	out := s.deps.Upstream.Do(ctx, w, proxy.Request{
		Target:  target,
		Path:    spec.path,
		Seconds: params.seconds,
		TargetHeaders: map[string]string{
			"X-Pprof-Target-Pod":     target.Pod,
			"X-Pprof-Target-Node":    target.Node,
			"X-Pprof-Target-Version": target.Version,
		},
	})
	q.audit.status = out.Status
	q.audit.code = out.Code
	if !out.Committed {
		// Status 0 is a client that left before anything was written: nobody to answer.
		if out.Status != 0 {
			WriteError(w, out.Status, out.Code, upstreamMessage(out.Code, target.Pod))
		}

		return
	}
	if out.Code == codeStreamFailed {
		// The deferred audit and metrics run during the unwind; net/http then drops the connection without a stack trace,
		// so the client sees a truncation rather than a cleanly finished body.
		panic(http.ErrAbortHandler)
	}
}

// upstreamMessage is the fixed envelope text for a proxy failure before headers;
// it names the Pod and never its address.
func upstreamMessage(code, pod string) string {
	switch code {
	case "upstream_timeout":
		return fmt.Sprintf("pod %s did not answer in time", pod)
	case "upstream_redirect":
		return fmt.Sprintf("pod %s answered with a redirect", pod)
	default:
		return fmt.Sprintf("pod %s is unreachable", pod)
	}
}
