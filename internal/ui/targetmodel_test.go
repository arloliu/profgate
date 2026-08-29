package ui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// targetModelName is the module holding the targets fetch's three pure functions.
const targetModelName = "targetmodel.js"

// targetModelFunctions is what the module exports, in the order of its export statement.
var targetModelFunctions = []string{"targetsQuery", "retryWithoutExplain", "targetSummary"}

// exclusionReasons is the gateway's exclusion vocabulary in report order,
// written out here rather than read from the gateway package,
// so a reason added there without wording on the console turns this suite red.
var exclusionReasons = []string{
	"pod_terminating",
	"pod_not_running",
	"pod_not_ready",
	"endpoint_missing",
	"endpoint_not_ready",
	"endpoint_address_mismatch",
	"endpoint_address_conflict",
	"port_name_not_declared",
	"version_mismatch",
	"pod_name_mismatch",
}

// reasonWording is the wording table of the console spec's Controls section.
var reasonWording = map[string]string{
	"pod_terminating":           "Pods being deleted",
	"pod_not_running":           "Pods not in phase Running",
	"pod_not_ready":             "Pods whose Ready condition is not True",
	"endpoint_missing":          "Pods with no trusted EndpointSlice entry naming the current Pod identity",
	"endpoint_not_ready":        "Pods whose EndpointSlice entry is not ready",
	"endpoint_address_mismatch": "Pods whose EndpointSlice address is not one the Pod holds",
	"endpoint_address_conflict": "Pods whose EndpointSlice entries disagree on the address",
	"port_name_not_declared":    "Pods declaring no TCP container port of the effective pprof port name",
	"version_mismatch":          "Pods carrying another version",
	"pod_name_mismatch":         "Pods with another name",
}

// loadTargetModel evaluates the target model with its three functions reachable as globals.
func loadTargetModel(tb testing.TB) *goja.Runtime {
	tb.Helper()

	return loadModel(tb, targetModelName, targetModelFunctions...)
}

func TestTargetModelShape(t *testing.T) {
	src := readSource(t, targetModelName)
	if bad := staticImportAnyRe.FindAllString(src, -1); len(bad) > 0 {
		t.Errorf("%s: imports something: %v", targetModelName, bad)
	}
	if bad := dynamicImportRe.FindAllString(src, -1); len(bad) > 0 {
		t.Errorf("%s: dynamic import: %v", targetModelName, bad)
	}
	if n := len(exportAnyRe.FindAllString(src, -1)); n != 1 {
		t.Errorf("%s: %d export statements, want one", targetModelName, n)
	}
	if rest := cutExport(t, targetModelName, src); exportAnyRe.MatchString(rest) {
		t.Errorf("%s: an export remains after the trailing statement is cut", targetModelName)
	}
	want := "export { " + strings.Join(targetModelFunctions, ", ") + " };"
	if !strings.Contains(src, want) {
		t.Errorf("%s: the export statement is not %q", targetModelName, want)
	}
}

// TestTargetModelNamesEveryReason pins each reason of the gateway's vocabulary to the module as a literal.
func TestTargetModelNamesEveryReason(t *testing.T) {
	src := readSource(t, targetModelName)
	if len(exclusionReasons) != 10 {
		t.Fatalf("the vocabulary written out here holds %d reasons, want ten", len(exclusionReasons))
	}
	for _, reason := range exclusionReasons {
		if !strings.Contains(src, `"`+reason+`"`) {
			t.Errorf("%s: does not hold the reason %q as a literal", targetModelName, reason)
		}
	}
}

func TestTargetModelQuery(t *testing.T) {
	ports := []struct {
		name   string
		params map[string]string
	}{
		{"default", map[string]string{}},
		{"numeric selection", map[string]string{"port": "6061"}},
		{"named selection", map[string]string{"portName": "pprof-alt"}},
	}
	for _, p := range ports {
		for _, withExplain := range []bool{true, false} {
			name := p.name + "/without explain"
			if withExplain {
				name = p.name + "/with explain"
			}
			t.Run(name, func(t *testing.T) {
				vm := loadTargetModel(t)
				got := callModel(t, vm, "targetsQuery", p.params, withExplain)
				if !got.Unchanged {
					t.Errorf("targetsQuery mutated the port selection")
				}
				var query map[string]string
				if err := json.Unmarshal(got.Result, &query); err != nil {
					t.Fatalf("decode %s: %v", got.Result, err)
				}
				want := map[string]string{}
				for k, v := range p.params {
					want[k] = v
				}
				if withExplain {
					want["explain"] = "true"
				}
				if !reflect.DeepEqual(query, want) {
					t.Errorf("targetsQuery = %v, want %v", query, want)
				}
				for _, never := range []string{"version", "pod"} {
					if _, ok := query[never]; ok {
						t.Errorf("targetsQuery sends %s=", never)
					}
				}
			})
		}
	}
}

func TestTargetModelRetryWithoutExplain(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		code        string
		sentExplain bool
		retried     bool
	}{
		{"400 invalid_parameter after explain", 400, "invalid_parameter", true, true},
		{"400 invalid_parameter without explain", 400, "invalid_parameter", false, false},
		{"400 with another code", 400, "port_not_allowed", true, false},
		{"400 with no code", 400, "", true, false},
		{"403", 403, "realm_denied", true, false},
		{"404", 404, "service_not_found", true, false},
		{"503", 503, "not_ready", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadTargetModel(t)
			got := callModel(t, vm, "retryWithoutExplain", tc.status, tc.code, tc.sentExplain)
			var retried bool
			if err := json.Unmarshal(got.Result, &retried); err != nil {
				t.Fatalf("decode %s: %v", got.Result, err)
			}
			if retried != tc.retried {
				t.Errorf("retryWithoutExplain(%d, %q, %v) = %v, want %v", tc.status, tc.code, tc.sentExplain, retried, tc.retried)
			}
		})
	}
}

// TestTargetModelRetryIsOnce drives the two calls a fetch makes:
// the retry sends the same port selection without explain,
// and a second refusal is not retried because the retry did not carry explain.
func TestTargetModelRetryIsOnce(t *testing.T) {
	vm := loadTargetModel(t)
	port := map[string]string{"port": "6061"}
	first := callModel(t, vm, "targetsQuery", port, true)
	retry := callModel(t, vm, "targetsQuery", port, false)
	if !sameJSON(t, first.Result, map[string]string{"port": "6061", "explain": "true"}) {
		t.Errorf("first query = %s", first.Result)
	}
	if !sameJSON(t, retry.Result, map[string]string{"port": "6061"}) {
		t.Errorf("retry query = %s", retry.Result)
	}
	var again bool
	if err := json.Unmarshal(callModel(t, vm, "retryWithoutExplain", 400, "invalid_parameter", false).Result, &again); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if again {
		t.Errorf("a second 400 invalid_parameter was retried")
	}
}

// summaryRow is one row of the empty state.
type summaryRow struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	Text   string `json:"text"`
}

// emptyTargets is what targetSummary reports when targets is empty.
type emptyTargets struct {
	Kind string       `json:"kind"`
	Rows []summaryRow `json:"rows"`
}

// summary is what targetSummary returns.
type summary struct {
	Pods     []string      `json:"pods"`
	Versions []string      `json:"versions"`
	Empty    *emptyTargets `json:"empty"`
}

func target(pod, version string) map[string]any {
	return map[string]any{"namespace": "ns", "service": "svc", "pod": pod, "node": "node-a", "version": version}
}

func excluded(reason string, count int) map[string]any {
	return map[string]any{"reason": reason, "count": count}
}

// summarize calls targetSummary and fails when the body handed in was changed.
func summarize(tb testing.TB, vm *goja.Runtime, body map[string]any) summary {
	tb.Helper()

	got := callModel(tb, vm, "targetSummary", body)
	if !got.Unchanged {
		tb.Errorf("targetSummary mutated its argument")
	}
	var s summary
	if err := json.Unmarshal(got.Result, &s); err != nil {
		tb.Fatalf("decode %s: %v", got.Result, err)
	}

	return s
}

func TestTargetModelSummaryWithTargets(t *testing.T) {
	vm := loadTargetModel(t)
	body := map[string]any{
		"targets": []map[string]any{
			target("api-2", "v2"),
			target("api-0", "v1"),
			target("api-1", ""),
			target("api-3", "v2"),
		},
		"selectorMatched": 6,
		"excluded":        []map[string]any{excluded("pod_not_ready", 2)},
	}
	got := summarize(t, vm, body)
	if want := []string{"api-2", "api-0", "api-1", "api-3"}; !reflect.DeepEqual(got.Pods, want) {
		t.Errorf("pods = %v, want %v", got.Pods, want)
	}
	if want := []string{"v2", "v1"}; !reflect.DeepEqual(got.Versions, want) {
		t.Errorf("versions = %v, want %v", got.Versions, want)
	}
	if got.Empty != nil {
		t.Errorf("empty = %+v, want null beside a non-empty targets", got.Empty)
	}
}

func TestTargetModelSummaryEmpty(t *testing.T) {
	vocabulary := make([]map[string]any, 0, len(exclusionReasons))
	vocabularyRows := make([]summaryRow, 0, len(exclusionReasons))
	for i, reason := range exclusionReasons {
		vocabulary = append(vocabulary, excluded(reason, i+1))
		vocabularyRows = append(vocabularyRows, summaryRow{reason, i + 1, reasonWording[reason]})
	}
	shuffled := make([]map[string]any, 0, len(exclusionReasons))
	shuffledRows := make([]summaryRow, 0, len(exclusionReasons))
	for i := len(exclusionReasons) - 1; i >= 0; i-- {
		shuffled = append(shuffled, vocabulary[i])
		shuffledRows = append(shuffledRows, vocabularyRows[i])
	}

	cases := []struct {
		name string
		body map[string]any
		want emptyTargets
	}{
		{"one entry",
			map[string]any{"targets": []any{}, "selectorMatched": 3, "excluded": []map[string]any{excluded("pod_not_ready", 3)}},
			emptyTargets{"reasons", []summaryRow{{"pod_not_ready", 3, "Pods whose Ready condition is not True"}}}},
		{"the vocabulary in order",
			map[string]any{"targets": []any{}, "selectorMatched": 55, "excluded": vocabulary},
			emptyTargets{"reasons", vocabularyRows}},
		{"the vocabulary shuffled keeps its order",
			map[string]any{"targets": []any{}, "selectorMatched": 55, "excluded": shuffled},
			emptyTargets{"reasons", shuffledRows}},
		{"an unrecognized reason is its own text",
			map[string]any{"targets": []any{}, "selectorMatched": 4,
				"excluded": []map[string]any{excluded("pod_not_ready", 1), excluded("pod_on_fire", 3)}},
			emptyTargets{"reasons", []summaryRow{
				{"pod_not_ready", 1, "Pods whose Ready condition is not True"},
				{"pod_on_fire", 3, "pod_on_fire"}}}},
		{"selectorMatched of 0 with no excluded",
			map[string]any{"targets": []any{}, "selectorMatched": 0},
			emptyTargets{"noSelector", nil}},
		{"selectorMatched of 0 beside excluded",
			map[string]any{"targets": []any{}, "selectorMatched": 0, "excluded": []map[string]any{excluded("pod_not_ready", 3)}},
			emptyTargets{"noSelector", nil}},
		{"no excluded field",
			map[string]any{"targets": []any{}},
			emptyTargets{"plain", nil}},
		{"excluded of []",
			map[string]any{"targets": []any{}, "selectorMatched": 2, "excluded": []any{}},
			emptyTargets{"plain", nil}},
		{"a count of 1 reads the plural",
			map[string]any{"targets": []any{}, "selectorMatched": 1, "excluded": []map[string]any{excluded("pod_terminating", 1)}},
			emptyTargets{"reasons", []summaryRow{{"pod_terminating", 1, "Pods being deleted"}}}},
		{"a count of 9 reads the same plural",
			map[string]any{"targets": []any{}, "selectorMatched": 9, "excluded": []map[string]any{excluded("pod_terminating", 9)}},
			emptyTargets{"reasons", []summaryRow{{"pod_terminating", 9, "Pods being deleted"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadTargetModel(t)
			got := summarize(t, vm, tc.body)
			if len(got.Pods) != 0 || len(got.Versions) != 0 {
				t.Errorf("pods = %v, versions = %v, want none", got.Pods, got.Versions)
			}
			if got.Empty == nil {
				t.Fatalf("empty = null, want %+v", tc.want)
			}
			if got.Empty.Kind != tc.want.Kind {
				t.Errorf("empty.kind = %q, want %q", got.Empty.Kind, tc.want.Kind)
			}
			if len(got.Empty.Rows) != 0 || len(tc.want.Rows) != 0 {
				if !reflect.DeepEqual(got.Empty.Rows, tc.want.Rows) {
					t.Errorf("empty.rows = %+v, want %+v", got.Empty.Rows, tc.want.Rows)
				}
			}
		})
	}
}
