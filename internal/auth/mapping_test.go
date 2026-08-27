package auth

import (
	"testing"

	"github.com/arloliu/profgate/internal/config"
)

func TestMapRealm(t *testing.T) {
	entries := func(pairs ...string) []config.OIDCMappingEntry {
		out := make([]config.OIDCMappingEntry, 0, len(pairs)/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, config.OIDCMappingEntry{Name: pairs[i], Realm: pairs[i+1]})
		}

		return out
	}
	rows := []struct {
		name    string
		mapping config.OIDCMapping
		claims  claims
		want    string
		ok      bool
	}{
		{
			name:    "user first",
			mapping: config.OIDCMapping{Users: entries("alice", "a"), Groups: entries("g", "b"), DefaultRealm: "c"},
			claims:  claims{Username: "alice", Groups: []string{"g"}},
			want:    "a", ok: true,
		},
		{
			name:    "group order",
			mapping: config.OIDCMapping{Groups: entries("g2", "b", "g1", "a")},
			claims:  claims{Username: "alice", Groups: []string{"g1", "g2"}},
			want:    "b", ok: true,
		},
		{
			name:    "default",
			mapping: config.OIDCMapping{Groups: entries("g", "b"), DefaultRealm: "c"},
			claims:  claims{Username: "alice"},
			want:    "c", ok: true,
		},
		{
			name:    "no default",
			mapping: config.OIDCMapping{Groups: entries("g", "b")},
			claims:  claims{Username: "alice"},
		},
		{
			name:    "exact group",
			mapping: config.OIDCMapping{Groups: entries("/eng/pay", "a")},
			claims:  claims{Username: "alice", Groups: []string{"pay"}},
		},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mapRealm(tc.mapping, tc.claims)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("mapRealm = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
