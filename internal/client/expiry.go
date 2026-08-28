package client

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxTokenBytes bounds the token whose exp the client reads.
const maxTokenBytes = 16 << 10

// ExpiryOf is expiresAt by token type: obtainedAt plus expires_in under
// access, and the token's own exp under id, where expires_in is the access
// token's lifetime and says nothing about the ID token.
func ExpiryOf(tokenType, token string, expiresIn int, obtainedAt time.Time) (time.Time, error) {
	switch tokenType {
	case "access":
		return obtainedAt.Add(time.Duration(expiresIn) * time.Second), nil
	case "id":
		return tokenExp(token)
	default:
		return time.Time{}, fmt.Errorf("token type %q is not one of id, access", tokenType)
	}
}

// RefreshExpiryOf is obtainedAt plus refresh_expires_in when the issuer sent
// the field, and the zero time when it did not.
func RefreshExpiryOf(refreshExpiresIn int, obtainedAt time.Time) time.Time {
	if refreshExpiresIn <= 0 {
		return time.Time{}
	}
	return obtainedAt.Add(time.Duration(refreshExpiresIn) * time.Second)
}

// tokenExp base64url-decodes the second segment of a token of at most 16 KiB,
// parses it as JSON, and takes exp as a number of seconds.
// It is not a verification and grants nothing:
// the client reads one number to decide when to refresh, and the gateway verifies everything.
func tokenExp(token string) (time.Time, error) {
	if len(token) > maxTokenBytes {
		return time.Time{}, fmt.Errorf("id token exceeds %d bytes", maxTokenBytes)
	}
	segments := strings.Split(token, ".")
	if len(segments) < 3 {
		return time.Time{}, errors.New("id token has fewer than three segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("id token payload is not base64url: %w", err)
	}
	// json.Number would accept a quoted number, so the claim is read untyped
	// and required to be a JSON number.
	var claims map[string]any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return time.Time{}, fmt.Errorf("id token payload is not JSON: %w", err)
	}
	exp, ok := claims["exp"].(json.Number)
	if !ok {
		return time.Time{}, errors.New("id token payload carries no numeric exp")
	}
	seconds, err := strconv.ParseFloat(exp.String(), 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("id token exp: %w", err)
	}
	return time.Unix(int64(seconds), 0).UTC(), nil
}
