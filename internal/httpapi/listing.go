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

// The response shapes of the four listing routes, field for field.

// namespacesBody answers the namespace list.
type namespacesBody struct {
	Namespaces []string `json:"namespaces"`
}

// servicesBody answers the Service list of one namespace.
type servicesBody struct {
	Namespace string   `json:"namespace"`
	Services  []string `json:"services"`
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

// pprofView is the port default and the two allowlists, each [] when empty.
type pprofView struct {
	Default          portDefault `json:"default"`
	AllowedPorts     []int32     `json:"allowedPorts"`
	AllowedPortNames []string    `json:"allowedPortNames"`
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

// serveListing answers one of the four listing routes after the realm step:
// any query parameter is refused, then whoami and limits answer from the configuration snapshot
// and the two lists read the Service cache through Catalog and apply the realm filter.
func (s *server) serveListing(
	w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config, p auth.Principal, realm config.Realm,
) {
	if r.URL.RawQuery != "" {
		q.fail(w, invalidParameter("this route takes no query parameter"))

		return
	}

	var body any
	switch q.route.kind {
	case kindWhoami:
		body = whoamiView(cfg, p, realm)
	case kindLimits:
		body = limitsView(cfg)
	case kindNamespaces, kindServices:
		refs, err := s.deps.Discovery.Catalog(r.Context(), q.route.namespace)
		if err != nil {
			q.fail(w, &requestError{
				status:  http.StatusServiceUnavailable,
				code:    "discovery_unavailable",
				message: "discovery cannot list services",
			})

			return
		}
		refs = filterCatalog(realm, refs)
		if q.route.kind == kindNamespaces {
			body = namespacesBody{Namespaces: namespacesOf(refs)}
		} else {
			names := make([]string, 0, len(refs))
			for _, ref := range refs {
				names = append(names, ref.Name)
			}
			slices.Sort(names)
			body = servicesBody{Namespace: q.route.namespace, Services: names}
		}
	case kindTargets, kindProfile, kindPGOPolicy, kindCollections, kindCollection, kindCollectionProfile, kindCollectionCancel:
		// Not a listing route; ServeHTTP never dispatches one here.
		q.fail(w, &requestError{status: http.StatusNotFound, code: "route_unknown", message: "no such route"})

		return
	default:
		q.fail(w, &requestError{status: http.StatusNotFound, code: "route_unknown", message: "no such route"})

		return
	}
	q.audit.status = http.StatusOK
	q.audit.code = codeOK
	writeJSON(w, http.StatusOK, body)
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

// limitsView is the configured limits, profile names, port default, allowlists, and pgo.enabled.
func limitsView(cfg *config.Config) limitsBody {
	pprof := cfg.Discovery.Pprof
	view := limitsBody{
		CPUSeconds:   cfg.Limits.CPUSeconds,
		TraceSeconds: cfg.Limits.TraceSeconds,
		Profiles:     config.Profiles(),
		Pprof: pprofView{
			AllowedPorts:     append(make([]int32, 0, len(pprof.AllowedPorts)), pprof.AllowedPorts...),
			AllowedPortNames: cloneList(pprof.AllowedPortNames),
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
