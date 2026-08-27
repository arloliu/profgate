package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
)

const (
	// authPrefix is where the three /auth/ routes live; nothing under it is a /v1 route.
	authPrefix = "/auth/"
	// failureMessage is the one message every authentication failure carries,
	// whatever the reason, so the response tells a caller nothing about which check failed.
	failureMessage = "authentication required"
	// codeAuthRedirect is the code of a browser navigation sent to login.
	codeAuthRedirect = "auth_redirect"
	// accessTokenParam is the query parameter that is refused in every mode.
	accessTokenParam = "access_token"
	// noPrincipal is the principal an audit record names when nobody was resolved.
	noPrincipal = "-"
)

// authRoute names the audit line of one of the three exact /auth/ paths, and
// reports false for any other path.
func authRoute(path string) (string, bool) {
	switch path {
	case "/auth/login":
		return "auth_login", true
	case "/auth/callback":
		return "auth_callback", true
	case "/auth/logout":
		return "auth_logout", true
	default:
		return "", false
	}
}

// ready is the readiness the /v1 and /auth/ routes gate on: the caller's
// closure when set, discovery alone otherwise.
func (s *server) ready() bool {
	if s.deps.Ready != nil {
		return s.deps.Ready()
	}

	return s.deps.Discovery.HasSynced()
}

// hasAccessToken reports whether the raw query names a parameter access_token.
// It never calls url.ParseQuery: a value the parser would drop (%ZZ) or a
// separator it would reject (;) still names the parameter, and the rejection
// must happen before any credential is read, whatever the value holds.
// The key is percent-decoded before the comparison, which is case-sensitive.
func hasAccessToken(rawQuery string) bool {
	for pair := range strings.FieldsFuncSeq(rawQuery, func(r rune) bool { return r == '&' || r == ';' }) {
		key, _, _ := strings.Cut(pair, "=")
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		if key == accessTokenParam {
			return true
		}
	}

	return false
}

// authenticate runs the authentication step and the realm lookup.
// It returns the principal and its realm when the request is admitted;
// otherwise it has answered the request and returns false.
func (s *server) authenticate(
	w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config,
) (auth.Principal, config.Realm, bool) {
	p, err := s.deps.Auth.Authenticate(r.Context(), r, cfg)
	if err != nil {
		var f *auth.Failure
		if !errors.As(err, &f) {
			// Not one of the classified failures: a programming error, answered
			// as unavailable so the caller retries rather than reads it as a denial.
			s.deps.Logger.Error("authenticator failed", "error", err)
			f = &auth.Failure{Status: http.StatusServiceUnavailable, Reason: auth.ReasonInternal}
		}
		s.failAuth(w, q, f, cfg.Auth.Mode)

		return auth.Principal{}, config.Realm{}, false
	}
	realm, ok := cfg.Realms[p.Realm]
	if !ok {
		s.failAuth(w, q, &auth.Failure{Status: http.StatusUnauthorized, Reason: auth.ReasonNoRealm}, cfg.Auth.Mode)

		return auth.Principal{}, config.Realm{}, false
	}
	q.audit.principal = p.Name

	return p, realm, true
}

// failAuth answers a Failure: the session cookie is deleted when asked,
// a navigation is redirected to login, and everything else is the envelope
// its status maps to, counted as a failure.
func (s *server) failAuth(w http.ResponseWriter, q *request, f *auth.Failure, mode string) {
	q.audit.principal = noPrincipal
	q.audit.reason = f.Reason
	if f.ClearSession {
		auth.DeleteSessionCookie(w)
	}
	if f.Redirect != "" {
		q.audit.status = http.StatusFound
		q.audit.code = codeAuthRedirect
		w.Header().Set("Location", f.Redirect)
		w.WriteHeader(http.StatusFound)

		return
	}
	s.deps.Recorder.AuthFailure(mode, f.Reason)
	switch f.Status {
	case http.StatusUnauthorized:
		if challenge := auth.Challenge(mode); challenge != "" {
			w.Header().Set("WWW-Authenticate", challenge)
		}
		q.fail(w, &requestError{status: http.StatusUnauthorized, code: "unauthenticated", message: failureMessage})
	case http.StatusTooManyRequests:
		w.Header().Set("Retry-After", "1")
		q.fail(w, &requestError{status: http.StatusTooManyRequests, code: "too_many_auth", message: failureMessage})
	default:
		w.Header().Set("Retry-After", "5")
		q.fail(w, &requestError{status: http.StatusServiceUnavailable, code: "auth_unavailable", message: failureMessage})
	}
}

// serveAuthRoute serves a path under /auth/: the three exact routes when the
// browser flow is configured, 404 route_unknown otherwise.
// The route writes the whole response; this owns the match, the method, readiness,
// the audit line, and the metrics row.
func (s *server) serveAuthRoute(w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config) {
	name, known := authRoute(r.URL.Path)
	if !known || s.deps.AuthRoutes == nil {
		q.fail(w, &requestError{status: http.StatusNotFound, code: "route_unknown", message: "no such route"})

		return
	}
	q.authRoute = true
	q.audit.route = name
	q.audit.method = r.Method
	q.audit.principal = noPrincipal
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		q.fail(w, &requestError{status: http.StatusMethodNotAllowed, code: "method_not_allowed", message: "method " + r.Method + " not allowed"})

		return
	}
	if !s.ready() {
		q.fail(w, &requestError{status: http.StatusServiceUnavailable, code: "not_ready", message: "the gateway is not ready"})

		return
	}
	out := s.deps.AuthRoutes.ServeAuth(w, r, cfg)
	q.audit.status = out.Status
	q.audit.code = out.Code
	q.audit.reason = out.Reason
	q.audit.principal = out.Principal
	if out.Status == http.StatusUnauthorized || out.Status == http.StatusServiceUnavailable {
		s.deps.Recorder.AuthFailure(cfg.Auth.Mode, out.Reason)
	}
}
