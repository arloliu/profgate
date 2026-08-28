package e2e

import "fmt"

// Scenario is the metadata of one end-to-end scenario: its name and the lane capabilities it needs.
// Runners live in the tagged files, so this registry compiles and is unit-tested without a cluster.
type Scenario struct {
	Name string
	// NeedsPodReach marks a scenario that port-forwards to a test-app Pod or needs the gateway to complete a proxy to one;
	// a degraded lane skips it.
	NeedsPodReach bool
	// NeedsNetworkPolicy marks a scenario that relies on NetworkPolicy enforcement;
	// a lane whose CNI does not enforce it skips the scenario.
	NeedsNetworkPolicy bool
}

// scenarios is the complete, ordered registry.
// Scenarios returns a copy.
var scenarios = [...]Scenario{
	{Name: "dedupe and wrong-address slice"},
	{Name: "ineligible pods", NeedsPodReach: true},
	{Name: "convergence on delete"},
	{Name: "convergence on ready", NeedsPodReach: true},
	{Name: "profiles parse", NeedsPodReach: true},
	{Name: "errors"},
	{Name: "version filter"},
	{Name: "rbac"},
	{Name: "replicas agree", NeedsPodReach: true},
	{Name: "api outage", NeedsPodReach: true, NeedsNetworkPolicy: true},
	{Name: "port selection", NeedsPodReach: true},
	{Name: "port selection refused", NeedsPodReach: true},
	{Name: "pgo-on-demand", NeedsPodReach: true},
	{Name: "pgo-scheduled-slot", NeedsPodReach: true},
	{Name: "pgo-cancel", NeedsPodReach: true},
	{Name: "pgo-version-conflict", NeedsPodReach: true},
	{Name: "pgo-reclaim", NeedsPodReach: true},
	{Name: "pgo-realm-flags"},
	{Name: "pgo-disabled"},
	{Name: "pgo-clusterrole"},
	{Name: "pgo-preflight-negative"},
	{Name: "tls-rotation"},
	{Name: "auth-oidc-browser", NeedsPodReach: true},
	{Name: "auth-basic", NeedsPodReach: true},
	{Name: "auth-oidc-keycloak", NeedsPodReach: true},
}

// Scenarios returns a copy of the complete, ordered scenario metadata.
func Scenarios() []Scenario {
	out := make([]Scenario, len(scenarios))
	copy(out, scenarios[:])
	return out
}

// Skips reports whether lane l cannot run s, with a reason that names the scenario
// and the missing capability so the skip is attributable in the test log.
func (s Scenario) Skips(l Lane) (bool, string) {
	if s.NeedsPodReach && l.Degraded {
		return true, fmt.Sprintf("scenario %q skipped: lane %q is degraded and the scenario needs a reachable Pod", s.Name, l.Name)
	}
	if s.NeedsNetworkPolicy && !l.NetworkPolicy {
		return true, fmt.Sprintf("scenario %q skipped: lane %q does not enforce NetworkPolicy", s.Name, l.Name)
	}
	return false, ""
}
