package client

import (
	"errors"
	"net/url"
	"testing"
)

func TestErrorMessages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"api", &APIError{Status: 403, Code: "realm_denied", Message: "namespace payments is not admitted"}, "realm_denied: namespace payments is not admitted"},
		{"transport", &TransportError{Origin: "https://gateway.example:443", Err: errors.New("connection refused")}, "https://gateway.example:443: connection refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTransportErrorUnwrapsThroughURLError(t *testing.T) {
	inner := errors.New("connection refused")
	te := newTransportError("https://gateway.example:443", &url.Error{Op: "Get", URL: "https://gateway.example/v1/secret", Err: inner})
	if !errors.Is(te, inner) {
		t.Fatal("the cause is lost")
	}
	if got := te.Error(); got != "https://gateway.example:443: connection refused" {
		t.Fatalf("message %q carries more than the origin and the cause", got)
	}
}

// TestStatusErrorLine pins the line a response this client could not read as
// the gateway's envelope prints: the status, its standard reason where the
// status has one, and a clause of this client's own.
func TestStatusErrorLine(t *testing.T) {
	cases := []struct {
		name string
		err  *StatusError
		want string
	}{
		{"a status with a reason", &StatusError{Status: 502}, "HTTP 502 Bad Gateway: body is not a profgate JSON document"},
		{"a status with no reason", &StatusError{Status: 599}, "HTTP 599: body is not a profgate JSON document"},
		{
			"a body that filled the bound",
			&StatusError{Status: 200, Detail: "body exceeds the 1048576-byte bound this client reads"},
			"HTTP 200 OK: body exceeds the 1048576-byte bound this client reads",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}
