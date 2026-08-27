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
	return []string{"app.js", "urls.js"}
}

// htmlInterfaceRe matches every interface that turns a string into markup.
var htmlInterfaceRe = regexp.MustCompile(`innerHTML|outerHTML|dangerouslySetInnerHTML|insertAdjacentHTML|document\.write|DOMParser`)

// pathLiteralRe matches a string literal, in any quote style, whose value
// begins with one of the gateway's path prefixes.
var pathLiteralRe = regexp.MustCompile("['\"`]/(?:v1|ui|auth)")

// stringConcatRe matches a + whose left operand is a string literal in any
// quote style, backticks included.
var stringConcatRe = regexp.MustCompile("['\"`][^'\"`]*['\"`]\\s*\\+")

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
	if bad := stringConcatFindings(`a + "b"`); len(bad) > 0 {
		t.Errorf("a literal on the right is not concatenation from a literal, got %v", bad)
	}
}
