package ui

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// portModelName is the module holding the port control's two pure functions.
const portModelName = "portmodel.js"

// exportRe matches the module's one export statement, which the interpreter
// cannot parse and the test cuts off before evaluating the rest.
var exportRe = regexp.MustCompile(`(?m)^export \{[^}]*\};\s*$`)

// staticImportAnyRe matches any static import statement; the model may hold none.
var staticImportAnyRe = regexp.MustCompile(`(?m)^import\b`)

// exportAnyRe matches any export statement, of any form.
var exportAnyRe = regexp.MustCompile(`(?m)^export\b`)

// cutExport returns the source of the named module with its trailing export statement removed.
// It fails when the statement is absent, repeated, or not the last statement,
// since evaluating the rest is only safe under that shape.
func cutExport(tb testing.TB, name, src string) string {
	tb.Helper()

	locs := exportRe.FindAllStringIndex(src, -1)
	if len(locs) != 1 {
		tb.Fatalf("%s: want exactly one export statement, found %d", name, len(locs))
	}
	if strings.TrimSpace(src[locs[0][1]:]) != "" {
		tb.Fatalf("%s: the export statement is not the last statement", name)
	}

	return src[:locs[0][0]]
}

// loadModel evaluates the named module in a fresh interpreter,
// and returns it with each named function reachable as a global.
func loadModel(tb testing.TB, name string, fns ...string) *goja.Runtime {
	tb.Helper()

	vm := goja.New()
	if _, err := vm.RunString(cutExport(tb, name, readSource(tb, name))); err != nil {
		tb.Fatalf("evaluate %s: %v", name, err)
	}
	for _, fn := range fns {
		if _, ok := goja.AssertFunction(vm.Get(fn)); !ok {
			tb.Fatalf("%s: %s is not a function", name, fn)
		}
	}

	return vm
}

// loadPortModel evaluates the port model with both functions reachable as globals.
func loadPortModel(tb testing.TB) *goja.Runtime {
	tb.Helper()

	return loadModel(tb, portModelName, "deriveControl", "applyInput")
}

// modelResult is what callModel returns: the function's result as JSON, and
// whether the arguments it was handed are byte-for-byte what they were.
type modelResult struct {
	Result    json.RawMessage `json:"result"`
	Unchanged bool            `json:"unchanged"`
}

// callModel hands args to fn the way the page would, as values parsed from
// JSON, and reports the result and whether any argument was mutated.
func callModel(tb testing.TB, vm *goja.Runtime, fn string, args ...any) modelResult {
	tb.Helper()

	encoded, err := json.Marshal(args)
	if err != nil {
		tb.Fatalf("encode arguments: %v", err)
	}
	if err := vm.Set("args", string(encoded)); err != nil {
		tb.Fatalf("set args: %v", err)
	}
	script := fmt.Sprintf(`(function () {
		const a = JSON.parse(args);
		const before = JSON.stringify(a);
		const r = %s(...a);
		return JSON.stringify({ result: r === undefined ? null : r, unchanged: JSON.stringify(a) === before });
	})()`, fn)
	v, err := vm.RunString(script)
	if err != nil {
		tb.Fatalf("call %s(%s): %v", fn, encoded, err)
	}
	var out modelResult
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		tb.Fatalf("decode %s result %q: %v", fn, v.String(), err)
	}

	return out
}

// decode unmarshals raw into a generic value so two JSON documents compare by content.
func decode(tb testing.TB, raw []byte) any {
	tb.Helper()

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		tb.Fatalf("decode %s: %v", raw, err)
	}

	return v
}

// sameJSON reports whether got, a JSON document, holds the same content as want, a Go value.
func sameJSON(tb testing.TB, got json.RawMessage, want any) bool {
	tb.Helper()

	w, err := json.Marshal(want)
	if err != nil {
		tb.Fatalf("encode want: %v", err)
	}

	return reflect.DeepEqual(decode(tb, got), decode(tb, w))
}

func TestPortModelShape(t *testing.T) {
	src := readSource(t, portModelName)
	if bad := staticImportAnyRe.FindAllString(src, -1); len(bad) > 0 {
		t.Errorf("%s: imports something: %v", portModelName, bad)
	}
	if bad := dynamicImportRe.FindAllString(src, -1); len(bad) > 0 {
		t.Errorf("%s: dynamic import: %v", portModelName, bad)
	}
	if n := len(exportAnyRe.FindAllString(src, -1)); n != 1 {
		t.Errorf("%s: %d export statements, want one", portModelName, n)
	}
	if rest := cutExport(t, portModelName, src); exportAnyRe.MatchString(rest) {
		t.Errorf("%s: an export remains after the trailing statement is cut", portModelName)
	}
	if !strings.Contains(src, "export { deriveControl, applyInput };") {
		t.Errorf("%s: the export statement does not name exactly deriveControl and applyInput", portModelName)
	}
}

// option is one entry of the menu deriveControl builds.
type option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// control is the shape deriveControl returns.
type control struct {
	Options     []option `json:"options"`
	NumberField bool     `json:"numberField"`
	NameField   bool     `json:"nameField"`
}

func sel(kind string, value any) map[string]any {
	return map[string]any{kind: value}
}

func TestPortModelDeriveControl(t *testing.T) {
	defaults := []struct {
		name  string
		pprof map[string]any
		label string
	}{
		{"numeric default", sel("port", 6060), "default (6060)"},
		{"named default", sel("portName", "pprof"), "default (pprof)"},
	}
	cases := []struct {
		name        string
		selections  []map[string]any
		options     []option // after the default option
		numberField bool
		nameField   bool
	}{
		{"empty", nil, nil, false, false},
		{"one port", []map[string]any{sel("port", 6061)}, []option{{"port:6061", "6061"}}, false, false},
		{"one name", []map[string]any{sel("portName", "pprof-alt")}, []option{{"name:pprof-alt", "pprof-alt"}}, false, false},
		{"both kinds in the configured order",
			[]map[string]any{sel("portName", "pprof-alt"), sel("port", 6061)},
			[]option{{"name:pprof-alt", "pprof-alt"}, {"port:6061", "6061"}}, false, false},
		{"port wildcard", []map[string]any{sel("port", "*")}, nil, true, false},
		{"name wildcard", []map[string]any{sel("portName", "*")}, nil, false, true},
		{"both wildcards", []map[string]any{sel("port", "*"), sel("portName", "*")}, nil, true, true},
		{"wildcards beside entries",
			[]map[string]any{sel("port", "*"), sel("port", 6061), sel("portName", "*")},
			[]option{{"port:6061", "6061"}}, true, true},
	}
	for _, def := range defaults {
		for _, tc := range cases {
			t.Run(def.name+"/"+tc.name, func(t *testing.T) {
				vm := loadPortModel(t)
				pprof := map[string]any{"default": def.pprof, "allowedSelections": tc.selections}
				if tc.selections == nil {
					pprof["allowedSelections"] = []map[string]any{}
				}
				got := callModel(t, vm, "deriveControl", pprof)
				if !got.Unchanged {
					t.Errorf("deriveControl mutated its argument")
				}
				want := control{Options: append([]option{{"default", def.label}}, tc.options...), NumberField: tc.numberField, NameField: tc.nameField}
				if !sameJSON(t, got.Result, want) {
					t.Errorf("deriveControl = %s, want %+v", got.Result, want)
				}
			})
		}
	}
}

func TestPortModelDefaultIsNotRepeated(t *testing.T) {
	cases := []struct {
		name       string
		def        map[string]any
		selections []map[string]any
		options    []option
	}{
		{"port equal to the numeric default is left out",
			sel("port", 6060), []map[string]any{sel("port", 6060), sel("port", 6061)},
			[]option{{"default", "default (6060)"}, {"port:6061", "6061"}}},
		{"name equal to the named default is left out",
			sel("portName", "pprof"), []map[string]any{sel("portName", "pprof-alt"), sel("portName", "pprof")},
			[]option{{"default", "default (pprof)"}, {"name:pprof-alt", "pprof-alt"}}},
		{"a name spelled like the numeric default is offered: kinds do not compare",
			sel("port", 6060), []map[string]any{sel("portName", "6060")},
			[]option{{"default", "default (6060)"}, {"name:6060", "6060"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadPortModel(t)
			got := callModel(t, vm, "deriveControl", map[string]any{"default": tc.def, "allowedSelections": tc.selections})
			if !got.Unchanged {
				t.Errorf("deriveControl mutated allowedSelections")
			}
			want := control{Options: tc.options}
			if !sameJSON(t, got.Result, want) {
				t.Errorf("deriveControl = %s, want %+v", got.Result, want)
			}
		})
	}
}

// edit is one call of applyInput: an empty source is no edit.
type edit struct {
	source string
	value  string
}

// portState is the control's state as the page keeps it.
type portState struct {
	PortChoice string `json:"portChoice"`
	PortNumber string `json:"portNumber"`
	PortName   string `json:"portName"`
}

// applied is what applyInput returns.
type applied struct {
	State  portState         `json:"state"`
	Params map[string]string `json:"params"`
}

func emptyState() portState {
	return portState{PortChoice: "default"}
}

// applyEdits runs the edits in order from start, failing when any step hands
// back both parameters or mutates the state it was handed.
func applyEdits(tb testing.TB, vm *goja.Runtime, start portState, edits []edit) applied {
	tb.Helper()

	out := applied{State: start}
	for i, e := range edits {
		args := []any{out.State}
		if e.source != "" {
			args = append(args, e.source, e.value)
		}
		got := callModel(tb, vm, "applyInput", args...)
		if !got.Unchanged {
			tb.Errorf("edit %d %+v: applyInput mutated the state it was handed", i, e)
		}
		out = applied{}
		if err := json.Unmarshal(got.Result, &out); err != nil {
			tb.Fatalf("edit %d %+v: decode %s: %v", i, e, got.Result, err)
		}
		_, hasPort := out.Params["port"]
		_, hasName := out.Params["portName"]
		if hasPort && hasName {
			tb.Errorf("edit %d %+v: params hold both port and portName: %v", i, e, out.Params)
		}
	}

	return out
}

func TestPortModelApplyInput(t *testing.T) {
	cases := []struct {
		name   string
		start  portState
		edits  []edit
		state  portState
		params map[string]string
	}{
		{"no edit on the empty state", emptyState(), []edit{{}}, emptyState(), map[string]string{}},
		{"menu default", emptyState(), []edit{{"menu", "default"}}, emptyState(), map[string]string{}},
		{"menu port option", emptyState(), []edit{{"menu", "port:6061"}},
			portState{PortChoice: "port:6061"}, map[string]string{"port": "6061"}},
		{"menu name option", emptyState(), []edit{{"menu", "name:pprof-alt"}},
			portState{PortChoice: "name:pprof-alt"}, map[string]string{"portName": "pprof-alt"}},
		{"number", emptyState(), []edit{{"number", "7000"}},
			portState{PortChoice: "default", PortNumber: "7000"}, map[string]string{"port": "7000"}},
		{"name", emptyState(), []edit{{"name", "pprof-alt"}},
			portState{PortChoice: "default", PortName: "pprof-alt"}, map[string]string{"portName": "pprof-alt"}},
		{"a number typed in the name field stays a name", emptyState(), []edit{{"name", "123"}},
			portState{PortChoice: "default", PortName: "123"}, map[string]string{"portName": "123"}},
		{"number after a menu option: the field wins", emptyState(), []edit{{"menu", "name:pprof-alt"}, {"number", "7000"}},
			portState{PortChoice: "name:pprof-alt", PortNumber: "7000"}, map[string]string{"port": "7000"}},
		{"name then number clears the name", emptyState(), []edit{{"name", "pprof-alt"}, {"number", "7000"}},
			portState{PortChoice: "default", PortNumber: "7000"}, map[string]string{"port": "7000"}},
		{"number then name clears the number", emptyState(), []edit{{"number", "7000"}, {"name", "pprof-alt"}},
			portState{PortChoice: "default", PortName: "pprof-alt"}, map[string]string{"portName": "pprof-alt"}},
		{"number cleared sends the menu choice", emptyState(), []edit{{"menu", "port:6061"}, {"number", "7000"}, {"number", ""}},
			portState{PortChoice: "port:6061"}, map[string]string{"port": "6061"}},
		{"no edit reads a formed state without changing it",
			portState{PortChoice: "port:6061", PortName: "pprof-alt"}, []edit{{}},
			portState{PortChoice: "port:6061", PortName: "pprof-alt"}, map[string]string{"portName": "pprof-alt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadPortModel(t)
			got := applyEdits(t, vm, tc.start, tc.edits)
			if got.State != tc.state {
				t.Errorf("state = %+v, want %+v", got.State, tc.state)
			}
			if !reflect.DeepEqual(got.Params, tc.params) {
				t.Errorf("params = %v, want %v", got.Params, tc.params)
			}
		})
	}
}

func TestPortModelNeverSendsBoth(t *testing.T) {
	vm := loadPortModel(t)
	sources := []edit{{"menu", "default"}, {"menu", "port:6061"}, {"menu", "name:pprof-alt"}, {"number", "7000"}, {"number", ""}, {"name", "pprof-alt"}, {"name", ""}, {}}
	// Every ordered pair of edits, then every ordered triple, from the empty state.
	for _, a := range sources {
		for _, b := range sources {
			applyEdits(t, vm, emptyState(), []edit{a, b})
			for _, c := range sources {
				applyEdits(t, vm, emptyState(), []edit{a, b, c})
			}
		}
	}
}
