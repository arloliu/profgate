package httpapi

import (
	"slices"

	"github.com/arloliu/profgate/internal/config"
)

// anonymousPrincipal is the principal every request carries while authentication is disabled.
const anonymousPrincipal = "anonymous"

// wildcard is the list entry that matches every value.
const wildcard = "*"

// principalRealm resolves the request's principal and its realm from cfg.
// The only authentication mode is disabled,
// which attributes every request to the anonymous principal and maps it to auth.anonymousRealm.
// A realm the configuration does not hold denies, so a bad snapshot fails closed rather than open.
func principalRealm(cfg *config.Config) (principal string, realm config.Realm, ok bool) {
	realm, ok = cfg.Realms[cfg.Auth.AnonymousRealm]

	return anonymousPrincipal, realm, ok
}

// realmAllows evaluates namespace, then Service, then, for the profile endpoint, profile.
// Each list matches by the wildcard or the exact string; there is no prefix or glob matching.
func realmAllows(r config.Realm, rt route) bool {
	if !listAllows(r.Namespaces, rt.namespace) || !listAllows(r.Services, rt.service) {
		return false
	}
	if rt.isProfile && !listAllows(r.Profiles, rt.profile) {
		return false
	}

	return true
}

// listAllows reports whether list admits value.
func listAllows(list []string, value string) bool {
	return slices.Contains(list, wildcard) || slices.Contains(list, value)
}
