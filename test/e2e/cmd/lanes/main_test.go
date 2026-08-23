package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// chdirToModuleRoot points the working directory at the module root for the duration of the test,
// matching how the CI workflow and the usage instructions invoke this command:
// "go run ./test/e2e/cmd/lanes" from the repository root, so run's hard-coded lanesFile path resolves.
func chdirToModuleRoot(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(wd, "..", "..", "..", "..")
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRun(t *testing.T) {
	chdirToModuleRoot(t)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "all lanes", args: nil, want: "[\"1.23\",\"1.24\",\"current\"]\n"},
		{name: "unfrozen only", args: []string{"-unfrozen"}, want: "[\"current\"]\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := run(tc.args, &stdout); err != nil {
				t.Fatalf("run(%v) error = %v", tc.args, err)
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	chdirToModuleRoot(t)

	var stdout bytes.Buffer
	if err := run([]string{"-bogus"}, &stdout); err == nil {
		t.Fatal("run with an unknown flag returned nil error")
	}
}
