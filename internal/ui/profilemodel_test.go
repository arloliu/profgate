package ui

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// profileModelName is the module holding the Profile panel's pure functions.
const profileModelName = "profilemodel.js"

// profileModelFunctions is what the module exports, in the order of its export statement.
var profileModelFunctions = []string{"offeredProfiles", "secondsLimit", "defaultSeconds", "secondsValid"}

// loadProfileModel evaluates the Profile-panel model with its functions reachable as globals.
func loadProfileModel(tb testing.TB) *goja.Runtime {
	tb.Helper()

	return loadModel(tb, profileModelName, profileModelFunctions...)
}

// profileLimits is a /v1/limits body carrying what the Profile panel reads:
// the profiles the gateway offers, in its order, and the two duration bounds.
func profileLimits(profiles []string, cpuSeconds, traceSeconds any) map[string]any {
	return map[string]any{"profiles": profiles, "cpuSeconds": cpuSeconds, "traceSeconds": traceSeconds}
}

// realmProfiles is a /v1/whoami body whose realm admits the given profiles.
func realmProfiles(profiles any) map[string]any {
	return map[string]any{"realm": map[string]any{"profiles": profiles}}
}

func TestProfileModelShape(t *testing.T) {
	src := readSource(t, profileModelName)
	if bad := staticImportAnyRe.FindAllString(src, -1); len(bad) > 0 {
		t.Errorf("%s: imports something: %v", profileModelName, bad)
	}
	if bad := dynamicImportRe.FindAllString(src, -1); len(bad) > 0 {
		t.Errorf("%s: dynamic import: %v", profileModelName, bad)
	}
	if n := len(exportAnyRe.FindAllString(src, -1)); n != 1 {
		t.Errorf("%s: %d export statements, want one", profileModelName, n)
	}
	if rest := cutExport(t, profileModelName, src); exportAnyRe.MatchString(rest) {
		t.Errorf("%s: an export remains after the trailing statement is cut", profileModelName)
	}
	want := "export { " + strings.Join(profileModelFunctions, ", ") + " };"
	if !strings.Contains(src, want) {
		t.Errorf("%s: the export statement is not %q", profileModelName, want)
	}
}

// TestProfileModelOfferedProfiles drives the profile menu,
// which is the page's copy of the gateway's realm filter drawn before the page can ask.
// A copy that drifts offers a profile the gateway then refuses with realm_denied.
func TestProfileModelOfferedProfiles(t *testing.T) {
	// offered is not alphabetical, so a function that sorted would fail the order assertion.
	offered := []string{"cpu", "heap", "allocs", "goroutine", "trace"}
	limits := profileLimits(offered, 60, 5)
	cases := []struct {
		name   string
		limits any
		whoami any
		want   []string
	}{
		{"the wildcard offers everything in the limits' order", limits, realmProfiles([]string{"*"}), offered},
		{"a realm naming some offers those, in the limits' order", limits, realmProfiles([]string{"trace", "heap"}), []string{"heap", "trace"}},
		{"a profile the gateway does not offer adds nothing", limits, realmProfiles([]string{"heap", "block"}), []string{"heap"}},
		{"a realm naming none offers nothing", limits, realmProfiles([]string{}), []string{}},
		{"a realm whose profiles is not a list offers nothing", limits, realmProfiles("*"), []string{}},
		{"an absent /v1/limits offers nothing", nil, realmProfiles([]string{"*"}), []string{}},
		{"an absent /v1/whoami offers nothing", limits, nil, []string{}},
		{"a /v1/whoami carrying no realm offers nothing", limits, map[string]any{}, []string{}},
		{"a profiles that is a string offers nothing", map[string]any{"profiles": "cpu"}, realmProfiles([]string{"*"}), []string{}},
		{"a profiles that is an object offers nothing", map[string]any{"profiles": map[string]any{"cpu": true}}, realmProfiles([]string{"*"}), []string{}},
		{"a profiles that is null offers nothing", map[string]any{"profiles": nil}, realmProfiles([]string{"*"}), []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadProfileModel(t)
			got := callModel(t, vm, "offeredProfiles", tc.limits, tc.whoami)
			if !got.Unchanged {
				t.Errorf("offeredProfiles mutated an argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("offeredProfiles = %s, want %v", got.Result, tc.want)
			}
		})
	}
}

// TestProfileModelSecondsLimit drives the duration bound,
// whose zero is how the page knows to draw no duration input at all.
func TestProfileModelSecondsLimit(t *testing.T) {
	limits := profileLimits([]string{"cpu", "heap", "trace"}, 60, 5)
	cases := []struct {
		name    string
		limits  any
		profile string
		want    float64
	}{
		{"cpu is its configured bound", limits, "cpu", 60},
		{"trace is its own configured bound", limits, "trace", 5},
		{"heap carries no duration", limits, "heap", 0},
		{"goroutine carries no duration", limits, "goroutine", 0},
		{"an empty profile carries no duration", limits, "", 0},
		{"an absent /v1/limits bounds nothing", nil, "cpu", 0},
		{"a bound carried as a string is read as its number", profileLimits(nil, "45", 5), "cpu", 45},
		{"a bound carried as a word is no bound", profileLimits(nil, "soon", 5), "cpu", 0},
		{"a bound carried as null is no bound", profileLimits(nil, nil, 5), "cpu", 0},
		{"a bound carried as true reads as the number it coerces to", profileLimits(nil, 60, true), "trace", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadProfileModel(t)
			got := callModel(t, vm, "secondsLimit", tc.limits, tc.profile)
			if !got.Unchanged {
				t.Errorf("secondsLimit mutated an argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("secondsLimit = %s, want %v", got.Result, tc.want)
			}
		})
	}
}

// TestProfileModelDefaultSeconds drives the value the duration input starts at.
// The page sends it explicitly, so a request never rests on an upstream default above the configured bound.
func TestProfileModelDefaultSeconds(t *testing.T) {
	cases := []struct {
		name    string
		limits  any
		profile string
		want    string
	}{
		{"cpu's upstream default below the bound", profileLimits(nil, 60, 5), "cpu", "30"},
		{"cpu's upstream default equal to the bound", profileLimits(nil, 30, 5), "cpu", "30"},
		{"cpu's upstream default above the bound sends the bound", profileLimits(nil, 5, 5), "cpu", "5"},
		// trace's upstream default is 1, the least bound the gateway writes, so no whole bound sits below it.
		{"trace's upstream default below the bound", profileLimits(nil, 60, 5), "trace", "1"},
		{"trace's upstream default equal to the bound", profileLimits(nil, 60, 1), "trace", "1"},
		{"a profile with no bound has no default", profileLimits(nil, 60, 5), "heap", ""},
		{"an absent /v1/limits has no default", nil, "cpu", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadProfileModel(t)
			got := callModel(t, vm, "defaultSeconds", tc.limits, tc.profile)
			if !got.Unchanged {
				t.Errorf("defaultSeconds mutated an argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("defaultSeconds = %s, want %q", got.Result, tc.want)
			}
		})
	}
}

// TestProfileModelSecondsValid drives whether a typed duration disables Download.
// The field sends the text that was typed, and the gateway reads decimal digits and nothing else,
// so a spelling that reads as a whole number and is not digits is refused here as it is there.
func TestProfileModelSecondsValid(t *testing.T) {
	limits := profileLimits([]string{"cpu", "heap"}, 60, 5)
	cases := []struct {
		name    string
		profile string
		seconds string
		want    bool
	}{
		{"the bound itself", "cpu", "60", true},
		{"one", "cpu", "1", true},
		{"zero", "cpu", "0", false},
		{"a negative", "cpu", "-1", false},
		{"a fraction", "cpu", "1.5", false},
		{"the bound plus one", "cpu", "61", false},
		{"an empty value", "cpu", "", false},
		{"a non-numeric string", "cpu", "abc", false},
		{"an exponent the gateway refuses", "cpu", "1e1", false},
		{"a hexadecimal the gateway refuses", "cpu", "0x10", false},
		{"surrounding spaces the gateway refuses", "cpu", " 10 ", false},
		{"a plus sign the gateway refuses", "cpu", "+10", false},
		{"a trailing point the gateway refuses", "cpu", "10.", false},
		{"the bound itself over no bound", "heap", "60", true},
		{"one over no bound", "heap", "1", true},
		{"zero over no bound", "heap", "0", true},
		{"a negative over no bound", "heap", "-1", true},
		{"a fraction over no bound", "heap", "1.5", true},
		{"the bound plus one over no bound", "heap", "61", true},
		{"an empty value over no bound", "heap", "", true},
		{"a non-numeric string over no bound", "heap", "abc", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadProfileModel(t)
			got := callModel(t, vm, "secondsValid", limits, tc.profile, tc.seconds)
			if !got.Unchanged {
				t.Errorf("secondsValid mutated an argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("secondsValid(%q, %q) = %s, want %v", tc.profile, tc.seconds, got.Result, tc.want)
			}
		})
	}
}
