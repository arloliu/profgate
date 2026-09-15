package ui

import (
	"encoding/json"
	"testing"

	"github.com/dop251/goja"
)

// catalogModelName is the module holding the catalog's pure functions.
const catalogModelName = "catalogmodel.js"

// catalogModelFunctions is what the module exports, in the order of its export statement.
var catalogModelFunctions = []string{"namespacesOf", "servicesOf", "filterOptions", "searchCatalog"}

// catalogEntry is one entry of the catalog: a namespace and the name of one Service in it.
type catalogEntry struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// loadCatalogModel evaluates the catalog model with its functions reachable as globals.
func loadCatalogModel(tb testing.TB) *goja.Runtime {
	tb.Helper()

	return loadModel(tb, catalogModelName, catalogModelFunctions...)
}

// TestCatalogModelNamespacesOf drives the namespace menu's derivation.
// The menu offers the distinct namespaces the catalog names, each once,
// in the order the catalog arrived rather than in an order the function chose.
func TestCatalogModelNamespacesOf(t *testing.T) {
	cases := []struct {
		name    string
		catalog []catalogEntry
		want    []string
	}{
		{
			"an empty catalog names no namespace",
			[]catalogEntry{},
			[]string{},
		},
		{
			"one namespace holding several Services is named once",
			[]catalogEntry{
				{"payments", "checkout"},
				{"payments", "ledger"},
				{"payments", "refunds"},
			},
			[]string{"payments"},
		},
		{
			"several namespaces each holding several Services are each named once",
			[]catalogEntry{
				{"billing", "invoices"},
				{"billing", "statements"},
				{"orders", "checkout"},
				{"orders", "shipping"},
				{"payments", "ledger"},
				{"payments", "refunds"},
			},
			[]string{"billing", "orders", "payments"},
		},
		{
			"the order is the catalog's, which is not alphabetical here",
			[]catalogEntry{
				{"payments", "ledger"},
				{"payments", "refunds"},
				{"orders", "checkout"},
				{"billing", "invoices"},
			},
			[]string{"payments", "orders", "billing"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCatalogModel(t)
			got := callModel(t, vm, "namespacesOf", tc.catalog)
			if !got.Unchanged {
				t.Errorf("namespacesOf mutated its argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("namespacesOf(%v) = %s, want %v", tc.catalog, got.Result, tc.want)
			}
		})
	}
}

// TestCatalogModelServicesOf drives the Service menu's derivation.
// The menu offers the names the catalog holds under the selected namespace, in the catalog's order,
// and a namespace the catalog does not hold leaves the menu empty rather than offering every name.
func TestCatalogModelServicesOf(t *testing.T) {
	cases := []struct {
		name    string
		catalog []catalogEntry
		ns      string
		want    []string
	}{
		{
			"an empty catalog holds no Service under any namespace",
			[]catalogEntry{},
			"payments",
			[]string{},
		},
		{
			"a namespace holding one Service offers that one",
			[]catalogEntry{
				{"billing", "invoices"},
				{"orders", "checkout"},
				{"payments", "ledger"},
			},
			"orders",
			[]string{"checkout"},
		},
		{
			"a namespace holding several offers them in the catalog's order",
			[]catalogEntry{
				{"billing", "invoices"},
				{"payments", "refunds"},
				{"payments", "checkout"},
				{"payments", "ledger"},
			},
			"payments",
			[]string{"refunds", "checkout", "ledger"},
		},
		{
			"a namespace the catalog does not hold offers nothing",
			[]catalogEntry{
				{"orders", "checkout"},
				{"payments", "ledger"},
			},
			"billing",
			[]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCatalogModel(t)
			got := callModel(t, vm, "servicesOf", tc.catalog, tc.ns)
			if !got.Unchanged {
				t.Errorf("servicesOf mutated an argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("servicesOf(%v, %q) = %s, want %v", tc.catalog, tc.ns, got.Result, tc.want)
			}
		})
	}
}

// TestCatalogModelFilterOptions drives the rule behind both Service menus.
// A filter narrows what a menu offers without ever dropping the value the menu shows
// and without ever offering one the list does not hold.
func TestCatalogModelFilterOptions(t *testing.T) {
	cases := []struct {
		name        string
		list        []string
		query       string
		keep        string
		want        []string
		wantMatched int
	}{
		{
			"an empty query returns every entry",
			[]string{"payments", "checkout"}, "", "",
			[]string{"payments", "checkout"}, 2,
		},
		{
			"a query matches case-insensitively",
			[]string{"Payments", "checkout"}, "PAY", "",
			[]string{"Payments"}, 1,
		},
		{
			"a query matches mid-string",
			[]string{"payments", "checkout"}, "eck", "",
			[]string{"checkout"}, 1,
		},
		{
			"a query is trimmed",
			[]string{"payments", "checkout"}, "  eck  ", "",
			[]string{"checkout"}, 1,
		},
		{
			"a query of whitespace alone returns every entry",
			[]string{"payments", "checkout"}, "   ", "",
			[]string{"payments", "checkout"}, 2,
		},
		{
			"keep survives a query that excludes it",
			[]string{"payments", "checkout"}, "pay", "checkout",
			[]string{"payments", "checkout"}, 1,
		},
		{
			"keep is not duplicated when it also matches",
			[]string{"payments", "checkout"}, "pay", "payments",
			[]string{"payments"}, 1,
		},
		{
			"a keep the list lacks is not added",
			[]string{"payments", "checkout"}, "pay", "billing",
			[]string{"payments"}, 1,
		},
		{
			"a keep the empty list lacks is not added",
			[]string{}, "pay", "payments",
			[]string{}, 0,
		},
		{
			"an empty list stays empty",
			[]string{}, "pay", "",
			[]string{}, 0,
		},
		{
			"the order is the input's, keep included",
			[]string{"zeta", "alpha", "beta"}, "et", "alpha",
			[]string{"zeta", "alpha", "beta"}, 2,
		},
		{
			"the input array is not mutated",
			[]string{"payments", "checkout", "billing"}, "bill", "payments",
			[]string{"payments", "billing"}, 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCatalogModel(t)
			got := callModel(t, vm, "filterOptions", tc.list, tc.query, tc.keep)
			if !got.Unchanged {
				t.Errorf("filterOptions mutated an argument")
			}
			want := map[string]any{"options": tc.want, "matched": tc.wantMatched}
			if !sameJSON(t, got.Result, want) {
				t.Errorf("filterOptions(%v, %q, %q) = %s, want %v", tc.list, tc.query, tc.keep, got.Result, want)
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
		const out = filterOptions(list, "", "").options;
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

// searchCatalogResult is what searchCatalog answers:
// the first limit matches, each one the catalog entry it matched,
// beside the number of entries that matched before the cap was applied.
// The design names what the two hold and not what they are called,
// so the two names are fixed here and the module and the page read them from this test.
// The entry is carried through whole rather than flattened to a string,
// because the row the page draws calls selectPair with the namespace and the name separately.
type searchCatalogResult struct {
	Matches []catalogEntry `json:"matches"`
	Total   int            `json:"total"`
}

// TestCatalogModelSearchCatalog drives the search field's results.
// The search matches a namespace and a name joined by a slash as one string,
// so a query holding the separator narrows by both halves at once,
// and it answers the first limit matches in the catalog's order beside the number that matched.
// An empty query matches nothing, so results appear only once someone has typed.
func TestCatalogModelSearchCatalog(t *testing.T) {
	cases := []struct {
		name    string
		catalog any
		query   string
		limit   int
		want    searchCatalogResult
	}{
		{
			"an empty query matches nothing",
			[]catalogEntry{{"payments", "checkout"}, {"payments", "ledger"}},
			"", 50,
			searchCatalogResult{[]catalogEntry{}, 0},
		},
		{
			// The filter beside this field leaves every option standing for the same query,
			// because the two functions share the trimming rule and differ on what an empty query means.
			// This query is empty only once it is trimmed.
			// A function that reads it for emptiness first answers the whole catalog instead.
			"a query of whitespace alone matches nothing",
			[]catalogEntry{{"payments", "checkout"}, {"payments", "ledger"}},
			"   ", 50,
			searchCatalogResult{[]catalogEntry{}, 0},
		},
		{
			// The query is trimmed before it is matched,
			// so a function that matches the raw text finds nothing here.
			"a query is trimmed before it is matched",
			[]catalogEntry{{"payments", "checkout"}, {"payments", "ledger"}},
			"  ledger  ", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "ledger"}}, 1},
		},
		{
			"a query matches the name alone",
			[]catalogEntry{{"billing", "invoices"}, {"payments", "ledger"}},
			"ledger", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "ledger"}}, 1},
		},
		{
			"a query matches the namespace alone",
			[]catalogEntry{{"billing", "invoices"}, {"billing", "statements"}, {"payments", "ledger"}},
			"billing", 50,
			searchCatalogResult{[]catalogEntry{{"billing", "invoices"}, {"billing", "statements"}}, 2},
		},
		{
			// The two halves are matched as one string with the separator between them,
			// which a function matching the namespace and the name apart from each other never finds.
			"a query holding the separator matches both parts",
			[]catalogEntry{{"orders", "checkout"}, {"payments", "checkout"}, {"payments", "ledger"}},
			"payments/check", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "checkout"}}, 1},
		},
		{
			"a query straddling the separator matches",
			[]catalogEntry{{"orders", "checkout"}, {"payments", "checkout"}},
			"ts/che", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "checkout"}}, 1},
		},
		{
			// Each half of this query matches its own side of the pair and the joined string holds neither,
			// so a function that splits the query at the separator answers a match the joined string does not hold.
			"a query whose halves match apart but not joined matches nothing",
			[]catalogEntry{{"payments", "checkout"}, {"payments", "ledger"}},
			"pay/out", 50,
			searchCatalogResult{[]catalogEntry{}, 0},
		},
		{
			"a query matches case-insensitively",
			[]catalogEntry{{"Payments", "Checkout"}, {"billing", "invoices"}},
			"PAYMENTS/CHECK", 50,
			searchCatalogResult{[]catalogEntry{{"Payments", "Checkout"}}, 1},
		},
		{
			"a query matches mid-string",
			[]catalogEntry{{"payments", "checkout"}, {"billing", "invoices"}},
			"eck", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "checkout"}}, 1},
		},
		{
			// The cap is on what the page draws and not on what it knows,
			// so a function that answers every match, or counts only the matches it returned,
			// leaves the page unable to say how many more there are.
			"the cap bounds the matches while the total counts every match",
			[]catalogEntry{
				{"payments", "refunds"},
				{"payments", "checkout"},
				{"orders", "checkout"},
				{"billing", "checkout-api"},
			},
			"check", 2,
			searchCatalogResult{[]catalogEntry{{"payments", "checkout"}, {"orders", "checkout"}}, 3},
		},
		{
			"a limit larger than the number matched returns them all",
			[]catalogEntry{{"payments", "checkout"}, {"payments", "ledger"}, {"billing", "invoices"}},
			"payments", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "checkout"}, {"payments", "ledger"}}, 2},
		},
		{
			// The catalog's order is neither alphabetical nor the order the query suggests,
			// so a function that sorts its answer, or sorts the catalog it was handed, answers these three in another order.
			"the order is the catalog's",
			[]catalogEntry{
				{"payments", "refunds"},
				{"payments", "checkout"},
				{"orders", "checkout"},
				{"billing", "checkout-api"},
			},
			"check", 50,
			searchCatalogResult{[]catalogEntry{
				{"payments", "checkout"},
				{"orders", "checkout"},
				{"billing", "checkout-api"},
			}, 3},
		},
		{
			"an empty catalog matches nothing",
			[]catalogEntry{},
			"payments", 50,
			searchCatalogResult{[]catalogEntry{}, 0},
		},
		{
			// The page holds the catalog answer alone,
			// and a body the gateway never sends narrows the results rather than stopping the search.
			"a catalog that is not an array matches nothing",
			nil,
			"payments", 50,
			searchCatalogResult{[]catalogEntry{}, 0},
		},
		{
			"an entry that is null is skipped",
			[]any{nil, catalogEntry{"payments", "ledger"}},
			"payments", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "ledger"}}, 1},
		},
		{
			"an entry that is not an object is skipped",
			[]any{"payments/checkout", catalogEntry{"payments", "ledger"}},
			"payments", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "ledger"}}, 1},
		},
		{
			// A function that joins the two halves without reading their types searches "payments/undefined" here.
			// It answers an entry naming no Service.
			"an entry missing its name is skipped",
			[]any{map[string]any{"namespace": "payments"}, catalogEntry{"payments", "ledger"}},
			"payments", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "ledger"}}, 1},
		},
		{
			"an entry whose name is not a string is skipped",
			[]any{map[string]any{"namespace": "payments", "name": 7}, catalogEntry{"payments", "ledger"}},
			"payments", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "ledger"}}, 1},
		},
		{
			// A row drawn for such an entry would be labelled with the separator and nothing after it,
			// and choosing it would clear the Service rather than choose one,
			// because the name it passes to the page is the empty string the menu means by no Service.
			"an entry whose name is the empty string is skipped",
			[]any{map[string]any{"namespace": "payments", "name": ""}, catalogEntry{"payments", "ledger"}},
			"payments", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "ledger"}}, 1},
		},
		{
			"an entry missing its namespace is skipped",
			[]any{map[string]any{"name": "payments-api"}, catalogEntry{"payments", "ledger"}},
			"payments", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "ledger"}}, 1},
		},
		{
			"an entry whose namespace is not a string is skipped",
			[]any{map[string]any{"namespace": 7, "name": "payments-api"}, catalogEntry{"payments", "ledger"}},
			"payments", 50,
			searchCatalogResult{[]catalogEntry{{"payments", "ledger"}}, 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCatalogModel(t)
			got := callModel(t, vm, "searchCatalog", tc.catalog, tc.query, tc.limit)
			if !got.Unchanged {
				t.Errorf("searchCatalog mutated an argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("searchCatalog(%v, %q, %d) = %s, want %+v", tc.catalog, tc.query, tc.limit, got.Result, tc.want)
			}
		})
	}
}
