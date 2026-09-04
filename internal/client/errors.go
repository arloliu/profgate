package client

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// ErrUsage marks a failure decided locally, before any request:
// a bad flag value, a credential the rules refuse, a certificate file with nothing in it.
// The command line maps it to exit 2; every other error from this package is
// the gateway's refusal or a transport failure, exit 1.
var ErrUsage = errors.New("usage")

// APIError is the gateway's own envelope: its code and message, verbatim.
type APIError struct {
	Status  int
	Code    string
	Message string
	Body    []byte // the envelope exactly as it arrived, for the --output json copy
}

func (e *APIError) Error() string {
	return e.Code + ": " + e.Message
}

// StatusError is a response this client could not read as the gateway's envelope:
// HTML from an Ingress, an empty body, truncated JSON, or a body that filled the response bound.
// It carries the status and a clause of this client's own, and never a byte of the body.
type StatusError struct {
	Status int
	Detail string // empty means "body is not a profgate JSON document"
}

func (e *StatusError) Error() string {
	detail := e.Detail
	if detail == "" {
		detail = "body is not a profgate JSON document"
	}
	return statusLine(e.Status) + ": " + detail
}

// statusLine is the status and its standard reason, "HTTP 502 Bad Gateway",
// and the status alone for one http.StatusText has no reason for.
func statusLine(status int) string {
	if reason := http.StatusText(status); reason != "" {
		return "HTTP " + strconv.Itoa(status) + " " + reason
	}
	return "HTTP " + strconv.Itoa(status)
}

// TransportError is a request that never got an answer.
// Its message names the URL's scheme, host, and port and no path.
type TransportError struct {
	Origin string
	Err    error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("%s: %v", e.Origin, e.Err)
}

func (e *TransportError) Unwrap() error {
	return e.Err
}

// newTransportError strips the *url.Error the HTTP client wraps a failure
// in, because that wrapper prints the full URL, path included.
func newTransportError(origin string, err error) *TransportError {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &TransportError{Origin: origin, Err: err}
}
