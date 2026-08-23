package httpapi

import (
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
			route{namespace: "payment", service: "payment-api", profile: "cpu", isProfile: true}, true},
		{"exact match on every list", config.Realm{Namespaces: []string{"payment"}, Services: []string{"payment-api"}, Profiles: []string{"cpu"}},
			route{namespace: "payment", service: "payment-api", profile: "cpu", isProfile: true}, true},
		{"namespace mismatch", config.Realm{Namespaces: []string{"billing"}, Services: wide, Profiles: wide},
			route{namespace: "payment", service: "payment-api", profile: "cpu", isProfile: true}, false},
		{"service mismatch", config.Realm{Namespaces: wide, Services: []string{"billing-api"}, Profiles: wide},
			route{namespace: "payment", service: "payment-api", profile: "cpu", isProfile: true}, false},
		{"profile mismatch", config.Realm{Namespaces: wide, Services: wide, Profiles: []string{"heap"}},
			route{namespace: "payment", service: "payment-api", profile: "cpu", isProfile: true}, false},
		{"profiles ignored for targets", config.Realm{Namespaces: wide, Services: wide, Profiles: []string{"heap"}},
			route{namespace: "payment", service: "payment-api"}, true},
		{"no prefix matching", config.Realm{Namespaces: []string{"pay"}, Services: wide, Profiles: wide},
			route{namespace: "payment", service: "payment-api"}, false},
		{"wildcard among exact entries", config.Realm{Namespaces: []string{"billing", "*"}, Services: wide, Profiles: wide},
			route{namespace: "payment", service: "payment-api"}, true},
		{"empty lists deny", config.Realm{},
			route{namespace: "payment", service: "payment-api"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := realmAllows(tc.realm, tc.route); got != tc.allowed {
				t.Errorf("realmAllows() = %v, want %v", got, tc.allowed)
			}
		})
	}
}
