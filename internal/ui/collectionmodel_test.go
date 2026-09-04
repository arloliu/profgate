package ui

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// collectionModelName is the module holding the Collection controls' pure functions.
const collectionModelName = "collectionmodel.js"

// collectionModelFunctions is what the module exports, in the order of its export statement.
var collectionModelFunctions = []string{
	"startOffered",
	"cancelOffered",
	"uuidFromBytes",
	"startRequest",
	"cancelRequest",
	"startOutcome",
	"cancelOutcome",
	"retryAfterSeconds",
	"startNext",
	"progressText",
}

// abandonMessage is what the page says when an attempt no answer ever classified is dropped.
const abandonMessage = "A Collection may already exist for this Service; the Collections table shows it."

// collectionID is an identifier of the grammar the gateway writes.
const collectionID = "7h2k9m4p6r8t0v1w3x5y"

// startRoute and cancelRoute are what the page hands the two request builders;
// the module spells no path of its own.
const (
	startRoute  = "/v1/namespaces/payment/services/payment-api/collections"
	cancelRoute = "/v1/collections/" + collectionID + "/cancel"
)

// loadCollectionModel evaluates the Collection model with its functions reachable as globals.
func loadCollectionModel(tb testing.TB) *goja.Runtime {
	tb.Helper()

	return loadModel(tb, collectionModelName, collectionModelFunctions...)
}

// str returns a pointer to s, which is how an expected non-null string is written.
func str(s string) *string {
	return &s
}

func TestCollectionModelShape(t *testing.T) {
	src := readSource(t, collectionModelName)
	if bad := staticImportAnyRe.FindAllString(src, -1); len(bad) > 0 {
		t.Errorf("%s: imports something: %v", collectionModelName, bad)
	}
	if bad := dynamicImportRe.FindAllString(src, -1); len(bad) > 0 {
		t.Errorf("%s: dynamic import: %v", collectionModelName, bad)
	}
	if n := len(exportAnyRe.FindAllString(src, -1)); n != 1 {
		t.Errorf("%s: %d export statements, want one", collectionModelName, n)
	}
	if rest := cutExport(t, collectionModelName, src); exportAnyRe.MatchString(rest) {
		t.Errorf("%s: an export remains after the trailing statement is cut", collectionModelName)
	}
	want := "export { " + strings.Join(collectionModelFunctions, ", ") + " };"
	if !strings.Contains(src, want) {
		t.Errorf("%s: the export statement is not %q", collectionModelName, want)
	}
}

// grammarRe captures the identifier grammar out of a module that spells it.
var grammarRe = regexp.MustCompile(`/\^\[[^]]+\]\{20\}\$/`)

// TestCollectionModelSpellsTheSameGrammar compares this module's Collection identifier grammar with urls.js's.
// The module imports nothing, so the two are held together here or nowhere.
func TestCollectionModelSpellsTheSameGrammar(t *testing.T) {
	model := grammarRe.FindString(readSource(t, collectionModelName))
	urls := grammarRe.FindString(readSource(t, "urls.js"))
	if urls == "" {
		t.Fatalf("urls.js: no identifier grammar found")
	}
	if model != urls {
		t.Errorf("%s: identifier grammar %q, urls.js has %q", collectionModelName, model, urls)
	}
}

// limitsWith is a /v1/limits body carrying only what the start control reads.
func limitsWith(enabled bool) map[string]any {
	return map[string]any{"pgo": map[string]any{"enabled": enabled}}
}

// whoamiWith is a /v1/whoami body carrying only the two PGO realm flags.
func whoamiWith(read, collect bool) map[string]any {
	return map[string]any{"realm": map[string]any{"pgo": map[string]any{"read": read, "collect": collect}}}
}

func TestCollectionModelStartOffered(t *testing.T) {
	cases := []struct {
		name    string
		limits  any
		whoami  any
		service string
		want    bool
	}{
		{"all four hold", limitsWith(true), whoamiWith(true, true), "payment-api", true},
		{"pgo disabled", limitsWith(false), whoamiWith(true, true), "payment-api", false},
		{"read alone", limitsWith(true), whoamiWith(true, false), "payment-api", false},
		{"collect alone", limitsWith(true), whoamiWith(false, true), "payment-api", false},
		{"neither flag", limitsWith(true), whoamiWith(false, false), "payment-api", false},
		{"no Service", limitsWith(true), whoamiWith(true, true), "", false},
		{"no limits body", nil, whoamiWith(true, true), "payment-api", false},
		{"no whoami body", limitsWith(true), nil, "payment-api", false},
		{"limits with no pgo block", map[string]any{}, whoamiWith(true, true), "payment-api", false},
		{"whoami with no realm block", limitsWith(true), map[string]any{}, "payment-api", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCollectionModel(t)
			got := callModel(t, vm, "startOffered", tc.limits, tc.whoami, tc.service)
			if !got.Unchanged {
				t.Errorf("startOffered mutated an argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("startOffered = %s, want %v", got.Result, tc.want)
			}
		})
	}
}

func TestCollectionModelCancelOffered(t *testing.T) {
	cases := []struct {
		name    string
		state   any
		collect bool
		want    bool
	}{
		{"pending", "pending", true, true},
		{"running", "running", true, true},
		{"pending without collect", "pending", false, false},
		{"running without collect", "running", false, false},
		{"initializing", "initializing", true, false},
		{"completed", "completed", true, false},
		{"failed", "failed", true, false},
		{"cancelled", "cancelled", true, false},
		{"expired", "expired", true, false},
		{"a state the console does not know", "draining", true, false},
		{"an empty state", "", true, false},
		{"no state at all", nil, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCollectionModel(t)
			got := callModel(t, vm, "cancelOffered", tc.state, whoamiWith(true, tc.collect))
			if !got.Unchanged {
				t.Errorf("cancelOffered mutated an argument")
			}
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("cancelOffered(%v) = %s, want %v", tc.state, got.Result, tc.want)
			}
		})
	}
}

// bytesOf repeats step, so each of the sixteen bytes differs from its neighbour.
func bytesOf(step int) []int {
	out := make([]int, 16)
	for i := range out {
		out[i] = i * step
	}

	return out
}

// uuidRe is the 8-4-4-4-12 grouping with the version nibble and the variant bits pinned.
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestCollectionModelUUIDFromBytes(t *testing.T) {
	cases := []struct {
		name  string
		bytes []int
		want  string
	}{
		{"every byte distinct", bytesOf(0x11), "00112233-4455-4677-8899-aabbccddeeff"},
		{"every bit set: the version and variant bits are overwritten",
			[]int{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			"ffffffff-ffff-4fff-bfff-ffffffffffff"},
		{"every bit clear: the version and variant bits are written anyway",
			make([]int, 16), "00000000-0000-4000-8000-000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCollectionModel(t)
			first := callModel(t, vm, "uuidFromBytes", tc.bytes)
			if !first.Unchanged {
				t.Errorf("uuidFromBytes mutated the bytes it was handed")
			}
			if !sameJSON(t, first.Result, tc.want) {
				t.Fatalf("uuidFromBytes = %s, want %q", first.Result, tc.want)
			}
			if !uuidRe.MatchString(tc.want) {
				t.Errorf("%q is not a UUIDv4", tc.want)
			}
			second := callModel(t, vm, "uuidFromBytes", tc.bytes)
			if !sameJSON(t, second.Result, tc.want) {
				t.Errorf("the same bytes formatted again = %s, want %q", second.Result, tc.want)
			}
		})
	}
}

func TestCollectionModelUUIDDistinguishesInputs(t *testing.T) {
	vm := loadCollectionModel(t)
	a := callModel(t, vm, "uuidFromBytes", bytesOf(0x11))
	b := callModel(t, vm, "uuidFromBytes", bytesOf(0x07))
	if string(a.Result) == string(b.Result) {
		t.Errorf("two different inputs formatted alike: %s", a.Result)
	}
}

// request is what the two request builders return.
type request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    *string           `json:"body"`
}

// buildRequest calls fn and decodes the request it returns.
func buildRequest(tb testing.TB, vm *goja.Runtime, fn string, args ...any) request {
	tb.Helper()

	got := callModel(tb, vm, fn, args...)
	var req request
	if err := json.Unmarshal(got.Result, &req); err != nil {
		tb.Fatalf("decode %s result %s: %v", fn, got.Result, err)
	}

	return req
}

func TestCollectionModelStartRequest(t *testing.T) {
	vm := loadCollectionModel(t)
	key := "3f8a1c2e-4b5d-4e6f-8a9b-0c1d2e3f4a5b"
	req := buildRequest(t, vm, "startRequest", startRoute, key)
	if req.Method != "POST" {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.URL != startRoute {
		t.Errorf("url = %q, want the route it was handed %q", req.URL, startRoute)
	}
	want := map[string]string{"Content-Type": "application/json", "Idempotency-Key": key}
	if !reflect.DeepEqual(req.Headers, want) {
		t.Errorf("headers = %v, want %v", req.Headers, want)
	}
	if req.Body == nil || *req.Body != "{}" {
		t.Errorf("body = %v, want the empty JSON object", req.Body)
	}
}

func TestCollectionModelCancelRequest(t *testing.T) {
	vm := loadCollectionModel(t)
	req := buildRequest(t, vm, "cancelRequest", cancelRoute)
	if req.Method != "POST" {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.URL != cancelRoute {
		t.Errorf("url = %q, want the route it was handed %q", req.URL, cancelRoute)
	}
	want := map[string]string{"Content-Type": "application/json"}
	if !reflect.DeepEqual(req.Headers, want) {
		t.Errorf("headers = %v, want %v: a cancel carries no idempotency key", req.Headers, want)
	}
	if req.Body != nil {
		t.Errorf("body = %q, want none", *req.Body)
	}
}

// answer is a response as the page hands it to the two outcome functions.
// A field left out of the map is a field the response did not carry.
func answer(status int, code, message string) map[string]any {
	return map[string]any{"rejected": false, "status": status, "code": code, "message": message}
}

// with returns a copy of an answer carrying one more field.
func with(base map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[key] = value

	return out
}

// rejected is the answer a fetch that never produced a response leaves.
func rejected() map[string]any {
	return map[string]any{"rejected": true, "status": 0, "code": "", "message": ""}
}

// startResult is what startOutcome returns.
type startResult struct {
	Keep           bool     `json:"keep"`
	Armed          bool     `json:"armed"`
	Select         *string  `json:"select"`
	Refetch        []string `json:"refetch"`
	Error          *string  `json:"error"`
	DisableSeconds int      `json:"disableSeconds"`
}

// runStartOutcome calls startOutcome and decodes what it returned.
func runStartOutcome(tb testing.TB, vm *goja.Runtime, a map[string]any) startResult {
	tb.Helper()

	got := callModel(tb, vm, "startOutcome", a)
	if !got.Unchanged {
		tb.Errorf("startOutcome mutated the answer it was handed")
	}
	var out startResult
	if err := json.Unmarshal(got.Result, &out); err != nil {
		tb.Fatalf("decode startOutcome result %s: %v", got.Result, err)
	}
	if out.Refetch == nil {
		tb.Errorf("refetch = null, want an array")
	}

	return out
}

func TestCollectionModelStartOutcome(t *testing.T) {
	cases := []struct {
		name   string
		answer map[string]any
		want   startResult
	}{
		{"202 carrying an identifier",
			with(answer(202, "", ""), "id", collectionID),
			startResult{Select: str(collectionID), Refetch: []string{"collections"}}},
		{"200 replaying the key carries the same identifier",
			with(answer(200, "", ""), "id", collectionID),
			startResult{Select: str(collectionID), Refetch: []string{"collections"}}},
		{"a success status neither 200 nor 202 still selects",
			with(answer(299, "", ""), "id", collectionID),
			startResult{Select: str(collectionID), Refetch: []string{"collections"}}},
		{"2xx with no identifier shows the body",
			with(answer(202, "", ""), "body", "{}"),
			startResult{Refetch: []string{}, Error: str("{}")}},
		{"2xx whose identifier is outside the grammar shows the body and selects nothing",
			with(with(answer(202, "", ""), "id", "NOT-AN-ID"), "body", `{"id":"NOT-AN-ID"}`),
			startResult{Refetch: []string{}, Error: str(`{"id":"NOT-AN-ID"}`)}},
		{"429 collection_in_progress refetches the list",
			answer(429, "collection_in_progress", "a collection is already running"),
			startResult{Refetch: []string{"collections"}}},
		{"429 rate_limited with a Retry-After",
			with(answer(429, "rate_limited", "too many requests"), "retryAfter", "7"),
			startResult{Refetch: []string{}, Error: str("too many requests; the control is disabled for 7 seconds"), DisableSeconds: 7}},
		{"429 rate_limited with no Retry-After waits the default",
			answer(429, "rate_limited", "too many requests"),
			startResult{Refetch: []string{}, Error: str("too many requests; the control is disabled for 5 seconds"), DisableSeconds: 5}},
		{"429 capacity_exhausted with a Retry-After of zero waits none",
			with(answer(429, "capacity_exhausted", "no capacity"), "retryAfter", "0"),
			startResult{Refetch: []string{}, Error: str("no capacity; the control is disabled for 0 seconds"), DisableSeconds: 0}},
		{"429 with another code is shown and nothing else",
			answer(429, "something_else", "no"),
			startResult{Refetch: []string{}, Error: str("no")}},
		{"409 idempotency_mismatch refetches the list",
			answer(409, "idempotency_mismatch", "the policy moved"),
			startResult{Refetch: []string{"collections"}, Error: str("the policy moved")}},
		{"403 realm_denied refetches whoami",
			answer(403, "realm_denied", "your realm does not admit this"),
			startResult{Refetch: []string{"whoami"}, Error: str("your realm does not admit this")}},
		{"501 pgo_disabled refetches limits",
			answer(501, "pgo_disabled", "PGO collection is disabled"),
			startResult{Refetch: []string{"limits"}, Error: str("PGO collection is disabled")}},
		{"501 with another code is a 5xx and keeps the key",
			answer(501, "not_implemented", "no"),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("no")}},
		{"503 pgo_unavailable keeps the key",
			answer(503, "pgo_unavailable", "PGO collection is unavailable"),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("PGO collection is unavailable")}},
		{"503 collector_unavailable drops the key",
			answer(503, "collector_unavailable", "no collector is fresh"),
			startResult{Refetch: []string{}, Error: str("no collector is fresh")}},
		{"503 with another code keeps the key",
			answer(503, "not_ready", "the gateway is still syncing"),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("the gateway is still syncing")}},
		{"500 keeps the key",
			answer(500, "internal", "internal error"),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("internal error")}},
		{"502 keeps the key",
			answer(502, "", ""),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("HTTP 502")}},
		{"504 keeps the key",
			answer(504, "", ""),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("HTTP 504")}},
		{"an answer with no envelope shows its status and reason phrase",
			with(answer(502, "", ""), "statusText", "Bad Gateway"),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("HTTP 502 Bad Gateway")}},
		{"an envelope outranks the reason phrase",
			with(answer(500, "internal", "internal error"), "statusText", "Internal Server Error"),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("internal error")}},
		{"599 keeps the key",
			answer(599, "", ""),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("HTTP 599")}},
		{"a rejected fetch keeps the key",
			with(rejected(), "message", "request failed"),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("request failed")}},
		{"a rejected fetch that named nothing keeps the key",
			rejected(),
			startResult{Keep: true, Armed: true, Refetch: []string{}, Error: str("request failed")}},
		{"400 invalid_parameter is shown and nothing else",
			answer(400, "invalid_parameter", "unknown query parameter"),
			startResult{Refetch: []string{}, Error: str("unknown query parameter")}},
		{"401 is shown and nothing else",
			answer(401, "unauthorized", "credentials required"),
			startResult{Refetch: []string{}, Error: str("credentials required")}},
		{"404 is shown and nothing else",
			answer(404, "service_not_found", "no such Service"),
			startResult{Refetch: []string{}, Error: str("no such Service")}},
		{"409 with another code is shown and nothing else",
			answer(409, "version_conflict", "the revision moved"),
			startResult{Refetch: []string{}, Error: str("the revision moved")}},
		{"an envelope with no message is shown by its code",
			answer(418, "brewing", ""),
			startResult{Refetch: []string{}, Error: str("brewing")}},
		{"an answer that parsed as no envelope is shown by its status",
			answer(418, "", ""),
			startResult{Refetch: []string{}, Error: str("HTTP 418")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCollectionModel(t)
			got := runStartOutcome(t, vm, tc.answer)
			want := tc.want
			if want.Refetch == nil {
				want.Refetch = []string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("startOutcome = %+v, want %+v", got, want)
			}
		})
	}
}

// cancelResult is what cancelOutcome returns.
type cancelResult struct {
	Replace      json.RawMessage `json:"replace"`
	Refetch      []string        `json:"refetch"`
	Error        *string         `json:"error"`
	RetryAfterMs int             `json:"retryAfterMs"`
}

func TestCollectionModelCancelOutcome(t *testing.T) {
	record := map[string]any{"id": collectionID, "state": "cancelled"}
	cases := []struct {
		name      string
		answer    map[string]any
		tryNumber int
		replace   any
		refetch   []string
		wantErr   *string
		retryMs   int
	}{
		{"200 replaces the row and refetches the list",
			with(answer(200, "", ""), "record", record), 1, record, []string{"collections"}, nil, 0},
		{"200 on the retry replaces the row too",
			with(answer(200, "", ""), "record", record), 2, record, []string{"collections"}, nil, 0},
		{"200 carrying no record still refetches the list",
			answer(200, "", ""), 1, nil, []string{"collections"}, nil, 0},
		{"409 collection_terminal refetches the list and shows nothing",
			answer(409, "collection_terminal", "the collection ended"), 1, nil, []string{"collections"}, nil, 0},
		{"409 collection_initializing on the first press asks for one retry",
			answer(409, "collection_initializing", "still publishing"), 1, nil, []string{}, nil, 1000},
		{"409 collection_initializing on the retry shows the code",
			answer(409, "collection_initializing", "still publishing"), 2, nil, []string{}, str("collection_initializing"), 0},
		{"404 collection_not_found refetches the list and whoami",
			answer(404, "collection_not_found", "no such collection"), 1, nil, []string{"collections", "whoami"}, nil, 0},
		{"403 realm_denied is shown and the row is left alone",
			answer(403, "realm_denied", "your realm does not admit this"), 1, nil, []string{}, str("your realm does not admit this"), 0},
		{"409 with another code is shown and the row is left alone",
			answer(409, "version_conflict", "the revision moved"), 1, nil, []string{}, str("the revision moved"), 0},
		{"500 is shown and the row is left alone",
			answer(500, "", ""), 1, nil, []string{}, str("HTTP 500"), 0},
		{"a status with a reason phrase shows both",
			with(answer(500, "", ""), "statusText", "Internal Server Error"), 1, nil, []string{},
			str("HTTP 500 Internal Server Error"), 0},
		{"a rejected fetch is shown and the row is left alone",
			with(rejected(), "message", "request failed"), 1, nil, []string{}, str("request failed"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCollectionModel(t)
			got := callModel(t, vm, "cancelOutcome", tc.answer, tc.tryNumber)
			if !got.Unchanged {
				t.Errorf("cancelOutcome mutated an argument")
			}
			var out cancelResult
			if err := json.Unmarshal(got.Result, &out); err != nil {
				t.Fatalf("decode cancelOutcome result %s: %v", got.Result, err)
			}
			if !sameJSON(t, out.Replace, tc.replace) {
				t.Errorf("replace = %s, want %v", out.Replace, tc.replace)
			}
			if !reflect.DeepEqual(out.Refetch, tc.refetch) {
				t.Errorf("refetch = %v, want %v", out.Refetch, tc.refetch)
			}
			if !reflect.DeepEqual(out.Error, tc.wantErr) {
				t.Errorf("error = %v, want %v", out.Error, tc.wantErr)
			}
			if out.RetryAfterMs != tc.retryMs {
				t.Errorf("retryAfterMs = %d, want %d", out.RetryAfterMs, tc.retryMs)
			}
		})
	}
}

func TestCollectionModelRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		name   string
		header any
		want   int
	}{
		{"a count of seconds", "7", 7},
		{"zero seconds is no wait at all", "0", 0},
		{"a count above the ceiling clamps", "900", 300},
		{"the ceiling itself", "300", 300},
		{"one second past the ceiling clamps", "301", 300},
		{"surrounding spaces are ignored", " 7 ", 7},
		{"a number rather than a string", 7, 7},
		{"absent", nil, 5},
		{"empty", "", 5},
		{"negative", "-3", 5},
		{"fractional", "1.5", 5},
		{"non-numeric", "soon", 5},
		{"an HTTP date", "Wed, 21 Oct 2015 07:28:00 GMT", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCollectionModel(t)
			got := callModel(t, vm, "retryAfterSeconds", tc.header)
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("retryAfterSeconds(%v) = %s, want %d", tc.header, got.Result, tc.want)
			}
		})
	}
}

// TestCollectionModelProgressText proves the detail's progress line counts rounds the way a person does,
// which is the way the command line prints the same record.
// The record stores the running round as a zero-based index,
// so a completed three-round Collection carries two and has to read three of three.
func TestCollectionModelProgressText(t *testing.T) {
	cases := []struct {
		name     string
		progress any
		want     string
	}{
		{"the first round of three", map[string]any{"round": 0, "rounds": 3, "samplesOK": 2, "samplesFailed": 0},
			"round 1 of 3, samples ok 2, failed 0"},
		{"the second round of three", map[string]any{"round": 1, "rounds": 3, "samplesOK": 4, "samplesFailed": 1},
			"round 2 of 3, samples ok 4, failed 1"},
		{"a completed three-round Collection", map[string]any{"round": 2, "rounds": 3, "samplesOK": 9, "samplesFailed": 0},
			"round 3 of 3, samples ok 9, failed 0"},
		{"one round", map[string]any{"round": 0, "rounds": 1, "samplesOK": 1, "samplesFailed": 0},
			"round 1 of 1, samples ok 1, failed 0"},
		{"a field the record left out counts as zero", map[string]any{"rounds": 2}, "round 1 of 2, samples ok 0, failed 0"},
		{"no round claimed yet", map[string]any{"round": 0, "rounds": 0, "samplesOK": 0, "samplesFailed": 0}, ""},
		{"no progress at all", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := loadCollectionModel(t)
			got := callModel(t, vm, "progressText", tc.progress)
			if !sameJSON(t, got.Result, tc.want) {
				t.Errorf("progressText(%v) = %s, want %q", tc.progress, got.Result, tc.want)
			}
			if !got.Unchanged {
				t.Errorf("progressText(%v) changed the record it was handed", tc.progress)
			}
		})
	}
}

// startState is the start control's state as the page keeps it.
type startState struct {
	Phase string  `json:"phase"`
	Key   *string `json:"key"`
	Route *string `json:"route"`
	Token int     `json:"token"`
	Until int     `json:"until"`
}

// startStep is what startNext returns.
type startStep struct {
	State   startState `json:"state"`
	Message *string    `json:"message"`
}

// step drives one event and reports the state and message it produced,
// failing when the state handed in was changed in place
// or when the phase and the attempt it holds disagree.
func step(tb testing.TB, vm *goja.Runtime, state startState, event map[string]any) startStep {
	tb.Helper()

	got := callModel(tb, vm, "startNext", state, event)
	if !got.Unchanged {
		tb.Errorf("startNext mutated an argument")
	}
	var out startStep
	if err := json.Unmarshal(got.Result, &out); err != nil {
		tb.Fatalf("decode startNext result %s: %v", got.Result, err)
	}
	holds := out.State.Phase == "armed" || out.State.Phase == "inflight" || out.State.Phase == "retained"
	if holds != (out.State.Key != nil) || holds != (out.State.Route != nil) {
		tb.Errorf("phase %q holds key %v and route %v: only an armed, inflight, or retained control holds an attempt",
			out.State.Phase, out.State.Key, out.State.Route)
	}

	return out
}

// idleState, armedState, and coolingState are one representative state per phase.
// Each carries the same attempt number,
// so a stale outcome is one the test can name.
func idleState() startState {
	return startState{Phase: "idle", Token: 3}
}

func armedState(phase string) startState {
	return startState{Phase: phase, Key: str("k1"), Route: str(startRoute), Token: 3}
}

func coolingState() startState {
	return startState{Phase: "cooling", Token: 3, Until: 5000}
}

func TestCollectionModelStartNext(t *testing.T) {
	events := []struct {
		name  string
		event map[string]any
	}{
		{"arm", map[string]any{"kind": "arm", "key": "k2", "route": cancelRoute}},
		{"submit", map[string]any{"kind": "submit"}},
		{"outcome kept", map[string]any{"kind": "outcome", "token": 3, "keep": true, "disableSeconds": 0, "now": 1000}},
		{"outcome dropped", map[string]any{"kind": "outcome", "token": 3, "keep": false, "disableSeconds": 0, "now": 1000}},
		{"outcome cooling", map[string]any{"kind": "outcome", "token": 3, "keep": false, "disableSeconds": 7, "now": 1000}},
		{"outcome of an abandoned attempt", map[string]any{"kind": "outcome", "token": 2, "keep": true, "disableSeconds": 0, "now": 1000}},
		{"timer before the wait ends", map[string]any{"kind": "timer", "now": 1000}},
		{"timer at the moment the wait ends", map[string]any{"kind": "timer", "now": 5000}},
		{"timer past the wait", map[string]any{"kind": "timer", "now": 9000}},
		{"keep", map[string]any{"kind": "keep"}},
		{"selection", map[string]any{"kind": "selection"}},
		{"an event the model does not know", map[string]any{"kind": "nothing"}},
	}
	phases := []struct {
		name  string
		state startState
	}{
		{"idle", idleState()},
		{"armed", armedState("armed")},
		{"inflight", armedState("inflight")},
		{"retained", armedState("retained")},
		{"cooling", coolingState()},
	}
	// Every pair not named here leaves the state as it was and says nothing.
	armedFresh := startState{Phase: "armed", Key: str("k2"), Route: str(cancelRoute), Token: 4}
	inflight := armedState("inflight")
	idle := idleState()
	named := map[string]startStep{
		"idle/arm":                                  {State: armedFresh},
		"idle/selection":                            {State: idle},
		"armed/submit":                              {State: inflight},
		"armed/timer before the wait ends":          {State: idle},
		"armed/timer at the moment the wait ends":   {State: idle},
		"armed/timer past the wait":                 {State: idle},
		"armed/keep":                                {State: idle},
		"armed/selection":                           {State: idle},
		"inflight/outcome kept":                     {State: armedState("retained")},
		"inflight/outcome dropped":                  {State: idle},
		"inflight/outcome cooling":                  {State: startState{Phase: "cooling", Token: 3, Until: 8000}},
		"inflight/selection":                        {State: idle, Message: str(abandonMessage)},
		"retained/submit":                           {State: inflight},
		"retained/keep":                             {State: idle, Message: str(abandonMessage)},
		"retained/selection":                        {State: idle, Message: str(abandonMessage)},
		"cooling/timer at the moment the wait ends": {State: idle},
		"cooling/timer past the wait":               {State: idle},
		"cooling/selection":                         {State: idle},
	}
	for _, p := range phases {
		for _, e := range events {
			t.Run(p.name+"/"+e.name, func(t *testing.T) {
				vm := loadCollectionModel(t)
				want, ok := named[p.name+"/"+e.name]
				if !ok {
					want = startStep{State: p.state}
				}
				got := step(t, vm, p.state, e.event)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("startNext = %+v, want %+v", got, want)
				}
			})
		}
	}
}

// TestCollectionModelKeySurvivesLostAnswers drives the sequence the key exists for:
// three answers that could not classify the attempt,
// a press after each one carrying the key the lost press sent,
// and the answer that finally classifies it, which drops the key.
func TestCollectionModelKeySurvivesLostAnswers(t *testing.T) {
	vm := loadCollectionModel(t)
	state := step(t, vm, idleState(), map[string]any{"kind": "arm", "key": "k1", "route": startRoute}).State
	if state.Phase != "armed" || state.Key == nil || *state.Key != "k1" {
		t.Fatalf("arm produced %+v, want an armed control holding k1", state)
	}
	if state.Token != 4 {
		t.Errorf("token = %d, want the attempt number raised to 4", state.Token)
	}
	unclassified := []struct {
		name   string
		answer map[string]any
	}{
		{"a rejected fetch", with(rejected(), "message", "request failed")},
		{"503 pgo_unavailable", answer(503, "pgo_unavailable", "PGO collection is unavailable")},
		{"500", answer(500, "internal", "internal error")},
	}
	for _, u := range unclassified {
		state = step(t, vm, state, map[string]any{"kind": "submit"}).State
		if state.Phase != "inflight" {
			t.Fatalf("%s: submit produced phase %q, want inflight", u.name, state.Phase)
		}
		req := buildRequest(t, vm, "startRequest", *state.Route, *state.Key)
		if req.Headers["Idempotency-Key"] != "k1" {
			t.Errorf("%s: the press sent %q, want the key the first press sent", u.name, req.Headers["Idempotency-Key"])
		}
		out := runStartOutcome(t, vm, u.answer)
		if !out.Keep {
			t.Fatalf("%s: the answer dropped the key", u.name)
		}
		state = step(t, vm, state, map[string]any{
			"kind": "outcome", "token": state.Token, "keep": out.Keep, "disableSeconds": out.DisableSeconds, "now": 1000,
		}).State
		if state.Phase != "retained" || state.Key == nil || *state.Key != "k1" {
			t.Fatalf("%s: produced %+v, want a retained control still holding k1", u.name, state)
		}
	}
	state = step(t, vm, state, map[string]any{"kind": "submit"}).State
	classified := runStartOutcome(t, vm, answer(503, "collector_unavailable", "no collector is fresh"))
	if classified.Keep {
		t.Errorf("503 collector_unavailable kept the key; no write can have happened")
	}
	state = step(t, vm, state, map[string]any{
		"kind": "outcome", "token": state.Token, "keep": classified.Keep, "disableSeconds": classified.DisableSeconds, "now": 1000,
	}).State
	if state.Phase != "idle" || state.Key != nil {
		t.Fatalf("after an answer that classified the attempt: %+v, want an idle control holding nothing", state)
	}
	next := step(t, vm, state, map[string]any{"kind": "arm", "key": "k2", "route": startRoute}).State
	if next.Key == nil || *next.Key == "k1" {
		t.Errorf("the next attempt reuses the classified attempt's key: %+v", next)
	}
}

// TestCollectionModelCooling drives what a 429 asking for a delay does:
// a delay of zero returns the control to idle, and a delay puts it in cooling,
// where a press sends nothing until the timer that ends the wait.
func TestCollectionModelCooling(t *testing.T) {
	vm := loadCollectionModel(t)
	zero := runStartOutcome(t, vm, with(answer(429, "rate_limited", "too many requests"), "retryAfter", "0"))
	if zero.DisableSeconds != 0 {
		t.Fatalf("a Retry-After of zero asked for %d seconds", zero.DisableSeconds)
	}
	after := step(t, vm, armedState("inflight"), map[string]any{
		"kind": "outcome", "token": 3, "keep": zero.Keep, "disableSeconds": zero.DisableSeconds, "now": 1000,
	}).State
	if after.Phase != "idle" || after.Until != 0 {
		t.Errorf("a delay of zero produced %+v, want an idle control and no wait", after)
	}

	waited := runStartOutcome(t, vm, answer(429, "capacity_exhausted", "no capacity"))
	if waited.DisableSeconds != 5 {
		t.Fatalf("an absent Retry-After asked for %d seconds, want five", waited.DisableSeconds)
	}
	cooling := step(t, vm, armedState("inflight"), map[string]any{
		"kind": "outcome", "token": 3, "keep": waited.Keep, "disableSeconds": waited.DisableSeconds, "now": 1000,
	}).State
	want := startState{Phase: "cooling", Token: 3, Until: 6000}
	if !reflect.DeepEqual(cooling, want) {
		t.Fatalf("cooling = %+v, want %+v", cooling, want)
	}
	pressed := step(t, vm, cooling, map[string]any{"kind": "submit"}).State
	if !reflect.DeepEqual(pressed, cooling) {
		t.Errorf("a press during the wait produced %+v, want the wait untouched", pressed)
	}
	early := step(t, vm, cooling, map[string]any{"kind": "timer", "now": 5999}).State
	if !reflect.DeepEqual(early, cooling) {
		t.Errorf("a timer before the wait ends produced %+v, want the wait untouched", early)
	}
	ended := step(t, vm, cooling, map[string]any{"kind": "timer", "now": 6000}).State
	if !reflect.DeepEqual(ended, idleState()) {
		t.Errorf("the timer that ends the wait produced %+v, want an idle control", ended)
	}
}
