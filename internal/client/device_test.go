package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testIssuer         = "https://issuer.example"
	testDeviceEndpoint = testIssuer + "/device"
	testTokenEndpoint  = testIssuer + "/token" //nolint:gosec // G101: a URL, not a credential
	testDeviceCode     = "device-code-secret"
	testUserCode       = "WDJB-MJHT"
)

// pollStep is one scripted answer of the token endpoint: a transport failure
// when fail is set, otherwise the status and body.
type pollStep struct {
	fail   bool
	status int
	body   string
	// html marks a body that is not the RFC 6749 error shape.
	html bool
}

func pending() pollStep {
	return pollStep{status: http.StatusBadRequest, body: `{"error":"authorization_pending"}`}
}

func issued() pollStep {
	return pollStep{status: http.StatusOK, body: `{"id_token":"id-secret","access_token":"access-secret","refresh_token":"refresh-secret","expires_in":300}`}
}

// deviceTransport answers the device endpoint with one scripted body and the
// token endpoint with the steps in order, recording every form it saw.
type deviceTransport struct {
	t          *testing.T
	deviceBody string
	steps      []pollStep
	device     []url.Values
	polls      []url.Values
}

func (d *deviceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d.t.Helper()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		d.t.Fatal(err)
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		d.t.Fatal(err)
	}
	switch req.URL.String() {
	case testDeviceEndpoint:
		d.device = append(d.device, form)
		return jsonResponse(req, http.StatusOK, d.deviceBody, false), nil
	case testTokenEndpoint:
		d.polls = append(d.polls, form)
		if len(d.steps) == 0 {
			d.t.Fatalf("poll %d has no scripted answer", len(d.polls))
		}
		step := d.steps[0]
		d.steps = d.steps[1:]
		if step.fail {
			return nil, errors.New("connection refused")
		}
		return jsonResponse(req, step.status, step.body, step.html), nil
	default:
		d.t.Fatalf("unexpected request to %s", req.URL)
		return nil, nil
	}
}

func jsonResponse(req *http.Request, status int, body string, html bool) *http.Response {
	contentType := "application/json"
	if html {
		contentType = "text/html"
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func deviceBody(interval int) string {
	b := `{"device_code":"` + testDeviceCode + `","user_code":"` + testUserCode + `",` +
		`"verification_uri":"https://issuer.example/device","verification_uri_complete":"https://issuer.example/device?user_code=` + testUserCode + `","expires_in":600`
	if interval > 0 {
		b += `,"interval":` + strconv.Itoa(interval)
	}
	return b + "}"
}

type deviceFixture struct {
	iss     *Issuer
	rt      *deviceTransport
	clock   *fakeClock
	verbose *bytes.Buffer
	meta    Metadata
}

func deviceIssuer(t *testing.T, interval int, steps ...pollStep) *deviceFixture {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 8, 28, 9, 30, 12, 0, time.UTC)}
	rt := &deviceTransport{t: t, deviceBody: deviceBody(interval), steps: steps}
	var verbose bytes.Buffer
	iss, err := NewIssuer(IssuerOptions{Transport: rt, Now: clock.Now, Sleep: clock.Sleep, Verbose: &verbose})
	if err != nil {
		t.Fatal(err)
	}
	return &deviceFixture{
		iss:     iss,
		rt:      rt,
		clock:   clock,
		verbose: &verbose,
		meta:    Metadata{Issuer: testIssuer, DeviceAuthorizationEndpoint: testDeviceEndpoint, TokenEndpoint: testTokenEndpoint},
	}
}

// login runs Authorize then Poll with the deadline the caller computes from expires_in and the login timeout, and returns the start of polling.
func (f *deviceFixture) login(t *testing.T, pkce bool, timeout time.Duration) (DeviceAuth, TokenResponse, time.Time, error) {
	t.Helper()
	d, err := f.iss.Authorize(context.Background(), f.meta, "profgate-cli", []string{"openid", "profile"}, pkce)
	if err != nil {
		t.Fatal(err)
	}
	start := f.clock.Now()
	deadline := start.Add(time.Duration(d.ExpiresIn) * time.Second)
	if byTimeout := start.Add(timeout); byTimeout.Before(deadline) {
		deadline = byTimeout
	}
	tr, err := f.iss.Poll(context.Background(), f.meta, "profgate-cli", d, deadline)
	return d, tr, start, err
}

// assertNoSecretPrinted fails when any secret of the flow reached the verbose
// writer, the only writer these tests supply.
func assertNoSecretPrinted(t *testing.T, f *deviceFixture, d DeviceAuth) {
	t.Helper()
	for _, secret := range []string{testDeviceCode, d.verifier, "id-secret", "access-secret", "refresh-secret"} {
		if secret != "" && strings.Contains(f.verbose.String(), secret) {
			t.Fatalf("verbose output carries %q: %q", secret, f.verbose.String())
		}
	}
}

func TestAuthorizeParsesTheResponse(t *testing.T) {
	f := deviceIssuer(t, 7)
	d, err := f.iss.Authorize(context.Background(), f.meta, "profgate-cli", []string{"openid"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.DeviceCode != testDeviceCode || d.UserCode != testUserCode || d.Interval != 7 || d.ExpiresIn != 600 {
		t.Fatalf("DeviceAuth = %+v", d)
	}
	if d.VerificationURI != "https://issuer.example/device" || !strings.HasSuffix(d.VerificationURIComplete, testUserCode) {
		t.Fatalf("verification URIs = %q, %q", d.VerificationURI, d.VerificationURIComplete)
	}
}

func TestAuthorizeRefusals(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"a response without a device code", `{"user_code":"X","verification_uri":"https://issuer.example/d","expires_in":600}`, "device_code"},
		{"a response without a user code", `{"device_code":"c","verification_uri":"https://issuer.example/d","expires_in":600}`, "user_code"},
		{"a response without a verification uri", `{"device_code":"c","user_code":"X","expires_in":600}`, "verification_uri"},
		{"a response without expires_in", `{"device_code":"c","user_code":"X","verification_uri":"https://issuer.example/d"}`, "expires_in"},
		{"a response that is not JSON", `<html>no</html>`, "200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := deviceIssuer(t, 5)
			f.rt.deviceBody = tc.body
			_, err := f.iss.Authorize(context.Background(), f.meta, "profgate-cli", nil, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "<html>") {
				t.Fatalf("the error carries the body: %v", err)
			}
		})
	}
}

func TestAuthorizeWithoutDeviceEndpoint(t *testing.T) {
	f := deviceIssuer(t, 5)
	f.meta.DeviceAuthorizationEndpoint = ""
	_, err := f.iss.Authorize(context.Background(), f.meta, "profgate-cli", nil, false)
	if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "device") || !strings.Contains(err.Error(), "--token-file") {
		t.Fatalf("err = %v, want a usage error naming the grant and --token-file", err)
	}
	if len(f.rt.device) != 0 {
		t.Fatal("a request was sent")
	}
}

func TestPollPendingThenIssued(t *testing.T) {
	f := deviceIssuer(t, 7, pending(), issued())
	d, tr, start, err := f.login(t, false, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if tr.AccessToken != "access-secret" || tr.IDToken != "id-secret" || tr.RefreshToken != "refresh-secret" {
		t.Fatalf("TokenResponse = %+v", tr)
	}
	if got := f.clock.Now().Sub(start); got != 7*time.Second {
		t.Fatalf("the clock advanced %s, want exactly one interval of 7s", got)
	}
	if len(f.rt.polls) != 2 {
		t.Fatalf("%d polls, want 2", len(f.rt.polls))
	}
	assertNoSecretPrinted(t, f, d)
}

func TestPollSlowDownRaisesTheInterval(t *testing.T) {
	f := deviceIssuer(t, 5,
		pollStep{status: http.StatusBadRequest, body: `{"error":"slow_down"}`},
		pending(),
		issued(),
	)
	d, _, start, err := f.login(t, false, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// slow_down waits 5+5 seconds, then authorization_pending waits the
	// raised 10 seconds again.
	if got := f.clock.Now().Sub(start); got != 20*time.Second {
		t.Fatalf("the clock advanced %s, want 10s after slow_down and 10s after the next poll", got)
	}
	if len(f.rt.polls) != 3 {
		t.Fatalf("%d polls, want 3", len(f.rt.polls))
	}
	assertNoSecretPrinted(t, f, d)
}

func TestPollDefaultsTheInterval(t *testing.T) {
	f := deviceIssuer(t, 0, pending(), pending(), issued())
	d, _, start, err := f.login(t, false, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if d.Interval != 0 {
		t.Fatalf("Interval = %d, want the absent value kept as 0", d.Interval)
	}
	if got := f.clock.Now().Sub(start); got != 10*time.Second {
		t.Fatalf("the clock advanced %s, want two default intervals of 5s", got)
	}
	assertNoSecretPrinted(t, f, d)
}

func TestPollStopsOnDeviceErrors(t *testing.T) {
	cases := []struct {
		name string
		step pollStep
		code string
		want []string
		// absent lists what the message must not carry.
		absent []string
	}{
		{
			name: "access_denied",
			step: pollStep{status: http.StatusBadRequest, body: `{"error":"access_denied","error_description":"the user said no"}`},
			code: "access_denied",
			want: []string{"denied at the issuer"},
		},
		{
			name: "expired_token",
			step: pollStep{status: http.StatusBadRequest, body: `{"error":"expired_token","error_description":"gone"}`},
			code: "expired_token",
			want: []string{"expired", "profgate login"},
		},
		{
			name:   "a 400 with an unrecognized error value",
			step:   pollStep{status: http.StatusBadRequest, body: `{"error":"invalid_client","error_description":"unknown client"}`},
			code:   "invalid_client",
			want:   []string{"invalid_client"},
			absent: []string{"unknown client"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := deviceIssuer(t, 5, tc.step)
			d, _, start, err := f.login(t, false, 10*time.Minute)
			var de *DeviceError
			if !errors.As(err, &de) || de.Code != tc.code {
				t.Fatalf("err = %v, want a DeviceError %q", err, tc.code)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("message %q does not name %q", err, w)
				}
			}
			for _, a := range append(tc.absent, "the user said no", "gone") {
				if strings.Contains(err.Error(), a) {
					t.Fatalf("message %q carries the body's %q", err, a)
				}
			}
			if len(f.rt.polls) != 1 || !f.clock.Now().Equal(start) {
				t.Fatalf("%d polls and %s slept, want one poll and no wait", len(f.rt.polls), f.clock.Now().Sub(start))
			}
			assertNoSecretPrinted(t, f, d)
		})
	}
}

func TestPollStopsOnA400WithoutTheErrorShape(t *testing.T) {
	f := deviceIssuer(t, 5, pollStep{status: http.StatusBadRequest, body: "<html>rejected by proxy</html>", html: true})
	d, _, _, err := f.login(t, false, 10*time.Minute)
	var ie *IssuerError
	if !errors.As(err, &ie) || ie.Status != http.StatusBadRequest || ie.Code != "" {
		t.Fatalf("err = %v, want an IssuerError 400 without a code", err)
	}
	if !strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "proxy") || strings.Contains(err.Error(), "<") {
		t.Fatalf("message = %q, want the status and nothing from the body", err)
	}
	if len(f.rt.polls) != 1 {
		t.Fatalf("%d polls, want 1", len(f.rt.polls))
	}
	assertNoSecretPrinted(t, f, d)
}

func TestPollRetriesUntilTheDeadline(t *testing.T) {
	cases := []struct {
		name string
		step pollStep
	}{
		{"a 500", pollStep{status: http.StatusInternalServerError, body: `{"error":"server_error"}`}},
		{"a transport failure", pollStep{fail: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A 12-second login timeout admits polls at 0, 5, and 10 seconds;
			// the fourth would fall at 15, after the deadline, and is never sent.
			f := deviceIssuer(t, 5, tc.step, tc.step, tc.step, issued())
			d, _, start, err := f.login(t, false, 12*time.Second)
			if err == nil {
				t.Fatal("expected the deadline to end polling")
			}
			var de *DeviceError
			if errors.As(err, &de) {
				t.Fatalf("err = %v, want the deadline rather than an issuer decision", err)
			}
			if !strings.Contains(err.Error(), "deadline") {
				t.Fatalf("message %q does not name the deadline", err)
			}
			if len(f.rt.polls) != 3 {
				t.Fatalf("%d polls, want 3", len(f.rt.polls))
			}
			if got := f.clock.Now().Sub(start); got != 10*time.Second {
				t.Fatalf("the clock advanced %s, want 10s", got)
			}
			assertNoSecretPrinted(t, f, d)
		})
	}
}

func TestPollStopsAtADeadlineSoonerThanTheInterval(t *testing.T) {
	f := deviceIssuer(t, 5, pending(), issued())
	d, _, start, err := f.login(t, false, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("err = %v, want the deadline to end polling", err)
	}
	if len(f.rt.polls) != 1 {
		t.Fatalf("%d polls, want 1 and none after the deadline", len(f.rt.polls))
	}
	if f.clock.Now().After(start.Add(3 * time.Second)) {
		t.Fatalf("the clock passed the deadline: %s", f.clock.Now().Sub(start))
	}
	assertNoSecretPrinted(t, f, d)
}

func TestPollStopsWhenTheContextEnds(t *testing.T) {
	f := deviceIssuer(t, 5, pending())
	d, err := f.iss.Authorize(context.Background(), f.meta, "profgate-cli", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.iss.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	_, err = f.iss.Poll(ctx, f.meta, "profgate-cli", d, f.clock.Now().Add(time.Hour))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(f.rt.polls) != 1 {
		t.Fatalf("%d polls, want 1", len(f.rt.polls))
	}
}

func TestDeviceFormsWithPKCE(t *testing.T) {
	f := deviceIssuer(t, 5, pending(), issued())
	d, _, _, err := f.login(t, true, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.verifier) < 43 || len(d.verifier) > 128 {
		t.Fatalf("verifier of %d bytes is outside RFC 7636's bounds", len(d.verifier))
	}
	req := f.rt.device[0]
	if req.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", req.Get("code_challenge_method"))
	}
	sum := sha256.Sum256([]byte(d.verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); req.Get("code_challenge") != want {
		t.Fatalf("code_challenge = %q, want the base64url SHA-256 of the verifier %q", req.Get("code_challenge"), want)
	}
	for n, poll := range f.rt.polls {
		if poll.Get("code_verifier") != d.verifier {
			t.Fatalf("poll %d code_verifier = %q, want the verifier", n+1, poll.Get("code_verifier"))
		}
	}
	assertNoSecretPrinted(t, f, d)
}

func TestDeviceFormsWithoutPKCE(t *testing.T) {
	f := deviceIssuer(t, 5, pending(), issued())
	d, _, _, err := f.login(t, false, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if d.verifier != "" {
		t.Fatal("a verifier was generated")
	}
	for _, key := range []string{"code_challenge", "code_challenge_method"} {
		if f.rt.device[0].Has(key) {
			t.Fatalf("the device request carries %s", key)
		}
	}
	for n, poll := range f.rt.polls {
		if poll.Has("code_verifier") {
			t.Fatalf("poll %d carries code_verifier", n+1)
		}
	}
}

func TestDeviceFormsCarryTheGrantAndNoSecret(t *testing.T) {
	f := deviceIssuer(t, 5, pending(), issued())
	_, _, _, err := f.login(t, true, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	device := f.rt.device[0]
	if device.Get("client_id") != "profgate-cli" || device.Get("scope") != "openid profile" {
		t.Fatalf("device request = %v, want client_id and the space-joined scope", device)
	}
	if device.Has("client_secret") {
		t.Fatal("the device request carries a client secret")
	}
	for n, poll := range f.rt.polls {
		if poll.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Fatalf("poll %d grant_type = %q", n+1, poll.Get("grant_type"))
		}
		if poll.Get("device_code") != testDeviceCode || poll.Get("client_id") != "profgate-cli" {
			t.Fatalf("poll %d = %v, want device_code and client_id", n+1, poll)
		}
		if poll.Has("client_secret") {
			t.Fatalf("poll %d carries a client secret", n+1)
		}
	}
}

func TestDeviceErrorMessages(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{"access_denied", "the request was denied at the issuer"},
		{"expired_token", "the code expired; run profgate login again"},
		{"invalid_client", "the issuer answered invalid_client"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			if got := (&DeviceError{Code: tc.code}).Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}
