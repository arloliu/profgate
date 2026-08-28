package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/arloliu/profgate/internal/client"
	"github.com/arloliu/profgate/internal/config"
)

// profileVerb is GET .../profiles/{profile}:
// every parameter is passed through and judged by the gateway,
// and the client refuses only what it can refuse without guessing the gateway's configuration.
func profileVerb() verb {
	var f profileFlags
	return verb{
		name: "profile", positionals: 2,
		grammar: "profile <ns>/<svc> <profile> [--seconds <n>] [--pod <name>] [--version <v>] [--port <n> | --port-name <name>] [-o <path>] [--open]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&f.seconds, "seconds", "", "seconds, for cpu and trace")
			fs.StringVar(&f.pod, "pod", "", "pin the exact Pod to profile")
			fs.StringVar(&f.version, "version", "", "keep only Pods whose version label equals this value")
			fs.StringVar(&f.port, "port", "", "the pprof port number, in place of the configured default")
			fs.StringVar(&f.portName, "port-name", "", "the pprof container-port name, in place of the configured default")
			fs.StringVar(&f.output, "o", "", "write the profile here; - writes it to stdout")
			fs.BoolVar(&f.open, "open", false, "run go tool pprof -http=:0 on the profile")
		},
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			if err := env.profile(ctx, in, f); err != nil {
				return fail(env, err)
			}
			return exitOK
		},
	}
}

// profileFlags is what the profile verb's own flags said.
type profileFlags struct {
	seconds, pod, version, port, portName, output string
	open                                          bool
}

// query is the flags as the route's query, after the local refusals:
// both port flags, and a --seconds that is not a positive integer.
func (f profileFlags) query() (url.Values, error) {
	if f.port != "" && f.portName != "" {
		return nil, fmt.Errorf("%w: --port and --port-name name two ports; pass one", client.ErrUsage)
	}
	q := url.Values{}
	if f.seconds != "" {
		n, err := strconv.Atoi(f.seconds)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%w: --seconds %q is not a positive integer", client.ErrUsage, f.seconds)
		}
		q.Set("seconds", strconv.Itoa(n))
	}
	for _, p := range []struct{ key, value string }{{"pod", f.pod}, {"version", f.version}, {"port", f.port}, {"portName", f.portName}} {
		if p.value != "" {
			q.Set(p.key, p.value)
		}
	}
	return q, nil
}

// target is the three headers the gateway adds to a forwarded profile,
// saying who was profiled.
type target struct {
	Pod     string `json:"pod"`
	Node    string `json:"node"`
	Version string `json:"version"`
}

// profile runs the verb: the local refusals, the go lookup under --open,
// the destination opened before the request, the fetch, and the viewer.
func (env *cmdEnv) profile(ctx context.Context, in *invocation, f profileFlags) error {
	q, err := f.query()
	if err != nil {
		return err
	}
	name := in.positionals[1]
	if !slices.Contains(config.Profiles(), name) {
		return fmt.Errorf("%w: %q is not a profile; the profiles are %s", client.ErrUsage, name, strings.Join(config.Profiles(), ", "))
	}
	if f.open && f.output == "-" {
		return fmt.Errorf("%w: -o - writes the profile to stdout and --open needs a file; pass one", client.ErrUsage)
	}
	// go is resolved before anything is fetched:
	// refusing here means no profile is collected and thrown away, and no message names a file
	// the cleanup has removed.
	goPath := ""
	if f.open {
		goPath, err = env.lookPath("go")
		if err != nil {
			return fmt.Errorf("%w: --open runs go tool pprof, and go is not on PATH", client.ErrUsage)
		}
	}
	gw, s, err := env.gateway(ctx, in.globals)
	if err != nil {
		return err
	}
	ns, svc, err := address(in.positionals[0], s.Namespace)
	if err != nil {
		return err
	}
	derived := fmt.Sprintf("%s-%s-%s-%s.pprof", ns, svc, name, env.now().UTC().Format("20060102T150405Z"))
	dest, err := env.openDestination(f.output, derived, f.open)
	if err != nil {
		return err
	}
	defer dest.cleanup()
	req := client.Request{Method: http.MethodGet, Path: servicePath(ns, svc) + "/profiles/" + url.PathEscape(name), Query: q}
	resp, err := gw.Do(ctx, req)
	if err != nil {
		dest.discard()
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := dest.write(resp.Body); err != nil {
		dest.discard()
		return fmt.Errorf("%s: %w", s.Origin, err)
	}
	if err := env.printTarget(s, resp.Header); err != nil {
		return err
	}
	if dest.path != "" {
		_, _ = fmt.Fprintf(env.stderr, "wrote %s\n", dest.path)
	}
	if !f.open {
		return nil
	}
	// The viewer is a child process rather than a replacement of this one,
	// so the temporary directory can be removed when it exits; the context
	// is what stops it when the command is cancelled.
	if err := env.run(ctx, goPath, "tool", "pprof", "-http=:0", dest.path); err != nil {
		return fmt.Errorf("go tool pprof: %w", err)
	}
	return nil
}

// printTarget prints the three target headers on stderr: one line each, or one JSON object under --output json.
func (env *cmdEnv) printTarget(s client.Settings, h http.Header) error {
	t := target{Pod: h.Get("X-Pprof-Target-Pod"), Node: h.Get("X-Pprof-Target-Node"), Version: h.Get("X-Pprof-Target-Version")}
	if s.Output == "json" {
		return json.NewEncoder(env.stderr).Encode(t)
	}
	_, err := fmt.Fprintf(env.stderr, "pod: %s\nnode: %s\nversion: %s\n", t.Pod, t.Node, t.Version)
	return err
}

// destination is where the profile bytes go: stdout, a file the user named,
// the derived name in the working directory, or a file in a temporary directory that --open removes on the way out.
type destination struct {
	w       io.Writer
	file    *os.File
	path    string // empty for stdout
	created bool   // the file did not exist before this command
	tempDir string // removed by cleanup when set
}

// openDestination opens the destination before the request is sent, so a
// path that cannot be written is refused before a profile is collected.
// output is the -o flag: - is stdout, empty is derived in the working
// directory, or in a temporary directory when temp is set.
func (env *cmdEnv) openDestination(output, derived string, temp bool) (*destination, error) {
	if output == "-" {
		return &destination{w: env.stdout}, nil
	}
	d := &destination{path: output}
	if d.path == "" {
		if temp {
			dir, err := os.MkdirTemp("", "profgate-")
			if err != nil {
				return nil, fmt.Errorf("%w: %w", client.ErrUsage, err)
			}
			d.tempDir = dir
			derived = filepath.Join(dir, derived)
		}
		d.path = derived
	}
	_, statErr := os.Stat(d.path)
	d.created = errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(d.path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		d.cleanup()
		return nil, fmt.Errorf("%w: %w", client.ErrUsage, err)
	}
	d.file = file
	d.w = file
	return d, nil
}

// write copies the body into the destination; a file is truncated only
// once a body has arrived, so a refusal leaves an existing file as it was.
func (d *destination) write(body io.Reader) error {
	if d.file != nil {
		if err := d.file.Truncate(0); err != nil {
			return err
		}
	}
	if _, err := io.Copy(d.w, body); err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if d.file != nil {
		if err := d.file.Close(); err != nil {
			d.file = nil
			return err
		}
		d.file = nil
	}
	return nil
}

// discard removes a file this command created, so neither a refusal nor a
// cancellation mid-body leaves a partial profile behind.
func (d *destination) discard() {
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
	}
	if d.created && d.path != "" {
		_ = os.Remove(d.path)
	}
}

// cleanup closes what is still open and removes the temporary directory.
func (d *destination) cleanup() {
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
	}
	if d.tempDir != "" {
		_ = os.RemoveAll(d.tempDir)
	}
}
