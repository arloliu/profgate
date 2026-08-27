package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	// maxTokenBytes bounds a bearer token before it is parsed.
	maxTokenBytes = 16 << 10
	// maxIdentityBytes bounds sub and the username claim.
	maxIdentityBytes = 256
	// accessTokenType is the JOSE typ an access token must carry (RFC 9068),
	// compared case-insensitively.
	accessTokenType = "at+jwt"
	// tokenTypeAccess is the tokenType value that selects the RFC 9068 profile;
	// every other value is the ID token profile.
	tokenTypeAccess = "access"
)

// claims is what verification keeps: everything else in the token is
// discarded.
type claims struct {
	Subject  string
	Username string
	Groups   []string
	Nonce    string // browser flow only
}

// verifier runs the ordered checks of a bearer token against the issuer
// state OIDC publishes.
type verifier struct {
	issuer, audience, tokenType, usernameClaim, groupsClaim string
	skew                                                    time.Duration
	// state is OIDC's pointer; nil until Discover succeeds.
	state *atomic.Pointer[issuerState]
	now   func() time.Time
	// onKeysLoaded, when set, runs right after the single key set load and
	// before key selection.
	// Production leaves it nil; the in-flight swap test blocks in it.
	onKeysLoaded func()
}

// verify runs the checks in the order the spec lists and returns the claims
// or the Failure.
// The shape and algorithm checks come first and answer 401 whatever the
// key state is; only a token that passes them can be answered 503 for
// stale keys.
func (v *verifier) verify(ctx context.Context, token string) (claims, *Failure) {
	unauthorized := func(reason string) (claims, *Failure) {
		return claims{}, &Failure{Status: http.StatusUnauthorized, Reason: reason}
	}
	if len(token) > maxTokenBytes {
		return unauthorized(ReasonMalformed)
	}
	jws, err := jose.ParseSignedCompact(token, signatureAlgs())
	alg, oneAlg := singleAlg(token)
	if err != nil {
		var unexpected *jose.ErrUnexpectedSignatureAlgorithm
		if errors.As(err, &unexpected) && oneAlg {
			return unauthorized(ReasonAlg)
		}

		return unauthorized(ReasonMalformed)
	}
	if !oneAlg || len(jws.Signatures) != 1 {
		return unauthorized(ReasonMalformed)
	}
	header := jws.Signatures[0].Header

	st := v.state.Load()
	if st == nil {
		return claims{}, &Failure{Status: http.StatusServiceUnavailable, Reason: ReasonKeysStale}
	}
	ks := st.keys.current()
	if v.onKeysLoaded != nil {
		v.onKeysLoaded()
	}
	if ks.stale(v.now(), st.keys.maxStale) {
		st.keys.refreshOnDemand(ctx)
		ks = st.keys.current()
		if ks.stale(v.now(), st.keys.maxStale) {
			return claims{}, &Failure{Status: http.StatusServiceUnavailable, Reason: ReasonKeysStale}
		}
	}
	key, ok := selectKey(ks, header.KeyID, alg)
	if !ok && header.KeyID != "" && st.keys.refreshOnDemand(ctx) {
		ks = st.keys.current()
		key, ok = selectKey(ks, header.KeyID, alg)
	}
	if !ok {
		return unauthorized(ReasonSignature)
	}
	payload, err := jws.Verify(key.Key)
	if err != nil {
		return unauthorized(ReasonSignature)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return unauthorized(ReasonMalformed)
	}
	if reason := v.checkClaims(header, body); reason != "" {
		return unauthorized(reason)
	}

	return v.keep(body)
}

// singleAlg decodes the protected header on its own and reports the alg it
// carries, and whether it carries exactly one and that one is a string.
// The JOSE parser keeps the last of two alg members and reads a non-string
// one as empty, so the header is walked a second time to refuse both.
func singleAlg(token string) (string, bool) {
	raw, _, _ := strings.Cut(token, ".")
	header, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(string(header)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false
	}
	var alg string
	count := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if key, ok := keyTok.(string); ok && key == "alg" {
			var value json.RawMessage
			if err := dec.Decode(&value); err != nil {
				return "", false
			}
			if err := json.Unmarshal(value, &alg); err != nil {
				return "", false
			}
			count++

			continue
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", false
		}
	}
	if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		return "", false
	}

	return alg, count == 1
}

// selectKey picks the one key a token may be verified with: the held key
// named by kid, which must be compatible with alg, or, without a kid, the
// single compatible key.
// The verifier never tries every key.
func selectKey(ks *keySet, kid, alg string) (jose.JSONWebKey, bool) {
	if ks == nil {
		return jose.JSONWebKey{}, false
	}
	if kid != "" {
		k, held := ks.byKID[kid]

		return k, held && compatible(alg, k)
	}
	var found jose.JSONWebKey
	n := 0
	for _, k := range ks.all {
		if compatible(alg, k) {
			found = k
			n++
		}
	}

	return found, n == 1
}

// checkClaims runs the claim checks in order and returns the reason of the
// first that fails, or "".
func (v *verifier) checkClaims(header jose.Header, body map[string]json.RawMessage) string {
	if iss, ok := asString(body["iss"]); !ok || iss != v.issuer {
		return ReasonIssuer
	}
	if _, ok := identity(body["sub"]); !ok {
		return ReasonClaim
	}
	now := v.now()
	iat, ok := numericDate(body["iat"])
	if !ok || iat > now.Add(v.skew).Unix() {
		return ReasonExpired
	}
	exp, ok := numericDate(body["exp"])
	if !ok || exp <= now.Add(-v.skew).Unix() {
		return ReasonExpired
	}
	if raw, present := body["nbf"]; present {
		nbf, ok := numericDate(raw)
		if !ok || nbf > now.Add(v.skew).Unix() {
			return ReasonExpired
		}
	}
	if _, ok := identity(body[v.usernameClaim]); !ok {
		return ReasonClaim
	}
	if v.tokenType == tokenTypeAccess {
		typ, _ := header.ExtraHeaders[jose.HeaderType].(string)
		if !strings.EqualFold(typ, accessTokenType) {
			return ReasonTokenType
		}
	}
	aud, ok := stringOrArray(body["aud"])
	if !ok || !slices.Contains(aud, v.audience) {
		return ReasonAudience
	}
	if v.tokenType != tokenTypeAccess && len(aud) > 1 {
		if azp, ok := asString(body["azp"]); !ok || azp != v.audience {
			return ReasonAudience
		}
	}
	if raw, present := body[v.groupsClaim]; present {
		if _, ok := stringOrArray(raw); !ok {
			return ReasonClaim
		}
	}

	return ""
}

// keep extracts the claims after every check passed.
func (v *verifier) keep(body map[string]json.RawMessage) (claims, *Failure) {
	c := claims{}
	c.Subject, _ = asString(body["sub"])
	c.Username, _ = asString(body[v.usernameClaim])
	if raw, present := body[v.groupsClaim]; present {
		c.Groups, _ = stringOrArray(raw)
	}
	c.Nonce, _ = asString(body["nonce"])

	return c, nil
}

// absent reports a claim that is missing or JSON null; encoding/json reads
// null into a string or slice without complaint, so it is refused here.
func absent(raw json.RawMessage) bool {
	return raw == nil || string(bytes.TrimSpace(raw)) == "null"
}

// asString decodes raw as a JSON string; an absent or differently typed
// value is not ok.
func asString(raw json.RawMessage) (string, bool) {
	if absent(raw) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}

	return s, true
}

// identity holds sub and the username claim to their shape: a non-empty
// string of at most 256 bytes with no NUL byte.
func identity(raw json.RawMessage) (string, bool) {
	s, ok := asString(raw)
	if !ok || s == "" || len(s) > maxIdentityBytes || strings.IndexByte(s, 0) >= 0 {
		return "", false
	}

	return s, true
}

// numericDate decodes a JSON number of seconds; a missing or non-numeric
// value is not ok, and so is one outside int64, because Go's conversion of
// such a float is implementation-defined and 1e100 must not become a date
// the window checks accept.
func numericDate(raw json.RawMessage) (int64, bool) {
	if absent(raw) {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	if math.IsNaN(f) || f < math.MinInt64 || f >= math.MaxInt64 {
		return 0, false
	}

	return int64(f), true
}

// stringOrArray decodes a JSON string as a one-element array, or an array
// of strings; an absent value or any other shape is not ok.
func stringOrArray(raw json.RawMessage) ([]string, bool) {
	if absent(raw) {
		return nil, false
	}
	if s, ok := asString(raw); ok {
		return []string{s}, true
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, false
	}

	return list, true
}
