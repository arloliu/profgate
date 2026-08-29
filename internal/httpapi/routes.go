package httpapi

import (
	"net/http"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/arloliu/profgate/internal/pgo"
)

// The names a path template captures.
// Each one has a grammar of its own and a field of route it fills;
// a segment outside that grammar leaves the declaration unmatched, which is
// 404 route_unknown rather than a refusal naming the segment.
const (
	paramNamespace = "namespace"
	paramService   = "service"
	paramProfile   = "profile"
	paramID        = "id"
	// paramFile is the console's remainder under /ui/.
	// It is the one parameter that spans separators,
	// because the vendored asset tree is nested and the console resolves the whole remainder itself.
	paramFile = "file"
)

// declaration is one route the API listener serves: the path template the
// document publishes, the kind that names its handler, and the methods it
// accepts, in the order the Allow header carries them.
type declaration struct {
	Template string
	Kind     routeKind
	Methods  []string
}

// routeTable is every route the API listener serves, in document order.
// The router matches a request against it, the Allow header of a 405 is the
// matched declaration's methods, and the check of the OpenAPI document reads it.
// A route absent from it cannot be reached.
// declarations hands out copies of it.
//
// The ops listener is outside it:
// /healthz, /readyz, and /metrics answer plain text,
// carry no error envelope, and are served by internal/ops.
var routeTable = [...]declaration{
	{"/v1/namespaces/{namespace}/services/{service}/targets", kindTargets, []string{
		http.MethodGet}},
	{"/v1/namespaces/{namespace}/services/{service}/profiles/{profile}", kindProfile, []string{
		http.MethodGet}},
	{"/v1/namespaces/{namespace}/services/{service}/pgo", kindPGOPolicy, []string{
		http.MethodGet, http.MethodPut, http.MethodDelete}},
	{"/v1/namespaces/{namespace}/services/{service}/collections", kindCollections, []string{
		http.MethodGet, http.MethodPost}},
	{"/v1/namespaces/{namespace}/services/{service}/collections/latest", kindCollectionLatest, []string{
		http.MethodGet}},
	{"/v1/namespaces/{namespace}/services/{service}/collections/latest/profile", kindCollectionLatestProfile,
		[]string{http.MethodGet}},
	{"/v1/collections/{id}", kindCollection, []string{http.MethodGet}},
	{"/v1/collections/{id}/profile", kindCollectionProfile, []string{http.MethodGet}},
	{"/v1/collections/{id}/cancel", kindCollectionCancel, []string{http.MethodPost}},
	{"/v1/namespaces", kindNamespaces, []string{http.MethodGet}},
	{"/v1/namespaces/{namespace}/services", kindServices, []string{http.MethodGet}},
	{"/v1/whoami", kindWhoami, []string{http.MethodGet}},
	{"/v1/limits", kindLimits, []string{http.MethodGet}},
	{"/v1/auth", kindAuth, []string{http.MethodGet}},
	{"/v1/openapi.json", kindOpenAPI, []string{http.MethodGet}},
	{"/auth/login", kindAuthLogin, []string{http.MethodGet}},
	{"/auth/callback", kindAuthCallback, []string{http.MethodGet}},
	{"/auth/logout", kindAuthLogout, []string{http.MethodGet}},
	{"/ui/", kindConsole, []string{http.MethodGet, http.MethodHead}},
	{"/ui/{file}", kindConsole, []string{http.MethodGet, http.MethodHead}},
	{"/", kindConsole, []string{http.MethodGet, http.MethodHead}},
}

// declarations is every route the API listener serves, in document order.
// The caller gets a copy, so nothing it does reaches the table itself.
func declarations() []declaration {
	out := make([]declaration, 0, len(routeTable))
	for _, d := range routeTable {
		d.Methods = slices.Clone(d.Methods)
		out = append(out, d)
	}

	return out
}

// match resolves a path to its declaration and the segments it captured.
// It is the only path dispatch in the package.
// Declarations are tried in table order,
// so the console's shell is matched before the remainder that would also accept it.
func match(path string) (route, declaration, bool) {
	for _, d := range routeTable {
		if rt, ok := bind(d, path); ok {
			return rt, d, true
		}
	}

	return route{}, declaration{}, false
}

// bind matches one path against one declaration, filling the route the
// template's parameters name.
// A path is matched segment by segment, so a captured segment never spans a
// separator; paramFile is the exception and takes the whole remainder.
func bind(d declaration, path string) (route, bool) {
	rt := route{kind: d.Kind}
	segments := strings.Split(path, "/")
	template := strings.Split(d.Template, "/")
	for i, part := range template {
		if i >= len(segments) {
			return route{}, false
		}
		name, isParam := strings.CutPrefix(part, "{")
		if !isParam {
			if segments[i] != part {
				return route{}, false
			}

			continue
		}
		name = strings.TrimSuffix(name, "}")
		if name == paramFile {
			// The remainder, separators included; the console resolves it.
			if !capture(&rt, name, strings.Join(segments[i:], "/")) {
				return route{}, false
			}

			return rt, true
		}
		if !capture(&rt, name, segments[i]) {
			return route{}, false
		}
	}
	if len(segments) != len(template) {
		return route{}, false
	}

	return rt, true
}

// capture validates one captured segment against its parameter's grammar and stores it,
// reporting false for a segment the grammar refuses.
// An empty segment is refused for every parameter: a template names a value.
func capture(rt *route, name, value string) bool {
	if value == "" {
		return false
	}
	switch name {
	case paramNamespace:
		if len(validation.IsDNS1123Label(value)) > 0 {
			return false
		}
		rt.namespace = value
	case paramService:
		if len(validation.IsDNS1123Label(value)) > 0 {
			return false
		}
		rt.service = value
	case paramProfile:
		// The name is looked up after the route resolves, so an unknown one is
		// 404 profile_unknown rather than 404 route_unknown.
		rt.profile = value
	case paramID:
		if !pgo.ValidID(value) {
			return false
		}
		rt.collection = value
	case paramFile:
		// The console reads the path itself; nothing of the remainder is stored.
	default:
		return false
	}

	return true
}
