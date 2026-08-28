//go:build e2e

package client

import "os"

// pollVerifier is the code_verifier a poll sends for d: the value of
// PROFGATE_E2E_PKCE_VERIFIER_OVERRIDE when that variable is set and
// non-empty, and the verifier Authorize generated otherwise.
// The end-to-end lanes set the variable to a verifier that does not match
// the challenge the device request carried, so an issuer that enforces PKCE
// refuses the login.
// The override touches the poll only, never the challenge, the cache, or the
// refresh, so a set variable can only make an issuer refuse a login.
func pollVerifier(d DeviceAuth) string {
	if v := os.Getenv("PROFGATE_E2E_PKCE_VERIFIER_OVERRIDE"); v != "" {
		return v
	}
	return d.verifier
}
