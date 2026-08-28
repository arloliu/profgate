package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
			if code := fail(te.env, tc.err); code != tc.wantCode {
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
			wantStderr: "profgate: HTTP 502 Bad Gateway\n",
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
