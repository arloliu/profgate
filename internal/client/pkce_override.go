//go:build !e2e

package client

// pollVerifier is the code_verifier a poll sends for d: the one Authorize generated.
// This is the file every release build compiles, and it reads no environment variable;
// the end-to-end build substitutes a mismatched verifier to prove the issuer enforces PKCE.
func pollVerifier(d DeviceAuth) string {
	return d.verifier
}
