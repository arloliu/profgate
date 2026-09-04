package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/client"
)

// roundTripFunc is an http.RoundTripper over one function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// refusingTransport fails the test on any request: what proves a local refusal.
func refusingTransport(t *testing.T) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("a request reached the transport: %s %s", r.Method, r.URL)
		return nil, errors.New("refused")
	})
}

// jsonResponse is one response with a JSON body.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// testEnv is a cmdEnv over buffers, a temporary HOME, and a refusing transport; each field a test needs is replaced after.
type testEnv struct {
	env    *cmdEnv
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	vars   map[string]string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	te := &testEnv{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, vars: map[string]string{"HOME": t.TempDir()}}
	te.env = &cmdEnv{
		stdin:  strings.NewReader(""),
		stdout: te.stdout,
		stderr: te.stderr,
		getenv: func(k string) (string, bool) {
			v, ok := te.vars[k]
			return v, ok
		},
		now:             func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) },
		random:          rand.Reader,
		transport:       refusingTransport(t),
		issuerTransport: refusingTransport(t),
		lookPath:        func(name string) (string, error) { return "", errors.New(name + " is not on the test path") },
		run: func(_ context.Context, name string, _ ...string) error {
			t.Errorf("the runner started %s", name)
			return errors.New("refused")
		},
	}
	te.env.prompt = func(user string) (string, error) { return promptPassword(te.env, user) }
	return te
}

// smokeRun is what the smoke verbs record: the parsed invocation.
type smokeRun struct {
	positionals []string
	globals     *globals
	seconds     int
}

// smokeVerbs are the verbs the dispatcher tests drive:
// one with two positionals and a flag of its own, one with subverbs, one with an optional positional, and one that sends GET /v1/whoami as the resolved principal.
func smokeVerbs(t *testing.T, got *smokeRun) []verb {
	t.Helper()
	record := func(in *invocation) {
		got.positionals = in.positionals
		got.globals = in.globals
	}
	var seconds int
	return []verb{
		{
			name: "smoke",
			leaves: []leaf{{
				grammar: "smoke <ns>/<svc> <profile>", positionals: 2,
				flags: func(fs *flag.FlagSet) { fs.IntVar(&seconds, "seconds", 30, "") },
			}},
			run: func(_ context.Context, _ *cmdEnv, in *invocation) int {
				record(in)
				got.seconds = seconds
				return 0
			},
		},
		{
			name: "smokesub",
			leaves: []leaf{
				{words: "get", grammar: "smokesub get <id>", positionals: 1},
				{words: "cancel", grammar: "smokesub cancel <id>", positionals: 1},
			},
			run: func(_ context.Context, _ *cmdEnv, in *invocation) int {
				record(in)
				return 0
			},
		},
		{
			name: "smokeopt", leaves: []leaf{{grammar: "smokeopt [<name>]", positionals: 1, optional: true}},
			run: func(_ context.Context, _ *cmdEnv, in *invocation) int {
				record(in)
				return 0
			},
		},
		{
			name: "smokewhoami", leaves: []leaf{{grammar: "smokewhoami"}},
			run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
				record(in)
				gw, _, err := env.gateway(ctx, in.globals)
				if err != nil {
					return fail(env, err)
				}
				body, _, err := gw.JSON(ctx, client.Request{Method: http.MethodGet, Path: "/v1/whoami"})
				if err != nil {
					return fail(env, err)
				}
				_, _ = env.stdout.Write(body)
				return 0
			},
		},
	}
}

func TestDispatchGrammar(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCode    int
		positionals []string
		server      string
		seconds     int
		wantStderr  string
	}{
		{name: "two positionals", args: []string{"smoke", "payments/checkout", "cpu"}, positionals: []string{"payments/checkout", "cpu"}, seconds: 30},
		{name: "own flag after the positionals", args: []string{"smoke", "payments/checkout", "cpu", "--seconds", "5"}, positionals: []string{"payments/checkout", "cpu"}, seconds: 5},
		{name: "subverb with its positional", args: []string{"smokesub", "cancel", "abc"}, positionals: []string{"abc"}},
		{name: "optional positional present", args: []string{"smokeopt", "prod"}, positionals: []string{"prod"}},
		{name: "optional positional absent", args: []string{"smokeopt"}, positionals: []string{}},
		{name: "no positionals", args: []string{"smokeopt", "--server", "https://g.example"}, positionals: []string{}, server: "https://g.example"},
		{name: "global flag before the verb", args: []string{"--server", "https://a.example", "smoke", "x/y", "cpu"}, positionals: []string{"x/y", "cpu"}, server: "https://a.example", seconds: 30},
		{name: "global flag after the positionals", args: []string{"smoke", "x/y", "cpu", "--server", "https://b.example"}, positionals: []string{"x/y", "cpu"}, server: "https://b.example", seconds: 30},
		{name: "global flag in both places, the later wins", args: []string{"--server", "https://a.example", "smoke", "x/y", "cpu", "--server", "https://b.example"}, positionals: []string{"x/y", "cpu"}, server: "https://b.example", seconds: 30},
		{name: "one positional too few", args: []string{"smoke", "payments/checkout"}, wantCode: 2, wantStderr: "usage: profgate smoke <ns>/<svc> <profile>"},
		{name: "one positional too many", args: []string{"smoke", "payments/checkout", "cpu", "extra"}, wantCode: 2, wantStderr: "usage: profgate smoke <ns>/<svc> <profile>"},
		{name: "subverb positional too many", args: []string{"smokesub", "get", "abc", "def"}, wantCode: 2, wantStderr: "usage: profgate smokesub"},
		{name: "a flag before a positional", args: []string{"smoke", "--seconds", "30", "payments/checkout", "cpu"}, wantCode: 2, wantStderr: "flags follow the positionals"},
		{name: "unknown verb", args: []string{"bogus"}, wantCode: 2, wantStderr: "usage: profgate <"},
		{name: "unknown subverb", args: []string{"smokesub", "bogus", "abc"}, wantCode: 2, wantStderr: "usage: profgate smokesub get|cancel [flags]"},
		{name: "missing subverb", args: []string{"smokesub"}, wantCode: 2, wantStderr: "usage: profgate smokesub get|cancel [flags]"},
		{name: "unknown flag", args: []string{"smoke", "x/y", "cpu", "--bogus"}, wantCode: 2, wantStderr: "usage: profgate smoke <ns>/<svc> <profile>"},
		{name: "unknown global flag before the verb", args: []string{"--bogus", "smoke", "x/y", "cpu"}, wantCode: 2, wantStderr: "usage: profgate <"},
		{name: "a global flag before an operator verb", args: []string{"--server", "https://a.example", "version"}, wantCode: 2, wantStderr: "usage: profgate <"},
		{name: "nothing after the global flags", args: []string{"--server", "https://a.example"}, wantCode: 2, wantStderr: "usage: profgate <"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			var got smokeRun
			code := dispatch(context.Background(), te.env, smokeVerbs(t, &got), tc.args)
			if code != tc.wantCode {
				t.Fatalf("dispatch(%v) = %d, want %d (stderr=%q)", tc.args, code, tc.wantCode, te.stderr.String())
			}
			if tc.wantStderr != "" && !strings.Contains(te.stderr.String(), tc.wantStderr) {
				t.Fatalf("dispatch(%v) stderr = %q, want it to contain %q", tc.args, te.stderr.String(), tc.wantStderr)
			}
			if tc.wantCode != 0 {
				if got.globals != nil {
					t.Fatalf("dispatch(%v) ran the verb on a usage error", tc.args)
				}
				return
			}
			if !slices.Equal(got.positionals, tc.positionals) {
				t.Fatalf("dispatch(%v) positionals = %q, want %q", tc.args, got.positionals, tc.positionals)
			}
			if got.globals.server != tc.server {
				t.Fatalf("dispatch(%v) --server = %q, want %q", tc.args, got.globals.server, tc.server)
			}
			if got.seconds != tc.seconds {
				t.Fatalf("dispatch(%v) --seconds = %d, want %d", tc.args, got.seconds, tc.seconds)
			}
		})
	}
}

// TestDispatchEveryVerbParses drives each registered client verb with its declared positional count and no gateway,
// which reaches the verb's own code and never a usage error.
func TestDispatchEveryVerbParses(t *testing.T) {
	for _, v := range clientVerbs() {
		t.Run(v.name, func(t *testing.T) {
			te := newTestEnv(t)
			for _, l := range v.leaves {
				args := append([]string{v.name}, strings.Fields(l.words)...)
				for i := range l.positionals {
					args = append(args, "positional"+string(rune('a'+i)))
				}
				code := dispatch(context.Background(), te.env, clientVerbs(), args)
				if code == 2 && strings.Contains(te.stderr.String(), "usage: profgate") {
					t.Fatalf("dispatch(%v) was a usage error: %q", args, te.stderr.String())
				}
			}
		})
	}
}

// TestVerbNamespaces asserts the one verb namespace over the two lists:
// the operator half, reserved names included, and the client half never share a name, and each half holds a name once.
func TestVerbNamespaces(t *testing.T) {
	operator := append(slices.Clone(operatorVerbs[:]), reservedOperatorNames[:]...)
	seen := map[string]bool{}
	for _, name := range operator {
		if seen[name] {
			t.Fatalf("operator name %q is listed twice", name)
		}
		seen[name] = true
	}
	for _, v := range clientVerbs() {
		if seen[v.name] {
			t.Fatalf("client verb %q is an operator name", v.name)
		}
		seen[v.name] = true
	}
	for _, want := range []string{"serve", "collector", "version", "config", "auth"} {
		if !slices.Contains(operator, want) {
			t.Fatalf("the operator half lacks %q", want)
		}
	}
}

// TestVerbFlagSets asserts over every command line's assembled flag set:
// nothing disables verification, no flag's value is a token or a password,
// and every command line carries the three flags that name where a credential is read from.
func TestVerbFlagSets(t *testing.T) {
	var got smokeRun
	verbs := append(clientVerbs(), smokeVerbs(t, &got)...)
	for _, v := range verbs {
		for _, l := range v.leaves {
			t.Run(v.subject(l), func(t *testing.T) {
				fs, _ := l.flagSet(v.name, &globals{})
				names := map[string]bool{}
				fs.VisitAll(func(f *flag.Flag) { names[f.Name] = true })
				for name := range names {
					lower := strings.ToLower(name)
					for _, banned := range []string{"insecure", "skip", "verify"} {
						if strings.Contains(lower, banned) {
							t.Fatalf("verb %s registers --%s, which reads as a way to skip verification", v.name, name)
						}
					}
					if strings.Contains(lower, "password") || strings.Contains(lower, "secret") {
						t.Fatalf("verb %s registers --%s, whose value would be a credential", v.name, name)
					}
					// login's --token-type names id or access, never a token.
					if strings.Contains(lower, "token") && name != "token-file" && name != "token-stdin" && name != "token-type" {
						t.Fatalf("verb %s registers --%s; a token is read from a file or stdin, never a flag value", v.name, name)
					}
				}
				for _, want := range []string{"token-file", "token-stdin", "u"} {
					if !names[want] {
						t.Fatalf("verb %s lacks -%s", v.name, want)
					}
				}
				if f := fs.Lookup("u"); f.Value.String() != "" {
					t.Fatalf("-u defaults to %q, want empty", f.Value.String())
				}
			})
		}
	}
}

func TestGlobalFlagsRegistersResolution(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	g := globalFlags(fs)
	args := []string{
		"--context", "prod", "--server", "https://g.example", "--ca-file", "ca.pem",
		"--issuer-ca-file", "iss.pem", "--server-name", "g.example", "--namespace", "payments",
		"--output", "json", "--verbose", "--token-file", "tok", "-u", "alice",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := client.Overrides{Context: "prod", Server: "https://g.example", CAFile: "ca.pem", IssuerCAFile: "iss.pem", ServerName: "g.example", Namespace: "payments", Output: "json"}
	if g.overrides() != want {
		t.Fatalf("overrides = %+v, want %+v", g.overrides(), want)
	}
	if !g.verbose || g.tokenFile != "tok" || g.user != "alice" || g.tokenStdin {
		t.Fatalf("globals = %+v", *g)
	}
}

func TestAddress(t *testing.T) {
	tests := []struct {
		name, arg, contextNamespace string
		wantNamespace, wantService  string
		wantErr                     string
	}{
		{name: "namespace and service", arg: "payments/checkout", wantNamespace: "payments", wantService: "checkout"},
		{name: "bare service with a context namespace", arg: "checkout", contextNamespace: "payments", wantNamespace: "payments", wantService: "checkout"},
		{name: "bare service with none", arg: "checkout", wantErr: "--namespace"},
		{name: "three parts", arg: "a/b/c", wantErr: "<namespace>/<service>"},
		{name: "empty namespace", arg: "/checkout", wantErr: "<namespace>/<service>"},
		{name: "empty service", arg: "payments/", wantErr: "<namespace>/<service>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns, svc, err := address(tc.arg, tc.contextNamespace)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("address(%q, %q) err = %v, want it to name %q", tc.arg, tc.contextNamespace, err, tc.wantErr)
				}
				if !errors.Is(err, client.ErrUsage) {
					t.Fatalf("address(%q) err = %v, want a usage error", tc.arg, err)
				}
				if tc.name == "bare service with none" && !strings.Contains(err.Error(), "namespace") {
					t.Fatalf("err = %v, want the context key named", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("address(%q, %q): %v", tc.arg, tc.contextNamespace, err)
			}
			if ns != tc.wantNamespace || svc != tc.wantService {
				t.Fatalf("address(%q, %q) = %q/%q, want %q/%q", tc.arg, tc.contextNamespace, ns, svc, tc.wantNamespace, tc.wantService)
			}
		})
	}
}

// whoamiTransport answers /v1/whoami with the Authorization header it saw,
// and hands that header to the caller.
func whoamiTransport(t *testing.T, authorization *string) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/whoami" {
			t.Errorf("request to %s, want /v1/whoami", r.URL.Path)
		}
		*authorization = r.Header.Get("Authorization")
		return jsonResponse(http.StatusOK, `{"principal":"alice","realm":{"name":"dev"}}`), nil
	})
}

func TestDispatchCredentials(t *testing.T) {
	t.Run("token file", func(t *testing.T) {
		te := newTestEnv(t)
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte(" tok-from-file \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var authorization string
		te.env.transport = whoamiTransport(t, &authorization)
		var got smokeRun
		code := dispatch(context.Background(), te.env, smokeVerbs(t, &got), []string{"smokewhoami", "--server", "https://g.example", "--token-file", path})
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if authorization != "Bearer tok-from-file" {
			t.Fatalf("Authorization = %q, want the file's token", authorization)
		}
		if !strings.Contains(te.stdout.String(), `"alice"`) {
			t.Fatalf("stdout = %q, want the body", te.stdout.String())
		}
	})

	t.Run("user with the prompt seam", func(t *testing.T) {
		te := newTestEnv(t)
		var prompted string
		te.env.prompt = func(user string) (string, error) {
			prompted = user
			return "s3cret", nil
		}
		var authorization string
		te.env.transport = whoamiTransport(t, &authorization)
		var got smokeRun
		code := dispatch(context.Background(), te.env, smokeVerbs(t, &got), []string{"smokewhoami", "--server", "https://g.example", "-u", "alice"})
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if prompted != "alice" {
			t.Fatalf("the prompt ran for %q, want alice", prompted)
		}
		r := &http.Request{Header: http.Header{"Authorization": {authorization}}}
		user, password, ok := r.BasicAuth()
		if !ok || user != "alice" || password != "s3cret" {
			t.Fatalf("Authorization = %q, want basic alice:s3cret", authorization)
		}
	})

	t.Run("user with stdin a pipe reads one line", func(t *testing.T) {
		te := newTestEnv(t)
		te.env.stdin = strings.NewReader("piped-secret\nsecond line\n")
		var authorization string
		te.env.transport = whoamiTransport(t, &authorization)
		var got smokeRun
		code := dispatch(context.Background(), te.env, smokeVerbs(t, &got), []string{"smokewhoami", "--server", "https://g.example", "-u", "alice"})
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		r := &http.Request{Header: http.Header{"Authorization": {authorization}}}
		_, password, _ := r.BasicAuth()
		if password != "piped-secret" {
			t.Fatalf("password = %q, want the first line of stdin", password)
		}
		if strings.Contains(te.stdout.String()+te.stderr.String(), "piped-secret") {
			t.Fatal("the password reached an output stream")
		}
	})

	t.Run("no gateway selected", func(t *testing.T) {
		te := newTestEnv(t)
		var got smokeRun
		code := dispatch(context.Background(), te.env, smokeVerbs(t, &got), []string{"smokewhoami"})
		if code != 2 {
			t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
		}
		if !strings.Contains(te.stderr.String(), "--server") {
			t.Fatalf("stderr = %q, want the flag named", te.stderr.String())
		}
	})
}
