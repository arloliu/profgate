package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// deviceGrantType is the grant every poll names (RFC 8628).
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// defaultPollInterval applies when the device response omits interval
	// (RFC 8628).
	defaultPollInterval = 5 * time.Second
	// slowDownIncrement is what slow_down adds to the interval (RFC 8628).
	slowDownIncrement = 5 * time.Second
	// verifierBytes is the entropy of a PKCE verifier: 32 bytes encode to 43 characters, RFC 7636's minimum length.
	verifierBytes = 32
)

// DeviceAuth is the device authorization response and, under PKCE, the
// verifier every poll must carry.
// Interval is the value the issuer sent, 0 when it sent none; Poll applies the 5-second default.
type DeviceAuth struct {
	DeviceCode, UserCode                     string
	VerificationURI, VerificationURIComplete string
	Interval, ExpiresIn                      int
	// verifier stays unexported: nothing outside this package may read it,
	// and the value travels from Authorize to Poll inside the struct.
	verifier string
}

// DeviceError is a polling outcome that ends the login: access_denied,
// expired_token, or an unrecognized error value, each with its own message.
// The message carries the issuer's error value and nothing else from the body.
type DeviceError struct {
	Code string
}

func (e *DeviceError) Error() string {
	switch e.Code {
	case "access_denied":
		return "the request was denied at the issuer"
	case "expired_token":
		return "the code expired; run profgate login again"
	default:
		return "the issuer answered " + e.Code
	}
}

// deviceResponse is the device endpoint's 200 as RFC 8628 shapes it.
type deviceResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// Authorize posts to the device endpoint;
// pkce adds code_challenge and code_challenge_method=S256,
// which is always sent explicitly because an issuer that defaults the method defaults it to plain.
// No client secret is sent: the device grant is a public client's grant.
func (i *Issuer) Authorize(ctx context.Context, m Metadata, clientID string, scopes []string, pkce bool) (DeviceAuth, error) {
	if m.DeviceAuthorizationEndpoint == "" {
		return DeviceAuth{}, fmt.Errorf("%w: the issuer %s publishes no device_authorization_endpoint, so the device grant is not available; pass a token obtained elsewhere with --token-file", ErrUsage, m.Issuer)
	}
	form := url.Values{"client_id": {clientID}}
	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}
	var verifier string
	if pkce {
		v, err := newVerifier()
		if err != nil {
			return DeviceAuth{}, err
		}
		verifier = v
		form.Set("code_challenge", challengeOf(verifier))
		form.Set("code_challenge_method", "S256")
	}
	body, err := i.postFormBody(ctx, "device", m.DeviceAuthorizationEndpoint, form)
	if err != nil {
		return DeviceAuth{}, err
	}
	var dr deviceResponse
	if err := decodeOne(body, &dr); err != nil {
		return DeviceAuth{}, fmt.Errorf("device: %w", &StatusError{Status: http.StatusOK, Detail: "body is not a device response"})
	}
	switch {
	case dr.DeviceCode == "":
		return DeviceAuth{}, errors.New("device: the response carries no device_code")
	case dr.UserCode == "":
		return DeviceAuth{}, errors.New("device: the response carries no user_code")
	case dr.VerificationURI == "":
		return DeviceAuth{}, errors.New("device: the response carries no verification_uri")
	case dr.ExpiresIn <= 0:
		return DeviceAuth{}, errors.New("device: the response carries no positive expires_in")
	}
	return DeviceAuth{
		DeviceCode:              dr.DeviceCode,
		UserCode:                dr.UserCode,
		VerificationURI:         dr.VerificationURI,
		VerificationURIComplete: dr.VerificationURIComplete,
		Interval:                dr.Interval,
		ExpiresIn:               dr.ExpiresIn,
		verifier:                verifier,
	}, nil
}

// Poll runs RFC 8628's polling loop on the injected clock until a token, a
// permanent failure, or the earlier of the code's expiry and the deadline.
// authorization_pending waits one interval;
// slow_down adds 5 seconds to the interval and waits;
// access_denied, expired_token, and any other 4xx stop;
// a transport failure or a 5xx waits one interval and polls again.
// No poll is sent at or after the deadline.
func (i *Issuer) Poll(ctx context.Context, m Metadata, clientID string, d DeviceAuth, deadline time.Time) (TokenResponse, error) {
	interval := time.Duration(d.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	form := url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {d.DeviceCode},
		"client_id":   {clientID},
	}
	if d.verifier != "" {
		form.Set("code_verifier", pollVerifier(d))
	}
	for {
		if !i.now().Before(deadline) {
			return TokenResponse{}, errDeadline
		}
		tr, err := i.postForm(ctx, "token", m.TokenEndpoint, form)
		if err == nil {
			return tr, nil
		}
		if ctx.Err() != nil {
			return TokenResponse{}, ctx.Err()
		}
		raise, stop := classifyPoll(err)
		if stop != nil {
			return TokenResponse{}, stop
		}
		if raise {
			interval += slowDownIncrement
		}
		if !i.now().Add(interval).Before(deadline) {
			return TokenResponse{}, errDeadline
		}
		if err := i.sleep(ctx, interval); err != nil {
			return TokenResponse{}, err
		}
	}
}

// errDeadline ends polling when the next poll would fall at or after the
// deadline, the earlier of the code's expiry and the login timeout.
var errDeadline = errors.New("the login deadline passed before the issuer issued a token; run profgate login again")

// classifyPoll sorts one failed poll: whether slow_down raised the interval,
// and the error that stops polling, nil when the poll is retried.
// An IssuerError with no code is a 4xx whose body is not the error shape and stops with its status;
// a 5xx or a transport failure is retried.
func classifyPoll(err error) (raise bool, stop error) {
	var ie *IssuerError
	var se *StatusError
	var te *TransportError
	switch {
	case errors.As(err, &ie):
		switch ie.Code {
		case "authorization_pending":
			return false, nil
		case "slow_down":
			return true, nil
		case "":
			return false, ie
		default:
			return false, &DeviceError{Code: ie.Code}
		}
	case errors.As(err, &se):
		if se.Status >= 500 {
			return false, nil
		}
		return false, err
	case errors.As(err, &te):
		return false, nil
	default:
		return false, err
	}
}

// newVerifier is a PKCE verifier: 32 random bytes, base64url without padding.
func newVerifier() (string, error) {
	b := make([]byte, verifierBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate code verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// challengeOf is the S256 challenge: the base64url SHA-256 of the verifier.
func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
