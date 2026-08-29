package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path"
	"strings"
	"testing"
)

// vendorRoot is where the vendored browser libraries live, relative to the
// static tree.
const vendorRoot = "vendor"

// vendoredFile records what one vendored file must be: the library it comes
// from, that library's pinned version and license, where the license text
// lives, the tarball the file was extracted from, and the file's exact
// SHA-256.
type vendoredFile struct {
	id          string
	version     string
	license     string
	licensePath string
	sourceURL   string
	sha256      string
}

// wantVendored returns the pinned set of vendored files, keyed by path relative
// to vendor/. It is written out here, not read from MANIFEST, so a change that
// edits a file and its manifest line together still fails until this table
// is edited too.
func wantVendored() map[string]vendoredFile {
	return map[string]vendoredFile{
		"preact/preact.module.js": {
			id:          "preact",
			version:     "10.29.8",
			license:     "MIT",
			licensePath: "preact/LICENSE",
			sourceURL:   "https://registry.npmjs.org/preact/-/preact-10.29.8.tgz",
			sha256:      "c30e721ebfdc6e2ad4c18c14d2dfb82667829c8aec27de1207774e3fc16858a8",
		},
		"preact/LICENSE": {
			id:          "preact",
			version:     "10.29.8",
			license:     "MIT",
			licensePath: "preact/LICENSE",
			sourceURL:   "https://registry.npmjs.org/preact/-/preact-10.29.8.tgz",
			sha256:      "1fe6958409c8c257a70c587a18b6f7f412b179b456630790d30b2ec9a8e4b7d4",
		},
		"htm/htm.module.js": {
			id:          "htm",
			version:     "3.1.1",
			license:     "Apache-2.0",
			licensePath: "htm/LICENSE",
			sourceURL:   "https://registry.npmjs.org/htm/-/htm-3.1.1.tgz",
			sha256:      "ab33dd3f38059b9be4d5f5350128eefb2356639c4e0bbe9d9e8b3ba75847e9e4",
		},
		"htm/LICENSE": {
			id:          "htm",
			version:     "3.1.1",
			license:     "Apache-2.0",
			licensePath: "htm/LICENSE",
			sourceURL:   "https://registry.npmjs.org/htm/-/htm-3.1.1.tgz",
			sha256:      "740725f7252e750af735d0028cc534970772f513331e9f68150fede8fb3ce00f",
		},
		"pico/pico.classless.min.css": {
			id:          "pico",
			version:     "2.1.1",
			license:     "MIT",
			licensePath: "pico/LICENSE",
			sourceURL:   "https://registry.npmjs.org/@picocss/pico/-/pico-2.1.1.tgz",
			sha256:      "61207a40ffc02a42d1e50143651c121beab70ed413c934c1ff84fa263ba436b0",
		},
		"pico/LICENSE": {
			id:          "pico",
			version:     "2.1.1",
			license:     "MIT",
			licensePath: "pico/LICENSE",
			sourceURL:   "https://registry.npmjs.org/@picocss/pico/-/pico-2.1.1.tgz",
			sha256:      "afaff063e044233f917b7807dccab022f09dca474f92b642846bee4850655bbf",
		},
	}
}

// staticTree opens the embedded static tree, rooted at the static directory.
func staticTree(tb testing.TB) fs.FS {
	tb.Helper()

	fsys, err := fs.Sub(static, "static")
	if err != nil {
		tb.Fatalf("fs.Sub static: %v", err)
	}

	return fsys
}

// vendorTreeFiles returns every regular file under vendor/ except MANIFEST
// itself, keyed by path relative to vendor/.
func vendorTreeFiles(tb testing.TB, fsys fs.FS) map[string]bool {
	tb.Helper()

	got := make(map[string]bool)
	err := fs.WalkDir(fsys, vendorRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, vendorRoot+"/")
		if rel == "MANIFEST" {
			return nil
		}
		got[rel] = true

		return nil
	})
	if err != nil {
		tb.Fatalf("walk %s: %v", vendorRoot, err)
	}

	return got
}

// vendorFileSHA256 hashes the vendored file at path p, relative to vendor/.
func vendorFileSHA256(tb testing.TB, fsys fs.FS, p string) string {
	tb.Helper()

	b, err := fs.ReadFile(fsys, path.Join(vendorRoot, p))
	if err != nil {
		tb.Fatalf("read %s: %v", p, err)
	}
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// manifestEntry is one non-comment MANIFEST line, split on whitespace.
type manifestEntry struct {
	raw    string
	fields []string
}

// readManifestEntries parses MANIFEST, skipping blank lines and lines
// starting with #.
func readManifestEntries(tb testing.TB, fsys fs.FS) []manifestEntry {
	tb.Helper()

	b, err := fs.ReadFile(fsys, path.Join(vendorRoot, "MANIFEST"))
	if err != nil {
		tb.Fatalf("read MANIFEST: %v", err)
	}

	var entries []manifestEntry
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, manifestEntry{raw: line, fields: strings.Fields(line)})
	}

	return entries
}

func TestVendorManifestCoversTree(t *testing.T) {
	fsys := staticTree(t)
	tree := vendorTreeFiles(t, fsys)
	entries := readManifestEntries(t, fsys)

	counts := make(map[string]int)
	for _, e := range entries {
		if len(e.fields) == 0 {
			t.Fatalf("manifest line %q: no fields", e.raw)
		}
		counts[e.fields[0]]++
		if !tree[e.fields[0]] {
			t.Errorf("manifest names %q, which does not exist under vendor/", e.fields[0])
		}
	}
	for p := range tree {
		if counts[p] != 1 {
			t.Errorf("%s: want exactly one manifest line, got %d", p, counts[p])
		}
	}
}

func TestVendorManifestHashes(t *testing.T) {
	fsys := staticTree(t)
	wanted := wantVendored()
	tree := vendorTreeFiles(t, fsys)

	entryByPath := make(map[string]manifestEntry)
	for _, e := range readManifestEntries(t, fsys) {
		if len(e.fields) > 0 {
			entryByPath[e.fields[0]] = e
		}
	}

	for p := range tree {
		want, ok := wanted[p]
		if !ok {
			t.Errorf("%s: not in wantVendored", p)
			continue
		}
		got := vendorFileSHA256(t, fsys, p)
		if got != want.sha256 {
			t.Errorf("%s: file sha256 = %s, want %s", p, got, want.sha256)
		}
		entry, ok := entryByPath[p]
		if !ok {
			t.Errorf("%s: no manifest line", p)
			continue
		}
		if len(entry.fields) < 6 {
			t.Errorf("manifest line %q: too few fields to check sha256", entry.raw)
			continue
		}
		if entry.fields[5] != want.sha256 {
			t.Errorf("%s: manifest sha256 = %s, want %s", p, entry.fields[5], want.sha256)
		}
	}
}

func TestVendorManifestFields(t *testing.T) {
	fsys := staticTree(t)
	wanted := wantVendored()

	for _, e := range readManifestEntries(t, fsys) {
		if len(e.fields) != 6 {
			t.Errorf("manifest line %q: want 6 fields, got %d", e.raw, len(e.fields))
			continue
		}
		p, id, version, license, sourceURL := e.fields[0], e.fields[1], e.fields[2], e.fields[3], e.fields[4]
		want, ok := wanted[p]
		if !ok {
			t.Errorf("manifest line %q: %s not in wantVendored", e.raw, p)
			continue
		}
		if id != want.id {
			t.Errorf("%s: manifest id = %q, want %q", p, id, want.id)
		}
		if version != want.version {
			t.Errorf("%s: manifest version = %q, want %q", p, version, want.version)
		}
		if license != want.license {
			t.Errorf("%s: manifest license = %q, want %q", p, license, want.license)
		}
		if sourceURL != want.sourceURL {
			t.Errorf("%s: manifest source URL = %q, want %q", p, sourceURL, want.sourceURL)
		}
	}
}

func TestVendorExpectedMapCoversTree(t *testing.T) {
	fsys := staticTree(t)
	wanted := wantVendored()
	tree := vendorTreeFiles(t, fsys)

	for p := range tree {
		if _, ok := wanted[p]; !ok {
			t.Errorf("%s exists under vendor/ but is not in wantVendored", p)
		}
	}
	for p, want := range wanted {
		if !tree[p] {
			t.Errorf("wantVendored names %q, which does not exist under vendor/", p)
			continue
		}
		if _, err := fs.Stat(fsys, path.Join(vendorRoot, want.licensePath)); err != nil {
			t.Errorf("%s: licensePath %q does not exist: %v", p, want.licensePath, err)
		}
	}
}

func TestVendorRelativeImports(t *testing.T) {
	fsys := staticTree(t)

	err := fs.WalkDir(fsys, vendorRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".js") {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if bad := nonRelativeImports(string(b)); len(bad) > 0 {
			t.Errorf("%s: non-relative import specifiers: %v", p, bad)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", vendorRoot, err)
	}
}

// TestVendorImportFreeModels holds the models the tests evaluate to the
// stricter rule: no import statement and no dynamic import at all, where
// app.js and urls.js may hold relative ones.
func TestVendorImportFreeModels(t *testing.T) {
	for _, name := range []string{"portmodel.js", "targetmodel.js", "collectionmodel.js"} {
		t.Run(name, func(t *testing.T) {
			src := readSource(t, name)
			if bad := anyImports(src); len(bad) > 0 {
				t.Errorf("%s: imports: %v", name, bad)
			}
		})
	}
}

func TestVendorNoInlineForms(t *testing.T) {
	fsys := staticTree(t)

	err := fs.WalkDir(fsys, vendorRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		isJS := strings.HasSuffix(p, ".js")
		isCSS := strings.HasSuffix(p, ".css")
		if d.IsDir() || (!isJS && !isCSS) {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if bad := inlineFormFindings(string(b), isJS); len(bad) > 0 {
			t.Errorf("%s: inline-form violations: %v", p, bad)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", vendorRoot, err)
	}
}

func TestVendorLicenseTexts(t *testing.T) {
	fsys := staticTree(t)

	for _, p := range []string{"preact/LICENSE", "pico/LICENSE"} {
		b, err := fs.ReadFile(fsys, path.Join(vendorRoot, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if !strings.Contains(string(b), "MIT License") {
			t.Errorf("%s: does not contain %q", p, "MIT License")
		}
	}

	b, err := fs.ReadFile(fsys, path.Join(vendorRoot, "htm/LICENSE"))
	if err != nil {
		t.Fatalf("read htm/LICENSE: %v", err)
	}
	for _, want := range []string{"Apache License", "Version 2.0"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("htm/LICENSE: does not contain %q", want)
		}
	}
}

func TestVendorFixtureScansFail(t *testing.T) {
	cases := []struct {
		name  string
		found int
	}{
		{"static import bare specifier", len(nonRelativeImports(`import x from "preact"`))},
		{"dynamic import bare specifier", len(nonRelativeImports(`import("htm")`))},
		{"relative static import is still an import", len(anyImports(`import { h } from "./x.js"`))},
		{"relative dynamic import is still an import", len(anyImports(`import("./x.js")`))},
		{"script tag", len(inlineFormFindings(`<script>1</script>`, false))},
		{"on attribute", len(inlineFormFindings(`onclick=`, false))},
		{"eval call", len(inlineFormFindings(`eval(`, true))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.found == 0 {
				t.Errorf("fixture %q: want at least one finding, got none", tc.name)
			}
		})
	}
}
