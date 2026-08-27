package auth

import (
	"slices"

	"github.com/arloliu/profgate/internal/config"
)

// mapRealm resolves the realm a verified token acts in: the username claim
// against mapping.users, then the groups claim against mapping.groups in the
// order written, then defaultRealm when it is set.
// No match is not ok; the caller answers no_realm rather than falling
// through to any other realm.
func mapRealm(m config.OIDCMapping, c claims) (string, bool) {
	for _, u := range m.Users {
		if u.Name == c.Username {
			return u.Realm, true
		}
	}
	for _, g := range m.Groups {
		if slices.Contains(c.Groups, g.Name) {
			return g.Realm, true
		}
	}
	if m.DefaultRealm != "" {
		return m.DefaultRealm, true
	}

	return "", false
}
