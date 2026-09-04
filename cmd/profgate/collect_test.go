package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const acceptedBody = `{"id":"7h2k9m4p6r8t0v1w3x5y","state":"pending"}` + "\n"

// acceptedResponse is the gateway's 202 with its Location.
func acceptedResponse() *http.Response {
	resp := jsonResponse(http.StatusAccepted, acceptedBody)
	resp.Header.Set("Location", "/v1/collections/7h2k9m4p6r8t0v1w3x5y")
	return resp
}

// createTransport records every request with its body and answers each with the response the test supplies.
type createTransport struct {
	requests []*http.Request
	bodies   []string
	answer   func() (*http.Response, error)
}

func (ct *createTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ct.requests = append(ct.requests, r)
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	ct.bodies = append(ct.bodies, body)
	return ct.answer()
}

func accepting() *createTransport {
	return &createTransport{answer: func() (*http.Response, error) { return acceptedResponse(), nil }}
}

// runCollect runs the collect verb against rt and returns the exit code.
func runCollect(te *testEnv, rt http.RoundTripper, args ...string) int {
	te.env.transport = rt
	args = append([]string{"collect"}, args...)
	args = append(args, "--server", "https://g.example")
	return dispatch(context.Background(), te.env, clientVerbs(), args)
}

func TestCollectAccepted(t *testing.T) {
	t.Run("table", func(t *testing.T) {
		te := newTestEnv(t)
		code := runCollect(te, accepting(), "payments/checkout")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if te.stdout.String() != "id\t7h2k9m4p6r8t0v1w3x5y\nstate\tpending\n" {
			t.Fatalf("stdout = %q, want the identifier and the state", te.stdout.String())
		}
	})
	t.Run("json", func(t *testing.T) {
		te := newTestEnv(t)
		code := runCollect(te, accepting(), "payments/checkout", "--output", "json")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if te.stdout.String() != acceptedBody {
			t.Fatalf("stdout = %q, want the body verbatim", te.stdout.String())
		}
	})
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestCollectRequest(t *testing.T) {
	te := newTestEnv(t)
	ct := accepting()
	if code := runCollect(te, ct, "payments/checkout"); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if len(ct.requests) != 1 {
		t.Fatalf("%d requests, want one", len(ct.requests))
	}
	r := ct.requests[0]
	if r.Method != http.MethodPost || r.URL.Path != "/v1/namespaces/payments/services/checkout/collections" {
		t.Fatalf("request = %s %s, want POST the collections route", r.Method, r.URL.Path)
	}
	if r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 {
		t.Fatalf("Idempotency-Key = %q, want exactly one", keys)
	}
	if !uuidV4.MatchString(keys[0]) {
		t.Fatalf("Idempotency-Key = %q, want a UUIDv4 with the version and variant bits set", keys[0])
	}
	if ct.bodies[0] != "{}" {
		t.Fatalf("body = %q, want {} when no field flag is set", ct.bodies[0])
	}
}

func TestCollectTwoInvocationsGenerateDifferentKeys(t *testing.T) {
	var keys []string
	for range 2 {
		te := newTestEnv(t)
		ct := accepting()
		if code := runCollect(te, ct, "payments/checkout"); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		key := ct.requests[0].Header.Get("Idempotency-Key")
		if !uuidV4.MatchString(key) {
			t.Fatalf("Idempotency-Key = %q, want a UUIDv4", key)
		}
		keys = append(keys, key)
	}
	if keys[0] == keys[1] {
		t.Fatalf("two invocations sent the same key %q", keys[0])
	}
}

func TestCollectBody(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		body  string
	}{
		{name: "duration and rounds", flags: []string{"--duration", "30s", "--rounds", "3"}, body: `{"sampling":{"duration":"30s","rounds":3}}`},
		{name: "round interval and max parallel", flags: []string{"--round-interval", "1m", "--max-parallel", "2"}, body: `{"sampling":{"maxParallel":2,"roundInterval":"1m"}}`},
		{name: "replicas all", flags: []string{"--replicas", "all"}, body: `{"sampling":{"replicas":"all"}}`},
		{name: "replicas count", flags: []string{"--replicas", "3"}, body: `{"sampling":{"replicas":3}}`},
		{name: "target version", flags: []string{"--target-version", "1.42.3"}, body: `{"target":{"version":"1.42.3"}}`},
		{name: "retention", flags: []string{"--retention", "4h"}, body: `{"artifact":{"retention":"4h"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			ct := accepting()
			args := append([]string{"payments/checkout"}, tc.flags...)
			if code := runCollect(te, ct, args...); code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if ct.bodies[0] != tc.body {
				t.Fatalf("body = %s, want %s", ct.bodies[0], tc.body)
			}
		})
	}
	t.Run("--file sends the file", func(t *testing.T) {
		te := newTestEnv(t)
		ct := accepting()
		path := filepath.Join(t.TempDir(), "body.json")
		const file = `{"sampling": {"rounds": 2}, "future": {"field": true}}`
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := runCollect(te, ct, "payments/checkout", "--file", path); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if ct.bodies[0] != file {
			t.Fatalf("body = %s, want the file's bytes", ct.bodies[0])
		}
	})
}

// cutReader yields its prefix and then fails, as a body cut off after the
// answer's headers had already arrived.
type cutReader struct {
	prefix string
	read   int
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.read < len(c.prefix) {
		n := copy(p, c.prefix[c.read:])
		c.read += n
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (c *cutReader) Close() error { return nil }

// replayID is the identifier a retry's replay carries; it differs from the
// one every other answer here carries, so a poll of it proves the replay decided what --wait reads.
const replayID = "3g7hk2m9p4qr8s1tvw5x"

// retryTransport answers each create with the next answer in order, repeating the last,
// and every other request with a completed record for replayID.
type retryTransport struct {
	creates  []func() (*http.Response, error)
	requests []*http.Request
	served   int
	polls    int
}

func (rt *retryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.requests = append(rt.requests, r)
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/collections") {
		answer := rt.creates[min(rt.served, len(rt.creates)-1)]
		rt.served++
		return answer()
	}
	rt.polls++
	return jsonResponse(http.StatusOK, strings.ReplaceAll(waitRecord("completed", "", 0, 1, 1, 0), "7h2k9m4p6r8t0v1w3x5y", replayID)), nil
}

// keys is the Idempotency-Key of every create the transport saw.
func (rt *retryTransport) keys() []string {
	var got []string
	for _, r := range rt.requests {
		if r.Method == http.MethodPost {
			got = append(got, r.Header.Get("Idempotency-Key"))
		}
	}
	return got
}

// replay is the 200 a retry under a recorded key meets.
func replay() (*http.Response, error) {
	resp := jsonResponse(http.StatusOK, `{"id":"`+replayID+`","state":"running"}`)
	resp.Header.Set("Location", "/v1/collections/"+replayID)
	return resp, nil
}

func TestCollectRetriesAnUnknownResult(t *testing.T) {
	tests := []struct {
		name   string
		answer func() (*http.Response, error)
	}{
		{name: "no answer at all", answer: func() (*http.Response, error) { return nil, errors.New("connection reset by peer") }},
		{name: "500 with no envelope", answer: func() (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}},
		{name: "503 pgo_unavailable", answer: func() (*http.Response, error) {
			return jsonResponse(http.StatusServiceUnavailable, `{"error":"the store is unavailable","code":"pgo_unavailable"}`), nil
		}},
		{name: "a 202 whose body was cut off", answer: func() (*http.Response, error) {
			resp := acceptedResponse()
			resp.Body = &cutReader{prefix: `{"id":"7h2k9m4p`}
			return resp, nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			clock := onClock(te)
			start := clock.Now()
			rt := &retryTransport{creates: []func() (*http.Response, error){tc.answer, replay}}
			code := runCollect(te, rt, "payments/checkout", "--wait")
			if code != 0 {
				t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
			}
			keys := rt.keys()
			if len(keys) != 2 {
				t.Fatalf("%d creates, want the failure and the retry", len(keys))
			}
			if keys[0] == "" || keys[0] != keys[1] {
				t.Fatalf("Idempotency-Key = %q, want the one key on both creates", keys)
			}
			if rt.polls != 1 {
				t.Fatalf("%d polls, want the one the replay's identifier earned", rt.polls)
			}
			if got := rt.requests[len(rt.requests)-1].URL.Path; got != "/v1/collections/"+replayID {
				t.Fatalf("the poll read %s, want the replay's record", got)
			}
			if got := clock.Now().Sub(start); got != time.Second {
				t.Fatalf("the clock advanced %s, want the one second before the retry", got)
			}
		})
	}
}

func TestCollectStopsWhenTheRetryWindowCloses(t *testing.T) {
	te := newTestEnv(t)
	clock := onClock(te)
	start := clock.Now()
	rt := &retryTransport{creates: []func() (*http.Response, error){
		func() (*http.Response, error) { return nil, errors.New("connection reset by peer") },
	}}
	code := runCollect(te, rt, "payments/checkout", "--wait")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	if got := clock.Now().Sub(start); got != 30*time.Second {
		t.Fatalf("the clock advanced %s, want the thirty-second window", got)
	}
	if len(rt.keys()) != 7 {
		t.Fatalf("%d creates, want the first and one per wait of the window", len(rt.keys()))
	}
	if rt.polls != 0 {
		t.Fatalf("%d polls, want none: no answer ever carried an identifier", rt.polls)
	}
	if !strings.Contains(te.stderr.String(), "profgate collections payments/checkout") {
		t.Fatalf("stderr = %q, want profgate collections payments/checkout named", te.stderr.String())
	}
	if strings.Count(te.stderr.String(), "profgate:") != 1 {
		t.Fatalf("stderr = %q, want the failure reported once", te.stderr.String())
	}
}

func TestCollectReplay(t *testing.T) {
	te := newTestEnv(t)
	const replay = `{"id":"7h2k9m4p6r8t0v1w3x5y","state":"running"}`
	ct := &createTransport{answer: func() (*http.Response, error) {
		resp := jsonResponse(http.StatusOK, replay)
		resp.Header.Set("Location", "/v1/collections/7h2k9m4p6r8t0v1w3x5y")
		return resp, nil
	}}
	code := runCollect(te, ct, "payments/checkout", "--output", "json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(te.stdout.Bytes(), &fields); err != nil {
		t.Fatalf("stdout = %q: %v", te.stdout.String(), err)
	}
	if string(fields["id"]) != `"7h2k9m4p6r8t0v1w3x5y"` || string(fields["state"]) != `"running"` {
		t.Fatalf("stdout = %q, want the replay's identifier and state", te.stdout.String())
	}
	delete(fields, "id")
	delete(fields, "state")
	if len(fields) != 0 {
		t.Fatalf("the replay carries record fields: %v", fields)
	}
}

func TestCollectGatewayRefusals(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "409 idempotency_mismatch", status: http.StatusConflict, body: `{"error":"the key names a different request","code":"idempotency_mismatch"}`, want: "idempotency_mismatch: the key names a different request"},
		{name: "429 collection_in_progress", status: http.StatusTooManyRequests, body: `{"error":"the service already has a live collection; GET /v1/namespaces/{namespace}/services/{service}/collections lists it","code":"collection_in_progress"}`, want: "collection_in_progress: the service already has a live collection; GET /v1/namespaces/{namespace}/services/{service}/collections lists it"},
		{name: "400 invalid_parameter", status: http.StatusBadRequest, body: `{"error":"a collection request sets neither enabled nor schedule","code":"invalid_parameter"}`, want: "invalid_parameter: a collection request sets neither enabled nor schedule"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			clock := onClock(te)
			start := clock.Now()
			ct := &createTransport{answer: func() (*http.Response, error) { return jsonResponse(tc.status, tc.body), nil }}
			code := runCollect(te, ct, "payments/checkout", "--wait")
			if code != 1 {
				t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
			}
			if len(ct.requests) != 1 {
				t.Fatalf("%d requests, want exactly one: no retry and no poll", len(ct.requests))
			}
			if !clock.Now().Equal(start) {
				t.Fatalf("the clock advanced %s, want nothing: an answer that arrived whole is not waited on", clock.Now().Sub(start))
			}
			if !strings.Contains(te.stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want the envelope %q", te.stderr.String(), tc.want)
			}
			if te.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", te.stdout.String())
			}
		})
	}
}

func TestCollectLocalRefusals(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "body.json")
	if err := os.WriteFile(jsonPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	notJSON := filepath.Join(dir, "body.yaml")
	if err := os.WriteFile(notJSON, []byte("sampling:\n  rounds: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		args       []string
		wantStderr []string
	}{
		{name: "--file beside a field flag", args: []string{"--file", jsonPath, "--rounds", "2"}, wantStderr: []string{"--file"}},
		{name: "--file that is not JSON", args: []string{"--file", notJSON}, wantStderr: []string{"body.yaml", "JSON"}},
		{name: "--file that does not exist", args: []string{"--file", filepath.Join(dir, "missing.json")}, wantStderr: []string{"missing.json"}},
		{name: "--duration not a duration", args: []string{"--duration", "thirty"}, wantStderr: []string{"--duration", "thirty"}},
		{name: "--duration zero", args: []string{"--duration", "0s"}, wantStderr: []string{"--duration", "0s"}},
		{name: "--rounds zero", args: []string{"--rounds", "0"}, wantStderr: []string{"--rounds", "0"}},
		{name: "--rounds abc", args: []string{"--rounds", "abc"}, wantStderr: []string{"--rounds", "abc"}},
		{name: "--round-interval negative", args: []string{"--round-interval", "-1s"}, wantStderr: []string{"--round-interval"}},
		{name: "--replicas neither all nor a count", args: []string{"--replicas", "some"}, wantStderr: []string{"--replicas", "some"}},
		{name: "--replicas zero", args: []string{"--replicas", "0"}, wantStderr: []string{"--replicas", "0"}},
		{name: "--max-parallel zero", args: []string{"--max-parallel", "0"}, wantStderr: []string{"--max-parallel", "0"}},
		{name: "--retention not a duration", args: []string{"--retention", "4 hours"}, wantStderr: []string{"--retention", "4 hours"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			args := append([]string{"payments/checkout"}, tc.args...)
			code := runCollect(te, refusingTransport(t), args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(te.stderr.String(), want) {
					t.Fatalf("stderr = %q, want %q in it", te.stderr.String(), want)
				}
			}
		})
	}
}

// TestCollectBodyFlagIsGone asserts --body registers no alias for --file:
// the removed spelling is refused as an unknown flag, before any request.
func TestCollectBodyFlagIsGone(t *testing.T) {
	te := newTestEnv(t)
	code := runCollect(te, refusingTransport(t), "payments/checkout", "--body", "/dev/null")
	if code != 2 {
		t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
	}
	if !strings.Contains(te.stderr.String(), "flag provided but not defined: -body") {
		t.Fatalf("stderr = %q, want flag provided but not defined: -body", te.stderr.String())
	}
}

// TestCollectFileTakesAHelpSpelling asserts the attached form reaches --file
// as a path rather than as help: the name before the = is file,
// so the flag receives the literal value --help and the refusal names that file.
func TestCollectFileTakesAHelpSpelling(t *testing.T) {
	te := newTestEnv(t)
	code := runCollect(te, refusingTransport(t), "payments/checkout", "--file=--help")
	if code != 2 {
		t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
	}
	if !strings.Contains(te.stderr.String(), "--file") || !strings.Contains(te.stderr.String(), "--help") {
		t.Fatalf("stderr = %q, want --file naming the file --help", te.stderr.String())
	}
	if te.stdout.String() != "" {
		t.Fatalf("stdout = %q, want nothing: this is not help", te.stdout.String())
	}
}

func TestCollectCancelledBeforeAnyResponse(t *testing.T) {
	te := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ct := &createTransport{answer: func() (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	}}
	te.env.transport = ct
	code := dispatch(ctx, te.env, clientVerbs(), []string{"collect", "payments/checkout", "--wait", "--server", "https://g.example"})
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	if len(ct.requests) != 1 {
		t.Fatalf("%d requests, want the one create and no poll", len(ct.requests))
	}
	if !strings.Contains(te.stderr.String(), "may already have been created") || !strings.Contains(te.stderr.String(), "profgate collections payments/checkout") {
		t.Fatalf("stderr = %q, want a Collection may already have been created and profgate collections payments/checkout", te.stderr.String())
	}
}

func TestCollectWaitIssuesNoGetWithoutAnIdentifier(t *testing.T) {
	te := newTestEnv(t)
	ct := &createTransport{answer: func() (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"error":"a collection request sets neither enabled nor schedule","code":"invalid_parameter"}`), nil
	}}
	code := runCollect(te, ct, "payments/checkout", "--wait")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	for _, r := range ct.requests {
		if r.Method == http.MethodGet {
			t.Fatalf("a GET %s was issued with no identifier to poll", r.URL.Path)
		}
	}
	if len(ct.requests) != 1 {
		t.Fatalf("%d requests, want the one create", len(ct.requests))
	}
}

const collectionsBody = `{"namespace":"payments","service":"checkout","collections":[{"id":"3g7hk2m9p4qr8s1tvw5x","origin":"api","state":"completed","attempt":1,"resolvedVersion":"1.42.0","createdAt":"2026-08-26T09:00:00Z","finishedAt":"2026-08-26T09:04:12Z","expiresAt":"2026-08-26T11:04:12Z"},{"id":"7h2k9m4p6r8t0v1w3x5y","origin":"schedule","state":"running","attempt":1,"createdAt":"2026-08-26T10:00:00Z"}]}` + "\n"

const emptyCollectionsBody = `{"namespace":"payments","service":"checkout","collections":[]}` + "\n"

const recordBody = `{"id":"7h2k9m4p6r8t0v1w3x5y","namespace":"payments","service":"checkout","origin":"schedule","state":"failed","attempt":1,"reason":"not_claimed","resolvedVersion":"1.42.3","progress":{"round":1,"rounds":2,"samplesOK":5,"samplesFailed":1},"manifest":null,"artifact":null,"createdAt":"2026-08-23T12:03:12Z","startedAt":null,"finishedAt":"2026-08-23T13:03:12Z","expiresAt":null}` + "\n"

const pendingRecordBody = `{"id":"7h2k9m4p6r8t0v1w3x5y","namespace":"payments","service":"checkout","origin":"api","state":"pending","attempt":0,"reason":"","resolvedVersion":"","progress":{"round":0,"rounds":0,"samplesOK":0,"samplesFailed":0},"manifest":null,"artifact":null,"createdAt":"2026-08-23T12:03:12Z","startedAt":null,"finishedAt":null,"expiresAt":null}` + "\n"

const completedRecordBody = `{"id":"7h2k9m4p6r8t0v1w3x5y","namespace":"payments","service":"checkout","origin":"api","state":"completed","attempt":1,"reason":"","resolvedVersion":"1.42.3","progress":{"round":2,"rounds":3,"samplesOK":9,"samplesFailed":0},"manifest":null,"artifact":{"object":"collection/7h2k9m4p6r8t0v1w3x5y/1.pprof","bytes":184320},"createdAt":"2026-08-23T12:03:12Z","startedAt":"2026-08-23T12:03:20Z","finishedAt":"2026-08-23T12:09:44Z","expiresAt":"2026-08-30T12:09:44Z"}` + "\n"

func TestCollectionsOneGET(t *testing.T) {
	te := newTestEnv(t)
	code, rt := runRead(t, te, collectionsBody, "collections", "payments/checkout")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if len(rt.requests) != 1 {
		t.Fatalf("%d requests, want exactly one", len(rt.requests))
	}
	r := rt.requests[0]
	if r.Method != http.MethodGet || r.URL.Path != "/v1/namespaces/payments/services/checkout/collections" {
		t.Fatalf("request = %s %s, want GET the collections route", r.Method, r.URL.Path)
	}
	if r.URL.RawQuery != "" {
		t.Fatalf("query = %q, want no filter and no cursor", r.URL.RawQuery)
	}
}

func TestCollectionsTable(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		terminal bool
		want     string
	}{
		{
			name: "one row per record",
			body: collectionsBody,
			want: "ID\tSTATE\tORIGIN\tCREATED\n3g7hk2m9p4qr8s1tvw5x\tcompleted\tapi\t2026-08-26T09:00:00Z\n7h2k9m4p6r8t0v1w3x5y\trunning\tschedule\t2026-08-26T10:00:00Z\n",
		},
		{
			name:     "on a terminal pads",
			body:     collectionsBody,
			terminal: true,
			want:     "ID                    STATE      ORIGIN    CREATED\n3g7hk2m9p4qr8s1tvw5x  completed  api       2026-08-26T09:00:00Z\n7h2k9m4p6r8t0v1w3x5y  running    schedule  2026-08-26T10:00:00Z\n",
		},
		{
			name: "an empty listing prints the header alone",
			body: emptyCollectionsBody,
			want: "ID\tSTATE\tORIGIN\tCREATED\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			te.env.terminal = tc.terminal
			code, _ := runRead(t, te, tc.body, "collections", "payments/checkout")
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if te.stdout.String() != tc.want {
				t.Fatalf("stdout = %q, want %q", te.stdout.String(), tc.want)
			}
		})
	}
}

func TestCollectionsJSON(t *testing.T) {
	te := newTestEnv(t)
	code, _ := runRead(t, te, collectionsBody, "collections", "payments/checkout", "--output", "json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if te.stdout.String() != collectionsBody {
		t.Fatalf("stdout = %q, want the body byte for byte", te.stdout.String())
	}
}

func TestCollectionGetOneGET(t *testing.T) {
	te := newTestEnv(t)
	code, rt := runRead(t, te, recordBody, "collection", "get", "7h2k9m4p6r8t0v1w3x5y")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if len(rt.requests) != 1 {
		t.Fatalf("%d requests, want exactly one", len(rt.requests))
	}
	r := rt.requests[0]
	if r.Method != http.MethodGet || r.URL.Path != "/v1/collections/7h2k9m4p6r8t0v1w3x5y" {
		t.Fatalf("request = %s %s, want GET the record route", r.Method, r.URL.Path)
	}
	if r.URL.RawQuery != "" {
		t.Fatalf("query = %q, want none", r.URL.RawQuery)
	}
}

// A failed record carries two of the four end-of-life fields and not the other two,
// so the table prints the two it has and skips the two it does not.
func TestCollectionGetTable(t *testing.T) {
	te := newTestEnv(t)
	code, _ := runRead(t, te, recordBody, "collection", "get", "7h2k9m4p6r8t0v1w3x5y")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	want := "id\t7h2k9m4p6r8t0v1w3x5y\nstate\tfailed\norigin\tschedule\n" +
		"progress\tround 2 of 2, 5 ok, 1 failed\nresolvedVersion\t1.42.3\nfinishedAt\t2026-08-23T13:03:12Z\nreason\tnot_claimed\n"
	if te.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
	}
}

// The progress row counts the round a person counts,
// so the first round of three reads one and the last reads three.
func TestRenderCollectionRound(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "the first round of three",
			body: waitRecord("running", "", 0, 3, 1, 0),
			want: "id\t7h2k9m4p6r8t0v1w3x5y\nstate\trunning\norigin\tapi\nprogress\tround 1 of 3, 1 ok, 0 failed\n",
		},
		{
			name: "the last round of three",
			body: waitRecord("running", "", 2, 3, 5, 0),
			want: "id\t7h2k9m4p6r8t0v1w3x5y\nstate\trunning\norigin\tapi\nprogress\tround 3 of 3, 5 ok, 0 failed\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			code, _ := runRead(t, te, tc.body, "collection", "get", "7h2k9m4p6r8t0v1w3x5y")
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if te.stdout.String() != tc.want {
				t.Fatalf("stdout = %q, want %q", te.stdout.String(), tc.want)
			}
		})
	}
}

// A completed record prints the version it profiled, when it finished,
// when the artifact expires, and how large that artifact is.
func TestRenderCollectionCompleted(t *testing.T) {
	te := newTestEnv(t)
	code, _ := runRead(t, te, completedRecordBody, "collection", "get", "7h2k9m4p6r8t0v1w3x5y")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	want := "id\t7h2k9m4p6r8t0v1w3x5y\nstate\tcompleted\norigin\tapi\nprogress\tround 3 of 3, 9 ok, 0 failed\n" +
		"resolvedVersion\t1.42.3\nfinishedAt\t2026-08-23T12:09:44Z\nexpiresAt\t2026-08-30T12:09:44Z\nartifactBytes\t184320\n"
	if te.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
	}
}

// A pending record carries null for the artifact and the timestamps, and an empty version,
// which decodes into the one record shape and prints none of the four rows.
func TestRenderCollectionPending(t *testing.T) {
	te := newTestEnv(t)
	code, _ := runRead(t, te, pendingRecordBody, "collection", "get", "7h2k9m4p6r8t0v1w3x5y")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	want := "id\t7h2k9m4p6r8t0v1w3x5y\nstate\tpending\norigin\tapi\n"
	if te.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
	}
}

// The rendering counts from one and the document does not:
// --output json copies the gateway's bytes, so the wire round is still the index the gateway stored.
func TestCollectionJSONIsUntouched(t *testing.T) {
	te := newTestEnv(t)
	code, _ := runRead(t, te, completedRecordBody, "collection", "get", "7h2k9m4p6r8t0v1w3x5y", "--output", "json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if te.stdout.String() != completedRecordBody {
		t.Fatalf("stdout = %q, want the body byte for byte", te.stdout.String())
	}
	if !strings.Contains(te.stdout.String(), `"round":2`) {
		t.Fatalf("stdout = %q, want the stored index 2 that the table prints as round 3", te.stdout.String())
	}
}

func TestCollectionGetJSON(t *testing.T) {
	te := newTestEnv(t)
	code, _ := runRead(t, te, recordBody, "collection", "get", "7h2k9m4p6r8t0v1w3x5y", "--output", "json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if te.stdout.String() != recordBody {
		t.Fatalf("stdout = %q, want the body byte for byte", te.stdout.String())
	}
}

// The plural takes a Service and the singular takes an identifier;
// each refuses the other's argument before any request reaches the transport.
func TestCollectionGrammarRefusals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "collections with an identifier", args: []string{"collections", "7h2k9m4p6r8t0v1w3x5y"}, want: "7h2k9m4p6r8t0v1w3x5y"},
		{name: "collection get with a Service", args: []string{"collection", "get", "payments/checkout"}, want: "payments/checkout"},
		{name: "collection get with a short identifier", args: []string{"collection", "get", "7h2k9m4p6r8t0v1w3x5"}, want: "7h2k9m4p6r8t0v1w3x5"},
		{name: "collection get with an uppercase identifier", args: []string{"collection", "get", "7H2K9M4P6R8T0V1W3X5Y"}, want: "7H2K9M4P6R8T0V1W3X5Y"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			args := append([]string{}, tc.args...)
			args = append(args, "--server", "https://g.example")
			code := dispatch(context.Background(), te.env, clientVerbs(), args)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
			}
			if !strings.Contains(te.stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want %q named", te.stderr.String(), tc.want)
			}
		})
	}
	t.Run("collection without a subverb", func(t *testing.T) {
		te := newTestEnv(t)
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"collection", "7h2k9m4p6r8t0v1w3x5y"})
		if code != 2 {
			t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
		}
	})
}

func TestCollectionEnvelopes(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "404 collection_not_found", status: http.StatusNotFound, body: `{"error":"no collection 7h2k9m4p6r8t0v1w3x5y","code":"collection_not_found"}`, want: "collection_not_found: no collection 7h2k9m4p6r8t0v1w3x5y"},
		{name: "403 realm_denied", status: http.StatusForbidden, body: `{"error":"the realm does not admit payments/checkout","code":"realm_denied"}`, want: "realm_denied: the realm does not admit payments/checkout"},
		{name: "503 pgo_unavailable", status: http.StatusServiceUnavailable, body: `{"error":"the store is unavailable","code":"pgo_unavailable"}`, want: "pgo_unavailable: the store is unavailable"},
	}
	verbs := [][]string{
		{"collections", "payments/checkout"},
		{"collection", "get", "7h2k9m4p6r8t0v1w3x5y"},
	}
	for _, tc := range tests {
		for _, args := range verbs {
			t.Run(tc.name+" under "+args[0], func(t *testing.T) {
				te := newTestEnv(t)
				rt := &recordingTransport{body: tc.body, status: tc.status}
				te.env.transport = rt
				full := append(append([]string{}, args...), "--server", "https://g.example")
				code := dispatch(context.Background(), te.env, clientVerbs(), full)
				if code != 1 {
					t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
				}
				if len(rt.requests) != 1 {
					t.Fatalf("%d requests, want exactly one: a one-shot read has no wait to resume", len(rt.requests))
				}
				if !strings.Contains(te.stderr.String(), tc.want) {
					t.Fatalf("stderr = %q, want the envelope %q", te.stderr.String(), tc.want)
				}
				if te.stdout.Len() != 0 {
					t.Fatalf("stdout = %q, want nothing", te.stdout.String())
				}
			})
		}
	}
}

// pollClock is the clock and sleeper a wait runs on: a sleep advances the
// clock and nothing waits.
type pollClock struct {
	now time.Time
}

func (c *pollClock) Now() time.Time { return c.now }

func (c *pollClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}

// onClock puts te on a pollClock and returns it.
func onClock(te *testEnv) *pollClock {
	clock := &pollClock{now: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)}
	te.env.now = clock.Now
	te.env.sleep = clock.Sleep
	return clock
}

// waitRecord is one record body with the fields the wait reads.
func waitRecord(state, reason string, round, rounds, ok, failed int) string {
	return `{"id":"7h2k9m4p6r8t0v1w3x5y","namespace":"payments","service":"checkout","origin":"api","state":"` + state + `","reason":"` + reason + `","progress":{"round":` + strconv.Itoa(round) + `,"rounds":` + strconv.Itoa(rounds) + `,"samplesOK":` + strconv.Itoa(ok) + `,"samplesFailed":` + strconv.Itoa(failed) + `}}` + "\n"
}

// pollAnswer is one answer to a poll or a cancel: its status and body.
type pollAnswer struct {
	status int
	body   string
}

// pollTransport answers the create with a 202 and every other request with
// the next answer, repeating the last, recording each request it saw.
type pollTransport struct {
	answers  []pollAnswer
	requests []*http.Request
	bodies   []string
	polls    int
}

func (pt *pollTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	pt.requests = append(pt.requests, r)
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	pt.bodies = append(pt.bodies, body)
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/collections") {
		return acceptedResponse(), nil
	}
	a := pt.answers[min(pt.polls, len(pt.answers)-1)]
	pt.polls++
	return jsonResponse(a.status, a.body), nil
}

func polling(bodies ...string) *pollTransport {
	pt := &pollTransport{}
	for _, b := range bodies {
		pt.answers = append(pt.answers, pollAnswer{status: http.StatusOK, body: b})
	}
	return pt
}

// waitReceipt is the two lines collect --wait prints to stderr before it polls,
// in both output modes: the identifier and state the create returned.
const waitReceipt = "id: 7h2k9m4p6r8t0v1w3x5y\nstate: pending\n"

func TestCollectWaitCompleted(t *testing.T) {
	te := newTestEnv(t)
	clock := onClock(te)
	start := clock.Now()
	pt := polling(waitRecord("pending", "", 0, 0, 0, 0), waitRecord("running", "", 0, 2, 3, 0), waitRecord("completed", "", 1, 2, 6, 0))
	code := runCollect(te, pt, "payments/checkout", "--wait", "--poll-interval", "5s")
	if code != 0 {
		t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
	}
	if pt.polls != 3 {
		t.Fatalf("%d polls, want three", pt.polls)
	}
	for _, r := range pt.requests[1:] {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/collections/7h2k9m4p6r8t0v1w3x5y" {
			t.Fatalf("poll = %s %s, want GET the record route", r.Method, r.URL.Path)
		}
	}
	if got := clock.Now().Sub(start); got != 10*time.Second {
		t.Fatalf("the clock advanced %s, want two intervals of 5s", got)
	}
	if !strings.HasPrefix(te.stderr.String(), waitReceipt) {
		t.Fatalf("stderr = %q, want the receipt %q first", te.stderr.String(), waitReceipt)
	}
	want := "id\t7h2k9m4p6r8t0v1w3x5y\nstate\tcompleted\norigin\tapi\nprogress\tround 2 of 2, 6 ok, 0 failed\n"
	if te.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
	}
}

// TestCollectWaitReceiptStream asserts collect --wait sends its id/state receipt to stderr,
// in both output modes,
// and stdout carries the final record alone.
func TestCollectWaitReceiptStream(t *testing.T) {
	t.Run("table", func(t *testing.T) {
		te := newTestEnv(t)
		onClock(te)
		pt := polling(waitRecord("completed", "", 0, 0, 0, 0))
		code := runCollect(te, pt, "payments/checkout", "--wait")
		if code != 0 {
			t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
		}
		if te.stderr.String() != waitReceipt {
			t.Fatalf("stderr = %q, want the receipt alone", te.stderr.String())
		}
		want := "id\t7h2k9m4p6r8t0v1w3x5y\nstate\tcompleted\norigin\tapi\n"
		if te.stdout.String() != want {
			t.Fatalf("stdout = %q, want the final record alone with no id row before it", te.stdout.String())
		}
	})
	t.Run("json", func(t *testing.T) {
		te := newTestEnv(t)
		onClock(te)
		final := waitRecord("completed", "", 0, 0, 0, 0)
		code := runCollect(te, polling(final), "payments/checkout", "--wait", "--output", "json")
		if code != 0 {
			t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
		}
		if te.stderr.String() != waitReceipt {
			t.Fatalf("stderr = %q, want the receipt alone", te.stderr.String())
		}
		if te.stdout.String() != final {
			t.Fatalf("stdout = %q, want exactly the final record's bytes", te.stdout.String())
		}
	})
}

// TestCollectWithoutWaitIsUnchanged asserts collect with no --wait still writes its receipt to stdout,
// as a table in table mode and as the response's own bytes under --output json:
// only the --wait path moves.
func TestCollectWithoutWaitIsUnchanged(t *testing.T) {
	t.Run("table", func(t *testing.T) {
		te := newTestEnv(t)
		code := runCollect(te, accepting(), "payments/checkout")
		if code != 0 {
			t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
		}
		if te.stdout.String() != "id\t7h2k9m4p6r8t0v1w3x5y\nstate\tpending\n" {
			t.Fatalf("stdout = %q, want the receipt as a table", te.stdout.String())
		}
		if te.stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty: the receipt is the document collect produced", te.stderr.String())
		}
	})
	t.Run("json", func(t *testing.T) {
		te := newTestEnv(t)
		code := runCollect(te, accepting(), "payments/checkout", "--output", "json")
		if code != 0 {
			t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
		}
		if te.stdout.String() != acceptedBody {
			t.Fatalf("stdout = %q, want the response bytes verbatim", te.stdout.String())
		}
		if te.stderr.String() != "" {
			t.Fatalf("stderr = %q, want empty", te.stderr.String())
		}
	})
}

func TestCollectWaitDefaultsToTwoSeconds(t *testing.T) {
	te := newTestEnv(t)
	clock := onClock(te)
	start := clock.Now()
	code := runCollect(te, polling(waitRecord("running", "", 0, 1, 0, 0), waitRecord("completed", "", 0, 1, 1, 0)), "payments/checkout", "--wait")
	if code != 0 {
		t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
	}
	if got := clock.Now().Sub(start); got != 2*time.Second {
		t.Fatalf("the clock advanced %s, want the default interval of 2s", got)
	}
}

func TestCollectWaitTerminalFailures(t *testing.T) {
	tests := []struct {
		name       string
		record     string
		wantStderr []string
		notStderr  []string
	}{
		{name: "failed prints the reason", record: waitRecord("failed", "not_claimed", 1, 2, 5, 1), wantStderr: []string{"7h2k9m4p6r8t0v1w3x5y", "failed", "not_claimed"}},
		{name: "cancelled prints the reason", record: waitRecord("cancelled", "cancelled_by_api", 1, 2, 5, 1), wantStderr: []string{"7h2k9m4p6r8t0v1w3x5y", "cancelled", "cancelled_by_api"}},
		{name: "expired prints the fixed retention message and no reason", record: waitRecord("expired", "unread_reason", 0, 0, 0, 0), wantStderr: []string{"7h2k9m4p6r8t0v1w3x5y", "expired", "retention"}, notStderr: []string{"unread_reason"}},
		{name: "a state outside the seven is named", record: waitRecord("paused", "", 0, 0, 0, 0), wantStderr: []string{"7h2k9m4p6r8t0v1w3x5y", `"paused"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			onClock(te)
			pt := polling(tc.record)
			code := runCollect(te, pt, "payments/checkout", "--wait")
			if code != 1 {
				t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
			}
			if pt.polls != 1 {
				t.Fatalf("%d polls, want one: the wait ended on the first terminal read", pt.polls)
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(te.stderr.String(), want) {
					t.Fatalf("stderr = %q, want %q in it", te.stderr.String(), want)
				}
			}
			for _, not := range tc.notStderr {
				if strings.Contains(te.stderr.String(), not) {
					t.Fatalf("stderr = %q, want %q absent", te.stderr.String(), not)
				}
			}
		})
	}
}

func TestCollectWaitMidPollRefusals(t *testing.T) {
	t.Run("503 pgo_unavailable is retried", func(t *testing.T) {
		te := newTestEnv(t)
		clock := onClock(te)
		start := clock.Now()
		pt := &pollTransport{answers: []pollAnswer{
			{status: http.StatusServiceUnavailable, body: `{"error":"the store is unavailable","code":"pgo_unavailable"}`},
			{status: http.StatusOK, body: waitRecord("completed", "", 0, 1, 1, 0)},
		}}
		code := runCollect(te, pt, "payments/checkout", "--wait", "--poll-interval", "3s")
		if code != 0 {
			t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
		}
		if pt.polls != 2 || clock.Now().Sub(start) != 3*time.Second {
			t.Fatalf("%d polls and %s slept, want the outage polled again one interval later", pt.polls, clock.Now().Sub(start))
		}
	})
	t.Run("404 collection_not_found ends the wait", func(t *testing.T) {
		te := newTestEnv(t)
		onClock(te)
		pt := &pollTransport{answers: []pollAnswer{{status: http.StatusNotFound, body: `{"error":"no collection 7h2k9m4p6r8t0v1w3x5y","code":"collection_not_found"}`}}}
		code := runCollect(te, pt, "payments/checkout", "--wait")
		if code != 1 {
			t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
		}
		if pt.polls != 1 {
			t.Fatalf("%d polls, want one: a missing record is not polled again", pt.polls)
		}
		if !strings.Contains(te.stderr.String(), "collection_not_found") {
			t.Fatalf("stderr = %q, want the envelope", te.stderr.String())
		}
	})
	t.Run("403 realm_denied on the record route after the 202", func(t *testing.T) {
		te := newTestEnv(t)
		onClock(te)
		pt := &pollTransport{answers: []pollAnswer{{status: http.StatusForbidden, body: `{"error":"the realm does not admit pgo.read","code":"realm_denied"}`}}}
		code := runCollect(te, pt, "payments/checkout", "--wait")
		if code != 1 {
			t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
		}
		if te.stdout.String() != "" {
			t.Fatalf("stdout = %q, want empty: no final record was ever read", te.stdout.String())
		}
		if !strings.Contains(te.stderr.String(), waitReceipt) || !strings.Contains(te.stderr.String(), "realm_denied: the realm does not admit pgo.read") {
			t.Fatalf("stderr = %q, want the receipt and the denial", te.stderr.String())
		}
	})
}

func TestCollectWaitTimeout(t *testing.T) {
	te := newTestEnv(t)
	clock := onClock(te)
	start := clock.Now()
	pt := polling(waitRecord("running", "", 1, 3, 2, 0))
	code := runCollect(te, pt, "payments/checkout", "--wait", "--poll-interval", "10s", "--wait-timeout", "1m")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	if got := clock.Now().Sub(start); got != time.Minute {
		t.Fatalf("the clock advanced %s, want exactly the timeout", got)
	}
	if !strings.Contains(te.stderr.String(), "7h2k9m4p6r8t0v1w3x5y") {
		t.Fatalf("stderr = %q, want the identifier named", te.stderr.String())
	}
}

func TestCollectWaitInterrupted(t *testing.T) {
	te := newTestEnv(t)
	clock := onClock(te)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	te.env.sleep = func(_ context.Context, d time.Duration) error {
		cancel()
		_ = clock.Sleep(ctx, d)
		return ctx.Err()
	}
	pt := polling(waitRecord("running", "", 1, 3, 2, 0))
	te.env.transport = pt
	code := dispatch(ctx, te.env, clientVerbs(), []string{"collect", "payments/checkout", "--wait", "--server", "https://g.example"})
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	for _, r := range pt.requests {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			t.Fatal("a cancel was sent: an interrupt stops the watching, not the collecting")
		}
	}
	if !strings.Contains(te.stderr.String(), "7h2k9m4p6r8t0v1w3x5y") || !strings.Contains(te.stderr.String(), "profgate collection get 7h2k9m4p6r8t0v1w3x5y") {
		t.Fatalf("stderr = %q, want the identifier and the command that resumes watching", te.stderr.String())
	}
}

func TestCollectWaitProgress(t *testing.T) {
	te := newTestEnv(t)
	onClock(te)
	final := waitRecord("completed", "", 1, 2, 6, 1)
	pt := polling(waitRecord("pending", "", 0, 0, 0, 0), waitRecord("running", "", 0, 2, 3, 0), waitRecord("running", "", 0, 2, 3, 0), final)
	code := runCollect(te, pt, "payments/checkout", "--wait", "--output", "json")
	if code != 0 {
		t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
	}
	if te.stderr.String() != waitReceipt+"round 1 of 2, 3 ok, 0 failed\nround 2 of 2, 6 ok, 1 failed\n" {
		t.Fatalf("stderr = %q, want the receipt then one progress line per change", te.stderr.String())
	}
	if te.stdout.String() != final {
		t.Fatalf("stdout = %q, want the final record and nothing else under --output json", te.stdout.String())
	}
}

func TestCollectWaitRanges(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "--poll-interval below 1s", args: []string{"--poll-interval", "500ms"}},
		{name: "--poll-interval above 1m", args: []string{"--poll-interval", "2m"}},
		{name: "--wait-timeout below 1m", args: []string{"--wait-timeout", "30s"}},
		{name: "--wait-timeout above 24h", args: []string{"--wait-timeout", "25h"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			args := append([]string{"payments/checkout", "--wait"}, tc.args...)
			code := runCollect(te, refusingTransport(t), args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
			}
			if !strings.Contains(te.stderr.String(), tc.args[0]) || !strings.Contains(te.stderr.String(), tc.args[1]) {
				t.Fatalf("stderr = %q, want %s %s named", te.stderr.String(), tc.args[0], tc.args[1])
			}
		})
	}
}

// runCancel runs collection cancel against rt on a pollClock.
func runCancel(te *testEnv, rt http.RoundTripper) (int, *pollClock) {
	clock := onClock(te)
	te.env.transport = rt
	return dispatch(context.Background(), te.env, clientVerbs(), []string{"collection", "cancel", "7h2k9m4p6r8t0v1w3x5y", "--server", "https://g.example"}), clock
}

func TestCollectionCancel(t *testing.T) {
	const initializing = `{"error":"the collection is not yet claimable","code":"collection_initializing"}`
	cancelled := waitRecord("cancelled", "cancelled_by_api", 1, 3, 2, 0)
	t.Run("the request carries the JSON media type and no body", func(t *testing.T) {
		te := newTestEnv(t)
		pt := polling(cancelled)
		code, _ := runCancel(te, pt)
		if code != 0 {
			t.Fatalf("code = %d, want 0 (stderr=%q)", code, te.stderr.String())
		}
		if len(pt.requests) != 1 {
			t.Fatalf("%d requests, want one", len(pt.requests))
		}
		r := pt.requests[0]
		if r.Method != http.MethodPost || r.URL.Path != "/v1/collections/7h2k9m4p6r8t0v1w3x5y/cancel" {
			t.Fatalf("request = %s %s, want POST the cancel route", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if pt.bodies[0] != "" {
			t.Fatalf("body = %q, want none", pt.bodies[0])
		}
		want := "id\t7h2k9m4p6r8t0v1w3x5y\nstate\tcancelled\norigin\tapi\nprogress\tround 2 of 3, 2 ok, 0 failed\nreason\tcancelled_by_api\n"
		if te.stdout.String() != want {
			t.Fatalf("stdout = %q, want the cancelled record %q", te.stdout.String(), want)
		}
	})
	t.Run("json prints the record verbatim", func(t *testing.T) {
		te := newTestEnv(t)
		onClock(te)
		te.env.transport = polling(cancelled)
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"collection", "cancel", "7h2k9m4p6r8t0v1w3x5y", "--output", "json", "--server", "https://g.example"})
		if code != 0 || te.stdout.String() != cancelled {
			t.Fatalf("code = %d, stdout = %q, want 0 and the body verbatim", code, te.stdout.String())
		}
	})
	t.Run("409 collection_initializing is retried once a second for ten seconds", func(t *testing.T) {
		te := newTestEnv(t)
		pt := &pollTransport{answers: []pollAnswer{{status: http.StatusConflict, body: initializing}}}
		code, clock := runCancel(te, pt)
		if code != 1 {
			t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
		}
		if len(pt.requests) != 11 {
			t.Fatalf("%d requests, want the first and ten retries", len(pt.requests))
		}
		if got := clock.Now().Sub(time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)); got != 10*time.Second {
			t.Fatalf("the clock advanced %s, want ten seconds", got)
		}
		if !strings.Contains(te.stderr.String(), "collection_initializing: the collection is not yet claimable") {
			t.Fatalf("stderr = %q, want the last envelope", te.stderr.String())
		}
	})
	t.Run("409 collection_terminal is never retried", func(t *testing.T) {
		te := newTestEnv(t)
		pt := &pollTransport{answers: []pollAnswer{{status: http.StatusConflict, body: `{"error":"the collection already finished","code":"collection_terminal"}`}}}
		code, clock := runCancel(te, pt)
		if code != 1 {
			t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
		}
		if len(pt.requests) != 1 || !clock.Now().Equal(time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("%d requests after %s, want one request and no wait", len(pt.requests), clock.Now())
		}
		if !strings.Contains(te.stderr.String(), "collection_terminal: the collection already finished") {
			t.Fatalf("stderr = %q, want the envelope", te.stderr.String())
		}
	})
	t.Run("a Service address is a usage error before any request", func(t *testing.T) {
		te := newTestEnv(t)
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"collection", "cancel", "payments/checkout", "--server", "https://g.example"})
		if code != 2 || !strings.Contains(te.stderr.String(), "payments/checkout") {
			t.Fatalf("code = %d, stderr = %q, want 2 naming the argument", code, te.stderr.String())
		}
	})
}

// artifactResponse is the gateway's 200 for an artifact: octet-stream bytes,
// the attachment name, and the two headers naming the Collection and the
// version it profiled.
func artifactResponse(body io.Reader) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":           {"application/octet-stream"},
			"Content-Disposition":    {`attachment; filename="7h2k9m4p6r8t0v1w3x5y.pprof"`},
			"X-Pprof-Collection":     {"7h2k9m4p6r8t0v1w3x5y"},
			"X-Pprof-Target-Version": {"1.42.3"},
		},
		Body: io.NopCloser(body),
	}
}

// artifactTransport answers every request with profileBytes and records it.
type artifactTransport struct {
	requests []*http.Request
}

func (at *artifactTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	at.requests = append(at.requests, r)
	return artifactResponse(strings.NewReader(profileBytes)), nil
}

// runDownload runs the download verb in a fresh working directory against rt and returns the exit code and that directory.
func runDownload(t *testing.T, te *testEnv, rt http.RoundTripper, args ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	te.env.transport = rt
	args = append([]string{"download"}, args...)
	args = append(args, "--server", "https://g.example")
	return dispatch(context.Background(), te.env, clientVerbs(), args), dir
}

func TestDownloadDerivedFileName(t *testing.T) {
	te := newTestEnv(t)
	te.env.terminal = true
	at := &artifactTransport{}
	code, dir := runDownload(t, te, at, "7h2k9m4p6r8t0v1w3x5y")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if len(at.requests) != 1 {
		t.Fatalf("%d requests, want one", len(at.requests))
	}
	if r := at.requests[0]; r.Method != http.MethodGet || r.URL.Path != "/v1/collections/7h2k9m4p6r8t0v1w3x5y/profile" {
		t.Fatalf("request = %s %s, want GET the artifact route", r.Method, r.URL.Path)
	}
	const name = "7h2k9m4p6r8t0v1w3x5y.pprof"
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a test reading the file it asked for
	if err != nil || string(data) != profileBytes {
		t.Fatalf("file = %q, %v; want the artifact bytes", data, err)
	}
	if !strings.Contains(te.stderr.String(), name) {
		t.Fatalf("stderr = %q, want the path named", te.stderr.String())
	}
	if te.stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing: a binary body never reaches a terminal by default", te.stdout.String())
	}
}

func TestDownloadToNamedFile(t *testing.T) {
	te := newTestEnv(t)
	out := filepath.Join(t.TempDir(), "merged.pprof")
	code, dir := runDownload(t, te, &artifactTransport{}, "7h2k9m4p6r8t0v1w3x5y", "-o", out)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	data, err := os.ReadFile(out) //nolint:gosec // a test reading the file it asked for
	if err != nil || string(data) != profileBytes {
		t.Fatalf("file = %q, %v; want the artifact bytes", data, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("-o left %d entries in the working directory", len(entries))
	}
}

func TestDownloadToStdout(t *testing.T) {
	te := newTestEnv(t)
	te.env.terminal = true
	code, dir := runDownload(t, te, &artifactTransport{}, "7h2k9m4p6r8t0v1w3x5y", "-o", "-")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if te.stdout.String() != profileBytes {
		t.Fatalf("stdout = %q, want the artifact bytes", te.stdout.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("-o - left %d entries in the working directory", len(entries))
	}
}

func TestDownloadMetadata(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		te := newTestEnv(t)
		code, _ := runDownload(t, te, &artifactTransport{}, "7h2k9m4p6r8t0v1w3x5y")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		for _, want := range []string{"collection: 7h2k9m4p6r8t0v1w3x5y\n", "version: 1.42.3\n"} {
			if !strings.Contains(te.stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", te.stderr.String(), want)
			}
		}
	})
	t.Run("json", func(t *testing.T) {
		te := newTestEnv(t)
		code, _ := runDownload(t, te, &artifactTransport{}, "7h2k9m4p6r8t0v1w3x5y", "--output", "json")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if !strings.Contains(te.stderr.String(), `{"collection":"7h2k9m4p6r8t0v1w3x5y","version":"1.42.3"}`) {
			t.Fatalf("stderr = %q, want the two values as one JSON object", te.stderr.String())
		}
		if te.stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want nothing: the artifact is bytes and goes to the file", te.stdout.String())
		}
	})
}

func TestDownloadLocalRefusals(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStderr []string
	}{
		{name: "output path not writable", args: []string{"7h2k9m4p6r8t0v1w3x5y", "-o", filepath.Join(t.TempDir(), "missing", "merged.pprof")}, wantStderr: []string{"missing"}},
		{name: "a Service address", args: []string{"payments/checkout"}, wantStderr: []string{"payments/checkout", "identifier"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			code, dir := runDownload(t, te, refusingTransport(t), tc.args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(te.stderr.String(), want) {
					t.Fatalf("stderr = %q, want %q in it", te.stderr.String(), want)
				}
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("the working directory holds %d entries, want none", len(entries))
			}
		})
	}
}

func TestDownloadRefusesAFormatAsAPath(t *testing.T) {
	const want = "profgate: usage: -o names the file the artifact is written to, not a format; " +
		"--output json is the format flag, and -o ./json writes a file named json\n"
	for _, value := range []string{"json", "yaml"} {
		t.Run(value, func(t *testing.T) {
			te := newTestEnv(t)
			code, dir := runDownload(t, te, refusingTransport(t), "7h2k9m4p6r8t0v1w3x5y", "-o", value)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
			}
			if te.stderr.String() != want {
				t.Fatalf("stderr = %q, want %q", te.stderr.String(), want)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("the working directory holds %d entries, want none", len(entries))
			}
		})
	}
}

func TestDownloadCancelledMidBodyRemovesThePartialFile(t *testing.T) {
	te := newTestEnv(t)
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return artifactResponse(&cancellingBody{}), nil
	})
	code, dir := runDownload(t, te, rt, "7h2k9m4p6r8t0v1w3x5y")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("the partial file survived: %d entries", len(entries))
	}
}

func TestDownloadEnvelopes(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "410 artifact_gone", status: http.StatusGone, body: `{"error":"the artifact has expired","code":"artifact_gone"}`, want: "artifact_gone: the artifact has expired"},
		{name: "409 collection_not_completed", status: http.StatusConflict, body: `{"error":"collection 7h2k9m4p6r8t0v1w3x5y is running","code":"collection_not_completed"}`, want: "collection_not_completed: collection 7h2k9m4p6r8t0v1w3x5y is running"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			rt := &recordingTransport{body: tc.body, status: tc.status}
			code, dir := runDownload(t, te, rt, "7h2k9m4p6r8t0v1w3x5y")
			if code != 1 {
				t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
			}
			if len(rt.requests) != 1 {
				t.Fatalf("%d requests, want exactly one", len(rt.requests))
			}
			if !strings.Contains(te.stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want the envelope %q", te.stderr.String(), tc.want)
			}
			if te.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", te.stdout.String())
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("a refusal left %d entries in the working directory", len(entries))
			}
		})
	}
}
