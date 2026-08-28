package main

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/arloliu/profgate/internal/client"
)

// The process exit codes of the client verbs.
const (
	exitOK      = 0 // the command did what it said
	exitRefused = 1 // the gateway refused, the transport failed, or a waited-for Collection did not complete
	exitUsage   = 2 // an unknown verb or flag, a bad positional, a local validation failure, a missing configuration
	exitLogin   = 3 // authentication is needed: no usable token, a refresh the issuer refused, or 401
)

// exitCode maps one error to the process's exit code:
// 1 for a refusal or a transport failure, 2 for usage, 3 for authentication.
// A 403 realm_denied is 1: a new login changes nothing when the realm is the
// thing refusing.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, client.ErrUsage):
		return exitUsage
	case errors.Is(err, client.ErrLoginNeeded), unauthorized(err):
		return exitLogin
	default:
		return exitRefused
	}
}

// unauthorized reports a 401, whether or not the body was the envelope.
func unauthorized(err error) bool {
	var ae *client.APIError
	var se *client.StatusError
	return (errors.As(err, &ae) && ae.Status == http.StatusUnauthorized) ||
		(errors.As(err, &se) && se.Status == http.StatusUnauthorized)
}

// fail prints the error as one stderr line and returns its exit code.
// The envelope's code and message, the status line of a response that is
// not the envelope, and a transport error's origin each arrive verbatim in
// the error itself.
func fail(env *cmdEnv, err error) int {
	_, _ = fmt.Fprintf(env.stderr, "profgate: %v\n", err)
	return exitCode(err)
}
