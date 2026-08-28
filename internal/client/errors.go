package client

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// ErrUsage marks a failure decided locally, before any request: a bad flag
// value, a credential the rules refuse, a certificate file with nothing in it.
// The command line maps it to exit 2; every other error from this package is
// the gateway's refusal or a transport failure, exit 1.
var ErrUsage = errors.New("usage")

// APIError is the gateway's own envelope: its code and message, verbatim.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return e.Code + ": " + e.Message
}

// StatusError is a response that is not the envelope: HTML from an Ingress,
// an empty body, truncated JSON. It carries the status and nothing from the body.
type StatusError struct {
	Status int
}

func (e *StatusError) Error() string {
	if reason := http.StatusText(e.Status); reason != "" {
		return "HTTP " + strconv.Itoa(e.Status) + " " + reason
	}
	return "HTTP " + strconv.Itoa(e.Status)
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
