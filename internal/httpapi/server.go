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
	"regexp"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
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
)

// routeRE matches the two /v1 routes; the namespace and Service segments are validated separately.
var routeRE = regexp.MustCompile(`^/v1/namespaces/([^/]+)/services/([^/]+)/(targets|profiles/([^/]+))$`)

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
	Logger    *slog.Logger
	Choose    func(n int) int // nil means math/rand/v2 IntN
}

// server is the handler's state: the dependencies and the admission slots.
type server struct {
	deps  Deps
	slots chan struct{}
}

// New builds the /v1 handler.
// The admission slots are sized once from the configuration loaded now:
// limits.maxConcurrentProfiles is a restart-only field.
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
	cfg := d.Config.Load()

	return &server{deps: d, slots: make(chan struct{}, cfg.Limits.MaxConcurrentProfiles)}
}

// route is a parsed /v1 path.
type route struct {
	namespace string
	service   string
	profile   string // empty for the targets endpoint
	isProfile bool
}

// parseRoute matches path against the two routes and validates the two name segments.
func parseRoute(path string) (route, bool) {
	m := routeRE.FindStringSubmatch(path)
	if m == nil {
		return route{}, false
	}
	if len(validation.IsDNS1123Label(m[1])) > 0 || len(validation.IsDNS1123Label(m[2])) > 0 {
		return route{}, false
	}
	rt := route{namespace: m[1], service: m[2]}
	if m[3] != "targets" {
		rt.isProfile = true
		rt.profile = m[4]
	}

	return rt, true
}

// request is what one request accumulates for its audit record and its metrics labels.
type request struct {
	routed bool
	route  route
	audit  auditRecord
}

// labels are the metrics endpoint and profile for this request:
// the resolved route when there is one, ("profile","none") before a route resolves
// or when the profile name is unknown.
func (q *request) labels() (metrics.Endpoint, string) {
	switch {
	case !q.routed:
		return metrics.EndpointProfile, labelNone
	case !q.route.isProfile:
		return metrics.EndpointTargets, labelNone
	case config.IsProfile(q.route.profile):
		return metrics.EndpointProfile, q.route.profile
	default:
		return metrics.EndpointProfile, labelNone
	}
}

// fail writes a gateway-generated error and records it.
func (q *request) fail(w http.ResponseWriter, e *requestError) {
	q.audit.status = e.status
	q.audit.code = e.code
	writeError(w, e.status, e.code, e.message)
}

// ServeHTTP runs the request algorithm:
// route, method, readiness, authentication, realm, parameters, discovery, filter and select, admit, confirm, proxy.
// The first failing step answers.
// The configuration is loaded once here and the request uses that snapshot throughout.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cfg := s.deps.Config.Load()
	w.Header().Set("Cache-Control", "no-store")

	q := &request{}
	defer func() {
		q.audit.duration = time.Since(start)
		endpoint, profile := q.labels()
		s.deps.Recorder.Request(endpoint, profile, q.audit.code, q.audit.duration)
		writeAudit(s.deps.Logger, q.audit)
	}()

	rt, ok := parseRoute(r.URL.Path)
	if !ok {
		q.fail(w, &requestError{status: http.StatusNotFound, code: "route_unknown", message: "no such route"})

		return
	}
	q.routed = true
	q.route = rt
	q.audit.namespace = rt.namespace
	q.audit.service = rt.service
	q.audit.profile = rt.profile
	var spec profileSpec
	if rt.isProfile {
		spec, ok = lookupProfile(rt.profile)
		if !ok {
			q.fail(w, &requestError{status: http.StatusNotFound, code: "profile_unknown", message: fmt.Sprintf("unknown profile %q", rt.profile)})

			return
		}
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		q.fail(w, &requestError{status: http.StatusMethodNotAllowed, code: "method_not_allowed", message: fmt.Sprintf("method %s not allowed", r.Method)})

		return
	}

	if !s.deps.Discovery.HasSynced() {
		q.fail(w, &requestError{status: http.StatusServiceUnavailable, code: "not_ready", message: "discovery has not synced"})

		return
	}

	principal, realm, ok := principalRealm(cfg)
	q.audit.principal = principal
	if !ok || !realmAllows(realm, rt) {
		// The denial names nothing: the same body whether or not the Service exists.
		q.fail(w, &requestError{status: http.StatusForbidden, code: "realm_denied", message: "access denied by realm"})

		return
	}

	if !rt.isProfile {
		if r.URL.RawQuery != "" {
			q.fail(w, invalidParameter("the targets endpoint takes no parameters"))

			return
		}
		s.serveTargets(w, r, q)

		return
	}

	params, perr := parseProfileParams(r.URL.RawQuery, spec, cfg.Limits)
	if perr != nil {
		q.fail(w, perr)

		return
	}
	q.audit.seconds = params.seconds
	s.serveProfile(w, r, q, spec, params)
}

// discover runs the discovery step and maps its errors.
func (s *server) discover(ctx context.Context, q *request) ([]k8s.Target, *requestError) {
	rt := q.route
	targets, err := s.deps.Discovery.Targets(ctx, rt.namespace, rt.service)
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
	select {
	case s.slots <- struct{}{}:
	default:
		q.fail(w, &requestError{status: http.StatusTooManyRequests, code: "too_many_profiles", message: "too many profiles in flight"})

		return
	}
	defer func() { <-s.slots }()
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
			writeError(w, out.Status, out.Code, upstreamMessage(out.Code, target.Pod))
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
