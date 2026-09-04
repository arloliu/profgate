package main

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestRunAuthHash(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantCode   int
		wantVerify string // the password the printed hash must verify; "" skips
		wantStderr string
	}{
		{
			name:       "hash from pipe",
			args:       []string{"hash"},
			stdin:      "secret\n",
			wantCode:   0,
			wantVerify: "secret",
		},
		{
			name:       "hash strips newline",
			args:       []string{"hash"},
			stdin:      "secret\r\n",
			wantCode:   0,
			wantVerify: "secret",
		},
		{
			name:       "hash without trailing newline",
			args:       []string{"hash"},
			stdin:      "secret",
			wantCode:   0,
			wantVerify: "secret",
		},
		{
			name:       "empty password",
			args:       []string{"hash"},
			stdin:      "",
			wantCode:   2,
			wantStderr: "empty",
		},
		{
			name:       "newline only is empty",
			args:       []string{"hash"},
			stdin:      "\n",
			wantCode:   2,
			wantStderr: "empty",
		},
		{
			name:       "over 72 bytes",
			args:       []string{"hash"},
			stdin:      strings.Repeat("a", 73) + "\n",
			wantCode:   2,
			wantStderr: "72",
		},
		{
			name:       "usage without subcommand",
			args:       nil,
			wantCode:   2,
			wantStderr: "auth hash",
		},
		{
			name:       "usage with unknown subcommand",
			args:       []string{"other"},
			wantCode:   2,
			wantStderr: "auth hash",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runAuth(tc.args, strings.NewReader(tc.stdin), &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("runAuth(%v) code = %d, want %d (stdout=%q stderr=%q)", tc.args, code, tc.wantCode, stdout.String(), stderr.String())
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
			if tc.wantCode != 0 {
				if stdout.Len() != 0 {
					t.Fatalf("stdout = %q, want nothing on a refused password", stdout.String())
				}

				return
			}
			out := stdout.String()
			if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
				t.Fatalf("stdout = %q, want exactly one line", out)
			}
			hash := strings.TrimSuffix(out, "\n")
			cost, err := bcrypt.Cost([]byte(hash))
			if err != nil {
				t.Fatalf("stdout %q is not a bcrypt hash: %v", hash, err)
			}
			if cost != 12 {
				t.Fatalf("cost = %d, want 12", cost)
			}
			if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(tc.wantVerify)); err != nil {
				t.Fatalf("the printed hash does not verify %q: %v", tc.wantVerify, err)
			}
		})
	}

	t.Run("hash answers help", func(t *testing.T) {
		want := "usage: profgate auth hash"
		var stdout, stderr bytes.Buffer
		if code := run([]string{"auth", "hash", "--help"}, &stdout, &stderr); code != 0 {
			t.Fatalf("run(auth hash --help) code = %d, want 0 (stderr=%q)", code, stderr.String())
		}
		if got := firstLine(stdout.String()); got != want {
			t.Fatalf("usage line = %q, want %q", got, want)
		}
		// runAuth answers on its own, against a stdin that fails the test when it is read:
		// the password prompt is inside the command help replaces.
		var direct bytes.Buffer
		if code := runAuth([]string{"hash", "--help"}, unreadableStdin{t}, &direct, &stderr); code != 0 {
			t.Fatalf("runAuth(hash --help) code = %d, want 0 (stderr=%q)", code, stderr.String())
		}
		if got := firstLine(direct.String()); got != want {
			t.Fatalf("usage line = %q, want %q", got, want)
		}
	})
}

// TestRunAuthUsage proves the top-level dispatch reaches auth,
// and that a line naming no subcommand prints the group's own grammar, as every other group does.
func TestRunAuthUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"auth"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run(auth) code = %d, want 2", code)
	}
	if want := "usage: profgate auth hash\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want exactly %q", stderr.String(), want)
	}
	if usage := usageLine(nil); usage != "usage: profgate <version|config validate|auth hash|serve> [flags]" {
		t.Fatalf("usage = %q", usage)
	}
}
