package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/arloliu/profgate/internal/client"
)

// verb is one client verb: its name, its subverbs, how many positionals it
// takes, its own flags, and what it does with them.
// A subverb is one or more words,
// so pgo declares "policy get" and its two siblings and the dispatcher matches all of them.
// The dispatcher removes exactly the declared positionals from the front of
// the arguments, parses everything after them over the global flags and the
// verb's own, and hands run the result.
type verb struct {
	name        string
	subverbs    []string
	positionals int
	optional    bool   // one trailing positional may be absent: context show
	grammar     string // the usage line after "profgate ": profile <ns>/<svc> <profile>
	flags       func(fs *flag.FlagSet)
	run         func(ctx context.Context, env *cmdEnv, in *invocation) int
}

// invocation is one parsed command line: the subverb, the positionals, the
// global flags after both positions, and the flag set the verb's own values were parsed from.
type invocation struct {
	subverb     string
	positionals []string
	globals     *globals
	fs          *flag.FlagSet
}

// cmdEnv is what every verb runs against: the three standard streams, the
// environment seam, the clock and the sleeper a wait paces itself on, the
// random source the idempotency key is drawn from, whether stdout is a
// terminal, the password prompt, the two transports a test replaces, and the
// path lookup and command runner that --open resolves and starts the viewer through.
type cmdEnv struct {
	stdin           io.Reader
	stdout          io.Writer
	stderr          io.Writer
	getenv          func(string) (string, bool)
	now             func() time.Time
	sleep           func(context.Context, time.Duration) error // nil is the real one
	random          io.Reader
	terminal        bool // stdout is a terminal
	prompt          func(user string) (password string, err error)
	transport       http.RoundTripper // nil builds one from the resolved settings
	issuerTransport http.RoundTripper // nil builds one from the issuer CA file
	lookPath        func(name string) (string, error)
	run             func(ctx context.Context, name string, args ...string) error
}

// newEnv is the process's own environment.
func newEnv(stdin io.Reader, stdout, stderr io.Writer) *cmdEnv {
	env := &cmdEnv{
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		getenv:   os.LookupEnv,
		now:      time.Now,
		random:   rand.Reader,
		terminal: isTerminal(stdout),
		lookPath: exec.LookPath,
	}
	env.prompt = func(user string) (string, error) { return promptPassword(env, user) }
	env.run = func(ctx context.Context, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: --open runs the go that LookPath resolved, with the arguments the verb fixes
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		return cmd.Run()
	}
	return env
}

// isTerminal reports whether w is a character device, which is what decides padded columns against tab-separated ones.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// globals is what the global flags said: the resolution flags, --verbose,
// and the three credential flags, each naming where a credential is read from and never the credential.
type globals struct {
	context, server, caFile, issuerCAFile, serverName, namespace, output string
	verbose                                                              bool
	tokenFile                                                            string
	tokenStdin                                                           bool
	user                                                                 string
}

// globalFlags registers the flags of Resolution and the three credential flags on fs, and returns what they said.
func globalFlags(fs *flag.FlagSet) *globals {
	g := &globals{}
	g.register(fs)
	return g
}

// register registers the global flags on fs with the values g already holds
// as their defaults, which is how a later occurrence wins over an earlier one.
func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.context, "context", g.context, "the context to use; PROFGATE_CONTEXT when absent")
	fs.StringVar(&g.server, "server", g.server, "the gateway's base URL; PROFGATE_SERVER when absent")
	fs.StringVar(&g.caFile, "ca-file", g.caFile, "extra certificates to trust for the gateway; PROFGATE_CA_FILE when absent")
	fs.StringVar(&g.issuerCAFile, "issuer-ca-file", g.issuerCAFile, "extra certificates to trust for the issuer; PROFGATE_ISSUER_CA_FILE when absent")
	fs.StringVar(&g.serverName, "server-name", g.serverName, "the TLS server name; PROFGATE_SERVER_NAME when absent")
	fs.StringVar(&g.namespace, "namespace", g.namespace, "the namespace a bare <service> means; PROFGATE_NAMESPACE when absent")
	fs.StringVar(&g.output, "output", g.output, "table or json; PROFGATE_OUTPUT when absent")
	fs.BoolVar(&g.verbose, "verbose", g.verbose, "print one line per HTTP request")
	fs.StringVar(&g.tokenFile, "token-file", g.tokenFile, "read the token from this file for this one command")
	fs.BoolVar(&g.tokenStdin, "token-stdin", g.tokenStdin, "read the token from stdin for this one command")
	fs.StringVar(&g.user, "u", g.user, "the basic user name; the password is prompted for, or PROFGATE_PASSWORD")
}

// overrides is the resolution half of the global flags.
func (g *globals) overrides() client.Overrides {
	return client.Overrides{
		Context:      g.context,
		Server:       g.server,
		CAFile:       g.caFile,
		IssuerCAFile: g.issuerCAFile,
		ServerName:   g.serverName,
		Namespace:    g.namespace,
		Output:       g.output,
	}
}

// operatorVerbs and reservedOperatorNames are the operator half of the
// binary's one verb namespace; clientVerbs is the client half.
// A name belongs to one half permanently, and reservedOperatorNames holds
// the operator names that have no implementation yet, so the client half can never take one.
var operatorVerbs = [...]string{"serve", "version", "config", "auth"}

var reservedOperatorNames = [...]string{"collector"}

// clientVerbs is the client half, in the order the usage line prints them.
func clientVerbs() []verb {
	return []verb{
		loginVerb(),
		logoutVerb(),
		whoamiVerb(),
		limitsVerb(),
		namespacesVerb(),
		servicesVerb(),
		targetsVerb(),
		profileVerb(),
		collectVerb(),
		collectionsVerb(),
		collectionVerb(),
		downloadVerb(),
		pgoVerb(),
		contextVerb(),
	}
}

// isOperatorVerb reports whether name belongs to the operator half.
func isOperatorVerb(name string) bool {
	return slices.Contains(operatorVerbs[:], name) || slices.Contains(reservedOperatorNames[:], name)
}

// usageLine is the one-line usage: the operator verbs, then the client verbs.
func usageLine(verbs []verb) string {
	names := []string{"version", "config validate", "auth hash", "serve"}
	for _, v := range verbs {
		names = append(names, v.name)
	}
	return "usage: profgate <" + strings.Join(names, "|") + "> [flags]"
}

// dispatch runs one command line:
// an operator verb through its own function, and a client verb through the grammar of the design.
func dispatch(ctx context.Context, env *cmdEnv, verbs []verb, args []string) int {
	if len(args) > 0 && isOperatorVerb(args[0]) {
		return runOperator(args, env)
	}
	// The global flags may precede the verb; flag stops at the first argument that is not a flag, which is the verb.
	fs := flag.NewFlagSet("profgate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g := globalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return usageError(env, verbs, err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageError(env, verbs, nil)
	}
	if isOperatorVerb(rest[0]) {
		return usageError(env, verbs, fmt.Errorf("%s takes no global flags", rest[0]))
	}
	i := slices.IndexFunc(verbs, func(v verb) bool { return v.name == rest[0] })
	if i < 0 {
		return usageError(env, verbs, fmt.Errorf("unknown verb %q", rest[0]))
	}
	v := verbs[i]
	in, err := v.parse(g, rest[1:])
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "profgate: %v\nusage: profgate %s [flags]\n", err, v.grammar)
		return 2
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return v.run(ctx, env, in)
}

// runOperator is the operator half, which takes no global flags and keeps its own behavior.
func runOperator(args []string, env *cmdEnv) int {
	switch args[0] {
	case "version":
		return runVersion(args[1:], env.stdout, env.stderr)
	case "config":
		return runConfig(args[1:], env.stdout, env.stderr)
	case "auth":
		return runAuth(args[1:], env.stdin, env.stdout, env.stderr)
	case "serve":
		return runServe(args[1:], env.stdout, env.stderr)
	default:
		_, _ = fmt.Fprintln(env.stderr, usageLine(clientVerbs()))
		return 2
	}
}

// usageError prints the cause when there is one, then the usage line, and returns 2.
func usageError(env *cmdEnv, verbs []verb, err error) int {
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "profgate: %v\n", err)
	}
	_, _ = fmt.Fprintln(env.stderr, usageLine(verbs))
	return 2
}

// parse applies the grammar: the subverb, exactly the declared positionals,
// then the flags.
// A flag where a positional belongs, too few positionals, and anything left after the flags are each a usage error.
func (v verb) parse(g *globals, args []string) (*invocation, error) {
	in := &invocation{positionals: []string{}}
	if len(v.subverbs) > 0 {
		words := 0
		for _, sub := range v.subverbs {
			// A subverb may be more than one word: pgo policy get.
			n := len(strings.Fields(sub))
			if len(args) >= n && strings.Join(args[:n], " ") == sub {
				in.subverb, words = sub, n
				break
			}
		}
		if words == 0 {
			return nil, fmt.Errorf("%s takes one of %s", v.name, strings.Join(v.subverbs, ", "))
		}
		args = args[words:]
	}
	for i := range v.positionals {
		if len(args) == 0 {
			if v.optional && i == v.positionals-1 {
				break
			}
			return nil, fmt.Errorf("%s takes %s", v.name, pluralPositionals(v.positionals))
		}
		if strings.HasPrefix(args[0], "-") {
			if v.optional && i == v.positionals-1 {
				break
			}
			return nil, fmt.Errorf("%q where a positional belongs: flags follow the positionals", args[0])
		}
		in.positionals = append(in.positionals, args[0])
		args = args[1:]
	}
	fs, merged := v.flagSet(g)
	in.globals = merged
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%s takes %s; %q is one too many", v.name, pluralPositionals(v.positionals), fs.Arg(0))
	}
	in.fs = fs
	return in, nil
}

// flagSet assembles the verb's flag set: the global flags over what the
// leading occurrence said, then the verb's own.
func (v verb) flagSet(g *globals) (*flag.FlagSet, *globals) {
	fs := flag.NewFlagSet(v.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	merged := *g
	merged.register(fs)
	if v.flags != nil {
		v.flags(fs)
	}
	return fs, &merged
}

func pluralPositionals(n int) string {
	if n == 1 {
		return "one positional"
	}
	return fmt.Sprintf("%d positionals", n)
}

// address parses <namespace>/<service>, or <service> against the context's namespace, and is a usage error otherwise.
func address(arg, contextNamespace string) (namespace, service string, err error) {
	parts := strings.Split(arg, "/")
	switch len(parts) {
	case 1:
		if contextNamespace == "" {
			return "", "", fmt.Errorf("%w: %q names no namespace: pass --namespace, set PROFGATE_NAMESPACE, or write namespace into the context", client.ErrUsage, arg)
		}
		if parts[0] == "" {
			return "", "", fmt.Errorf("%w: a Service is addressed as <namespace>/<service>", client.ErrUsage)
		}
		return contextNamespace, parts[0], nil
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("%w: %q is not <namespace>/<service>", client.ErrUsage, arg)
		}
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("%w: %q is not <namespace>/<service>", client.ErrUsage, arg)
	}
}

// promptPassword reads a password without echo through readPassword,
// which reads without echo when stdin is the process's terminal and one line otherwise.
func promptPassword(env *cmdEnv, user string) (string, error) {
	p, err := readPassword(env.stdin, env.stderr)
	if err != nil {
		return "", fmt.Errorf("password for %s: %w", user, err)
	}
	return string(p), nil
}

// settings resolves the context file and the global flags into one command's settings; a failure here is a usage error.
func (env *cmdEnv) settings(g *globals) (client.Settings, *client.File, error) {
	path, err := client.ConfigPath(env.getenv)
	if err != nil {
		return client.Settings{}, nil, fmt.Errorf("%w: %w", client.ErrUsage, err)
	}
	f, err := client.LoadFile(path)
	if err != nil {
		return client.Settings{}, nil, fmt.Errorf("%w: %w", client.ErrUsage, err)
	}
	s, err := client.Resolve(f, g.overrides(), env.getenv)
	if err != nil {
		return client.Settings{}, nil, fmt.Errorf("%w: %w", client.ErrUsage, err)
	}
	return s, f, nil
}

// store is the token cache under the state directory.
func (env *cmdEnv) store() (*client.Store, error) {
	dir, err := client.StatePath(env.getenv)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", client.ErrUsage, err)
	}
	return client.NewStore(client.StoreOptions{Dir: dir, Now: env.now}), nil
}

// issuer is the issuer client over the resolved issuer CA file.
func (env *cmdEnv) issuer(g *globals, s client.Settings) (*client.Issuer, error) {
	return client.NewIssuer(client.IssuerOptions{
		IssuerCAFile: s.IssuerCAFile,
		Transport:    env.issuerTransport,
		Now:          env.now,
		Verbose:      env.verboseWriter(g),
	})
}

// verboseWriter is stderr under --verbose and nothing otherwise.
func (env *cmdEnv) verboseWriter(g *globals) io.Writer {
	if g.verbose {
		return env.stderr
	}
	return nil
}

// gateway resolves the settings and the credential and builds the gateway
// client speaking as that principal, returning the settings beside it.
// No verb touches a credential.
func (env *cmdEnv) gateway(_ context.Context, g *globals) (*client.Client, client.Settings, error) {
	s, _, err := env.settings(g)
	if err != nil {
		return nil, client.Settings{}, err
	}
	store, err := env.store()
	if err != nil {
		return nil, client.Settings{}, err
	}
	iss, err := env.issuer(g, s)
	if err != nil {
		return nil, client.Settings{}, err
	}
	cred, err := client.ResolveCredential(client.CredentialInput{
		TokenFile:  g.tokenFile,
		TokenStdin: g.tokenStdin,
		Stdin:      env.stdin,
		User:       g.user,
		Getenv:     env.getenv,
		Settings:   s,
		Store:      store,
		Issuer:     iss,
		Now:        env.now,
		Prompt:     env.prompt,
	})
	if err != nil {
		return nil, client.Settings{}, err
	}
	c, err := client.New(client.Options{
		Settings:   s,
		Credential: cred,
		Transport:  env.transport,
		Now:        env.now,
		Sleep:      env.sleep,
		Verbose:    env.verboseWriter(g),
		Warn:       env.stderr,
	})
	if err != nil {
		return nil, client.Settings{}, err
	}
	return c, s, nil
}
