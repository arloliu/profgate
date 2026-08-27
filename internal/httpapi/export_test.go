package httpapi

import "net/http"

// setBeforeAllowlist installs fn on a handler built by New, so a test can act
// between the realm check and the allowlist check of one request.
func setBeforeAllowlist(h http.Handler, fn func()) {
	h.(*server).beforeAllowlist = fn
}
