package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantCode        int
		wantStdout      string
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
			name:       "validate good",
			args:       []string{"config", "validate", "--config", "testdata/good.yaml"},
			wantCode:   0,
			wantStdout: "required terminationGracePeriodSeconds: 120",
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
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Fatalf("run(%v) stdout = %q, want it to contain %q", tc.args, stdout.String(), tc.wantStdout)
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
