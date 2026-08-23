// Command profgate is the pprof gateway's entry point,
// with subcommands for version reporting, configuration validation, and serving.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/arloliu/profgate/internal/config"
)

// version is set by the linker at build time; "dev" is the fallback for local builds.
var version = "dev"

const usage = "usage: profgate <version|config validate|serve> [flags]"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to the profgate subcommands and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}

	switch args[0] {
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stderr)
	default:
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
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
		_, _ = fmt.Fprintln(stderr, usage)
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
	return 0
}

// runServe loads the configuration and returns 0. It does not start a listener.
func runServe(args []string, stderr io.Writer) int {
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

	if _, err := config.Load(*path); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}

	return 0
}
