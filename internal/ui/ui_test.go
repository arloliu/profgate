package ui

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
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

func newHandler(tb testing.TB) *Handler {
	tb.Helper()

	h, err := New()
	if err != nil {
		tb.Fatalf("New: %v", err)
	}

	return h
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, target, nil))

	return rec
}

var hexRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestHashStable(t *testing.T) {
	a, b := newHandler(t), newHandler(t)
	if a.Hash() != b.Hash() {
		t.Errorf("hash differs across constructions: %s vs %s", a.Hash(), b.Hash())
	}
	if !hexRe.MatchString(a.Hash()) {
		t.Errorf("hash %q is not 16 lowercase hex digits", a.Hash())
	}
}

func TestHashMoves(t *testing.T) {
	base := newHandler(t).Hash()

	cases := []struct {
		name string
		edit func(m fstest.MapFS)
	}{
		{"one byte of app.css", func(m fstest.MapFS) {
			d := append([]byte(nil), m["app.css"].Data...)
			d[len(d)-1] ^= 1
			m["app.css"] = &fstest.MapFile{Data: d}
		}},
		{"a file added", func(m fstest.MapFS) {
			m["extra.txt"] = &fstest.MapFile{Data: []byte("x")}
		}},
		{"two contents swapped", func(m fstest.MapFS) {
			m["app.js"], m["urls.js"] = m["urls.js"], m["app.js"]
		}},
		{"one byte of index.html", func(m fstest.MapFS) {
			d := append([]byte(nil), m["index.html"].Data...)
			d = append(d, '\n')
			m["index.html"] = &fstest.MapFile{Data: d}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := copyTree(t, staticTree(t))
			tc.edit(m)
			h, err := newFromFS(m)
			if err != nil {
				t.Fatalf("newFromFS: %v", err)
			}
			if h.Hash() == base {
				t.Errorf("hash did not move: %s", base)
			}
		})
	}
}

func TestHashIsFramed(t *testing.T) {
	a, err := treeHash(fstest.MapFS{"ab": &fstest.MapFile{Data: []byte("c")}})
	if err != nil {
		t.Fatalf("treeHash: %v", err)
	}
	b, err := treeHash(fstest.MapFS{"a": &fstest.MapFile{Data: []byte("bc")}})
	if err != nil {
		t.Fatalf("treeHash: %v", err)
	}
	if a == b {
		t.Errorf("{ab: c} and {a: bc} hash alike: %s", a)
	}
}

// checkCommonHeaders asserts what every console response carries and lacks.
func checkCommonHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
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
	for _, k := range []string{"ETag", "Last-Modified"} {
		if _, ok := rec.Header()[k]; ok {
			t.Errorf("%s set: no validator belongs on a console response", k)
		}
	}
	for k := range rec.Header() {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Errorf("%s set: the console is same-origin and carries no CORS header", k)
		}
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
	if !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Errorf("body = %q, want code %q", rec.Body.String(), code)
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
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(rec.Body.Len()) {
				t.Errorf("Content-Length = %q, body is %d", got, rec.Body.Len())
			}
			body := rec.Body.String()
			for _, want := range []string{"/ui/static/" + h.Hash() + "/app.css", "/ui/static/" + h.Hash() + "/app.js"} {
				if !strings.Contains(body, want) {
					t.Errorf("shell lacks %q", want)
				}
			}
			if strings.Contains(body, "__") {
				t.Errorf("shell still holds a placeholder: %s", body)
			}
		})
	}
}

func TestAssets(t *testing.T) {
	cases := []struct {
		path        string
		contentType string
	}{
		{"app.js", "text/javascript; charset=utf-8"},
		{"app.css", "text/css; charset=utf-8"},
		{"urls.js", "text/javascript; charset=utf-8"},
		{"vendor/preact/preact.module.js", "text/javascript; charset=utf-8"},
		{"vendor/htm/htm.module.js", "text/javascript; charset=utf-8"},
		{"vendor/pico/pico.classless.min.css", "text/css; charset=utf-8"},
		{"vendor/MANIFEST", "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			h := newHandler(t)
			want, err := fs.ReadFile(staticTree(t), tc.path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			rec := do(h, http.MethodGet, "/ui/static/"+h.Hash()+"/"+tc.path)
			checkCommonHeaders(t, rec)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if rec.Body.String() != string(want) {
				t.Errorf("body differs from the embedded file")
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(want)) {
				t.Errorf("Content-Length = %q, want %d", got, len(want))
			}
			if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
				t.Errorf("Cache-Control = %q", got)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.contentType {
				t.Errorf("Content-Type = %q, want %q", got, tc.contentType)
			}
		})
		t.Run("wrong hash "+tc.path, func(t *testing.T) {
			rec := do(newHandler(t), http.MethodGet, "/ui/static/0000000000000000/"+tc.path)
			checkEnvelope(t, rec, http.StatusNotFound, "route_unknown")
		})
	}
}

func TestMisses(t *testing.T) {
	// The hash is stable across constructions (TestHashStable), so the cases
	// name it while every subtest still serves from a handler of its own.
	hash := newHandler(t).Hash()
	cases := []struct {
		name string
		path string
	}{
		{"traversal dot dot", "/ui/static/" + hash + "/../app.js"},
		{"traversal double slash", "/ui/static/" + hash + "//app.js"},
		{"traversal encoded", "/ui/static/" + hash + "/vendor/..%2Fapp.js"},
		{"traversal backslash", `/ui/static/` + hash + `/vendor\preact\LICENSE`},
		{"traversal empty", "/ui/static/" + hash + "/"},
		{"index is not an asset", "/ui/static/" + hash + "/index.html"},
		{"directory", "/ui/static/" + hash + "/vendor"},
		{"directory slash", "/ui/static/" + hash + "/vendor/"},
		{"ui without slash", "/ui"},
		{"under ui", "/ui/x"},
		{"static root", "/ui/static/"},
		{"hash only", "/ui/static/" + hash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(newHandler(t), http.MethodGet, tc.path)
			checkEnvelope(t, rec, http.StatusNotFound, "route_unknown")
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
	hash := newHandler(t).Hash()
	for _, target := range []string{
		"/ui/",
		"/ui/static/" + hash + "/app.js",
		"/",
		"/ui/x",
		"/ui/static/0000000000000000/app.js",
		"/ui/static/" + hash + "/index.html",
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
	hash := newHandler(t).Hash()
	for _, target := range []string{"/", "/ui/", "/ui", "/ui/x", "/ui/static/", "/ui/static/" + hash + "/app.js", "/ui/static/" + hash + "/index.html"} {
		t.Run(target, func(t *testing.T) {
			if rec := do(newHandler(t), http.MethodHead, target); rec.Code == http.StatusMethodNotAllowed {
				t.Errorf("HEAD %s: 405; HEAD is accepted wherever GET is", target)
			}
		})
	}
}

func TestMethod(t *testing.T) {
	hash := newHandler(t).Hash()
	cases := []struct{ method, target string }{
		{http.MethodPost, "/ui/"},
		{http.MethodPut, "/"},
		{http.MethodDelete, "/ui/static/" + hash + "/app.js"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			rec := do(newHandler(t), tc.method, tc.target)
			checkEnvelope(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
			if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
			}
		})
	}
}

func TestPlaceholdersRequired(t *testing.T) {
	cases := []struct {
		name string
		edit func(s string) string
	}{
		{"missing", func(s string) string { return strings.ReplaceAll(s, scriptPlaceholder, "app.js") }},
		{"repeated", func(s string) string { return s + scriptPlaceholder }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := copyTree(t, staticTree(t))
			m["index.html"] = &fstest.MapFile{Data: []byte(tc.edit(string(m["index.html"].Data)))}
			_, err := newFromFS(m)
			if err == nil {
				t.Fatal("newFromFS succeeded over a broken shell")
			}
			if !strings.Contains(err.Error(), scriptPlaceholder) {
				t.Errorf("error %q does not name %s", err, scriptPlaceholder)
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

func TestRenderShellReadsOnce(t *testing.T) {
	h := newHandler(t)
	a := do(h, http.MethodGet, "/ui/")
	b := do(h, http.MethodGet, "/ui/")
	if got, _ := io.ReadAll(a.Body); string(got) != b.Body.String() {
		t.Errorf("two renders of the shell differ")
	}
}
