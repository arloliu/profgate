// Command lanes prints the end-to-end lane matrix as a JSON array of lane
// names, for the "e2e" workflow's lane job to fan out into a matrix.
// Run from the module root: "go run ./test/e2e/cmd/lanes [-unfrozen]".
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/arloliu/profgate/test/e2e"
)

// lanesFile is the lane matrix path, relative to the module root the caller runs from.
const lanesFile = "test/e2e/versions.yaml"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run loads the lane matrix and writes the requested lane names to stdout as a JSON array.
func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("lanes", flag.ContinueOnError)
	unfrozen := fs.Bool("unfrozen", false, "print only lanes with frozen: false")
	if err := fs.Parse(args); err != nil {
		return err
	}

	lanes, err := e2e.LoadLanes(lanesFile)
	if err != nil {
		return fmt.Errorf("load lanes: %w", err)
	}

	names := e2e.LaneNames(lanes, *unfrozen)
	if err := json.NewEncoder(stdout).Encode(names); err != nil {
		return fmt.Errorf("encode lane names: %w", err)
	}
	return nil
}
