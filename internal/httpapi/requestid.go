package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

const (
	// requestIDHeader carries the identifier on every response of both listeners.
	requestIDHeader = "X-Request-Id"
	// requestIDMaxBytes is the longest client value the gateway echoes;
	// it caps what one request can reflect back to itself.
	requestIDMaxBytes = 128
	// requestIDBytes is how much randomness a generated value carries.
	requestIDBytes = 16
)

// requestIDKey is the context key the identifier is stored under.
type requestIDKey struct{}

// RequestID returns the identifier for one request:
// the client's when the request carries exactly one X-Request-Id of 1 to 128 bytes drawn from [A-Za-z0-9._-],
// and 16 bytes from crypto/rand as 32 lowercase hexadecimal characters otherwise.
// A value the gateway will not take is replaced, never refused: the identifier decides nothing.
func RequestID(r *http.Request) string {
	if values := r.Header.Values(requestIDHeader); len(values) == 1 && echoable(values[0]) {
		return values[0]
	}

	return generateRequestID()
}

// WithRequestID sets X-Request-Id on every response next writes,
// from the client's value or a generated one,
// and puts the value on the request context so a handler names the same one in its audit record.
// internal/ops wraps its mux with it; the API listener sets it in ServeHTTP.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestID(r)
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// requestIDFrom returns the identifier WithRequestID put on ctx,
// and the empty string when nothing put one there.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)

	return id
}

// echoable reports whether a client value is one the gateway reflects:
// 1 to 128 bytes drawn from [A-Za-z0-9._-].
// The set is what makes echoing client text safe.
// It excludes CR, LF, and every character that could split or forge a header.
func echoable(value string) bool {
	if value == "" || len(value) > requestIDMaxBytes {
		return false
	}
	for i := range len(value) {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}

	return true
}

// generateRequestID is 16 bytes from crypto/rand as 32 lowercase hexadecimal characters.
func generateRequestID() string {
	var b [requestIDBytes]byte
	// crypto/rand.Read fills b or crashes the process; it has no failure a caller can see.
	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}
