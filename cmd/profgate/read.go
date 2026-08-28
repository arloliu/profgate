package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arloliu/profgate/internal/client"
)

// A read verb issues exactly one GET and prints the result: the body byte
// for byte under --output json, and a table otherwise.

// reading is what one read verb declares: how to build its request from the
// resolved settings, and how to render the body as a table.
type reading struct {
	build  func(s client.Settings, in *invocation) (client.Request, error)
	render func(env *cmdEnv, body []byte) error
}

// read runs one reading: the settings and credential, the request, then
// the body verbatim or the table.
func (env *cmdEnv) read(ctx context.Context, in *invocation, r reading) int {
	gw, s, err := env.gateway(ctx, in.globals)
	if err != nil {
		return fail(env, err)
	}
	req, err := r.build(s, in)
	if err != nil {
		return fail(env, err)
	}
	body, _, err := gw.JSON(ctx, req)
	if err != nil {
		return fail(env, err)
	}
	if s.Output == "json" {
		if _, err := env.stdout.Write(body); err != nil {
			return fail(env, err)
		}
		return exitOK
	}
	if err := r.render(env, body); err != nil {
		return fail(env, err)
	}
	return exitOK
}

// getPath is a request for one path with no query.
func getPath(path string) func(client.Settings, *invocation) (client.Request, error) {
	return func(client.Settings, *invocation) (client.Request, error) {
		return client.Request{Method: http.MethodGet, Path: path}, nil
	}
}

func whoamiVerb() verb {
	return verb{
		name: "whoami", grammar: "whoami",
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			return env.read(ctx, in, reading{build: getPath("/v1/whoami"), render: renderWhoami})
		},
	}
}

// renderWhoami prints the six rows: principal, realm, namespaces, services,
// profiles, and the pgo flags that are set.
func renderWhoami(env *cmdEnv, body []byte) error {
	w, err := client.Decode[client.WhoamiResponse](body)
	if err != nil {
		return err
	}
	var pgo []string
	for _, f := range []struct {
		name string
		set  bool
	}{{"read", w.Realm.PGO.Read}, {"collect", w.Realm.PGO.Collect}, {"configure", w.Realm.PGO.Configure}} {
		if f.set {
			pgo = append(pgo, f.name)
		}
	}
	return writeTable(env.stdout, env.terminal, nil, [][]string{
		{"principal", w.Principal},
		{"realm", w.Realm.Name},
		{"namespaces", strings.Join(w.Realm.Namespaces, ", ")},
		{"services", strings.Join(w.Realm.Services, ", ")},
		{"profiles", strings.Join(w.Realm.Profiles, ", ")},
		{"pgo", strings.Join(pgo, ", ")},
	})
}

func limitsVerb() verb {
	return verb{
		name: "limits", grammar: "limits",
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			return env.read(ctx, in, reading{build: getPath("/v1/limits"), render: renderLimits})
		},
	}
}

// renderLimits prints one row per limit, the port selections listed.
func renderLimits(env *cmdEnv, body []byte) error {
	l, err := client.Decode[client.LimitsResponse](body)
	if err != nil {
		return err
	}
	selections := make([]string, 0, len(l.Pprof.AllowedSelections))
	for _, sel := range l.Pprof.AllowedSelections {
		selections = append(selections, sel.String())
	}
	pgo := "disabled"
	if l.PGO.Enabled {
		pgo = "enabled"
	}
	return writeTable(env.stdout, env.terminal, nil, [][]string{
		{"cpuSeconds", fmt.Sprint(l.CPUSeconds)},
		{"traceSeconds", fmt.Sprint(l.TraceSeconds)},
		{"profiles", strings.Join(l.Profiles, ", ")},
		{"default", l.Pprof.Default.String()},
		{"allowedSelections", strings.Join(selections, ", ")},
		{"pgo", pgo},
	})
}

func namespacesVerb() verb {
	return verb{
		name: "namespaces", grammar: "namespaces",
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			return env.read(ctx, in, reading{build: getPath("/v1/namespaces"), render: func(env *cmdEnv, body []byte) error {
				n, err := client.Decode[client.NamespacesResponse](body)
				if err != nil {
					return err
				}
				return writeTable(env.stdout, env.terminal, []string{"NAMESPACE"}, oneColumn(n.Namespaces))
			}})
		},
	}
}

func servicesVerb() verb {
	return verb{
		name: "services", positionals: 1, grammar: "services <namespace>",
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			return env.read(ctx, in, reading{
				build: func(_ client.Settings, in *invocation) (client.Request, error) {
					ns := in.positionals[0]
					if ns == "" || strings.Contains(ns, "/") {
						return client.Request{}, fmt.Errorf("%w: %q is not a namespace", client.ErrUsage, ns)
					}
					return client.Request{Method: http.MethodGet, Path: "/v1/namespaces/" + url.PathEscape(ns) + "/services"}, nil
				},
				render: func(env *cmdEnv, body []byte) error {
					s, err := client.Decode[client.ServicesResponse](body)
					if err != nil {
						return err
					}
					return writeTable(env.stdout, env.terminal, []string{"SERVICE"}, oneColumn(s.Services))
				},
			})
		},
	}
}

// oneColumn is a list as rows of one cell.
func oneColumn(values []string) [][]string {
	rows := make([][]string, 0, len(values))
	for _, v := range values {
		rows = append(rows, []string{v})
	}
	return rows
}

// targetsVerb is GET .../targets with --port or --port-name, which the
// gateway needs in order to decide eligibility; both together is a usage
// error before any request.
func targetsVerb() verb {
	var port, portName string
	return verb{
		name: "targets", positionals: 1, grammar: "targets <ns>/<svc> [--port <n> | --port-name <name>]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&port, "port", "", "the pprof port number, in place of the configured default")
			fs.StringVar(&portName, "port-name", "", "the pprof container-port name, in place of the configured default")
		},
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			return env.read(ctx, in, reading{
				build: func(s client.Settings, in *invocation) (client.Request, error) {
					if port != "" && portName != "" {
						return client.Request{}, fmt.Errorf("%w: --port and --port-name name two ports; pass one", client.ErrUsage)
					}
					ns, svc, err := address(in.positionals[0], s.Namespace)
					if err != nil {
						return client.Request{}, err
					}
					q := url.Values{}
					if port != "" {
						q.Set("port", port)
					}
					if portName != "" {
						q.Set("portName", portName)
					}
					return client.Request{Method: http.MethodGet, Path: servicePath(ns, svc) + "/targets", Query: q}, nil
				},
				render: func(env *cmdEnv, body []byte) error {
					r, err := client.Decode[client.TargetsResponse](body)
					if err != nil {
						return err
					}
					rows := make([][]string, 0, len(r.Targets))
					for _, t := range r.Targets {
						rows = append(rows, []string{t.Pod, t.Node, t.Version})
					}
					return writeTable(env.stdout, env.terminal, []string{"POD", "NODE", "VERSION"}, rows)
				},
			})
		},
	}
}

// servicePath is /v1/namespaces/{ns}/services/{svc}.
func servicePath(namespace, service string) string {
	return "/v1/namespaces/" + url.PathEscape(namespace) + "/services/" + url.PathEscape(service)
}

// scopeList is the repeatable --scope flag.
type scopeList []string

func (s *scopeList) String() string { return strings.Join(*s, ",") }

func (s *scopeList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// loginVerb runs the login for the gateway's mode; each flag overrides what
// /v1/auth reported, and --issuer-ca-file is the global flag of that name.
func loginVerb() verb {
	var issuer, clientID, tokenType string
	var scopes scopeList
	var pkce, noPKCE bool
	var timeout time.Duration
	return verb{
		name: "login", grammar: "login [--issuer <url>] [--client-id <id>] [--token-type id|access] [--scope <scope>]... [--pkce|--no-pkce] [--login-timeout <duration>]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&issuer, "issuer", "", "the issuer, in place of what the gateway reports")
			fs.StringVar(&clientID, "client-id", "", "the client identifier, in place of what the gateway reports")
			fs.StringVar(&tokenType, "token-type", "", "id or access, in place of what the gateway reports")
			fs.Var(&scopes, "scope", "a scope to ask for, repeatable, in place of what the gateway reports")
			fs.BoolVar(&pkce, "pkce", false, "send a PKCE challenge with the device request")
			fs.BoolVar(&noPKCE, "no-pkce", false, "send no PKCE challenge")
			fs.DurationVar(&timeout, "login-timeout", 0, "how long to wait for the code to be entered, 1m to 30m (default 10m)")
		},
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			pkceFlag, err := client.PKCEFlag(pkce, noPKCE)
			if err != nil {
				return fail(env, err)
			}
			flags := client.LoginFlags{
				Issuer: issuer, ClientID: clientID, TokenType: tokenType, IssuerCAFile: in.globals.issuerCAFile,
				Scopes: scopes, PKCE: pkceFlag, LoginTimeout: timeout,
			}
			return env.login(ctx, in.globals, flags)
		},
	}
}

// login assembles LoginInput: the gateway client with no credential, the
// issuer, the store, the basic prompt, and the contexts file with its writer.
func (env *cmdEnv) login(ctx context.Context, g *globals, flags client.LoginFlags) int {
	s, f, err := env.settings(g)
	if err != nil {
		return fail(env, err)
	}
	path, err := client.ConfigPath(env.getenv)
	if err != nil {
		return fail(env, fmt.Errorf("%w: %w", client.ErrUsage, err))
	}
	store, err := env.store()
	if err != nil {
		return fail(env, err)
	}
	iss, err := env.issuer(g, s)
	if err != nil {
		return fail(env, err)
	}
	gw, err := client.New(client.Options{Settings: s, Transport: env.transport, Now: env.now, Verbose: env.verboseWriter(g), Warn: env.stderr})
	if err != nil {
		return fail(env, err)
	}
	_, err = client.Login(ctx, client.LoginInput{
		Settings: s,
		Gateway:  gw,
		Issuer:   iss,
		Store:    store,
		Flags:    flags,
		Now:      env.now,
		Stdout:   env.stdout,
		Stderr:   env.stderr,
		Basic:    func() (string, string, error) { return env.basicPair(g) },
		SaveFile: func(f *client.File) error { return client.SaveFile(path, f) },
		File:     f,
	})
	if err != nil {
		return fail(env, err)
	}
	return exitOK
}

// basicPair is the user name from -u or PROFGATE_USER and the password from
// PROFGATE_PASSWORD or the prompt, which login under basic verifies and
// never stores.
func (env *cmdEnv) basicPair(g *globals) (user, password string, err error) {
	user = g.user
	if user == "" {
		user, _ = env.getenv("PROFGATE_USER")
	}
	if user == "" {
		return "", "", fmt.Errorf("the gateway authenticates with a user name and password; pass -u <name> or set PROFGATE_USER")
	}
	if p, ok := env.getenv("PROFGATE_PASSWORD"); ok {
		return user, p, nil
	}
	password, err = env.prompt(user)
	if err != nil {
		return "", "", err
	}
	return user, password, nil
}

func logoutVerb() verb {
	return verb{
		name: "logout", grammar: "logout",
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			s, _, err := env.settings(in.globals)
			if err != nil {
				return fail(env, err)
			}
			store, err := env.store()
			if err != nil {
				return fail(env, err)
			}
			iss, err := env.issuer(in.globals, s)
			if err != nil {
				return fail(env, err)
			}
			if err := client.Logout(ctx, client.LogoutInput{Settings: s, Issuer: iss, Store: store, Stdout: env.stdout, Stderr: env.stderr}); err != nil {
				return fail(env, err)
			}
			return exitOK
		},
	}
}
