// Command profgate is the pprof gateway's entry point,
// with subcommands for version reporting, configuration validation,
// password hashing for basic authentication, and serving.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/proxy"
)

// serviceAccountNamespaceFile is where the projected ServiceAccount token mounts the namespace.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// secondSignalExit is what a process that gave up on its own drain returns:
// requests were cut and Collections were left running, which is not a clean exit.
const secondSignalExit = 1

// version is set by the linker at build time; "dev" is the fallback for local builds.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to the profgate subcommands and returns the process exit code:
// the operator verbs through their own functions, the client verbs through
// the grammar of docs/specs/cli.md.
func run(args []string, stdout, stderr io.Writer) int {
	return dispatch(context.Background(), newEnv(os.Stdin, stdout, stderr), clientVerbs(), args)
}

// runVersion prints the binary's version.
func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, _ = fmt.Fprintf(stdout, "profgate %s\n", version)
	return 0
}

// runConfig dispatches the "config" subcommands.
func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "validate" {
		_, _ = fmt.Fprintln(stderr, usageLine(clientVerbs()))
		return 2
	}

	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path to the configuration file")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *path == "" {
		_, _ = fmt.Fprintln(stderr, "usage: profgate config validate --config <path>")
		return 2
	}

	cfg, err := config.Load(*path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}

	_, _ = fmt.Fprintf(stdout, "required terminationGracePeriodSeconds: %d\n", int(cfg.RequiredGracePeriod().Seconds()))
	if cfg.PGO.Enabled {
		_, _ = fmt.Fprintf(stdout, "pgo working set bytes: %d\n", cfg.PGOMemoryBytes())
	} else {
		_, _ = fmt.Fprintln(stdout, "pgo collection: disabled")
	}
	_, _ = fmt.Fprintf(stdout, "container memory bytes: %d\n", cfg.GatewayMemoryBytes())
	return 0
}

// runServe runs the gateway with its production dependencies until SIGINT or SIGTERM.
func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" {
		_, _ = fmt.Fprintln(stderr, "usage: profgate serve --config <path>")
		return 2
	}

	// The signals are only the stop request; the gateway's own work runs under
	// Background so a signal drains the listeners rather than cancelling them.
	// The buffer holds both signals the handling below reads,
	// so neither is dropped while the drain runs.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	stop := make(chan struct{})
	// The gateway's logger is built inside serve, behind the configuration
	// load, and this record has to escape from outside it.
	escalation := slog.New(slog.NewJSONHandler(stdout, nil))
	go watchSignals(sigCh, stop, func() {
		escalation.Warn("second signal; exiting without finishing the drain")
		os.Exit(secondSignalExit)
	})

	registry := prometheus.NewRegistry()
	// One proxy serves both callers:
	// an interactive request and a Collection sample are the same fetch from the same pprof port.
	upstream := proxy.New(proxy.Options{})
	deps := serveDeps{
		namespaceFile: serviceAccountNamespaceFile,
		runtime:       k8s.NewRuntime,
		upstream:      upstream,
		sampler:       upstream,
		registry:      registry,
		recorder:      metrics.NewPrometheus(registry),
		stop:          stop,
		natsPreflight: natskv.Preflight,
	}

	return serve(context.Background(), *path, deps, stdout, stderr)
}

// watchSignals closes stop on the first signal and calls escalate on the second.
// An operator who signals twice is saying the drain is taking longer than they
// will wait for; the drain has no way to know that on its own,
// because the wait it is in is the one the work legitimately needs.
func watchSignals(sigCh <-chan os.Signal, stop chan<- struct{}, escalate func()) {
	<-sigCh
	close(stop)
	<-sigCh
	escalate()
}
