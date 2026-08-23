package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const lanesFile = "versions.yaml"

// goodLane is a lane that passes validation; each failing case mutates one field of it.
func goodLane() Lane {
	return Lane{
		Name:          "current",
		NetworkPolicy: true,
		Kind:          "0.32.0",
		Image:         "kindest/node:v1.36.1@sha256:" + strings.Repeat("ab", 32),
	}
}

// writeLanes renders lanes as YAML in a temporary directory and returns the file path.
func writeLanes(t *testing.T, lanes []Lane) string {
	t.Helper()
	var sb strings.Builder
	for _, l := range lanes {
		sb.WriteString("- name: \"" + l.Name + "\"\n")
		sb.WriteString("  frozen: " + boolString(l.Frozen) + "\n")
		sb.WriteString("  degraded: " + boolString(l.Degraded) + "\n")
		sb.WriteString("  networkPolicy: " + boolString(l.NetworkPolicy) + "\n")
		sb.WriteString("  kind: \"" + l.Kind + "\"\n")
		sb.WriteString("  image: \"" + l.Image + "\"\n")
	}
	path := filepath.Join(t.TempDir(), lanesFile)
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestLoadLanesRealFile(t *testing.T) {
	lanes, err := LoadLanes(lanesFile)
	if err != nil {
		t.Fatalf("LoadLanes(%s): %v", lanesFile, err)
	}
	want := []string{"1.23", "1.24", "current"}
	if got := LaneNames(lanes, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("lane names = %v, want %v", got, want)
	}
}

func TestLaneNames(t *testing.T) {
	lanes := []Lane{
		{Name: "1.23", Frozen: true},
		{Name: "1.24", Frozen: true},
		{Name: "current"},
	}
	tests := []struct {
		name         string
		unfrozenOnly bool
		want         []string
	}{
		{name: "all lanes in file order", unfrozenOnly: false, want: []string{"1.23", "1.24", "current"}},
		{name: "unfrozen only", unfrozenOnly: true, want: []string{"current"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LaneNames(lanes, tc.unfrozenOnly); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("LaneNames(unfrozenOnly=%v) = %v, want %v", tc.unfrozenOnly, got, tc.want)
			}
		})
	}
}

func TestLoadLanesRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(l *Lane)
		wantErr string
	}{
		{
			name:    "registry-prefixed image",
			mutate:  func(l *Lane) { l.Image = "docker.io/" + l.Image },
			wantErr: "registry host",
		},
		{
			name:    "localhost registry",
			mutate:  func(l *Lane) { l.Image = "localhost:5000/" + l.Image },
			wantErr: "registry host",
		},
		{
			name:    "short digest",
			mutate:  func(l *Lane) { l.Image = "kindest/node:v1.36.1@sha256:abc123" },
			wantErr: "digest",
		},
		{
			name:    "non-hex digest",
			mutate:  func(l *Lane) { l.Image = "kindest/node:v1.36.1@sha256:" + strings.Repeat("zz", 32) },
			wantErr: "digest",
		},
		{
			name:    "no digest",
			mutate:  func(l *Lane) { l.Image = "kindest/node:v1.36.1" },
			wantErr: "digest",
		},
		{
			name:    "unknown kind version",
			mutate:  func(l *Lane) { l.Kind = "0.99.0" },
			wantErr: "kind",
		},
		{
			name:    "degraded on a non-frozen lane",
			mutate:  func(l *Lane) { l.Degraded = true },
			wantErr: "degraded",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := goodLane()
			tc.mutate(&l)
			_, err := LoadLanes(writeLanes(t, []Lane{l}))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadLanes error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadLanesRejectsMatrix(t *testing.T) {
	t.Run("no lane with networkPolicy true", func(t *testing.T) {
		l := goodLane()
		l.NetworkPolicy = false
		_, err := LoadLanes(writeLanes(t, []Lane{l}))
		if err == nil || !strings.Contains(err.Error(), "networkPolicy") {
			t.Fatalf("LoadLanes error = %v, want one mentioning networkPolicy", err)
		}
	})
	t.Run("duplicate lane name", func(t *testing.T) {
		_, err := LoadLanes(writeLanes(t, []Lane{goodLane(), goodLane()}))
		if err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("LoadLanes error = %v, want one mentioning a lane defined twice", err)
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), lanesFile)
		body := "- name: \"current\"\n  networkPolicy: true\n  kind: \"0.32.0\"\n  image: \"" + goodLane().Image + "\"\n  extra: 1\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadLanes(path); err == nil {
			t.Fatal("LoadLanes accepted an unknown field")
		}
	})
	t.Run("degraded on a frozen lane is allowed", func(t *testing.T) {
		frozen := goodLane()
		frozen.Name = "1.23"
		frozen.Frozen = true
		frozen.Degraded = true
		frozen.NetworkPolicy = false
		frozen.Kind = "0.22.0"
		if _, err := LoadLanes(writeLanes(t, []Lane{frozen, goodLane()})); err != nil {
			t.Fatalf("LoadLanes rejected a degraded frozen lane: %v", err)
		}
	})
}

// TestKnownKindVersionsMatchMise pins the kind versions a lane may name to the ones
// mise.toml installs, so a lane cannot name a binary `mise x kind@<v>` cannot run.
func TestKnownKindVersionsMatchMise(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "mise.toml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^kind = \[(.*)\]`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("mise.toml has no kind tool line")
	}
	var want []string
	for _, v := range strings.Split(m[1], ",") {
		want = append(want, strings.Trim(strings.TrimSpace(v), `"`))
	}
	got := KnownKindVersions()
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("known kind versions = %v, mise.toml pins %v", got, want)
	}
}

func TestScenariosRegistry(t *testing.T) {
	all := Scenarios()
	if len(all) == 0 {
		t.Fatal("no scenarios registered")
	}
	seen := map[string]bool{}
	for _, s := range all {
		if seen[s.Name] {
			t.Fatalf("scenario %q registered twice", s.Name)
		}
		seen[s.Name] = true
	}
	i := slices.IndexFunc(all, func(s Scenario) bool { return s.Name == "convergence on ready" })
	if i < 0 {
		t.Fatal("no scenario named \"convergence on ready\"")
	}
	if !all[i].NeedsPodReach {
		t.Fatal("\"convergence on ready\" flips readiness through the test app, so it must declare NeedsPodReach")
	}

	// Scenarios hands out a copy: mutating it must not reach the registry.
	all[i].Name = "mutated"
	if Scenarios()[i].Name != "convergence on ready" {
		t.Fatal("Scenarios returned the registry itself rather than a copy")
	}
}

func TestScenarioSkips(t *testing.T) {
	tests := []struct {
		name string
		lane Lane
		want []string
	}{
		{
			name: "degraded lane skips every scenario that needs Pod reach",
			lane: Lane{Name: "1.23", Frozen: true, Degraded: true, NetworkPolicy: true},
			want: []string{"ineligible pods", "convergence on ready", "profiles parse", "replicas agree", "api outage"},
		},
		{
			name: "lane without NetworkPolicy enforcement skips only api outage",
			lane: Lane{Name: "1.24", Frozen: true, NetworkPolicy: false},
			want: []string{"api outage"},
		},
		{
			name: "full lane skips nothing",
			lane: Lane{Name: "current", NetworkPolicy: true},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, s := range Scenarios() {
				skip, why := s.Skips(tc.lane)
				if !skip {
					if why != "" {
						t.Fatalf("scenario %q: not skipped but reason %q given", s.Name, why)
					}
					continue
				}
				if !strings.Contains(why, s.Name) || !strings.Contains(why, tc.lane.Name) {
					t.Fatalf("skip reason %q does not name scenario %q and lane %q", why, s.Name, tc.lane.Name)
				}
				got = append(got, s.Name)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("skipped = %v, want %v", got, tc.want)
			}
		})
	}
}
