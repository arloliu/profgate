package httpapi

import (
	_ "embed"
	"net/http"
)

// openAPIDocument describes every route the API listener serves.
// It is hand-maintained and served byte for byte:
// the route writes these bytes and transforms nothing,
// so the file a reviewer reads in a diff is the file a client parses.
// Nothing generates it, and no build step stands between the two;
// what keeps it true is the check in openapi_test.go.
//
//go:embed openapi.json
var openAPIDocument []byte

// serveOpenAPI answers the document route after the readiness step.
// The route has no credential-placement, authentication, or realm step:
// what it publishes is the route grammar,
// which 404 route_unknown and the Allow header of a 405 already publish one request at a time.
// It names namespaces and Services as path templates and never as values from a cluster,
// so a realm has nothing to bound.
// Any query parameter is refused, access_token included.
func (s *server) serveOpenAPI(w http.ResponseWriter, r *http.Request, q *request) {
	if r.URL.RawQuery != "" {
		q.fail(w, noParameters(r.URL.RawQuery))

		return
	}
	q.audit.status = http.StatusOK
	q.audit.code = codeOK
	// Cache-Control: no-store is already set, as it is on every /v1 response;
	// the bytes are in the binary and cost nothing to serve again.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPIDocument)
}
