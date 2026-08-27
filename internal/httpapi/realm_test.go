package httpapi

import (
	"net/http"
	"testing"

	"github.com/arloliu/profgate/internal/config"
)

func TestRealmAllows(t *testing.T) {
	wide := []string{"*"}
	cases := []struct {
		name    string
		realm   config.Realm
		route   route
		allowed bool
	}{
		{"wildcards allow everything", config.Realm{Namespaces: wide, Services: wide, Profiles: wide},
			route{namespace: "payment", service: "payment-api", profile: "cpu", kind: kindProfile}, true},
		{"exact match on every list", config.Realm{Namespaces: []string{"payment"}, Services: []string{"payment-api"}, Profiles: []string{"cpu"}},
			route{namespace: "payment", service: "payment-api", profile: "cpu", kind: kindProfile}, true},
		{"namespace mismatch", config.Realm{Namespaces: []string{"billing"}, Services: wide, Profiles: wide},
			route{namespace: "payment", service: "payment-api", profile: "cpu", kind: kindProfile}, false},
		{"service mismatch", config.Realm{Namespaces: wide, Services: []string{"billing-api"}, Profiles: wide},
			route{namespace: "payment", service: "payment-api", profile: "cpu", kind: kindProfile}, false},
		{"profile mismatch", config.Realm{Namespaces: wide, Services: wide, Profiles: []string{"heap"}},
			route{namespace: "payment", service: "payment-api", profile: "cpu", kind: kindProfile}, false},
		{"profiles ignored for targets", config.Realm{Namespaces: wide, Services: wide, Profiles: []string{"heap"}},
			route{namespace: "payment", service: "payment-api"}, true},
		{"no prefix matching", config.Realm{Namespaces: []string{"pay"}, Services: wide, Profiles: wide},
			route{namespace: "payment", service: "payment-api"}, false},
		{"wildcard among exact entries", config.Realm{Namespaces: []string{"billing", "*"}, Services: wide, Profiles: wide},
			route{namespace: "payment", service: "payment-api"}, true},
		{"empty lists deny", config.Realm{},
			route{namespace: "payment", service: "payment-api"}, false},
		{"namespaces list ignores the lists", config.Realm{},
			route{kind: kindNamespaces}, true},
		{"whoami ignores the lists", config.Realm{},
			route{kind: kindWhoami}, true},
		{"limits ignores the lists", config.Realm{},
			route{kind: kindLimits}, true},
		{"services admitted by namespace", config.Realm{Namespaces: []string{"payment"}},
			route{namespace: "payment", kind: kindServices}, true},
		{"services denied by namespace", config.Realm{Namespaces: []string{"billing"}, Services: wide},
			route{namespace: "payment", kind: kindServices}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := realmAllows(tc.realm, tc.route, http.MethodGet); got != tc.allowed {
				t.Errorf("realmAllows() = %v, want %v", got, tc.allowed)
			}
		})
	}
}
