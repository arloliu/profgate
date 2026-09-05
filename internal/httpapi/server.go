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
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/arloliu/profgate/internal/admit"
	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/pgo"
	"github.com/arloliu/profgate/internal/proxy"
)

const (
	// RequestReadTimeout is how long a client gets to deliver its request headers,
	// and, on a route that reads or probes a body, how long it gets to deliver the body.
	// Both listeners read it for ReadHeaderTimeout; a body is at most 64 KiB, so one bound serves both.
	RequestReadTimeout = 10 * time.Second
	// budgetGrace is added to the effective duration to form the overall request budget;
	// it is the whole budget for profiles without a duration.
	budgetGrace = 30 * time.Second
	// codeOK is the code of a request that was answered as asked.
	codeOK = "ok"
	// codeStreamFailed is the proxy outcome after which the connection is dropped.
	codeStreamFailed = "upstream_stream_failed"
	// codeDrainExpired is the outcome of a request the drain bound cut.
	codeDrainExpired = "drain_expired"
	// codeInternalError is the console outcome for a status that is neither a
	// success, a redirect, nor one of the two envelopes the console writes.
	codeInternalError = "internal_error"
	// labelNone is the metrics profile label when no profile applies.
	labelNone = "none"
	// labelCPU is the metrics profile label every Collection route carries:
	// a Collection profiles CPU and nothing else.
	labelCPU = "cpu"
)

// ErrDrainExpired is the cancellation cause the gateway sets on every request still in flight
// when its drain bound ends, just before it closes their connections.
// Every request context derives from the cancelled one and reports it through context.Cause,
// which is how a cut is told from a client that left on its own.
var ErrDrainExpired = errors.New("the drain bound ended with the request in flight")

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
	Ready func() bool
	// Drain closes the moment the replica begins draining,
	// which is when /readyz turns 503 and before the drain delay.
	// A request held open by a wait answers then with the record it last read,
	// so no poll outlasts the window the deployment sized.
	// nil is a channel that never closes, which is a handler no drain reaches.
	Drain  <-chan struct{}
	Logger *slog.Logger
	Choose func(n int) int // nil means math/rand/v2 IntN
}

// server is the handler's state.
type server struct {
	deps Deps
	// budgetGrace is the grace the overall request budget adds to the effective duration.
	// Production sets it from the constant above; a test on a real socket shortens it.
	budgetGrace time.Duration
	// bodyReadTimeout is how long a PGO route gives a client to deliver its body, or to prove it sent none.
	// Production sets it from RequestReadTimeout; a test on a real socket shortens it.
	bodyReadTimeout time.Duration
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

	return &server{deps: d, budgetGrace: budgetGrace, bodyReadTimeout: RequestReadTimeout}
}

// routeKind is which of the /v1 routes a path matched.
type routeKind int

// The routes the gateway serves: the two interactive ones, the seven PGO ones, the four listing ones,
// the one authentication-discovery route, which runs no authentication step,
// the three browser-login routes, the document route, and the console.
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
	// The two Service-scoped routes that answer for the newest completed
	// Collection of a Service: its record, and its stored profile.
	kindCollectionLatest
	kindCollectionLatestProfile
	kindNamespaces
	kindServices
	kindWhoami
	kindLimits
	kindAuth
	kindAuthLogin
	kindAuthCallback
	kindAuthLogout
	// kindOpenAPI is the document route, which runs no authentication and no
	// realm step: what it publishes is the route grammar.
	kindOpenAPI
	// kindConsole is the three declarations the console answers: the shell, the
	// asset remainder under it, and the redirect at the root.
	// One kind serves all three because the console resolves the path itself.
	kindConsole
)

// isAuthRoute reports whether the route is one of the three browser-login routes,
// which take the shorter algorithm of serveAuthRoute.
func (k routeKind) isAuthRoute() bool {
	switch k {
	case kindAuthLogin, kindAuthCallback, kindAuthLogout:
		return true
	case kindTargets, kindProfile, kindPGOPolicy, kindCollections, kindCollection, kindCollectionProfile,
		kindCollectionCancel, kindCollectionLatest, kindCollectionLatestProfile, kindNamespaces, kindServices,
		kindWhoami, kindLimits, kindAuth, kindOpenAPI, kindConsole:
		return false
	default:
		return false
	}
}

// isPGO reports whether the route reads or writes PGO state, which is what the
// pgo.enabled and replay-barrier steps of the request algorithm gate.
func (k routeKind) isPGO() bool {
	switch k {
	case kindPGOPolicy, kindCollections, kindCollection, kindCollectionProfile, kindCollectionCancel,
		kindCollectionLatest, kindCollectionLatestProfile:
		return true
	case kindTargets, kindProfile, kindNamespaces, kindServices, kindWhoami, kindLimits, kindAuth,
		kindAuthLogin, kindAuthCallback, kindAuthLogout, kindOpenAPI, kindConsole:
		return false
	default:
		return false
	}
}

// isPGOWrite reports whether a POST to the route creates or ends a Collection,
// which are the two routes that require a JSON media type.
// The listing shares a declaration with the create, so the caller checks the
// method as well: a GET of the same path declares nothing.
func (k routeKind) isPGOWrite() bool {
	switch k {
	case kindCollections, kindCollectionCancel:
		return true
	case kindTargets, kindProfile, kindPGOPolicy, kindCollection, kindCollectionProfile,
		kindCollectionLatest, kindCollectionLatestProfile, kindNamespaces,
		kindServices, kindWhoami, kindLimits, kindAuth, kindAuthLogin, kindAuthCallback, kindAuthLogout,
		kindOpenAPI, kindConsole:
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
	case kindTargets, kindProfile, kindPGOPolicy, kindCollections, kindCollectionLatest,
		kindCollectionLatestProfile, kindNamespaces, kindServices, kindWhoami,
		kindLimits, kindAuth, kindAuthLogin, kindAuthCallback, kindAuthLogout, kindOpenAPI, kindConsole:
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
	case kindTargets, kindProfile, kindPGOPolicy, kindCollections, kindCollection, kindCollectionProfile,
		kindCollectionCancel, kindCollectionLatest, kindCollectionLatestProfile, kindAuth, kindAuthLogin,
		kindAuthCallback, kindAuthLogout, kindOpenAPI, kindConsole:
		return false
	default:
		return false
	}
}

// route is a path matched against a declaration, with the segments it captured.
type route struct {
	kind       routeKind
	namespace  string
	service    string
	profile    string // empty for every route but the profile endpoint
	collection string // set for the three Collection-scoped routes
}

// request is what one request accumulates for its audit record and its metrics labels.
type request struct {
	// ctx is the request's own context, read for the cause of its cancellation.
	ctx    context.Context
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
	// The two latest routes are counted under the two shapes they answer:
	// they differ only in how the record was chosen,
	// and a value per route would split a series to record a path the audit line already names.
	case kindCollection, kindCollectionLatest:
		return metrics.EndpointCollection, labelCPU
	case kindCollectionProfile, kindCollectionLatestProfile:
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
	case kindAuth, kindAuthLogin, kindAuthCallback, kindAuthLogout:
		return metrics.EndpointAuth, labelNone
	case kindOpenAPI:
		return metrics.EndpointOpenAPI, labelNone
	case kindConsole:
		return metrics.EndpointUI, labelNone
	case kindProfile:
		if config.IsProfile(q.route.profile) {
			return metrics.EndpointProfile, q.route.profile
		}

		return metrics.EndpointProfile, labelNone
	default:
		return metrics.EndpointProfile, labelNone
	}
}

// narrated reports whether the request writes an audit record.
// A console request, a /v1/auth request,
// and a request for the OpenAPI document are counted, not narrated:
// each carries no principal and names nothing a realm bounds.
func (q *request) narrated() bool {
	if q.console {
		return false
	}

	if !q.routed {
		return true
	}

	return q.route.kind != kindAuth && q.route.kind != kindOpenAPI
}

// cut reports whether the drain bound ended while this request was in flight.
// A request built with no context, as a test may build one, was not cut.
func (q *request) cut() bool {
	if q.ctx == nil {
		return false
	}

	return errors.Is(context.Cause(q.ctx), ErrDrainExpired)
}

// gone reports whether the request's context was cancelled by something other than the drain:
// the client left, or a read on its connection failed.
func (q *request) gone() bool {
	if q.ctx == nil {
		return false
	}

	return errors.Is(q.ctx.Err(), context.Canceled) && !q.cut()
}

// fail writes a gateway-generated error and records it.
// A request the drain cut is recorded as the cut and answered nothing:
// the error it reached is what the cancelled call mapped to, not what happened,
// and the abort keeps net/http from writing the empty 200 a handler that returns silently gets.
// A request whose client left is recorded as client_gone and answered nothing for the same reason.
// The one cancellation a client did not cause is its body missing the read deadline,
// which net/http reports by cancelling the context too; that client is still there, and gets its envelope.
func (q *request) fail(w http.ResponseWriter, e *requestError) {
	if q.cut() {
		q.audit.status = 0
		q.audit.code = codeDrainExpired
		panic(http.ErrAbortHandler)
	}
	if q.gone() && !e.bodyDeadline {
		q.audit.status = 0
		q.audit.code = codeClientGone
		panic(http.ErrAbortHandler)
	}
	q.audit.status = e.status
	q.audit.code = e.code
	if e.auditCode != "" {
		q.audit.code = e.auditCode
	}
	writeError(w, e)
}

// ServeHTTP runs the request algorithm:
// route, method, JSON media type, readiness, credential placement, authentication, realm, parameters,
// discovery, filter and select, admit, confirm, proxy.
// The first failing step answers.
// The configuration is loaded once here and the request uses that snapshot throughout.
// /v1/auth stops after readiness and answers without an authentication step.
// The path is matched once against the route table, and the declaration it matched
// carries the methods the Allow header of a 405 lists.
// A path under /auth/ is not a /v1 route and takes the shorter algorithm of serveAuthRoute;
// a path under /ui/ or exactly / is handed to the console, which runs its own.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cfg := s.deps.Config.Load()
	w.Header().Set("Cache-Control", "no-store")
	// The identifier is set before any routing decision, so the console's answers,
	// an /auth/ redirect, and every error envelope carry it without a second writer.
	id := RequestID(r)
	w.Header().Set(requestIDHeader, id)

	q := &request{ctx: r.Context()}
	q.audit.requestID = id
	defer func() {
		// A route that ends on its cancelled context records client_gone, which is true of what it saw;
		// when the drain caused that cancellation the record says so instead,
		// and the request is aborted after the row and the record are written,
		// so a committed stream is truncated and an uncommitted one ends with nothing written.
		// Every other code is a response the route produced, which the cut does not touch.
		abort := q.cut() && q.audit.code == codeClientGone
		if abort {
			q.audit.code = codeDrainExpired
		}
		q.audit.duration = time.Since(start)
		endpoint, profile := q.labels()
		s.deps.Recorder.Request(endpoint, profile, q.audit.code, q.audit.duration)
		if q.narrated() {
			writeAudit(s.deps.Logger, q.audit)
		}
		if abort {
			panic(http.ErrAbortHandler)
		}
	}()

	// A request that carries a body gets the read deadline before any route can refuse it unread.
	// net/http discards an unread body before it flushes the response, and that discard reads no context,
	// so against a body that never arrives a refusal would wait on the client.
	// A route that reads the body arms the deadline again and clears it at the body's end.
	if r.ContentLength != 0 {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.bodyReadTimeout))
	}

	rt, decl, ok := match(r.URL.Path)
	if !ok {
		q.fail(w, errRouteUnknown)

		return
	}

	// The console and the three browser-login routes are dispatched straight off the match,
	// before the /v1 algorithm and before the request counts as routed.
	// The console serves files while the caches are still syncing,
	// and an /auth/ route runs a readiness step of its own after its method check.
	if rt.kind == kindConsole {
		s.serveConsole(w, r, q)

		return
	}
	if rt.kind.isAuthRoute() {
		s.serveAuthRoute(w, r, q, cfg, decl)

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
			q.fail(w, &requestError{status: http.StatusNotFound, code: CodeProfileUnknown, message: fmt.Sprintf("unknown profile %q", rt.profile)})

			return
		}
	}

	if !slices.Contains(decl.Methods, r.Method) {
		w.Header().Set("Allow", strings.Join(decl.Methods, ", "))
		q.fail(w, methodNotAllowed(r.Method))

		return
	}

	// The media type the two PGO write routes require.
	// It answers alike for every caller,
	// so a request another origin could have produced is refused here:
	// before readiness, before the PGO steps, and before anything reads a credential.
	if r.Method == http.MethodPost && rt.kind.isPGOWrite() {
		if e := mediaTypeFault(r.Header); e != nil {
			q.fail(w, e)

			return
		}
	}

	if !s.ready() {
		q.fail(w, errNotReady)

		return
	}

	// The two /v1 routes with no authentication step:
	// they answer here, before the PGO and credential-placement steps,
	// because each is what a client reads before it holds a credential.
	if rt.kind == kindAuth {
		s.serveAuthInfo(w, r, q, cfg)

		return
	}
	if rt.kind == kindOpenAPI {
		s.serveOpenAPI(w, r, q)

		return
	}

	// PGO collection is off, or its stores cannot be decided from yet.
	// The step sits between readiness and authentication, so a caller learns
	// the feature is absent before anything about the realm it asked through.
	var sess *pgo.Session
	if rt.kind.isPGO() {
		if !cfg.PGO.Enabled {
			q.fail(w, &requestError{status: http.StatusNotImplemented, code: CodePGODisabled, message: "pgo collection is not enabled; the gateway's pgo.enabled is false"})

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
		q.fail(w, invalidParameter("access_token is not accepted as a query parameter",
			paramFault(detailUnknownParameter, accessTokenParam,
				"access_token is not accepted as a query parameter")))

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
		q.fail(w, &requestError{status: http.StatusForbidden, code: CodeRealmDenied, message: "access denied by realm"})

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
		q.fail(w, invalidParameter("the query string is malformed",
			paramFault(detailMalformedParameter, "", "the query string does not parse")))

		return
	}
	var (
		params  profileParams
		tparams targetsParams
		perr    *requestError
	)
	if rt.kind == kindTargets {
		tparams, perr = parseTargetsParams(values)
		q.port = tparams.port
	} else {
		params, perr = parseProfileParams(values, spec, cfg.Limits)
		q.port = params.port
	}
	// The selection is recorded as sent even when another parameter fails;
	// a fault in the selection itself leaves it empty.
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
		s.serveTargets(w, r, q, tparams)

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
	if err != nil {
		return nil, discoveryError(rt, err)
	}

	return targets, nil
}

// discoveryError maps a Discovery error to its response:
// the two sentinels and the 503 everything else earns.
// Targets and Explain share it.
func discoveryError(rt route, err error) *requestError {
	switch {
	case errors.Is(err, k8s.ErrServiceNotFound):
		return &requestError{
			status:  http.StatusNotFound,
			code:    CodeServiceNotFound,
			message: fmt.Sprintf("service %s not found in namespace %s", rt.service, rt.namespace),
		}
	case errors.Is(err, k8s.ErrServiceSelectorless):
		return &requestError{
			status:  http.StatusUnprocessableEntity,
			code:    CodeServiceSelectorless,
			message: fmt.Sprintf("service %s in namespace %s has no selector", rt.service, rt.namespace),
		}
	default:
		return &requestError{
			status:  http.StatusServiceUnavailable,
			code:    CodeDiscoveryUnavailable,
			message: fmt.Sprintf("discovery cannot resolve service %s in namespace %s", rt.service, rt.namespace),
		}
	}
}

// serveTargets answers the targets endpoint from the cache:
// Explain when the caller asked for the counts and Targets otherwise, then the version and Pod filters.
// The explain body carries the seam's counts plus one entry per reason the filters produced.
func (s *server) serveTargets(w http.ResponseWriter, r *http.Request, q *request, params targetsParams) {
	rt := q.route
	var (
		targets []k8s.Target
		explain k8s.Explanation
		err     error
	)
	if params.explain {
		q.audit.explain = true
		explain, err = s.deps.Discovery.Explain(r.Context(), rt.namespace, rt.service, q.port.sel)
		targets = explain.Targets
	} else {
		targets, err = s.deps.Discovery.Targets(r.Context(), rt.namespace, rt.service, q.port.sel)
	}
	if err != nil {
		q.fail(w, discoveryError(rt, err))

		return
	}
	targets, filtered := filterTargets(targets, params)

	q.audit.status = http.StatusOK
	q.audit.code = codeOK
	if !params.explain {
		writeTargets(w, rt.namespace, rt.service, targets)

		return
	}
	writeExplain(w, rt.namespace, rt.service, targets, explain.SelectorMatched, mergeExclusions(explain.Excluded, filtered))
}

// filterTargets applies version, then pod, and counts what each dropped under its reason.
func filterTargets(targets []k8s.Target, params targetsParams) ([]k8s.Target, map[string]int) {
	dropped := map[string]int{}
	remaining := targets
	if params.version != "" {
		remaining = nil
		for _, t := range targets {
			if t.Version == params.version {
				remaining = append(remaining, t)
			} else {
				dropped[k8s.ReasonVersionMismatch]++
			}
		}
	}
	if params.pod != "" {
		kept := remaining
		remaining = nil
		for _, t := range kept {
			if t.Pod == params.pod {
				remaining = append(remaining, t)
			} else {
				dropped[k8s.ReasonPodNameMismatch]++
			}
		}
	}

	return remaining, dropped
}

// mergeExclusions joins the seam's counts and the filters' counts in vocabulary order,
// keeping only the reasons with a non-zero count.
func mergeExclusions(seam []k8s.Exclusion, filtered map[string]int) []exclusionView {
	counts := make(map[string]int, len(seam)+len(filtered))
	for _, ex := range seam {
		counts[ex.Reason] += ex.Count
	}
	for reason, n := range filtered {
		counts[reason] += n
	}
	views := make([]exclusionView, 0, len(counts))
	for _, reason := range k8s.ExclusionReasons() {
		if n := counts[reason]; n > 0 {
			views = append(views, exclusionView{Reason: reason, Count: n})
		}
	}

	return views
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
		q.fail(w, &requestError{status: http.StatusTooManyRequests, code: CodeTooManyProfiles, message: "too many profiles in flight"})

		return
	}
	defer release()
	s.deps.Recorder.ProfilesInFlight(1)
	defer s.deps.Recorder.ProfilesInFlight(-1)

	// The overall budget starts here and bounds confirmation, dial, header wait, and streaming.
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(params.seconds)*time.Second+s.budgetGrace)
	defer cancel()

	if err := s.deps.Discovery.Confirm(ctx, target); err != nil {
		if errors.Is(err, k8s.ErrTargetChanged) {
			s.deps.Recorder.Confirm("changed")
			q.fail(w, &requestError{
				status:  http.StatusServiceUnavailable,
				code:    CodeTargetChanged,
				message: fmt.Sprintf("pod %s changed since it was selected; retry", target.Pod),
			})

			return
		}
		if errors.Is(err, context.Canceled) {
			// The client left during the read: nobody to answer, and an attempt the counter still sees.
			s.deps.Recorder.Confirm(codeClientGone)
			q.audit.code = codeClientGone

			return
		}
		// Anything the gateway cannot classify is treated as the API server not vouching for the target.
		s.deps.Recorder.Confirm("unavailable")
		q.fail(w, &requestError{
			status:  http.StatusServiceUnavailable,
			code:    CodeDiscoveryUnavailable,
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
		// A request the drain cut is answered nothing, for the timeout that lands in the same instant as the cut;
		// the abort keeps net/http from writing the empty 200 a handler that returns silently gets.
		if q.cut() {
			q.audit.status = 0
			q.audit.code = codeDrainExpired
			panic(http.ErrAbortHandler)
		}
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
	case CodeUpstreamTimeout:
		return fmt.Sprintf("pod %s did not answer in time", pod)
	case CodeUpstreamRedirect:
		return fmt.Sprintf("pod %s answered with a redirect", pod)
	default:
		return fmt.Sprintf("pod %s is unreachable", pod)
	}
}
