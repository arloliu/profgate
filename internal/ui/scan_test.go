package ui

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// consoleSources returns the console's own modules, the files the source scan
// of Rendering response values runs against.
func consoleSources() []string {
	return []string{"app.js", "urls.js", "portmodel.js", "targetmodel.js", "collectionmodel.js"}
}

// htmlInterfaceRe matches every interface that turns a string into markup.
var htmlInterfaceRe = regexp.MustCompile(`innerHTML|outerHTML|dangerouslySetInnerHTML|insertAdjacentHTML|document\.write|DOMParser`)

// pathLiteralRe matches a string literal, in any quote style, whose value
// begins with one of the gateway's path prefixes.
var pathLiteralRe = regexp.MustCompile("['\"`]/(?:v1|ui|auth)")

// stringConcatRe matches a + whose left operand is a string literal in any
// quote style, backticks included.
var stringConcatRe = regexp.MustCompile("['\"`][^'\"`]*['\"`]\\s*\\+")

// dialogRe matches a call of one of the three blocking browser dialogs.
// The opening parenthesis is part of the pattern: the same three words appear
// in prose the page shows and in the ARIA role of its error region.
var dialogRe = regexp.MustCompile(`\b(?:confirm|alert|prompt)\(`)

// topLevelStateRe matches a let or var declaration at column zero, which is
// module-level mutable state.
var topLevelStateRe = regexp.MustCompile(`(?m)^(?:let|var)\b`)

// htmlInterfaceFindings returns every HTML injection interface content names.
func htmlInterfaceFindings(content string) []string {
	return htmlInterfaceRe.FindAllString(content, -1)
}

// pathLiteralFindings returns every string literal in content that begins
// with /v1, /ui, or /auth.
func pathLiteralFindings(content string) []string {
	return pathLiteralRe.FindAllString(content, -1)
}

// stringConcatFindings returns every + in content whose left operand is a
// string literal.
func stringConcatFindings(content string) []string {
	return stringConcatRe.FindAllString(content, -1)
}

// dialogFindings returns every blocking-dialog call content makes.
func dialogFindings(content string) []string {
	return dialogRe.FindAllString(content, -1)
}

// topLevelStateFindings returns every top-level let or var in content.
func topLevelStateFindings(content string) []string {
	return topLevelStateRe.FindAllString(content, -1)
}

func readSource(tb testing.TB, name string) string {
	tb.Helper()

	b, err := fs.ReadFile(staticTree(tb), name)
	if err != nil {
		tb.Fatalf("read %s: %v", name, err)
	}

	return string(b)
}

func TestScanNoHTMLInterfaces(t *testing.T) {
	for _, name := range consoleSources() {
		t.Run(name, func(t *testing.T) {
			if bad := htmlInterfaceFindings(readSource(t, name)); len(bad) > 0 {
				t.Errorf("%s: HTML injection interfaces: %v", name, bad)
			}
		})
	}
}

func TestScanPathsLiveInURLs(t *testing.T) {
	if bad := pathLiteralFindings(readSource(t, "app.js")); len(bad) > 0 {
		t.Errorf("app.js: path literals belong in urls.js: %v", bad)
	}
}

// TestScanNoBlockingDialogs holds the page to confirming in place.
// A dialog blocks the event loop until it is answered,
// and a browser that suppresses one turns a confirmation the operator never saw into a request never sent.
func TestScanNoBlockingDialogs(t *testing.T) {
	if bad := dialogFindings(readSource(t, "app.js")); len(bad) > 0 {
		t.Errorf("app.js: blocking browser dialogs: %v", bad)
	}
}

func TestScanURLsBuilds(t *testing.T) {
	src := readSource(t, "urls.js")
	for _, want := range []string{"encodeURIComponent", "URLSearchParams"} {
		if !strings.Contains(src, want) {
			t.Errorf("urls.js: does not use %s", want)
		}
	}
	if bad := stringConcatFindings(src); len(bad) > 0 {
		t.Errorf("urls.js: string concatenation: %v", bad)
	}
}

func TestScanRelativeImports(t *testing.T) {
	for _, name := range consoleSources() {
		t.Run(name, func(t *testing.T) {
			if bad := nonRelativeImports(readSource(t, name)); len(bad) > 0 {
				t.Errorf("%s: non-relative import specifiers: %v", name, bad)
			}
		})
	}
}

// portModelImportRe matches app.js's import of the model and captures the
// names it binds.
var portModelImportRe = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*["']\./portmodel\.js["']`)

// TestScanPageUsesPortModel holds the page to the model: app.js imports both
// functions from ./portmodel.js and calls each at least once, so a page that
// spells the port rules again by hand turns the suite red.
func TestScanPageUsesPortModel(t *testing.T) {
	src := readSource(t, "app.js")
	m := portModelImportRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("app.js: no import from ./portmodel.js")
	}
	for _, fn := range []string{"deriveControl", "applyInput"} {
		if !regexp.MustCompile(`\b` + fn + `\b`).MatchString(m[1]) {
			t.Errorf("app.js: the import from ./portmodel.js does not name %s: %q", fn, m[1])
		}
		if !strings.Contains(src, fn+"(") {
			t.Errorf("app.js: never calls %s(", fn)
		}
	}
}

// targetModelImportRe matches app.js's import of the targets model and captures the names it binds.
var targetModelImportRe = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*["']\./targetmodel\.js["']`)

// TestScanPageUsesTargetModel holds the page to the targets model:
// app.js imports the three functions from ./targetmodel.js and calls each at least once,
// so a page that builds the query, the retry rule, or the summary by hand turns the suite red.
func TestScanPageUsesTargetModel(t *testing.T) {
	src := readSource(t, "app.js")
	m := targetModelImportRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("app.js: no import from ./targetmodel.js")
	}
	for _, fn := range []string{"targetsQuery", "retryWithoutExplain", "targetSummary"} {
		if !regexp.MustCompile(`\b` + fn + `\b`).MatchString(m[1]) {
			t.Errorf("app.js: the import from ./targetmodel.js does not name %s: %q", fn, m[1])
		}
		if !strings.Contains(src, fn+"(") {
			t.Errorf("app.js: never calls %s(", fn)
		}
	}
}

// collectionModelImportRe matches app.js's import of the Collection-control model
// and captures the names it binds.
var collectionModelImportRe = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*["']\./collectionmodel\.js["']`)

// TestScanPageUsesCollectionModel holds the page to the Collection-control model:
// app.js imports the eight functions it calls from ./collectionmodel.js and calls each at least once,
// so a page that decides whether a control exists, what a request carries,
// what an answer does, or what the armed state holds by hand turns the suite red.
// The ninth export, retryAfterSeconds, is not named here:
// startOutcome is what reads Retry-After, and the page hands it the header rather than the delay.
func TestScanPageUsesCollectionModel(t *testing.T) {
	src := readSource(t, "app.js")
	m := collectionModelImportRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("app.js: no import from ./collectionmodel.js")
	}
	fns := []string{
		"startOffered",
		"cancelOffered",
		"uuidFromBytes",
		"startRequest",
		"cancelRequest",
		"startOutcome",
		"cancelOutcome",
		"startNext",
	}
	for _, fn := range fns {
		if !regexp.MustCompile(`\b` + fn + `\b`).MatchString(m[1]) {
			t.Errorf("app.js: the import from ./collectionmodel.js does not name %s: %q", fn, m[1])
		}
		if !strings.Contains(src, fn+"(") {
			t.Errorf("app.js: never calls %s(", fn)
		}
	}
}

func TestScanNoInlineForms(t *testing.T) {
	for _, name := range consoleSources() {
		t.Run(name, func(t *testing.T) {
			if bad := inlineFormFindings(readSource(t, name), true); len(bad) > 0 {
				t.Errorf("%s: inline-form violations: %v", name, bad)
			}
		})
	}
}

func TestScanCoversEveryJSFile(t *testing.T) {
	fsys := staticTree(t)

	var got []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == vendorRoot {
				return fs.SkipDir
			}

			return nil
		}
		if strings.HasSuffix(p, ".js") {
			got = append(got, p)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(got)
	want := consoleSources()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("js files outside vendor/ = %v, want %v: a new file must join the scan", got, want)
	}
}

func TestScanNoMutableTopLevelState(t *testing.T) {
	for _, name := range consoleSources() {
		t.Run(name, func(t *testing.T) {
			if bad := topLevelStateFindings(readSource(t, name)); len(bad) > 0 {
				t.Errorf("%s: top-level mutable state: %v", name, bad)
			}
		})
	}
}

func TestScanFixturesFail(t *testing.T) {
	cases := []struct {
		name  string
		found int
	}{
		{"innerHTML", len(htmlInterfaceFindings(`el.innerHTML = x`))},
		{"document.write", len(htmlInterfaceFindings(`document.write(x)`))},
		{"path literal double quoted", len(pathLiteralFindings(`fetch("/v1/namespaces")`))},
		{"path literal single quoted", len(pathLiteralFindings(`location.assign('/auth/login')`))},
		{"path literal backtick", len(pathLiteralFindings("`/ui/?ns=${ns}`"))},
		{"string concatenation double quoted", len(stringConcatFindings(`"/v1/" + ns`))},
		{"string concatenation single quoted", len(stringConcatFindings(`'/v1/' + ns`))},
		{"string concatenation backtick", len(stringConcatFindings("`/v1/` + ns"))},
		{"bare import", len(nonRelativeImports(`import h from "htm"`))},
		{"on attribute", len(inlineFormFindings(`<div onclick=alert(1)>`, true))},
		{"new Function", len(inlineFormFindings(`new Function("x")`, true))},
		{"confirm dialog", len(dialogFindings(`if (confirm("sure?")) {`))},
		{"alert dialog", len(dialogFindings(`window.alert(message)`))},
		{"prompt dialog", len(dialogFindings(`const name = prompt("name")`))},
		{"top-level let", len(topLevelStateFindings("let navigated = false;\n"))},
		{"top-level var", len(topLevelStateFindings("import x from './x.js';\nvar n = 0;\n"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.found == 0 {
				t.Errorf("fixture %q: want at least one finding, got none", tc.name)
			}
		})
	}
}

func TestScanFixturesPass(t *testing.T) {
	if bad := topLevelStateFindings("  let inner = 1;\nconst x = 1;\n"); len(bad) > 0 {
		t.Errorf("indented let and const are not top-level state, got %v", bad)
	}
	if bad := dialogFindings(`"could not read its cache or confirm the Pod"`); len(bad) > 0 {
		t.Errorf("the word confirm in prose is not a dialog, got %v", bad)
	}
	if bad := dialogFindings(`<div class="error" role="alert">`); len(bad) > 0 {
		t.Errorf("the alert role is not a dialog, got %v", bad)
	}
	if bad := stringConcatFindings(`a + "b"`); len(bad) > 0 {
		t.Errorf("a literal on the right is not concatenation from a literal, got %v", bad)
	}
}

// collectionIDReRe matches the identifier-grammar declaration both modules carry.
var collectionIDReRe = regexp.MustCompile(`(?m)^const collectionIDRe = (/\^.*\$/);$`)

// TestScanIdentifierGrammarAgrees holds together the two copies of the Collection identifier grammar.
// urls.js refuses an identifier before it builds a path from one,
// and collectionmodel.js refuses one before it selects a record;
// collectionmodel.js imports nothing, so it cannot read the first and carries its own.
// Two copies that drift would let the model select an identifier no path can be built for,
// which is a page that names a record and then cannot fetch it.
func TestScanIdentifierGrammarAgrees(t *testing.T) {
	grammars := map[string]string{}
	for _, name := range []string{"urls.js", "collectionmodel.js"} {
		m := collectionIDReRe.FindStringSubmatch(readSource(t, name))
		if m == nil {
			t.Fatalf("%s declares no collectionIDRe the scan recognises", name)
		}
		grammars[name] = m[1]
	}
	if grammars["urls.js"] != grammars["collectionmodel.js"] {
		t.Errorf("urls.js spells the identifier grammar %s and collectionmodel.js %s",
			grammars["urls.js"], grammars["collectionmodel.js"])
	}

	// The grammar is the alphabet of internal/pgo's newID, which is where an identifier is made:
	// the ten digits, and the twenty-two letters that cannot be confused with them,
	// so i, l, o, and u are absent.
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	re := regexp.MustCompile(strings.ReplaceAll(grammars["urls.js"], "/", ""))
	for _, c := range alphabet {
		if id := strings.Repeat(string(c), 20); !re.MatchString(id) {
			t.Errorf("the grammar refuses %q, which is 20 of the alphabet's %q", id, c)
		}
	}
	for _, c := range "ilou-_." {
		if id := strings.Repeat(string(c), 20); re.MatchString(id) {
			t.Errorf("the grammar admits %q, which the alphabet does not hold", id)
		}
	}
}
