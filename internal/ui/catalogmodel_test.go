package ui

import (
	"encoding/json"
	"testing"

	"github.com/dop251/goja"
)

// catalogModelName is the module holding the catalog's pure functions.
const catalogModelName = "catalogmodel.js"

// catalogModelFunctions is what the module exports, in the order of its export statement.
var catalogModelFunctions = []string{"filterOptions"}

// loadCatalogModel evaluates the catalog model with its functions reachable as globals.
func loadCatalogModel(tb testing.TB) *goja.Runtime {
	tb.Helper()

	return loadModel(tb, catalogModelName, catalogModelFunctions...)
}

// TestCatalogModelFilterOptions drives the rule behind both Service menus.
// A filter narrows what a menu offers without ever dropping the value the menu shows
// and without ever offering one the list does not hold.
func TestCatalogModelFilterOptions(t *testing.T) {
	cases := []struct {
		name  string
		list  []string
		query string
		keep  string
		want  []string
	}{
		{
			"an empty query returns every entry",
			[]string{"payments", "checkout"}, "", "",
			[]string{"payments", "checkout"},
		},
		{
			"a query matches case-insensitively",
			[]string{"Payments", "checkout"}, "PAY", "",
			[]string{"Payments"},
		},
		{
			"a query matches mid-string",
			[]string{"payments", "checkout"}, "eck", "",
			[]string{"checkout"},
		},
		{
			"a query is trimmed",
			[]string{"payments", "checkout"}, "  eck  ", "",
			[]string{"checkout"},
		},
		{
			"a query of whitespace alone returns every entry",
			[]string{"payments", "checkout"}, "   ", "",
			[]string{"payments", "checkout"},
		},
		{
			"keep survives a query that excludes it",
			[]string{"payments", "checkout"}, "pay", "checkout",
			[]string{"payments", "checkout"},
		},
		{
			"keep is not duplicated when it also matches",
			[]string{"payments", "checkout"}, "pay", "payments",
			[]string{"payments"},
		},
		{
			"a keep the list lacks is not added",
			[]string{"payments", "checkout"}, "pay", "billing",
			[]string{"payments"},
		},
		{
			"a keep the empty list lacks is not added",
			[]string{}, "pay", "payments",
			[]string{},
		},
		{
			"an empty list stays empty",
			[]string{}, "pay", "",
			[]string{},
		},
		{
			"the order is the input's, keep included",
			[]string{"zeta", "alpha", "beta"}, "et", "alpha",
			[]string{"zeta", "alpha", "beta"},
		},
		{
			"the input array is not mutated",
			[]string{"payments", "checkout", "billing"}, "bill", "payments",
			[]string{"payments", "billing"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCatalogModel(t)
			got := callModel(t, vm, "filterOptions", tc.list, tc.query, tc.keep)
			if !got.Unchanged {
				t.Errorf("filterOptions mutated an argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("filterOptions(%v, %q, %q) = %s, want %v", tc.list, tc.query, tc.keep, got.Result, tc.want)
			}
		})
	}
}

// filterResult is what the identity check reads back:
// whether the returned array is the one handed in, and what that array holds afterwards.
type filterResult struct {
	Same bool     `json:"same"`
	List []string `json:"list"`
}

// TestCatalogModelFilterOptionsReturnsANewArray holds the empty query to a copy.
// An empty query keeps every entry, so a function that hands its own argument back answers correctly
// and leaves the caller writing into the list the page holds;
// comparing values cannot tell the two apart, and comparing references can.
func TestCatalogModelFilterOptionsReturnsANewArray(t *testing.T) {
	vm := loadCatalogModel(t)
	v, err := vm.RunString(`(function () {
		const list = ["payments", "checkout"];
		const out = filterOptions(list, "", "");
		out.push("billing");
		return JSON.stringify({ same: out === list, list: list });
	})()`)
	if err != nil {
		t.Fatalf("call filterOptions: %v", err)
	}
	var got filterResult
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decode %q: %v", v.String(), err)
	}
	if got.Same {
		t.Errorf("filterOptions returned the array it was handed")
	}
	if len(got.List) != 2 {
		t.Errorf("the list handed in holds %v after the result was appended to, want its two entries", got.List)
	}
}
