package httpapi

import (
	"net/http"
	"slices"

	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
)

// logoutPath is the one logout route the browser flow serves.
const logoutPath = "/auth/logout"

// The response shapes of the five listing routes, field for field.

// namespacesBody answers the namespace list.
type namespacesBody struct {
	Namespaces []string `json:"namespaces"`
}

// servicesBody answers the Service list of one namespace.
type servicesBody struct {
	Namespace string   `json:"namespace"`
	Services  []string `json:"services"`
}

// catalogBody answers every Service the caller's realm admits, across every namespace it admits.
type catalogBody struct {
	Catalog []serviceRefView `json:"catalog"`
}

// serviceRefView is one entry of the catalog: the namespace a Service sits in, and its name.
type serviceRefView struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// whoamiBody describes the caller: the principal, its realm as configured, and the authentication mode.
type whoamiBody struct {
	Principal string    `json:"principal"`
	Realm     realmView `json:"realm"`
	Auth      authView  `json:"auth"`
}

// realmView is the caller's own realm exactly as configured, the wildcard included.
type realmView struct {
	Name       string   `json:"name"`
	Namespaces []string `json:"namespaces"`
	Services   []string `json:"services"`
	Profiles   []string `json:"profiles"`
	PGO        pgoFlags `json:"pgo"`
}

// pgoFlags is the realm's pgo block.
type pgoFlags struct {
	Read      bool `json:"read"`
	Collect   bool `json:"collect"`
	Configure bool `json:"configure"`
}

// authView names the mode and, only when the browser flow is configured, the logout route.
type authView struct {
	Mode   string `json:"mode"`
	Logout string `json:"logout,omitempty"`
}

// limitsBody is the operator configuration a client may name values from.
type limitsBody struct {
	CPUSeconds   int       `json:"cpuSeconds"`
	TraceSeconds int       `json:"traceSeconds"`
	Profiles     []string  `json:"profiles"`
	Pprof        pprofView `json:"pprof"`
	PGO          pgoView   `json:"pgo"`
}

// pprofView is the port default and the allowedSelections list, [] when empty.
type pprofView struct {
	Default           portDefault        `json:"default"`
	AllowedSelections []config.Selection `json:"allowedSelections"`
}

// portDefault carries whichever of the port number and the port name is configured.
type portDefault struct {
	Port     int32  `json:"port,omitempty"`
	PortName string `json:"portName,omitempty"`
}

// pgoView says whether PGO collection is enabled.
type pgoView struct {
	Enabled bool `json:"enabled"`
}

// serveListing answers one of the five listing routes after the realm step:
// any query parameter is refused, then whoami and limits answer from the configuration snapshot
// and the three lists read the Service cache through Catalog and apply the realm filter.
func (s *server) serveListing(
	w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config, p auth.Principal, realm config.Realm,
) {
	if r.URL.RawQuery != "" {
		q.fail(w, noParameters(r.URL.RawQuery))

		return
	}

	var body any
	switch q.route.kind {
	case kindWhoami:
		body = whoamiView(cfg, p, realm)
	case kindLimits:
		body = limitsView(cfg)
	case kindNamespaces:
		refs, ok := s.admittedCatalog(w, r, q, realm)
		if !ok {
			return
		}
		body = namespacesBody{Namespaces: namespacesOf(refs)}
	case kindServices:
		refs, ok := s.admittedCatalog(w, r, q, realm)
		if !ok {
			return
		}
		body = servicesBody{Namespace: q.route.namespace, Services: servicesOf(refs)}
	case kindCatalog:
		refs, ok := s.admittedCatalog(w, r, q, realm)
		if !ok {
			return
		}
		body = catalogBody{Catalog: catalogOf(refs)}
	case kindTargets, kindProfile, kindPGOPolicy, kindCollections, kindCollection, kindCollectionProfile,
		kindCollectionCancel, kindCollectionLatest, kindCollectionLatestProfile, kindAuth, kindAuthLogin,
		kindAuthCallback, kindAuthLogout, kindOpenAPI, kindConsole:
		// Not a listing route; ServeHTTP never dispatches one here.
		q.fail(w, errRouteUnknown)

		return
	default:
		q.fail(w, errRouteUnknown)

		return
	}
	q.audit.status = http.StatusOK
	q.audit.code = codeOK
	writeJSON(w, http.StatusOK, body)
}

// admittedCatalog is what the three cache-reading lists share:
// one read of the Service cache through Catalog, then the realm filter.
// The namespace it asks for is the route's own,
// which the Service list captures from its path and the other two leave empty, reading every namespace.
// A read that fails is answered here, and the caller returns on the false.
func (s *server) admittedCatalog(
	w http.ResponseWriter, r *http.Request, q *request, realm config.Realm,
) ([]k8s.ServiceRef, bool) {
	refs, err := s.deps.Discovery.Catalog(r.Context(), q.route.namespace)
	if err != nil {
		q.fail(w, &requestError{
			status:  http.StatusServiceUnavailable,
			code:    CodeDiscoveryUnavailable,
			message: "discovery cannot list services",
		})

		return nil, false
	}

	return filterCatalog(realm, refs), true
}

// whoamiView describes the caller from the configuration snapshot and the resolved principal.
func whoamiView(cfg *config.Config, p auth.Principal, realm config.Realm) whoamiBody {
	view := whoamiBody{
		Principal: p.Name,
		Realm: realmView{
			Name:       p.Realm,
			Namespaces: cloneList(realm.Namespaces),
			Services:   cloneList(realm.Services),
			Profiles:   cloneList(realm.Profiles),
			PGO:        pgoFlags{Read: realm.PGO.Read, Collect: realm.PGO.Collect, Configure: realm.PGO.Configure},
		},
		Auth: authView{Mode: cfg.Auth.Mode},
	}
	if cfg.Auth.Mode == config.ModeOIDC && cfg.Auth.OIDC != nil && cfg.Auth.OIDC.Browser != nil {
		view.Auth.Logout = logoutPath
	}

	return view
}

// limitsView is the configured limits, profile names, port default, allowed selections, and pgo.enabled.
func limitsView(cfg *config.Config) limitsBody {
	pprof := cfg.Discovery.Pprof
	view := limitsBody{
		CPUSeconds:   cfg.Limits.CPUSeconds,
		TraceSeconds: cfg.Limits.TraceSeconds,
		Profiles:     config.Profiles(),
		Pprof: pprofView{
			AllowedSelections: append(make([]config.Selection, 0, len(pprof.AllowedSelections)), pprof.AllowedSelections...),
		},
		PGO: pgoView{Enabled: cfg.PGO.Enabled},
	}
	if pprof.Port != 0 {
		view.Pprof.Default.Port = pprof.Port
	} else {
		view.Pprof.Default.PortName = pprof.PortName
	}

	return view
}

// filterCatalog keeps the Services whose namespace and name the realm's lists admit.
func filterCatalog(realm config.Realm, refs []k8s.ServiceRef) []k8s.ServiceRef {
	kept := make([]k8s.ServiceRef, 0, len(refs))
	for _, ref := range refs {
		if listAllows(realm.Namespaces, ref.Namespace) && listAllows(realm.Services, ref.Name) {
			kept = append(kept, ref)
		}
	}

	return kept
}

// servicesOf is the sorted Service names of a filtered catalog.
func servicesOf(refs []k8s.ServiceRef) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	slices.Sort(names)

	return names
}

// catalogOf is one entry per Service of a filtered catalog, in the order Catalog returned them.
// Nothing is sorted here: Catalog orders by namespace and then by name, and the filter keeps that order.
func catalogOf(refs []k8s.ServiceRef) []serviceRefView {
	views := make([]serviceRefView, 0, len(refs))
	for _, ref := range refs {
		views = append(views, serviceRefView{Namespace: ref.Namespace, Name: ref.Name})
	}

	return views
}

// namespacesOf is the sorted distinct namespaces of a filtered catalog.
func namespacesOf(refs []k8s.ServiceRef) []string {
	namespaces := make([]string, 0, len(refs))
	for _, ref := range refs {
		namespaces = append(namespaces, ref.Namespace)
	}
	slices.Sort(namespaces)

	return slices.Compact(namespaces)
}

// cloneList copies a configured list so a response never aliases the configuration; nil becomes [].
func cloneList(list []string) []string {
	return append(make([]string, 0, len(list)), list...)
}
