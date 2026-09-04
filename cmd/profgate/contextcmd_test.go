package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// threeContexts is the file the context tests start from: prod current,
// with an auth snapshot; dev and staging without one.
const threeContexts = `currentContext: prod
contexts:
  dev:
    server: https://dev.example
  prod:
    server: https://prod.example
    namespace: payments
    auth:
      mode: oidc
      issuer: https://issuer.example/realms/eng
      clientID: profgate
      tokenType: id
      scopes: [openid]
      pkce: true
  staging:
    server: https://staging.example:8443
    namespace: orders
`

// prodShown is context show's YAML for prod.
const prodShown = `server: https://prod.example
namespace: payments
auth:
  mode: oidc
  issuer: https://issuer.example/realms/eng
  clientID: profgate
  tokenType: id
  scopes:
    - openid
  pkce: true
`

// prodJSON is the same context under --output json.
const prodJSON = `{"server":"https://prod.example","namespace":"payments","auth":{"mode":"oidc","issuer":"https://issuer.example/realms/eng","clientID":"profgate","tokenType":"id","scopes":["openid"],"pkce":true}}` + "\n"

// The token material a cache entry holds, none of which show may print.
const (
	cachedToken   = "eyJ-the-token-bytes" //nolint:gosec // G101: a fixture the test proves is never printed
	cachedRefresh = "the-refresh-token-bytes"
	cachedExpiry  = "2026-08-28T12:05:00Z"
)

// contextFile is $HOME/.config/profgate/config.yaml under the test HOME.
func contextFile(te *testEnv) string {
	return filepath.Join(te.vars["HOME"], ".config", "profgate", "config.yaml")
}

// tokensDir is $HOME/.local/state/profgate/tokens under the test HOME.
func tokensDir(te *testEnv) string {
	return filepath.Join(te.vars["HOME"], ".local", "state", "profgate", "tokens")
}

// writeContextFile writes body as the contexts file, 0600 in a 0700 directory.
func writeContextFile(t *testing.T, te *testEnv, body string) {
	t.Helper()
	path := contextFile(te)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeCacheEntry writes a cache entry for name holding the token material.
func writeCacheEntry(t *testing.T, te *testEnv, name string) string {
	t.Helper()
	dir := tokensDir(te)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".json")
	body := `{"origin":"https://prod.example:443","issuer":"https://issuer.example/realms/eng","clientID":"profgate","tokenType":"id","token":"` + cachedToken + `","expiresAt":"` + cachedExpiry + `","refreshToken":"` + cachedRefresh + `","obtainedAt":"2026-08-28T12:00:00Z"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runContext drives one context command against the refusing transport the
// test env starts with, so any request fails the test.
func runContext(te *testEnv, args ...string) int {
	return dispatch(context.Background(), te.env, clientVerbs(), append([]string{"context"}, args...))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a test reading its own fixture
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestContextList(t *testing.T) {
	t.Run("three contexts, the current one marked", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "list"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		want := "CURRENT\tNAME\tSERVER\tNAMESPACE\n" +
			"\tdev\thttps://dev.example\t\n" +
			"*\tprod\thttps://prod.example\tpayments\n" +
			"\tstaging\thttps://staging.example:8443\torders\n"
		if te.stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
		}
	})
	t.Run("no file prints the header alone", func(t *testing.T) {
		te := newTestEnv(t)
		if code := runContext(te, "list"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if te.stdout.String() != "CURRENT\tNAME\tSERVER\tNAMESPACE\n" {
			t.Fatalf("stdout = %q, want the header alone", te.stdout.String())
		}
	})
	t.Run("a positional is a usage error", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "list", "prod"); code != 2 {
			t.Fatalf("code = %d, stderr = %q; want 2", code, te.stderr.String())
		}
	})
}

func TestContextShow(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		args       []string
		vars       map[string]string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "the current context as YAML with its auth block", file: threeContexts, args: []string{"show"}, wantStdout: prodShown},
		{name: "a named context", file: threeContexts, args: []string{"show", "staging"}, wantStdout: "server: https://staging.example:8443\nnamespace: orders\n"},
		{name: "a context with no snapshot has no auth block", file: threeContexts, args: []string{"show", "dev"}, wantStdout: "server: https://dev.example\n"},
		{name: "--context selects the one shown", file: threeContexts, args: []string{"show", "--context", "dev"}, wantStdout: "server: https://dev.example\n"},
		{name: "PROFGATE_CONTEXT selects the one shown", file: threeContexts, args: []string{"show"}, vars: map[string]string{"PROFGATE_CONTEXT": "dev"}, wantStdout: "server: https://dev.example\n"},
		{name: "a name with no entry exits 2 naming it", file: threeContexts, args: []string{"show", "nope"}, wantCode: 2, wantStderr: `"nope"`},
		{name: "no current context and no name exits 2", file: "contexts:\n  dev:\n    server: https://dev.example\n", args: []string{"show"}, wantCode: 2, wantStderr: "context use"},
		{name: "no file exits 2", args: []string{"show"}, wantCode: 2, wantStderr: "context use"},
		{name: "--output json is the same context as JSON", file: threeContexts, args: []string{"show", "--output", "json"}, wantStdout: prodJSON},
		{name: "PROFGATE_OUTPUT json", file: threeContexts, args: []string{"show", "prod"}, vars: map[string]string{"PROFGATE_OUTPUT": "json"}, wantStdout: prodJSON},
		{name: "an unknown output is a usage error", file: threeContexts, args: []string{"show", "--output", "xml"}, wantCode: 2, wantStderr: "xml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			if tc.file != "" {
				writeContextFile(t, te, tc.file)
			}
			for k, v := range tc.vars {
				te.vars[k] = v
			}
			code := runContext(te, tc.args...)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stderr = %q", code, tc.wantCode, te.stderr.String())
			}
			if te.stdout.String() != tc.wantStdout {
				t.Fatalf("stdout = %q, want %q", te.stdout.String(), tc.wantStdout)
			}
			if !strings.Contains(te.stderr.String(), tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", te.stderr.String(), tc.wantStderr)
			}
		})
	}

	t.Run("prints no token material with a cache entry present", func(t *testing.T) {
		for _, output := range []string{"table", "json"} {
			t.Run(output, func(t *testing.T) {
				te := newTestEnv(t)
				writeContextFile(t, te, threeContexts)
				writeCacheEntry(t, te, "prod")
				if code := runContext(te, "show", "prod", "--output", output); code != 0 {
					t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
				}
				out := te.stdout.String() + te.stderr.String()
				for _, secret := range []string{cachedToken, cachedRefresh, cachedExpiry} {
					if strings.Contains(out, secret) {
						t.Fatalf("output %q contains the cache entry's %q", out, secret)
					}
				}
			})
		}
	})
}

func TestContextUse(t *testing.T) {
	t.Run("rewrites currentContext atomically", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "use", "staging"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		back := readFile(t, contextFile(te))
		if !strings.HasPrefix(back, "currentContext: staging\n") {
			t.Fatalf("rewritten file = %q, want currentContext: staging first", back)
		}
		for _, name := range []string{"dev:", "prod:", "staging:", "issuer: https://issuer.example/realms/eng"} {
			if !strings.Contains(back, name) {
				t.Fatalf("rewritten file = %q lost %q", back, name)
			}
		}
		info, err := os.Stat(contextFile(te))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
		}
		// The atomic write renames a temporary file over the target and leaves nothing beside it.
		entries, err := os.ReadDir(filepath.Dir(contextFile(te)))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "config.yaml" {
			t.Fatalf("directory holds %d entries, want config.yaml alone", len(entries))
		}
	})
	t.Run("a name with no entry exits 2 and writes nothing", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		before, err := os.Stat(contextFile(te))
		if err != nil {
			t.Fatal(err)
		}
		if code := runContext(te, "use", "nope"); code != 2 {
			t.Fatalf("code = %d, stderr = %q; want 2", code, te.stderr.String())
		}
		if !strings.Contains(te.stderr.String(), `"nope"`) {
			t.Fatalf("stderr = %q, want it to name nope", te.stderr.String())
		}
		after, err := os.Stat(contextFile(te))
		if err != nil {
			t.Fatal(err)
		}
		if readFile(t, contextFile(te)) != threeContexts || !after.ModTime().Equal(before.ModTime()) {
			t.Fatal("the file was rewritten")
		}
	})
	t.Run("no name exits 2", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "use"); code != 2 {
			t.Fatalf("code = %d, stderr = %q; want 2", code, te.stderr.String())
		}
	})
	t.Run("no file exits 2", func(t *testing.T) {
		te := newTestEnv(t)
		if code := runContext(te, "use", "prod"); code != 2 {
			t.Fatalf("code = %d, stderr = %q; want 2", code, te.stderr.String())
		}
		if exists(t, contextFile(te)) {
			t.Fatal("a file was written")
		}
	})
}

// steppingClock advances by step on every call, which is how a lock held by
// another process reaches its bound without a sleep.
func steppingClock(start time.Time, step time.Duration) func() time.Time {
	now := start
	return func() time.Time {
		t := now
		now = now.Add(step)
		return t
	}
}

func TestContextDelete(t *testing.T) {
	t.Run("removes the cache entry and the file entry and releases the lock", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		entry := writeCacheEntry(t, te, "staging")
		if code := runContext(te, "delete", "staging"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if exists(t, entry) {
			t.Fatal("the cache entry survived")
		}
		if exists(t, filepath.Join(tokensDir(te), "staging.lock")) {
			t.Fatal("the lock was not released")
		}
		back := readFile(t, contextFile(te))
		if strings.Contains(back, "staging") {
			t.Fatalf("rewritten file = %q still names staging", back)
		}
		if !strings.HasPrefix(back, "currentContext: prod\n") || !strings.Contains(back, "dev:") {
			t.Fatalf("rewritten file = %q lost the other entries", back)
		}
	})
	t.Run("deleting the current context clears currentContext", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "delete", "prod"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		back := readFile(t, contextFile(te))
		if strings.Contains(back, "currentContext") || strings.Contains(back, "prod") {
			t.Fatalf("rewritten file = %q, want no currentContext and no prod", back)
		}
	})
	t.Run("succeeds with no cache entry", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "delete", "dev"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
	})
	t.Run("a lock held past its bound exits 1 naming the lock and touches neither file", func(t *testing.T) {
		te := newTestEnv(t)
		te.env.now = steppingClock(te.env.now(), 31*time.Second)
		writeContextFile(t, te, threeContexts)
		entry := writeCacheEntry(t, te, "prod")
		lock := filepath.Join(tokensDir(te), "prod.lock")
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if code := runContext(te, "delete", "prod"); code != 1 {
			t.Fatalf("code = %d, stderr = %q; want 1", code, te.stderr.String())
		}
		if !strings.Contains(te.stderr.String(), lock) {
			t.Fatalf("stderr = %q, want it to name %s", te.stderr.String(), lock)
		}
		if !exists(t, entry) || readFile(t, contextFile(te)) != threeContexts {
			t.Fatal("a file was touched under a lock the command does not hold")
		}
	})
	t.Run("a failed cache deletion exits 1 naming the cache file and leaves the context file unchanged", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		// A non-empty directory in the entry's place passes the mode check
		// and cannot be removed.
		entry := filepath.Join(tokensDir(te), "prod.json")
		if err := os.MkdirAll(filepath.Join(entry, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		if code := runContext(te, "delete", "prod"); code != 1 {
			t.Fatalf("code = %d, stderr = %q; want 1", code, te.stderr.String())
		}
		if !strings.Contains(te.stderr.String(), entry) {
			t.Fatalf("stderr = %q, want it to name %s", te.stderr.String(), entry)
		}
		if readFile(t, contextFile(te)) != threeContexts {
			t.Fatal("the context file was rewritten while the credential still exists")
		}
		if exists(t, filepath.Join(tokensDir(te), "prod.lock")) {
			t.Fatal("the lock was not released")
		}
	})
	t.Run("a failed file write exits 1 naming the context file, and a second run completes", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes into a read-only directory")
		}
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		entry := writeCacheEntry(t, te, "prod")
		dir := filepath.Dir(contextFile(te))
		if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // G302: the config directory made read-only
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: the config directory's own mode
		if code := runContext(te, "delete", "prod"); code != 1 {
			t.Fatalf("code = %d, stderr = %q; want 1", code, te.stderr.String())
		}
		if !strings.Contains(te.stderr.String(), contextFile(te)) {
			t.Fatalf("stderr = %q, want it to name %s", te.stderr.String(), contextFile(te))
		}
		if exists(t, entry) {
			t.Fatal("the cache entry survived: the deletion happens before the write")
		}
		if readFile(t, contextFile(te)) != threeContexts {
			t.Fatal("the context file changed under a failed write")
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: the config directory's own mode
			t.Fatal(err)
		}
		te.stderr.Reset()
		if code := runContext(te, "delete", "prod"); code != 0 {
			t.Fatalf("second run: code = %d, stderr = %q", code, te.stderr.String())
		}
		if strings.Contains(readFile(t, contextFile(te)), "prod") {
			t.Fatal("the second run left prod in the file")
		}
	})
	t.Run("a name with no entry exits 2", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "delete", "nope"); code != 2 {
			t.Fatalf("code = %d, stderr = %q; want 2", code, te.stderr.String())
		}
		if readFile(t, contextFile(te)) != threeContexts {
			t.Fatal("the file was rewritten")
		}
	})
	t.Run("no name exits 2", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "delete"); code != 2 {
			t.Fatalf("code = %d, stderr = %q; want 2", code, te.stderr.String())
		}
	})
}

// TestContextSubverbs asserts the four subverbs make no request, which the
// refusing transport in the test env proves, and that an unknown subverb prints the usage line and exits 2.
func TestContextSubverbs(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"show"}, {"use", "dev"}, {"delete", "dev"}} {
		t.Run(args[0], func(t *testing.T) {
			te := newTestEnv(t)
			writeContextFile(t, te, threeContexts)
			writeCacheEntry(t, te, "dev")
			if code := runContext(te, args...); code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
		})
	}
	t.Run("unknown subverb", func(t *testing.T) {
		te := newTestEnv(t)
		if code := runContext(te, "bogus"); code != 2 {
			t.Fatalf("code = %d, want 2", code)
		}
		if !strings.Contains(te.stderr.String(), "usage: profgate context list|show|use|delete") {
			t.Fatalf("stderr = %q, want the usage line", te.stderr.String())
		}
	})
}

// TestContextLeafPositionals asserts each context subverb takes the positionals of its own command line,
// and that a usage error prints that subverb's grammar line below its cause.
func TestContextLeafPositionals(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		cause   string
		grammar string
	}{
		{
			name: "list takes none", args: []string{"list", "foo"},
			cause: `context list takes no positional; "foo" is one too many`, grammar: "usage: profgate context list",
		},
		{
			name: "use takes a name", args: []string{"use"},
			cause: "context use takes one positional", grammar: "usage: profgate context use <name>",
		},
		{
			name: "delete takes a name", args: []string{"delete"},
			cause: "context delete takes one positional", grammar: "usage: profgate context delete <name>",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			if code := runContext(te, tc.args...); code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
			}
			for _, want := range []string{tc.cause, tc.grammar} {
				if !strings.Contains(te.stderr.String(), want) {
					t.Fatalf("stderr = %q, want it to contain %q", te.stderr.String(), want)
				}
			}
		})
	}
}

// TestContextDeleteSpeaks pins what a successful delete says on stderr, and
// that clearing the selection is said as well.
func TestContextDeleteSpeaks(t *testing.T) {
	t.Run("a context that was not selected says what was deleted", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "delete", "staging"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if te.stderr.String() != "deleted context staging\n" {
			t.Fatalf("stderr = %q, want the line naming what was deleted", te.stderr.String())
		}
		if te.stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want nothing", te.stdout.String())
		}
	})

	t.Run("the selected context says the selection is gone", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "delete", "prod"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		want := "deleted context prod\nno context is selected; select one with profgate context use\n"
		if te.stderr.String() != want {
			t.Fatalf("stderr = %q, want %q", te.stderr.String(), want)
		}
	})

	t.Run("a name the file does not hold says only the refusal", func(t *testing.T) {
		te := newTestEnv(t)
		writeContextFile(t, te, threeContexts)
		if code := runContext(te, "delete", "nope"); code != 2 {
			t.Fatalf("code = %d, stderr = %q; want 2", code, te.stderr.String())
		}
		if strings.Contains(te.stderr.String(), "deleted context") {
			t.Fatalf("stderr = %q, want no deletion line", te.stderr.String())
		}
	})
}
