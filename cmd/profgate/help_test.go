package main

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// unreadableStdin fails the test on any read: help resolves no context file,
// reads no token cache, and reads no byte of stdin.
type unreadableStdin struct{ t *testing.T }

func (s unreadableStdin) Read([]byte) (int, error) {
	s.t.Errorf("help read stdin")
	return 0, errors.New("stdin was read")
}

// globalFlagNames is what every client page prints beside its own.
var globalFlagNames = []string{
	"context", "server", "ca-file", "issuer-ca-file", "server-name", "namespace",
	"output", "verbose", "token-file", "token-stdin", "u",
}

// helpCase is one command line the binary answers a help argument for:
// the words that name it,
// the whole usage line it prints,
// the flag names its own page carries,
// and the subverbs a group lists.
type helpCase struct {
	name     string
	args     []string
	usage    string
	flags    []string
	children []string
}

const bareUsage = "usage: profgate <version|config validate|auth hash|serve|login|logout|whoami|limits|" +
	"namespaces|services|targets|profile|collect|collections|collection|download|pgo|context> [flags]"

// clientHelpCases is the bare binary, the twenty client leaves, and the four client groups.
func clientHelpCases() []helpCase {
	return []helpCase{
		{name: "bare binary", args: nil, usage: bareUsage},
		{
			name: "login", args: []string{"login"},
			usage: "usage: profgate login [--issuer <url>] [--client-id <id>] [--token-type id|access] " +
				"[--scope <scope>]... [--pkce|--no-pkce] [--login-timeout <duration>]",
			flags: []string{"issuer", "client-id", "token-type", "scope", "pkce", "no-pkce", "login-timeout"},
		},
		{name: "logout", args: []string{"logout"}, usage: "usage: profgate logout"},
		{name: "whoami", args: []string{"whoami"}, usage: "usage: profgate whoami"},
		{name: "limits", args: []string{"limits"}, usage: "usage: profgate limits"},
		{name: "namespaces", args: []string{"namespaces"}, usage: "usage: profgate namespaces"},
		{name: "services", args: []string{"services"}, usage: "usage: profgate services <namespace>"},
		{
			name: "targets", args: []string{"targets"},
			usage: "usage: profgate targets <ns>/<svc> [--port <n> | --port-name <name>] [--explain]",
			flags: []string{"port", "port-name", "explain"},
		},
		{
			name: "profile", args: []string{"profile"},
			usage: "usage: profgate profile <ns>/<svc> <profile> [--seconds <n>] [--pod <name>] [--version <v>] " +
				"[--port <n> | --port-name <name>] [-o <path>] [--open]",
			flags: []string{"seconds", "pod", "version", "port", "port-name", "o", "open"},
		},
		{
			name: "collect", args: []string{"collect"},
			usage: "usage: profgate collect <ns>/<svc> [--duration <d>] [--rounds <n>] [--round-interval <d>] " +
				"[--replicas all|<n>] [--max-parallel <n>] [--target-version <v>] [--retention <d>] [--file <path>] " +
				"[--wait] [--poll-interval <d>] [--wait-timeout <d>]",
			flags: []string{
				"duration", "rounds", "round-interval", "replicas", "max-parallel", "target-version",
				"retention", "file", "wait", "poll-interval", "wait-timeout",
			},
		},
		{name: "collections", args: []string{"collections"}, usage: "usage: profgate collections <ns>/<svc>"},
		{
			name: "collection group", args: []string{"collection"},
			usage: "usage: profgate collection get|cancel", children: []string{"get", "cancel"},
		},
		{name: "collection get", args: []string{"collection", "get"}, usage: "usage: profgate collection get <id>"},
		{name: "collection cancel", args: []string{"collection", "cancel"}, usage: "usage: profgate collection cancel <id>"},
		{
			name: "download", args: []string{"download"},
			usage: "usage: profgate download <id> [-o <path>]", flags: []string{"o"},
		},
		{name: "pgo group", args: []string{"pgo"}, usage: "usage: profgate pgo policy", children: []string{"policy"}},
		{
			name: "pgo policy group", args: []string{"pgo", "policy"},
			usage: "usage: profgate pgo policy get|set|delete", children: []string{"get", "set", "delete"},
		},
		{name: "pgo policy get", args: []string{"pgo", "policy", "get"}, usage: "usage: profgate pgo policy get <ns>/<svc>"},
		{
			name: "pgo policy set", args: []string{"pgo", "policy", "set"},
			usage: "usage: profgate pgo policy set <ns>/<svc> [--file <path> | --enabled[=false] --every <d> " +
				"--jitter <d> <field flags of collect>]",
			flags: []string{
				"file", "enabled", "every", "jitter", "duration", "rounds", "round-interval",
				"replicas", "max-parallel", "target-version", "retention",
			},
		},
		{name: "pgo policy delete", args: []string{"pgo", "policy", "delete"}, usage: "usage: profgate pgo policy delete <ns>/<svc>"},
		{
			name: "context group", args: []string{"context"},
			usage: "usage: profgate context list|show|use|delete", children: []string{"list", "show", "use", "delete"},
		},
		{name: "context list", args: []string{"context", "list"}, usage: "usage: profgate context list"},
		{name: "context show", args: []string{"context", "show"}, usage: "usage: profgate context show [<name>]"},
		{name: "context use", args: []string{"context", "use"}, usage: "usage: profgate context use <name>"},
		{name: "context delete", args: []string{"context", "delete"}, usage: "usage: profgate context delete <name>"},
	}
}

// helpOf runs one command line and returns its stdout, failing the test
// unless it exited 0 against a transport and a stdin that refuse to be used.
func helpOf(t *testing.T, args ...string) string {
	t.Helper()
	te := newTestEnv(t)
	te.env.stdin = unreadableStdin{t}
	code := dispatch(context.Background(), te.env, clientVerbs(), args)
	if code != exitOK {
		t.Fatalf("dispatch(%v) = %d, want 0 (stderr=%q)", args, code, te.stderr.String())
	}
	return te.stdout.String()
}

// firstLine is a page's usage line.
func firstLine(page string) string {
	line, _, _ := strings.Cut(page, "\n")
	return line
}

// hasFlagName reports whether a page's flag blocks name the flag, which
// PrintDefaults writes as two leading spaces, a dash, and the name.
func hasFlagName(page, name string) bool {
	return strings.Contains(page, "\n  -"+name+"\n") || strings.Contains(page, "\n  -"+name+" ")
}

// helpBlock is the indented lines under a page's heading, up to the blank line that ends them.
func helpBlock(page, heading string) string {
	_, after, ok := strings.Cut(page, "\n"+heading+"\n")
	if !ok {
		return ""
	}
	block, _, _ := strings.Cut(after, "\n\n")
	return block + "\n"
}

func TestHelpEveryClientCommandLine(t *testing.T) {
	for _, tc := range clientHelpCases() {
		for _, spelling := range []string{"-h", "--help"} {
			t.Run(tc.name+" "+spelling, func(t *testing.T) {
				page := helpOf(t, append(slices.Clone(tc.args), spelling)...)
				if got := firstLine(page); got != tc.usage {
					t.Fatalf("usage line = %q, want %q", got, tc.usage)
				}
				for _, name := range tc.flags {
					if !hasFlagName(page, name) {
						t.Fatalf("page = %q, want it to name -%s", page, name)
					}
				}
				for _, child := range tc.children {
					if !strings.Contains(helpBlock(page, "Subcommands:"), "  "+child+"\n") {
						t.Fatalf("page = %q, want it to list the subverb %s", page, child)
					}
				}
				for _, name := range globalFlagNames {
					if !hasFlagName(page, name) {
						t.Fatalf("page = %q, want it to name the global flag -%s", page, name)
					}
				}
			})
		}
	}
}

// TestHelpLeafPagesAreTheLeafs asserts a subverb's page carries what that command line takes,
// and nothing a sibling takes.
func TestHelpLeafPagesAreTheLeafs(t *testing.T) {
	setOnly := []string{"file", "enabled", "every"}
	for _, args := range [][]string{{"pgo", "policy", "get"}, {"pgo", "policy", "delete"}} {
		page := helpOf(t, append(slices.Clone(args), "--help")...)
		for _, name := range setOnly {
			if hasFlagName(page, name) {
				t.Fatalf("the %v page names -%s, which only set registers: %q", args, name, page)
			}
		}
	}
	page := helpOf(t, "pgo", "policy", "set", "--help")
	for _, name := range setOnly {
		if !hasFlagName(page, name) {
			t.Fatalf("the pgo policy set page lacks -%s: %q", name, page)
		}
	}
	// The whole line, because the group's line opens with the same characters.
	if got := firstLine(helpOf(t, "context", "list", "--help")); got != "usage: profgate context list" {
		t.Fatalf("context list usage line = %q", got)
	}
	if got := firstLine(helpOf(t, "context", "show", "--help")); got != "usage: profgate context show [<name>]" {
		t.Fatalf("context show usage line = %q", got)
	}
	for _, sub := range []string{"use", "delete"} {
		want := "usage: profgate context " + sub + " <name>"
		if got := firstLine(helpOf(t, "context", sub, "--help")); got != want {
			t.Fatalf("context %s usage line = %q, want %q", sub, got, want)
		}
	}
}

// TestHelpGroupPagesAreTheirChildren asserts each group's page names its own immediate children,
// and no flag.
func TestHelpGroupPagesAreTheirChildren(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		usage string
		block string
	}{
		{name: "pgo", args: []string{"pgo"}, usage: "usage: profgate pgo policy", block: "  policy\n"},
		{
			name: "pgo policy", args: []string{"pgo", "policy"},
			usage: "usage: profgate pgo policy get|set|delete", block: "  get\n  set\n  delete\n",
		},
		{
			name: "collection", args: []string{"collection"},
			usage: "usage: profgate collection get|cancel", block: "  get\n  cancel\n",
		},
		{
			name: "context", args: []string{"context"},
			usage: "usage: profgate context list|show|use|delete", block: "  list\n  show\n  use\n  delete\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page := helpOf(t, append(slices.Clone(tc.args), "--help")...)
			if got := firstLine(page); got != tc.usage {
				t.Fatalf("usage line = %q, want %q", got, tc.usage)
			}
			if got := helpBlock(page, "Subcommands:"); got != tc.block {
				t.Fatalf("subcommands = %q, want %q", got, tc.block)
			}
			if strings.Contains(page, "\nFlags:\n") {
				t.Fatalf("the %s page carries a Flags block: %q", tc.name, page)
			}
			for _, name := range []string{"file", "enabled", "every"} {
				if hasFlagName(page, name) {
					t.Fatalf("the %s page names -%s, which belongs to a leaf: %q", tc.name, name, page)
				}
			}
		})
	}
	if page := helpOf(t, "pgo", "--help"); strings.Contains(helpBlock(page, "Subcommands:"), "  get\n") {
		t.Fatalf("the pgo page lists a grandchild: %q", page)
	}
}

// TestValueTakingGlobalFlags pins the global half of the set the page walk steps over:
// a flag misclassified here silently changes which page prints.
func TestValueTakingGlobalFlags(t *testing.T) {
	want := []string{
		"ca-file", "context", "issuer-ca-file", "namespace", "output",
		"server", "server-name", "token-file", "u",
	}
	got := slices.Sorted(maps.Keys(valueGlobals()))
	if !slices.Equal(got, want) {
		t.Fatalf("valueGlobals() = %q, want %q", got, want)
	}
}

// TestHelpSpellings asserts the five spellings flag itself reads as help are one flag here too.
func TestHelpSpellings(t *testing.T) {
	for _, args := range [][]string{nil, {"profile"}} {
		var first string
		for _, spelling := range []string{"-h", "--h", "-help", "--help", "--help=0"} {
			page := helpOf(t, append(slices.Clone(args), spelling)...)
			if first == "" {
				first = page
				continue
			}
			if page != first {
				t.Fatalf("%v %s printed %q, want %q", args, spelling, page, first)
			}
		}
	}
}

// TestHelpWinsOverParsing asserts help is answered before positionals are stripped and before any flag set parses.
func TestHelpWinsOverParsing(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		usage string
	}{
		{name: "too few positionals", args: []string{"profile", "--help"}, usage: "usage: profgate profile <ns>/<svc> <profile> [--seconds <n>] [--pod <name>] [--version <v>] [--port <n> | --port-name <name>] [-o <path>] [--open]"},
		{name: "missing subverb", args: []string{"collection", "--help"}, usage: "usage: profgate collection get|cancel"},
		{name: "after the positionals", args: []string{"profile", "payments/checkout", "cpu", "--help"}, usage: "usage: profgate profile <ns>/<svc> <profile> [--seconds <n>] [--pod <name>] [--version <v>] [--port <n> | --port-name <name>] [-o <path>] [--open]"},
		{name: "after a flag with a value", args: []string{"collect", "payments/checkout", "--duration", "30s", "--help"}, usage: "usage: profgate collect <ns>/<svc> [--duration <d>] [--rounds <n>] [--round-interval <d>] [--replicas all|<n>] [--max-parallel <n>] [--target-version <v>] [--retention <d>] [--file <path>] [--wait] [--poll-interval <d>] [--wait-timeout <d>]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstLine(helpOf(t, tc.args...)); got != tc.usage {
				t.Fatalf("usage line = %q, want %q", got, tc.usage)
			}
		})
	}
}

// TestHelpPosition asserts the page is the deepest verb the line names, wherever the help argument sits.
func TestHelpPosition(t *testing.T) {
	before := helpOf(t, "--help", "profile")
	after := helpOf(t, "profile", "--help")
	if before != after {
		t.Fatalf("--help before the verb printed %q, want %q", before, after)
	}
}

// TestHelpUnknownName asserts a name the binary does not have prints the bare binary's help,
// while the same name without a help argument is still a usage error.
func TestHelpUnknownName(t *testing.T) {
	bare := helpOf(t, "--help")
	for _, args := range [][]string{{"frobnicate", "--help"}, {"collection", "frobnicate", "--help"}} {
		if page := helpOf(t, args...); page != bare {
			t.Fatalf("%v printed %q, want the bare binary's help %q", args, page, bare)
		}
	}
	te := newTestEnv(t)
	code := dispatch(context.Background(), te.env, clientVerbs(), []string{"frobnicate"})
	if code != 2 || !strings.Contains(te.stderr.String(), "usage: profgate <") {
		t.Fatalf("frobnicate = %d, stderr = %q, want 2 and the usage line", code, te.stderr.String())
	}
}

// TestHelpStopsAtDoubleDash asserts the search stops where flag stops, and
// that the attached form reaches the flag it names.
func TestHelpStopsAtDoubleDash(t *testing.T) {
	t.Run("after a bare dash dash", func(t *testing.T) {
		te := newTestEnv(t)
		args := []string{"collect", "payments/checkout", "--", "--help"}
		code := dispatch(context.Background(), te.env, clientVerbs(), args)
		if code != 2 || !strings.Contains(te.stderr.String(), "is one too many") {
			t.Fatalf("dispatch(%v) = %d, stderr = %q, want 2 and one too many", args, code, te.stderr.String())
		}
		if te.stdout.String() != "" {
			t.Fatalf("stdout = %q, want nothing", te.stdout.String())
		}
	})
	t.Run("attached to a flag that takes a path", func(t *testing.T) {
		te := newTestEnv(t)
		code, dir := runProfile(t, te, &profileTransport{}, "payments/checkout", "cpu", "-o=--help")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if _, err := os.Stat(dir + "/--help"); err != nil {
			t.Fatalf("-o=--help wrote no file named --help: %v", err)
		}
	})
}

// TestHelpFlagValueIsNotAVerb asserts the page walk steps over a flag's separated value,
// so a value that spells a verb names no page.
func TestHelpFlagValueIsNotAVerb(t *testing.T) {
	bare := helpOf(t, "--help")
	for _, args := range [][]string{
		{"--namespace", "collect", "--help"},
		{"--namespace=collect", "--help"},
	} {
		if page := helpOf(t, args...); page != bare {
			t.Fatalf("%v printed %q, want the bare binary's help", args, page)
		}
	}
	verbose := helpOf(t, "--verbose", "collect", "--help")
	if got := firstLine(verbose); !strings.HasPrefix(got, "usage: profgate collect <ns>/<svc>") {
		t.Fatalf("--verbose collect --help usage line = %q, want the collect page", got)
	}
	// The whole line: the get leaf's line opens with the same four words.
	group := helpOf(t, "pgo", "policy", "--file", "get", "--help")
	if got := firstLine(group); got != "usage: profgate pgo policy get|set|delete" {
		t.Fatalf("pgo policy --file get --help usage line = %q, want the group's", got)
	}
	if got := helpBlock(group, "Subcommands:"); got != "  get\n  set\n  delete\n" {
		t.Fatalf("subcommands = %q, want the group's three", got)
	}
	collect := helpOf(t, "collect", "payments/checkout", "--duration", "30s", "--help")
	if got := firstLine(collect); !strings.HasPrefix(got, "usage: profgate collect <ns>/<svc>") {
		t.Fatalf("collect usage line = %q", got)
	}
}

// TestHelpWritesNothingToStderr asserts help is what the command asked for:
// it goes to stdout alone, and flag's own refusal never reaches either stream.
func TestHelpWritesNothingToStderr(t *testing.T) {
	for _, tc := range append(clientHelpCases(), operatorHelpCases()...) {
		for _, spelling := range []string{"-h", "--help"} {
			t.Run(tc.name+" "+spelling, func(t *testing.T) {
				te := newTestEnv(t)
				te.env.stdin = unreadableStdin{t}
				args := append(slices.Clone(tc.args), spelling)
				if code := dispatch(context.Background(), te.env, clientVerbs(), args); code != exitOK {
					t.Fatalf("dispatch(%v) = %d, want 0", args, code)
				}
				if te.stderr.String() != "" {
					t.Fatalf("stderr = %q, want nothing", te.stderr.String())
				}
				if strings.Contains(te.stdout.String(), "flag: help requested") {
					t.Fatalf("stdout = %q, want no word of flag's own", te.stdout.String())
				}
			})
		}
	}
}

// operatorHelpCases is the six operator command lines that have a grammar to print.
// Each takes no global flag, so no page of theirs prints one.
func operatorHelpCases() []helpCase {
	return []helpCase{
		{
			name: "serve", args: []string{"serve"},
			usage: "usage: profgate serve --config <path>", flags: []string{"config"},
		},
		{name: "version", args: []string{"version"}, usage: "usage: profgate version"},
		{
			name: "config group", args: []string{"config"},
			usage: "usage: profgate config validate", children: []string{"validate"},
		},
		{
			name: "config validate", args: []string{"config", "validate"},
			usage: "usage: profgate config validate --config <path>", flags: []string{"config"},
		},
		{
			name: "auth group", args: []string{"auth"},
			usage: "usage: profgate auth hash", children: []string{"hash"},
		},
		{name: "auth hash", args: []string{"auth", "hash"}, usage: "usage: profgate auth hash"},
	}
}

// TestHelpEveryOperatorCommandLine asserts the operator half answers for itself,
// and prints no flag it does not accept.
func TestHelpEveryOperatorCommandLine(t *testing.T) {
	for _, tc := range operatorHelpCases() {
		for _, spelling := range []string{"-h", "--help"} {
			t.Run(tc.name+" "+spelling, func(t *testing.T) {
				page := helpOf(t, append(slices.Clone(tc.args), spelling)...)
				if got := firstLine(page); got != tc.usage {
					t.Fatalf("usage line = %q, want %q", got, tc.usage)
				}
				for _, name := range tc.flags {
					if !hasFlagName(page, name) {
						t.Fatalf("page = %q, want it to name -%s", page, name)
					}
				}
				for _, child := range tc.children {
					if !strings.Contains(helpBlock(page, "Subcommands:"), "  "+child+"\n") {
						t.Fatalf("page = %q, want it to list the subverb %s", page, child)
					}
				}
				for _, name := range globalFlagNames {
					if hasFlagName(page, name) {
						t.Fatalf("page = %q, want no global flag; it names -%s", page, name)
					}
				}
			})
		}
	}
	// collector names no implementation yet, so it has no grammar to print and
	// gets the answer any name the binary does not run gets.
	// That page is the bare binary's, which lists the global flags by definition.
	bare := helpOf(t, "--help")
	for _, spelling := range []string{"-h", "--help"} {
		t.Run("collector "+spelling, func(t *testing.T) {
			if page := helpOf(t, "collector", spelling); page != bare {
				t.Fatalf("collector %s printed %q, want the bare binary's help %q", spelling, page, bare)
			}
		})
	}
}

// TestHelpAnswersAuthHashBeforeThePassword asserts the one command line that would otherwise read a stream:
// help is answered before the prompt.
func TestHelpAnswersAuthHashBeforeThePassword(t *testing.T) {
	te := newTestEnv(t)
	te.env.stdin = unreadableStdin{t}
	args := []string{"auth", "hash", "--help"}
	if code := dispatch(context.Background(), te.env, clientVerbs(), args); code != exitOK {
		t.Fatalf("dispatch(%v) = %d, want 0 (stderr=%q)", args, code, te.stderr.String())
	}
	if got := firstLine(te.stdout.String()); got != "usage: profgate auth hash" {
		t.Fatalf("usage line = %q, want %q", got, "usage: profgate auth hash")
	}
	if te.stderr.String() != "" {
		t.Fatalf("stderr = %q, want nothing", te.stderr.String())
	}
}

// TestOperatorNameBehindAGlobalFlag asserts that help answers the whole line before the rule
// that an operator command line takes no global flag applies,
// and that the search for a page does not turn that refusal into a success.
func TestOperatorNameBehindAGlobalFlag(t *testing.T) {
	te := newTestEnv(t)
	args := []string{"--server", "https://g.example", "version"}
	code := dispatch(context.Background(), te.env, clientVerbs(), args)
	if code != 2 || !strings.Contains(te.stderr.String(), "version takes no global flags") {
		t.Fatalf("dispatch(%v) = %d, stderr = %q, want 2 and the rule", args, code, te.stderr.String())
	}
	page := helpOf(t, "--server", "https://g.example", "version", "--help")
	if got := firstLine(page); got != "usage: profgate version" {
		t.Fatalf("usage line = %q, want %q", got, "usage: profgate version")
	}
}

// TestOperatorGroupsAnswerHelpOnTheirOwn asserts the two operator groups answer a help argument themselves:
// neither reaches a flag set that could return flag's own help error,
// so a page they did not print would be a usage error instead.
func TestOperatorGroupsAnswerHelpOnTheirOwn(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runConfig([]string{"--help"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("runConfig(--help) = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if got := firstLine(stdout.String()); got != "usage: profgate config validate" {
		t.Fatalf("usage line = %q, want %q", got, "usage: profgate config validate")
	}
	stdout.Reset()
	if code := runAuth([]string{"--help"}, unreadableStdin{t}, &stdout, &stderr); code != exitOK {
		t.Fatalf("runAuth(--help) = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	if got := firstLine(stdout.String()); got != "usage: profgate auth hash" {
		t.Fatalf("usage line = %q, want %q", got, "usage: profgate auth hash")
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want nothing", stderr.String())
	}
}
