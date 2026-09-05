package httpapi

import (
	"net/http"
	"time"
)

// setBeforeAllowlist installs fn on a handler built by New, so a test can act
// between the realm check and the allowlist check of one request.
func setBeforeAllowlist(h http.Handler, fn func()) {
	h.(*server).beforeAllowlist = fn
}

// setBudgetGrace shortens the overall budget's grace on a handler built by New,
// so a test on a real socket sees the budget end in milliseconds rather than thirty seconds.
func setBudgetGrace(h http.Handler, d time.Duration) {
	h.(*server).budgetGrace = d
}

// setBodyReadTimeout shortens the body read deadline on a handler built by New,
// so a test on a real socket sees a body that never arrives refused in milliseconds rather than ten seconds.
func setBodyReadTimeout(h http.Handler, d time.Duration) {
	h.(*server).bodyReadTimeout = d
}
