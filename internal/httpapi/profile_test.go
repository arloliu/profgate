package httpapi

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
)

func TestLookupProfile(t *testing.T) {
	for _, name := range config.Profiles() {
		spec, ok := lookupProfile(name)
		if !ok {
			t.Errorf("lookupProfile(%q) missing", name)
		}
		takes := name == "cpu" || name == "trace"
		if spec.takesSeconds != takes {
			t.Errorf("%s takesSeconds = %v, want %v", name, spec.takesSeconds, takes)
		}
	}
	if _, ok := lookupProfile("bogus"); ok {
		t.Error("lookupProfile(bogus) found a profile")
	}
	cpu, _ := lookupProfile("cpu")
	trace, _ := lookupProfile("trace")
	heap, _ := lookupProfile("heap")
	if cpu.path != "/debug/pprof/profile" || cpu.defaultSeconds != 30 {
		t.Errorf("cpu = %+v", cpu)
	}
	if trace.path != "/debug/pprof/trace" || trace.defaultSeconds != 1 {
		t.Errorf("trace = %+v", trace)
	}
	if heap.path != "/debug/pprof/heap" || heap.defaultSeconds != 0 {
		t.Errorf("heap = %+v", heap)
	}
}

func TestParseProfileParams(t *testing.T) {
	limits := config.LimitsConfig{CPUSeconds: 60, TraceSeconds: 10}
	cases := []struct {
		name    string
		profile string
		query   string
		want    profileParams
		code    string
	}{
		{"cpu default", "cpu", "", profileParams{seconds: 30}, ""},
		{"trace default", "trace", "", profileParams{seconds: 1}, ""},
		{"heap has no duration", "heap", "", profileParams{}, ""},
		{"explicit seconds", "cpu", "seconds=5", profileParams{seconds: 5}, ""},
		{"all parameters", "cpu", "seconds=5&pod=a.b&version=1.0&strategy=random", profileParams{seconds: 5, pod: "a.b", version: "1.0"}, ""},
		{"port beside pod", "heap", "pod=a.b&port=6061", profileParams{pod: "a.b", port: portParams{sel: k8s.PortSelection{Port: 6061}, sent: "6061"}}, ""},
		{"portName beside pod", "heap", "pod=a.b&portName=pprof-alt", profileParams{pod: "a.b", port: portParams{sel: k8s.PortSelection{PortName: "pprof-alt"}, sent: "pprof-alt"}}, ""},
		{"port and portName", "heap", "port=6060&portName=pprof", profileParams{}, "invalid_parameter"},
		{"port malformed", "heap", "port=abc", profileParams{}, "invalid_parameter"},
		{"cpu over limit", "cpu", "seconds=61", profileParams{}, "seconds_exceeds_limit"},
		{"trace default over limit", "trace", "", profileParams{seconds: 1}, ""},
		{"trace over limit", "trace", "seconds=11", profileParams{}, "seconds_exceeds_limit"},
		{"unknown beats limit", "cpu", "seconds=61&foo=1", profileParams{}, "invalid_parameter"},
		{"seconds on heap", "heap", "seconds=1", profileParams{}, "invalid_parameter"},
		{"seconds not a number", "cpu", "seconds=abc", profileParams{}, "invalid_parameter"},
		{"seconds zero", "cpu", "seconds=0", profileParams{}, "invalid_parameter"},
		{"seconds negative", "cpu", "seconds=-1", profileParams{}, "invalid_parameter"},
		{"seconds signed", "cpu", "seconds=+1", profileParams{}, "invalid_parameter"},
		{"seconds above the grammar", "cpu", "seconds=86401", profileParams{}, "invalid_parameter"},
		{"seconds huge", "cpu", "seconds=99999999999999999999", profileParams{}, "invalid_parameter"},
		{"seconds repeated", "cpu", "seconds=1&seconds=2", profileParams{}, "invalid_parameter"},
		{"seconds empty", "cpu", "seconds=", profileParams{}, "invalid_parameter"},
		{"pod empty", "heap", "pod=", profileParams{}, "invalid_parameter"},
		{"pod repeated", "heap", "pod=a&pod=b", profileParams{}, "invalid_parameter"},
		{"pod not a subdomain", "heap", "pod=Bad_", profileParams{}, "invalid_parameter"},
		{"version empty", "heap", "version=", profileParams{}, "invalid_parameter"},
		{"version repeated", "heap", "version=a&version=b", profileParams{}, "invalid_parameter"},
		{"strategy unknown", "heap", "strategy=roundrobin", profileParams{}, "invalid_parameter"},
		{"strategy empty", "heap", "strategy=", profileParams{}, "invalid_parameter"},
		{"unknown parameter", "heap", "foo=1", profileParams{}, "invalid_parameter"},
		{"bare key", "heap", "pod", profileParams{}, "invalid_parameter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, ok := lookupProfile(tc.profile)
			if !ok {
				t.Fatalf("unknown profile %q", tc.profile)
			}
			values, qerr := url.ParseQuery(tc.query)
			if qerr != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.query, qerr)
			}
			got, err := parseProfileParams(values, spec, limits)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("error = %+v, want none", err)
				}
				if got != tc.want {
					t.Errorf("params = %+v, want %+v", got, tc.want)
				}

				return
			}
			if err == nil {
				t.Fatalf("params = %+v, want code %s", got, tc.code)
			}
			if err.code != tc.code || err.status != http.StatusBadRequest {
				t.Errorf("error = %d %s, want 400 %s", err.status, err.code, tc.code)
			}
		})
	}
}
