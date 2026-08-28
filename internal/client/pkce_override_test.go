//go:build !e2e

package client

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

// TestDefaultBuildIgnoresTheVerifierOverride proves the binary every release compiles reads no environment variable:
// with the override set to another valid verifier, the poll still sends the verifier whose SHA-256 the device request carried.
func TestDefaultBuildIgnoresTheVerifierOverride(t *testing.T) {
	t.Setenv("PROFGATE_E2E_PKCE_VERIFIER_OVERRIDE", "another-valid-verifier-of-at-least-43-characters-x")

	d := DeviceAuth{verifier: "the-generated-verifier"}
	if got := pollVerifier(d); got != d.verifier {
		t.Fatalf("pollVerifier = %q, want the struct's own verifier %q", got, d.verifier)
	}

	f := deviceIssuer(t, 5, pending(), issued())
	d, _, _, err := f.login(t, true, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(d.verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); f.rt.device[0].Get("code_challenge") != want {
		t.Fatalf("code_challenge = %q, want the SHA-256 of the generated verifier", f.rt.device[0].Get("code_challenge"))
	}
	for n, poll := range f.rt.polls {
		if poll.Get("code_verifier") != d.verifier {
			t.Fatalf("poll %d code_verifier = %q, want the generated verifier", n+1, poll.Get("code_verifier"))
		}
	}
}
