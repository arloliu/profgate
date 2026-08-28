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
		{"status", &StatusError{Status: 502}, "HTTP 502 Bad Gateway"},
		{"status unknown reason", &StatusError{Status: 599}, "HTTP 599"},
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
