package httpapi

import (
	"net/http"
	"slices"

	"github.com/arloliu/profgate/internal/config"
)

// wildcard is the list entry that matches every value.
const wildcard = "*"

// realmAllows evaluates namespace, then Service, then the route's own check:
// the profile list for the profile endpoint, and the realm's pgo flag for a PGO route.
// Each list matches by the wildcard or the exact string; there is no prefix or glob matching.
func realmAllows(r config.Realm, rt route, method string) bool {
	if !listAllows(r.Namespaces, rt.namespace) || !listAllows(r.Services, rt.service) {
		return false
	}
	if rt.kind == kindProfile && !listAllows(r.Profiles, rt.profile) {
		return false
	}
	if rt.kind.isPGO() && !pgoAllows(r.PGO, rt.kind, method) {
		return false
	}

	return true
}

// pgoAllows evaluates the realm's pgo flag for one PGO route and method:
// reading state needs pgo.read, creating or cancelling a Collection needs
// pgo.collect, and writing a policy needs pgo.configure.
// A realm without a pgo block has every flag false, so it reaches no PGO route.
func pgoAllows(p config.RealmPGO, kind routeKind, method string) bool {
	switch kind {
	case kindPGOPolicy:
		if method == http.MethodGet {
			return p.Read
		}

		return p.Configure
	case kindCollections:
		if method == http.MethodGet {
			return p.Read
		}

		return p.Collect
	case kindCollection, kindCollectionProfile:
		return p.Read
	case kindCollectionCancel:
		return p.Collect
	case kindTargets, kindProfile:
		return true
	default:
		return false
	}
}

// listAllows reports whether list admits value.
func listAllows(list []string, value string) bool {
	return slices.Contains(list, wildcard) || slices.Contains(list, value)
}
