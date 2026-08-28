package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/arloliu/profgate/internal/client"
	"gopkg.in/yaml.v3"
)

// The context verb acts on the contexts file and the token cache and sends
// nothing: list prints the contexts and marks the current one, show prints
// one with no token material in it, use sets currentContext, and delete
// removes an entry and its cache file under the entry's lock.
func contextVerb() verb {
	return verb{
		name:        "context",
		subverbs:    []string{"list", "show", "use", "delete"},
		positionals: 1,
		optional:    true, // show takes the current context when the name is absent; list takes none
		grammar:     "context list|show [<name>]|use <name>|delete <name>",
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			if err := env.runContext(ctx, in); err != nil {
				return fail(env, err)
			}
			return exitOK
		},
	}
}

// runContext loads the file and runs the subverb over it.
func (env *cmdEnv) runContext(ctx context.Context, in *invocation) error {
	path, err := client.ConfigPath(env.getenv)
	if err != nil {
		return fmt.Errorf("%w: %w", client.ErrUsage, err)
	}
	f, err := client.LoadFile(path)
	if err != nil {
		return fmt.Errorf("%w: %w", client.ErrUsage, err)
	}
	name := ""
	if len(in.positionals) > 0 {
		name = in.positionals[0]
	}
	switch in.subverb {
	case "list":
		if name != "" {
			return fmt.Errorf("%w: context list takes no positional; %q is one too many", client.ErrUsage, name)
		}
		return env.listContexts(f)
	case "show":
		return env.showContext(f, in.globals, name)
	case "use":
		if name == "" {
			return fmt.Errorf("%w: context use takes one positional", client.ErrUsage)
		}
		if err := client.UseContext(f, name); err != nil {
			return err
		}
		return client.SaveFile(path, f)
	default:
		if name == "" {
			return fmt.Errorf("%w: context delete takes one positional", client.ErrUsage)
		}
		store, err := env.store()
		if err != nil {
			return err
		}
		return client.DeleteContext(ctx, f, name, store, func(f *client.File) error { return client.SaveFile(path, f) })
	}
}

// listContexts prints one row per context in name order, the current one
// marked in the first column; a missing file prints the header alone.
func (env *cmdEnv) listContexts(f *client.File) error {
	var rows [][]string
	if f != nil {
		names := slices.Sorted(func(yield func(string) bool) {
			for name := range f.Contexts {
				if !yield(name) {
					return
				}
			}
		})
		for _, name := range names {
			c := f.Contexts[name]
			current := ""
			if name == f.CurrentContext {
				current = "*"
			}
			rows = append(rows, []string{current, name, c.Server, c.Namespace})
		}
	}
	return writeTable(env.stdout, env.terminal, []string{"CURRENT", "NAME", "SERVER", "NAMESPACE"}, rows)
}

// showContext prints one context, the selected one when name is absent, as
// YAML or under --output json as JSON.
// It reads the file alone: the token cache is never opened, so no token,
// refresh token, or expiry can reach the output.
func (env *cmdEnv) showContext(f *client.File, g *globals, name string) error {
	output, err := env.outputFormat(g)
	if err != nil {
		return err
	}
	if name == "" {
		name = g.context
	}
	if name == "" {
		name, _ = env.getenv("PROFGATE_CONTEXT")
	}
	if name == "" && f != nil {
		name = f.CurrentContext
	}
	if name == "" {
		return fmt.Errorf("%w: no context selected: pass a name, or select one with profgate context use", client.ErrUsage)
	}
	var c client.Context
	ok := false
	if f != nil {
		c, ok = f.Contexts[name]
	}
	if !ok {
		return fmt.Errorf("%w: context %q is not in the contexts file", client.ErrUsage, name)
	}
	if output == "json" {
		enc := json.NewEncoder(env.stdout)
		return enc.Encode(c)
	}
	enc := yaml.NewEncoder(env.stdout)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return err
	}
	return enc.Close()
}

// outputFormat is --output, PROFGATE_OUTPUT when the flag is absent, and
// table otherwise; the context verb resolves no gateway, so this is the one
// value of Resolution it needs.
func (env *cmdEnv) outputFormat(g *globals) (string, error) {
	output := g.output
	if output == "" {
		output, _ = env.getenv("PROFGATE_OUTPUT")
	}
	switch output {
	case "", "table":
		return "table", nil
	case "json":
		return "json", nil
	default:
		return "", fmt.Errorf("%w: output %q is not one of table, json", client.ErrUsage, output)
	}
}
