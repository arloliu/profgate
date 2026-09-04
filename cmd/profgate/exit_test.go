package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/client"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantLine string // what fail prints, after "profgate: "
		notLine  string
	}{
		{name: "did what it said", err: nil, wantCode: 0},
		{
			name:     "envelope error",
			err:      &client.APIError{Status: 400, Code: "seconds_exceeds_limit", Message: "effective duration 120s exceeds the limit of 60s"},
			wantCode: 1,
			wantLine: "seconds_exceeds_limit: effective duration 120s exceeds the limit of 60s",
		},
		{
			name:     "401 unauthenticated",
			err:      &client.APIError{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: "authentication required"},
			wantCode: 3,
			wantLine: "unauthenticated: authentication required",
		},
		{
			name:     "401 without the envelope",
			err:      &client.StatusError{Status: http.StatusUnauthorized},
			wantCode: 3,
			wantLine: "HTTP 401 Unauthorized",
		},
		{
			name: "401 wrapped in a diagnostic",
			err: &client.AuthDiagnostic{
				Issuer:   "https://issuer.example/realms/eng",
				ClientID: "profgate",
				Err:      &client.APIError{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: "authentication required"},
			},
			wantCode: 3,
			wantLine: `https://issuer.example/realms/eng for client "profgate"`,
		},
		{
			name:     "403 realm_denied",
			err:      &client.APIError{Status: http.StatusForbidden, Code: "realm_denied", Message: "the realm does not admit payments"},
			wantCode: 1,
			wantLine: "realm_denied: the realm does not admit payments",
		},
		{
			name:     "a response that is not the envelope",
			err:      &client.StatusError{Status: http.StatusBadGateway},
			wantCode: 1,
			wantLine: "HTTP 502 Bad Gateway",
		},
		{
			name:     "a transport failure",
			err:      &client.TransportError{Origin: "https://g.example:443", Err: errors.New("connection refused")},
			wantCode: 1,
			wantLine: "https://g.example:443: connection refused",
		},
		{
			name:     "a usage error",
			err:      fmt.Errorf("%w: --token-file and --token-stdin name two sources for one token", client.ErrUsage),
			wantCode: 2,
			wantLine: "--token-file and --token-stdin",
		},
		{
			name:     "a local validation failure",
			err:      fmt.Errorf("%w: a basic user name cannot contain a colon", client.ErrUsage),
			wantCode: 2,
			wantLine: "colon",
		},
		{
			name:     "login needed",
			err:      fmt.Errorf("context %q: %w", "prod", client.ErrLoginNeeded),
			wantCode: 3,
			wantLine: "run profgate login",
		},
		{
			name:     "a refresh the issuer refused",
			err:      fmt.Errorf("the refresh was refused (%w): %w", &client.IssuerError{Status: 400, Code: "invalid_grant"}, client.ErrLoginNeeded),
			wantCode: 3,
			wantLine: "invalid_grant",
		},
		{
			name:     "a cancelled context",
			err:      context.Canceled,
			wantCode: 1,
			wantLine: "context canceled",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code := exitCode(tc.err); code != tc.wantCode {
				t.Fatalf("exitCode(%v) = %d, want %d", tc.err, code, tc.wantCode)
			}
			if tc.err == nil {
				return
			}
			te := newTestEnv(t)
			if code := fail(te.env, "table", tc.err); code != tc.wantCode {
				t.Fatalf("fail(%v) = %d, want %d", tc.err, code, tc.wantCode)
			}
			line := te.stderr.String()
			if !strings.HasPrefix(line, "profgate: ") || !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
				t.Fatalf("fail printed %q, want one line starting with \"profgate: \"", line)
			}
			if !strings.Contains(line, tc.wantLine) {
				t.Fatalf("fail printed %q, want it to contain %q", line, tc.wantLine)
			}
			if te.stdout.Len() != 0 {
				t.Fatalf("fail wrote %q to stdout", te.stdout.String())
			}
		})
	}
}

// TestDispatchExitCodes drives the exit codes through the dispatcher, where
// the error comes from a real request rather than a constructed value.
func TestDispatchExitCodes(t *testing.T) {
	tests := []struct {
		name       string
		transport  http.RoundTripper
		wantCode   int
		wantStderr string
		notStderr  string
	}{
		{
			name: "envelope error",
			transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusForbidden, `{"error":"the realm does not admit payments","code":"realm_denied"}`), nil
			}),
			wantCode:   1,
			wantStderr: "profgate: realm_denied: the realm does not admit payments\n",
		},
		{
			name: "401",
			transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusUnauthorized, `{"error":"authentication required","code":"unauthenticated"}`), nil
			}),
			wantCode:   3,
			wantStderr: "profgate: unauthenticated: authentication required\n",
		},
		{
			name: "html body",
			transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				resp := jsonResponse(http.StatusBadGateway, "<html>upstream sadness</html>")
				resp.Header.Set("Content-Type", "text/html")
				return resp, nil
			}),
			wantCode:   1,
			wantStderr: "profgate: HTTP 502 Bad Gateway: body is not a profgate JSON document\n",
			notStderr:  "sadness",
		},
		{
			name: "transport failure",
			transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial tcp: connection refused")
			}),
			wantCode:   1,
			wantStderr: "https://g.example:443: dial tcp: connection refused",
			notStderr:  "/v1/whoami",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			te.env.transport = tc.transport
			te.env.stdin = strings.NewReader("tok\n")
			var got smokeRun
			code := dispatch(context.Background(), te.env, smokeVerbs(t, &got), []string{"smokewhoami", "--server", "https://g.example", "--token-stdin"})
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d (stderr=%q)", code, tc.wantCode, te.stderr.String())
			}
			if !strings.Contains(te.stderr.String(), tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", te.stderr.String(), tc.wantStderr)
			}
			if tc.notStderr != "" && strings.Contains(te.stderr.String(), tc.notStderr) {
				t.Fatalf("stderr = %q, must not contain %q", te.stderr.String(), tc.notStderr)
			}
		})
	}
}

// notEnvelopeShaped is one response this client cannot read as the gateway's envelope,
// carrying the headers and the body a metadata-printing client would have quoted.
type notEnvelopeShaped struct {
	name        string
	status      int
	contentType string
	body        string
	marker      string // a recognizable string inside the body, empty for an empty body
	wantStatus  string
}

// notEnvelopeCases are the four bodies that are one failure:
// HTML from an Ingress, an empty body, truncated JSON,
// and a 2xx from a JSON route whose body is not one JSON document.
var notEnvelopeCases = []notEnvelopeShaped{
	{
		name: "html from an ingress", status: http.StatusBadGateway,
		contentType: "text/html; charset=utf-8",
		body:        "<html><body>upstream secret-host is down</body></html>",
		marker:      "secret-host", wantStatus: "HTTP 502 Bad Gateway",
	},
	{
		name: "an empty body", status: http.StatusBadGateway,
		contentType: "application/octet-stream", wantStatus: "HTTP 502 Bad Gateway",
	},
	{
		name: "truncated json", status: http.StatusBadGateway,
		contentType: "application/json",
		body:        `{"code":"realm_denied","error":"secret-host is down`,
		marker:      "secret-host", wantStatus: "HTTP 502 Bad Gateway",
	},
	{
		name: "a 2xx that is not one json document", status: http.StatusOK,
		contentType: "application/json",
		body:        `{"namespaces":["secret-host"`,
		marker:      "secret-host", wantStatus: "HTTP 200 OK",
	},
}

// answerWith is one response with the headers a client that quoted metadata would have had to hand:
// a media type and a length.
func answerWith(status int, contentType, body string) http.RoundTripper {
	return roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header: http.Header{
				"Content-Type":   {contentType},
				"Content-Length": {strconv.Itoa(len(body))},
			},
			ContentLength: int64(len(body)),
			Body:          io.NopCloser(strings.NewReader(body)),
		}, nil
	})
}

// TestNotEnvelopeShapedPrintsNothingCarried proves the fixed line is the whole message:
// the status, its reason, and a clause of this client's own,
// with no media type, no length, and no byte of the body on either stream.
func TestNotEnvelopeShapedPrintsNothingCarried(t *testing.T) {
	for _, tc := range notEnvelopeCases {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			te.env.transport = answerWith(tc.status, tc.contentType, tc.body)
			code := dispatch(context.Background(), te.env, clientVerbs(), []string{"whoami", "--server", "https://g.example"})
			if code != exitRefused {
				t.Fatalf("code = %d, want %d (stderr=%q)", code, exitRefused, te.stderr.String())
			}
			want := "profgate: " + tc.wantStatus + ": body is not a profgate JSON document\n"
			if te.stderr.String() != want {
				t.Fatalf("stderr = %q, want %q", te.stderr.String(), want)
			}
			if te.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want it empty", te.stdout.String())
			}
			both := te.stdout.String() + te.stderr.String()
			carried := []string{tc.contentType, "text/html", "application/octet-stream", tc.marker}
			// An empty body's length is a digit the status itself holds, so
			// the length is checked where there is a length to leak.
			if tc.body != "" {
				carried = append(carried, strconv.Itoa(len(tc.body)))
			}
			for _, c := range carried {
				if c == "" {
					continue
				}
				if strings.Contains(both, c) {
					t.Fatalf("the streams carry %q, which the response carried: %q", c, both)
				}
			}
		})
	}
}

// TestNotEnvelopeShapedExitCodes proves a 401 in that state still asks for a login
// and every other status is a refusal,
// while a 2xx carrying one JSON document is the success path.
func TestNotEnvelopeShapedExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode int
		wantOut  string
	}{
		{name: "401", status: http.StatusUnauthorized, body: "<html>no</html>", wantCode: exitLogin},
		{name: "403", status: http.StatusForbidden, body: "<html>no</html>", wantCode: exitRefused},
		{name: "502", status: http.StatusBadGateway, body: "<html>no</html>", wantCode: exitRefused},
		{name: "a 2xx carrying one json document", status: http.StatusOK, body: whoamiBody, wantCode: exitOK, wantOut: whoamiBody},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			te.env.transport = answerWith(tc.status, "text/html", tc.body)
			code := dispatch(context.Background(), te.env, clientVerbs(), []string{"whoami", "--output", "json", "--server", "https://g.example"})
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d (stderr=%q)", code, tc.wantCode, te.stderr.String())
			}
			if te.stdout.String() != tc.wantOut {
				t.Fatalf("stdout = %q, want %q", te.stdout.String(), tc.wantOut)
			}
		})
	}
}

// refusalBody is one envelope with whitespace of its own,
// so a copy that rebuilt the document instead of copying it would not match.
const refusalBody = `{
  "error": "effective duration 120s exceeds the limit of 60s",
  "code": "seconds_exceeds_limit"
}
`

// TestRefusalUnderJSONCopiesTheEnvelope proves the gateway's own bytes reach stdout under --output json,
// from every kind of caller,
// while the one line stays on stderr.
func TestRefusalUnderJSONCopiesTheEnvelope(t *testing.T) {
	tests := []struct {
		name string
		args []string
		copy bool // a verb that sends no request holds no envelope to copy
	}{
		{name: "a read verb", args: []string{"whoami"}, copy: true},
		{name: "profile", args: []string{"profile", "payments/checkout", "cpu", "-o", "PROFILE"}, copy: true},
		{name: "collect", args: []string{"collect", "payments/checkout", "--duration", "30s"}, copy: true},
		{name: "pgo policy set", args: []string{"pgo", "policy", "set", "payments/checkout", "--enabled"}, copy: true},
		{name: "download", args: []string{"download", "7h2k9m4p6r8t0v1w3x5y", "-o", "PROFILE"}, copy: true},
		{name: "context", args: []string{"context", "show", "nope"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			te.env.transport = answerWith(http.StatusBadRequest, "application/json", refusalBody)
			args := make([]string, 0, len(tc.args)+4)
			for _, a := range tc.args {
				if a == "PROFILE" {
					a = filepath.Join(t.TempDir(), "artifact.pprof")
				}
				args = append(args, a)
			}
			args = append(args, "--output", "json", "--server", "https://g.example")
			if code := dispatch(context.Background(), te.env, clientVerbs(), args); code == exitOK {
				t.Fatalf("code = 0, want a refusal (stderr=%q)", te.stderr.String())
			}
			want := ""
			if tc.copy {
				want = refusalBody
				line := "profgate: seconds_exceeds_limit: effective duration 120s exceeds the limit of 60s\n"
				if !strings.Contains(te.stderr.String(), line) {
					t.Fatalf("stderr = %q, want it to contain %q", te.stderr.String(), line)
				}
			}
			if te.stdout.String() != want {
				t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
			}
		})
	}
}

// TestNoEnvelopeLeavesStdoutEmpty proves the copy is the one thing that reaches stdout under --output json:
// nothing this client composed does.
func TestNoEnvelopeLeavesStdoutEmpty(t *testing.T) {
	tests := []struct {
		name      string
		transport http.RoundTripper
		args      []string
	}{
		{
			name:      "a transport failure",
			transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial tcp: connection refused") }),
			args:      []string{"whoami"},
		},
		{
			name:      "a response that is not envelope-shaped",
			transport: answerWith(http.StatusBadGateway, "text/html", "<html>no</html>"),
			args:      []string{"whoami"},
		},
		{
			name:      "a usage error",
			transport: refusingTransport(t),
			args:      []string{"services", "payments/checkout"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			te.env.transport = tc.transport
			args := append(append([]string{}, tc.args...), "--output", "json", "--server", "https://g.example")
			if code := dispatch(context.Background(), te.env, clientVerbs(), args); code == exitOK {
				t.Fatalf("code = 0, want a failure (stderr=%q)", te.stderr.String())
			}
			if te.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want it empty", te.stdout.String())
			}
		})
	}
}
