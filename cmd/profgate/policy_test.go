package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const defaultsPolicyBody = `{"namespace":"payments","service":"checkout","source":"defaults","override":null,"effective":{"enabled":false,"schedule":{"every":"6h","jitter":"10m"},"sampling":{"duration":"30s","rounds":1,"roundInterval":"30s","replicas":"all","maxParallel":4},"target":{"versionPolicy":"strict","version":""},"artifact":{"retention":"24h"}},"violations":[]}` + "\n"

const overridePolicyBody = `{"namespace":"payments","service":"checkout","source":"override","override":{"enabled":true,"sampling":{"rounds":3}},"effective":{"enabled":true,"schedule":{"every":"6h","jitter":"10m"},"sampling":{"duration":"30s","rounds":3,"roundInterval":"30s","replicas":2,"maxParallel":4},"target":{"versionPolicy":"strict","version":""},"artifact":{"retention":"2h"}},"violations":[{"field":"/artifact/retention","ceiling":"pgo.defaults.schedule.every","detail":"retention 2h is under the interval 6h"}],"updatedBy":"alice","updatedAt":"2026-08-26T09:00:00Z"}` + "\n"

// policyTransport is the gateway's policy route: every GET answers the read
// with its ETag when one is set, and every other method answers with what
// the test supplies.
// It records each request with its body and counts the modifying ones.
type policyTransport struct {
	etag      string
	read      string
	write     answer
	requests  []*http.Request
	bodies    []string
	modifying int
}

func (pt *policyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	pt.requests = append(pt.requests, r)
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	pt.bodies = append(pt.bodies, body)
	if r.Method == http.MethodGet {
		resp := jsonResponse(http.StatusOK, pt.read)
		if pt.etag != "" {
			resp.Header.Set("ETag", pt.etag)
		}
		return resp, nil
	}
	pt.modifying++
	resp := jsonResponse(pt.write.status, pt.write.body)
	if pt.write.etag != "" {
		resp.Header.Set("ETag", pt.write.etag)
	}
	return resp, nil
}

// answer is what a write is answered with: a status, a body, and an ETag
// when one is given.
type answer struct {
	status     int
	body, etag string
}

// answering is one answer.
func answering(status int, body, etag string) answer {
	return answer{status: status, body: body, etag: etag}
}

// runPolicy runs pgo policy <sub> against rt and returns the exit code.
func runPolicy(te *testEnv, rt http.RoundTripper, sub string, args ...string) int {
	te.env.transport = rt
	args = append([]string{"pgo", "policy", sub}, args...)
	args = append(args, "--server", "https://g.example")
	return dispatch(context.Background(), te.env, clientVerbs(), args)
}

// modifyingRequest is the one request that was not a GET.
func modifyingRequest(t *testing.T, pt *policyTransport) *http.Request {
	t.Helper()
	if pt.modifying != 1 {
		t.Fatalf("%d modifying requests, want exactly one", pt.modifying)
	}
	var found *http.Request
	for _, r := range pt.requests {
		if r.Method != http.MethodGet {
			found = r
		}
	}
	if found == nil {
		t.Fatalf("no modifying request among %d", len(pt.requests))
	}
	return found
}

func TestPolicySetSendsIfMatchFromTheRead(t *testing.T) {
	te := newTestEnv(t)
	pt := &policyTransport{etag: `"42"`, read: overridePolicyBody, write: answering(http.StatusOK, overridePolicyBody, `"43"`)}
	if code := runPolicy(te, pt, "set", "payments/checkout", "--rounds", "3", "--enabled"); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if len(pt.requests) != 2 || pt.requests[0].Method != http.MethodGet {
		t.Fatalf("%d requests, want the read then the write", len(pt.requests))
	}
	put := modifyingRequest(t, pt)
	if put.Method != http.MethodPut || put.URL.Path != "/v1/namespaces/payments/services/checkout/pgo" {
		t.Fatalf("write = %s %s, want PUT the policy route", put.Method, put.URL.Path)
	}
	if got := put.Header.Values("If-Match"); len(got) != 1 || got[0] != `"42"` {
		t.Fatalf("If-Match = %q, want the ETag the read carried", got)
	}
	if put.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", put.Header.Get("Content-Type"))
	}
	if pt.bodies[1] != `{"enabled":true,"sampling":{"rounds":3}}` {
		t.Fatalf("body = %s", pt.bodies[1])
	}
}

func TestPolicySetWithoutETagSendsNoIfMatch(t *testing.T) {
	te := newTestEnv(t)
	pt := &policyTransport{read: defaultsPolicyBody, write: answering(http.StatusCreated, overridePolicyBody, `"1"`)}
	if code := runPolicy(te, pt, "set", "payments/checkout", "--enabled"); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	put := modifyingRequest(t, pt)
	if _, present := put.Header["If-Match"]; present {
		t.Fatalf("If-Match = %q, want no header at all when the read carried no ETag", put.Header.Values("If-Match"))
	}
	for _, r := range pt.requests {
		if r.Header.Get("If-Match") == "*" {
			t.Fatalf("%s sent If-Match: *", r.Method)
		}
	}
}

func TestPolicyWritesAreNotRetried(t *testing.T) {
	tests := []struct {
		name   string
		sub    string
		etag   string
		read   string
		status int
		body   string
		code   string
	}{
		{name: "set against a concurrent create", sub: "set", read: defaultsPolicyBody, status: http.StatusPreconditionRequired, body: `{"error":"the service already has a policy override; send If-Match with its ETag","code":"precondition_required"}`, code: "precondition_required"},
		{name: "set against a concurrent update", sub: "set", etag: `"42"`, read: overridePolicyBody, status: http.StatusPreconditionFailed, body: `{"error":"the policy has moved since the revision If-Match names","code":"precondition_failed"}`, code: "precondition_failed"},
		{name: "delete against a concurrent delete", sub: "delete", etag: `"42"`, read: overridePolicyBody, status: http.StatusPreconditionFailed, body: `{"error":"the policy has moved since the revision If-Match names","code":"precondition_failed"}`, code: "precondition_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			pt := &policyTransport{etag: tc.etag, read: tc.read, write: answering(tc.status, tc.body, "")}
			args := []string{"payments/checkout"}
			if tc.sub == "set" {
				args = append(args, "--enabled")
			}
			code := runPolicy(te, pt, tc.sub, args...)
			if code != 1 {
				t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
			}
			if pt.modifying != 1 {
				t.Fatalf("%d modifying requests, want exactly one: the write is not retried", pt.modifying)
			}
			if len(pt.requests) != 2 {
				t.Fatalf("%d requests, want the read and the one write: nothing re-reads", len(pt.requests))
			}
			if !strings.Contains(te.stderr.String(), tc.code) || !strings.Contains(te.stderr.String(), "profgate pgo policy get payments/checkout") {
				t.Fatalf("stderr = %q, want the code and the command to run again", te.stderr.String())
			}
			if strings.Count(te.stderr.String(), "profgate:") != 1 {
				t.Fatalf("stderr = %q, want the failure reported once", te.stderr.String())
			}
		})
	}
}

func TestPolicyDeleteNotFoundIsReportedAsIs(t *testing.T) {
	te := newTestEnv(t)
	pt := &policyTransport{read: defaultsPolicyBody, write: answering(http.StatusNotFound, `{"error":"the service has no policy override","code":"pgo_override_not_found"}`, "")}
	code := runPolicy(te, pt, "delete", "payments/checkout")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	if pt.modifying != 1 {
		t.Fatalf("%d modifying requests, want exactly one", pt.modifying)
	}
	if te.stderr.String() != "profgate: pgo_override_not_found: the service has no policy override\n" {
		t.Fatalf("stderr = %q, want the envelope as-is", te.stderr.String())
	}
}

func TestPolicyDeleteSendsIfMatchFromTheRead(t *testing.T) {
	te := newTestEnv(t)
	pt := &policyTransport{etag: `"42"`, read: overridePolicyBody, write: answering(http.StatusNoContent, "", "")}
	if code := runPolicy(te, pt, "delete", "payments/checkout"); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	del := modifyingRequest(t, pt)
	if del.Method != http.MethodDelete || del.URL.Path != "/v1/namespaces/payments/services/checkout/pgo" {
		t.Fatalf("write = %s %s, want DELETE the policy route", del.Method, del.URL.Path)
	}
	if del.Header.Get("If-Match") != `"42"` {
		t.Fatalf("If-Match = %q, want the ETag the read carried", del.Header.Get("If-Match"))
	}
	if te.stdout.String() != "" {
		t.Fatalf("stdout = %q, want nothing: a 204 has no body", te.stdout.String())
	}
}

func TestPolicySetBody(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		body  string
	}{
		{name: "enabled alone", flags: []string{"--enabled"}, body: `{"enabled":true}`},
		{name: "enabled false", flags: []string{"--enabled=false"}, body: `{"enabled":false}`},
		{name: "schedule", flags: []string{"--every", "1h", "--jitter", "5m"}, body: `{"schedule":{"every":"1h","jitter":"5m"}}`},
		{name: "the field flags of collect", flags: []string{"--duration", "30s", "--rounds", "3", "--replicas", "all", "--target-version", "1.42.3", "--retention", "4h"}, body: `{"artifact":{"retention":"4h"},"sampling":{"duration":"30s","replicas":"all","rounds":3},"target":{"version":"1.42.3"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			pt := &policyTransport{read: defaultsPolicyBody, write: answering(http.StatusCreated, overridePolicyBody, `"1"`)}
			args := append([]string{"payments/checkout"}, tc.flags...)
			if code := runPolicy(te, pt, "set", args...); code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if pt.bodies[1] != tc.body {
				t.Fatalf("body = %s, want %s", pt.bodies[1], tc.body)
			}
		})
	}
	t.Run("--file sends the file", func(t *testing.T) {
		te := newTestEnv(t)
		pt := &policyTransport{read: defaultsPolicyBody, write: answering(http.StatusCreated, overridePolicyBody, `"1"`)}
		path := filepath.Join(t.TempDir(), "override.json")
		const file = `{"enabled": true, "schedule": {"every": "1h"}}`
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := runPolicy(te, pt, "set", "payments/checkout", "--file", path); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if pt.bodies[1] != file {
			t.Fatalf("body = %s, want the file's bytes", pt.bodies[1])
		}
	})
}

func TestPolicySetUsageErrorsBeforeAnyRequest(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "--file beside a field flag", args: []string{"payments/checkout", "--file", "override.json", "--rounds", "3"}},
		{name: "--file beside --enabled", args: []string{"payments/checkout", "--file", "override.json", "--enabled"}},
		{name: "--every that is not a duration", args: []string{"payments/checkout", "--every", "soon"}},
		{name: "nothing to set", args: []string{"payments/checkout"}},
		{name: "a Collection identifier in place of a Service", args: []string{"7h2k9m4p6r8t0v1w3x5y", "--enabled"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			if code := runPolicy(te, refusingTransport(t), "set", tc.args...); code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
			}
		})
	}
}

func TestPolicyGet(t *testing.T) {
	t.Run("json prints the body byte for byte", func(t *testing.T) {
		te := newTestEnv(t)
		pt := &policyTransport{etag: `"42"`, read: overridePolicyBody}
		if code := runPolicy(te, pt, "get", "payments/checkout", "--output", "json"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if len(pt.requests) != 1 || pt.requests[0].Method != http.MethodGet || pt.requests[0].URL.Path != "/v1/namespaces/payments/services/checkout/pgo" {
			t.Fatalf("requests = %v, want one GET of the policy route", pt.requests)
		}
		if te.stdout.String() != overridePolicyBody {
			t.Fatalf("stdout = %q, want the body byte for byte", te.stdout.String())
		}
	})
	t.Run("table prints the effective policy, its source, and the violations", func(t *testing.T) {
		te := newTestEnv(t)
		pt := &policyTransport{etag: `"42"`, read: overridePolicyBody}
		if code := runPolicy(te, pt, "get", "payments/checkout"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		want := "source\toverride\nenabled\ttrue\nevery\t6h\njitter\t10m\nduration\t30s\nrounds\t3\nroundInterval\t30s\nreplicas\t2\nmaxParallel\t4\nversionPolicy\tstrict\nversion\t\nretention\t2h\nupdatedBy\talice\nupdatedAt\t2026-08-26T09:00:00Z\nviolation\t/artifact/retention: retention 2h is under the interval 6h\n"
		if te.stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
		}
	})
	t.Run("table on defaults has no update rows and no violation", func(t *testing.T) {
		te := newTestEnv(t)
		pt := &policyTransport{read: defaultsPolicyBody}
		if code := runPolicy(te, pt, "get", "payments/checkout"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		want := "source\tdefaults\nenabled\tfalse\nevery\t6h\njitter\t10m\nduration\t30s\nrounds\t1\nroundInterval\t30s\nreplicas\tall\nmaxParallel\t4\nversionPolicy\tstrict\nversion\t\nretention\t24h\n"
		if te.stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
		}
	})
}

func TestPolicySetPrintsTheWrittenPolicy(t *testing.T) {
	te := newTestEnv(t)
	pt := &policyTransport{read: defaultsPolicyBody, write: answering(http.StatusCreated, overridePolicyBody, `"1"`)}
	if code := runPolicy(te, pt, "set", "payments/checkout", "--enabled", "--output", "json"); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if te.stdout.String() != overridePolicyBody {
		t.Fatalf("stdout = %q, want the write's body byte for byte", te.stdout.String())
	}
}

func TestPolicyUnknownSubverbIsUsage(t *testing.T) {
	for _, args := range [][]string{{"pgo", "policy", "bogus", "payments/checkout"}, {"pgo", "bogus"}, {"pgo"}} {
		te := newTestEnv(t)
		code := dispatch(context.Background(), te.env, clientVerbs(), args)
		if code != 2 || !strings.Contains(te.stderr.String(), "usage: profgate pgo policy get|set|delete <ns>/<svc>") {
			t.Fatalf("dispatch(%v) = %d, stderr = %q, want 2 and the verb's grammar", args, code, te.stderr.String())
		}
	}
}
