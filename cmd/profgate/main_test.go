package main

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantCode        int
		wantStdout      []string
		wantStdoutExact string
		wantStderr      string
	}{
		{
			name:            "version",
			args:            []string{"version"},
			wantCode:        0,
			wantStdoutExact: "profgate dev\n",
		},
		{
			name:     "validate good",
			args:     []string{"config", "validate", "--config", "testdata/good.yaml"},
			wantCode: 0,
			wantStdout: []string{
				"required terminationGracePeriodSeconds: 125",
				"required terminationGracePeriodSeconds for pgo: 122465",
				"another replica reclaims it",
				"pgo memory bytes: 4294967296",
			},
		},
		{
			name:       "validate bad",
			args:       []string{"config", "validate", "--config", "testdata/bad.yaml"},
			wantCode:   2,
			wantStderr: "realms",
		},
		{
			name:       "unknown",
			args:       []string{"bogus"},
			wantCode:   2,
			wantStderr: "usage",
		},
		{
			name:     "no subcommand",
			args:     nil,
			wantCode: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("run(%v) code = %d, want %d (stdout=%q stderr=%q)", tc.args, code, tc.wantCode, stdout.String(), stderr.String())
			}
			for _, want := range tc.wantStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("run(%v) stdout = %q, want it to contain %q", tc.args, stdout.String(), want)
				}
			}
			if tc.wantStdoutExact != "" && stdout.String() != tc.wantStdoutExact {
				t.Fatalf("run(%v) stdout = %q, want exactly %q", tc.args, stdout.String(), tc.wantStdoutExact)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Fatalf("run(%v) stderr = %q, want it to contain %q", tc.args, stderr.String(), tc.wantStderr)
			}
		})
	}
}

// TestWatchSignals proves the escalation an operator gets by signalling twice:
// the first signal asks for the drain, and the second gives up on it rather
// than leaving the operator with a process that ignores them.
func TestWatchSignals(t *testing.T) {
	sigCh := make(chan os.Signal, 2)
	stop := make(chan struct{})
	escalated := make(chan struct{})
	go watchSignals(sigCh, stop, func() { close(escalated) })

	sigCh <- syscall.SIGTERM
	select {
	case <-stop:
	case <-time.After(time.Second):
		t.Fatal("the first signal did not request the drain")
	}
	select {
	case <-escalated:
		t.Fatal("the first signal ended the process instead of draining")
	case <-time.After(50 * time.Millisecond):
	}

	sigCh <- syscall.SIGTERM
	select {
	case <-escalated:
	case <-time.After(time.Second):
		t.Fatal("the second signal did not end the drain")
	}
}
