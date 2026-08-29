package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/arloliu/profgate/internal/httpapi"
)

// copyTree copies every regular file of fsys into a MapFS a test can edit.
func copyTree(tb testing.TB, fsys fs.FS) fstest.MapFS {
	tb.Helper()

	out := fstest.MapFS{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		out[p] = &fstest.MapFile{Data: b}

		return nil
	})
	if err != nil {
		tb.Fatalf("copy tree: %v", err)
	}

	return out
}

// treeFiles is every regular file of fsys, in path order.
// The asset cases walk the tree rather than listing it,
// so a file vendored later is covered without an edit here.
func treeFiles(tb testing.TB, fsys fs.FS) []string {
	tb.Helper()

	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			names = append(names, p)
		}

		return nil
	})
	if err != nil {
		tb.Fatalf("walk tree: %v", err)
	}
	slices.Sort(names)

	return names
}

// wantTag is the entity tag a file's content earns, spelled independently of the handler:
// the quoted lowercase hex of its whole SHA-256, as sha256sum prints it.
func wantTag(b []byte) string {
	sum := sha256.Sum256(b)

	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// wantContentType is the Content-Type each extension of the tree earns,
// written out here so the handler's own mapping is not the thing that checks it.
func wantContentType(name string) string {
	switch path.Ext(name) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	}

	return "application/octet-stream"
}

func newHandler(tb testing.TB) *Handler {
	tb.Helper()

	h, err := New()
	if err != nil {
		tb.Fatalf("New: %v", err)
	}

	return h
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	return doWith(h, method, target, nil)
}

// doWith sends one request carrying header,
// which the conditional cases need on GET and on HEAD alike.
func doWith(h http.Handler, method, target string, header http.Header) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	h.ServeHTTP(rec, req)

	return rec
}

// ifNoneMatch is the header a conditional request carries.
func ifNoneMatch(value string) http.Header {
	return http.Header{"If-None-Match": {value}}
}

var tagRe = regexp.MustCompile(`^"[0-9a-f]{64}"$`)

// TestAssetTags reads the table the constructor builds:
// every regular file of the tree has an entry, index.html included,
// and its tag is the whole digest of its content and nothing else.
// A tag that sha256sum reproduces on the file in this repository turns a mismatch in the field into a fact.
func TestAssetTags(t *testing.T) {
	first, second := newHandler(t), newHandler(t)
	tree := staticTree(t)
	names := treeFiles(t, tree)
	if !slices.Contains(names, indexName) {
		t.Fatalf("the tree holds no %s", indexName)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			got, ok := first.assets[name]
			if !ok {
				t.Fatalf("the table holds no entry for %s", name)
			}
			if !tagRe.MatchString(got.etag) {
				t.Errorf("ETag = %q, want 64 lowercase hex digits in quotes", got.etag)
			}
			b, err := fs.ReadFile(tree, name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			if want := wantTag(b); got.etag != want {
				t.Errorf("ETag = %q, want %q", got.etag, want)
			}
			if other := second.assets[name].etag; other != got.etag {
				t.Errorf("ETag = %q in one construction and %q in another", got.etag, other)
			}
		})
	}
}

// TestAssetTagsAreOneFileEach changes one file and asserts the blast radius is that file:
// under a tree hash every asset's URL moved when any file did,
// and a per-file tag is what lets a browser keep the files a release did not touch.
func TestAssetTagsAreOneFileEach(t *testing.T) {
	base := newHandler(t)
	names := treeFiles(t, staticTree(t))
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			m := copyTree(t, staticTree(t))
			edited := append(append([]byte(nil), m[name].Data...), '\n')
			m[name] = &fstest.MapFile{Data: edited}
			h, err := newFromFS(m)
			if err != nil {
				t.Fatalf("newFromFS: %v", err)
			}
			if h.assets[name].etag == base.assets[name].etag {
				t.Errorf("the tag of %s did not move: %s", name, base.assets[name].etag)
			}
			for _, other := range names {
				if other == name {
					continue
				}
				if h.assets[other].etag != base.assets[other].etag {
					t.Errorf("editing %s moved the tag of %s", name, other)
				}
			}
		})
	}
}

// checkPolicyHeaders asserts the headers every console response carries,
// and the two it never carries.
func checkPolicyHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	want := map[string]string{
		"Content-Security-Policy":      contentSecurityPolicy,
		"X-Frame-Options":              "DENY",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.HasPrefix(got, "default-src 'none'; script-src 'self'; style-src 'self'; img-src data:; connect-src 'self'; ") {
		t.Errorf("Content-Security-Policy = %q does not open as the spec states", got)
	}
	// An embedded file has no meaningful modification time, so no response dates one.
	if got := rec.Header().Values("Last-Modified"); len(got) != 0 {
		t.Errorf("Last-Modified = %v: an embedded file has no modification time to report", got)
	}
	for k := range rec.Header() {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Errorf("%s set: the console is same-origin and carries no CORS header", k)
		}
	}
}

// checkCommonHeaders asserts the policy headers on a response that carries no validator:
// the shell, the redirect, and the two error envelopes.
func checkCommonHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	checkPolicyHeaders(t, rec)
	if got := rec.Header().Values("ETag"); len(got) != 0 {
		t.Errorf("ETag = %v: this response has no entity to validate", got)
	}
}

// checkValidatorHeaders asserts the policy headers on an asset answer,
// which carries the file's tag whether it is a 200 or a 304.
func checkValidatorHeaders(t *testing.T, rec *httptest.ResponseRecorder, tag string) {
	t.Helper()

	checkPolicyHeaders(t, rec)
	if got := rec.Header().Get("ETag"); got != tag {
		t.Errorf("ETag = %q, want %q", got, tag)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

func checkEnvelope(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	checkCommonHeaders(t, rec)
	if rec.Code != status {
		t.Errorf("status = %d, want %d", rec.Code, status)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not a JSON envelope: %v", rec.Body.String(), err)
	}
	if body.Code != code {
		t.Errorf("code = %q, want %q", body.Code, code)
	}
	// The console writes into the gateway's envelope,
	// so the code it names has to be one the gateway's registry holds.
	// Anything else would be a code the OpenAPI document could not enumerate.
	if !slices.Contains(httpapi.EnvelopeCodes(), body.Code) {
		t.Errorf("code %q is not in the gateway's registry", body.Code)
	}
}

// TestConsoleCodesComeFromTheRegistry reads the console's own source:
// the two codes it answers with are registry constants,
// so the console and the rest of the gateway cannot spell the same refusal two ways.
func TestConsoleCodesComeFromTheRegistry(t *testing.T) {
	b, err := os.ReadFile("ui.go")
	if err != nil {
		t.Fatalf("read ui.go: %v", err)
	}
	source := string(b)
	for _, code := range []string{httpapi.CodeRouteUnknown, httpapi.CodeMethodNotAllowed} {
		if strings.Contains(source, `"`+code+`"`) {
			t.Errorf("ui.go spells %q as a literal; it takes the code from the registry", code)
		}
	}
	for _, name := range []string{"httpapi.CodeRouteUnknown", "httpapi.CodeMethodNotAllowed"} {
		if !strings.Contains(source, name) {
			t.Errorf("ui.go does not name %s", name)
		}
	}
}

func TestShell(t *testing.T) {
	for _, target := range []string{"/ui/", "/ui/?ns=x&svc=y"} {
		t.Run(target, func(t *testing.T) {
			h := newHandler(t)
			rec := do(h, http.MethodGet, target)
			checkCommonHeaders(t, rec)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			// The shell is the one file whose content decides what the browser fetches next,
			// so it is never stored.
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(rec.Body.Len()) {
				t.Errorf("Content-Length = %q, body is %d", got, rec.Body.Len())
			}
			want, err := fs.ReadFile(staticTree(t), indexName)
			if err != nil {
				t.Fatalf("read %s: %v", indexName, err)
			}
			if rec.Body.String() != string(want) {
				t.Errorf("the shell differs from %s as it is written", indexName)
			}
		})
	}
}

// shellRefRe captures every absolute path the shell names.
var shellRefRe = regexp.MustCompile(`(?:href|src)="([^"]*)"`)

// TestShellNamesItsAssets fetches what the shell says rather than what it is expected to say.
// Nothing rewrites the shell, so a mistyped path in index.html is caught here or nowhere.
func TestShellNamesItsAssets(t *testing.T) {
	h := newHandler(t)
	body := do(h, http.MethodGet, Prefix).Body.String()
	var refs []string
	for _, m := range shellRefRe.FindAllStringSubmatch(body, -1) {
		refs = append(refs, m[1])
	}
	want := []string{Prefix + "app.css", Prefix + "app.js"}
	if !slices.Equal(refs, want) {
		t.Fatalf("the shell references %v, want %v", refs, want)
	}
	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			if rec := do(h, http.MethodGet, ref); rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", ref, rec.Code)
			}
		})
	}
}

// TestAssets walks the embedded tree and asserts the answer for every file it finds,
// so a file vendored later is covered without an edit.
func TestAssets(t *testing.T) {
	h := newHandler(t)
	tree := staticTree(t)
	for _, name := range treeFiles(t, tree) {
		if name == indexName {
			continue
		}
		t.Run(name, func(t *testing.T) {
			want, err := fs.ReadFile(tree, name)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			rec := do(h, http.MethodGet, Prefix+name)
			checkValidatorHeaders(t, rec, wantTag(want))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if rec.Body.String() != string(want) {
				t.Errorf("body differs from the embedded file")
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(want)) {
				t.Errorf("Content-Length = %q, want %d", got, len(want))
			}
			if got := rec.Header().Get("Content-Type"); got != wantContentType(name) {
				t.Errorf("Content-Type = %q, want %q", got, wantContentType(name))
			}
		})
	}
}

// TestConditional drives the revalidation no-cache buys:
// a browser sends the tag it holds and the handler answers 304 while the bytes are the same.
func TestConditional(t *testing.T) {
	const name = "app.js"
	h := newHandler(t)
	body, err := fs.ReadFile(staticTree(t), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	tag := wantTag(body)
	cases := []struct {
		name   string
		header string
		status int
	}{
		{"no condition", "", http.StatusOK},
		{"the tag itself", tag, http.StatusNotModified},
		{"any entity", "*", http.StatusNotModified},
		{"a list holding the tag", `"aaa", ` + tag + `, "bbb"`, http.StatusNotModified},
		{"the tag weakened by a proxy", "W/" + tag, http.StatusNotModified},
		{"another tag", `"` + strings.Repeat("0", 64) + `"`, http.StatusOK},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(tc.name+" "+method, func(t *testing.T) {
				var header http.Header
				if tc.header != "" {
					header = ifNoneMatch(tc.header)
				}
				rec := doWith(h, method, Prefix+name, header)
				checkValidatorHeaders(t, rec, tag)
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d", rec.Code, tc.status)
				}
				switch {
				case tc.status == http.StatusNotModified:
					// A 304 carries no representation, so it sizes none.
					if got := rec.Header().Values("Content-Length"); len(got) != 0 {
						t.Errorf("Content-Length = %v on a 304, which carries no representation", got)
					}
					if rec.Body.Len() != 0 {
						t.Errorf("304 wrote a body: %q", rec.Body.String())
					}
				case method == http.MethodHead:
					if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
						t.Errorf("Content-Length = %q, want %d", got, len(body))
					}
					if rec.Body.Len() != 0 {
						t.Errorf("HEAD wrote a body: %q", rec.Body.String())
					}
				default:
					if rec.Body.String() != string(body) {
						t.Errorf("body differs from the embedded file")
					}
				}
			})
		}
	}
}

func TestMisses(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"traversal dot dot", "/ui/../app.js"},
		{"traversal double slash", "/ui//app.js"},
		{"traversal encoded", "/ui/vendor/..%2Fapp.js"},
		{"traversal backslash", `/ui/vendor\preact\LICENSE`},
		{"index is not an asset", "/ui/index.html"},
		{"directory", "/ui/vendor"},
		{"directory slash", "/ui/vendor/"},
		{"ui without slash", "/ui"},
		{"no such file", "/ui/nothing.js"},
		{"under ui", "/ui/x"},
		// Every replica of one release serves one set of paths, so a path
		// carrying a content hash names no file rather than resolving somewhere.
		{"a hashed path", "/ui/static/0123456789abcdef/app.js"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(newHandler(t), http.MethodGet, tc.path)
			checkEnvelope(t, rec, http.StatusNotFound, httpapi.CodeRouteUnknown)
		})
		t.Run("head "+tc.name, func(t *testing.T) {
			rec := do(newHandler(t), http.MethodHead, tc.path)
			checkCommonHeaders(t, rec)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("HEAD wrote a body: %q", rec.Body.String())
			}
		})
	}
}

// TestAssetNameRefusesAnEmptyRest reaches the lookup directly
// because the shell's route answers /ui/ before the lookup ever sees it.
func TestAssetNameRefusesAnEmptyRest(t *testing.T) {
	if name, ok := newHandler(t).assetName(Prefix); ok {
		t.Errorf("assetName(%q) = %q, true; an empty rest names no file", Prefix, name)
	}
}

func TestRoot(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := do(newHandler(t), method, "/")
			checkCommonHeaders(t, rec)
			if rec.Code != http.StatusFound {
				t.Errorf("status = %d, want 302", rec.Code)
			}
			if got := rec.Header().Get("Location"); got != Prefix {
				t.Errorf("Location = %q, want %q", got, Prefix)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", rec.Body.String())
			}
		})
	}
}

func TestHead(t *testing.T) {
	// Every route answers HEAD with the headers of its GET and no body,
	// the error envelopes' Content-Length included.
	for _, target := range []string{
		"/ui/",
		"/ui/app.js",
		"/ui/vendor/preact/preact.module.js",
		"/",
		"/ui/x",
		"/ui/index.html",
	} {
		t.Run(target, func(t *testing.T) {
			h := newHandler(t)
			get := do(h, http.MethodGet, target)
			head := do(h, http.MethodHead, target)
			if head.Code != get.Code {
				t.Errorf("HEAD status = %d, GET %d", head.Code, get.Code)
			}
			for k, v := range get.Header() {
				if got := head.Header()[k]; strings.Join(got, ",") != strings.Join(v, ",") {
					t.Errorf("HEAD %s = %v, GET %v", k, got, v)
				}
			}
			if got := head.Header().Get("Content-Length"); got != strconv.Itoa(get.Body.Len()) {
				t.Errorf("HEAD Content-Length = %q, GET body is %d", got, get.Body.Len())
			}
			if head.Body.Len() != 0 {
				t.Errorf("HEAD wrote a body: %d bytes", head.Body.Len())
			}
		})
	}
}

func TestHeadIsNever405(t *testing.T) {
	for _, target := range []string{"/", "/ui/", "/ui", "/ui/x", "/ui/app.js", "/ui/index.html"} {
		t.Run(target, func(t *testing.T) {
			if rec := do(newHandler(t), http.MethodHead, target); rec.Code == http.StatusMethodNotAllowed {
				t.Errorf("HEAD %s: 405; HEAD is accepted wherever GET is", target)
			}
		})
	}
}

// TestMethod pins the Allow header the console writes.
// The gateway declares the same two methods for the console's three routes,
// and asserts that string in its own package, so the two cannot drift.
func TestMethod(t *testing.T) {
	cases := []struct{ method, target string }{
		{http.MethodPost, "/ui/"},
		{http.MethodPut, "/"},
		{http.MethodPost, "/ui/app.js"},
		{http.MethodDelete, "/ui/app.js"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			rec := do(newHandler(t), tc.method, tc.target)
			checkEnvelope(t, rec, http.StatusMethodNotAllowed, httpapi.CodeMethodNotAllowed)
			if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
			}
		})
	}
}

var (
	scriptBodyRe = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
	linkRe       = regexp.MustCompile(`<link rel="stylesheet"`)
	moduleRe     = regexp.MustCompile(`<script type="module"`)
)

func TestShellInlineForms(t *testing.T) {
	shell := readSource(t, "index.html")
	for _, m := range scriptBodyRe.FindAllStringSubmatch(shell, -1) {
		if strings.TrimSpace(m[1]) != "" {
			t.Errorf("inline script body: %q", m[1])
		}
	}
	if bad := inlineFormFindings(strings.ReplaceAll(shell, "<script", ""), false); len(bad) > 0 {
		t.Errorf("inline forms: %v", bad)
	}
	if n := len(linkRe.FindAllString(shell, -1)); n != 1 {
		t.Errorf("stylesheet links = %d, want 1", n)
	}
	if n := len(moduleRe.FindAllString(shell, -1)); n != 1 {
		t.Errorf("module scripts = %d, want 1", n)
	}
	for _, want := range []string{"<!doctype html>", `<html lang="en">`, "<meta charset", `<meta name="viewport"`, "<title>", `<main id="app"></main>`} {
		if !strings.Contains(shell, want) {
			t.Errorf("shell lacks %q", want)
		}
	}
}

// TestShellIsOneAnswer asserts two requests get the same bytes:
// the shell is held once at startup and nothing composes it per request.
func TestShellIsOneAnswer(t *testing.T) {
	h := newHandler(t)
	a := do(h, http.MethodGet, "/ui/")
	b := do(h, http.MethodGet, "/ui/")
	if a.Body.String() != b.Body.String() {
		t.Errorf("two answers for the shell differ")
	}
}
