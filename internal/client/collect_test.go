package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// uuidV4 is the textual form with the version nibble 4 and the variant bits 10.
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewKey(t *testing.T) {
	t.Run("is a UUIDv4", func(t *testing.T) {
		key, err := NewKey(bytes.NewReader(bytes.Repeat([]byte{0xff}, 16)))
		if err != nil {
			t.Fatal(err)
		}
		if !uuidV4.MatchString(key) {
			t.Fatalf("key = %q, want a UUIDv4 with the version and variant bits set", key)
		}
		// All-ones input proves the two bit fields are overwritten, not left.
		if key != "ffffffff-ffff-4fff-bfff-ffffffffffff" {
			t.Fatalf("key = %q, want ffffffff-ffff-4fff-bfff-ffffffffffff", key)
		}
	})
	t.Run("two keys differ", func(t *testing.T) {
		src := bytes.NewReader(append(bytes.Repeat([]byte{0x01}, 16), bytes.Repeat([]byte{0x02}, 16)...))
		a, err := NewKey(src)
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewKey(src)
		if err != nil {
			t.Fatal(err)
		}
		if a == b {
			t.Fatalf("two keys are both %q", a)
		}
	})
	t.Run("a short random source is an error", func(t *testing.T) {
		if _, err := NewKey(strings.NewReader("short")); err == nil {
			t.Fatal("no error from a source that ran out")
		}
	})
}

func TestCreate(t *testing.T) {
	const body = `{"sampling":{"rounds":2}}`
	t.Run("202", func(t *testing.T) {
		var got *http.Request
		var gotBody []byte
		c := serve(t, func(w http.ResponseWriter, r *http.Request) {
			got = r
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Location", "/v1/collections/7h2k9m4p6r8t0v1w3x5y")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"7h2k9m4p6r8t0v1w3x5y","state":"pending"}` + "\n"))
		}, tokenCredential("tok"), nil)
		created, err := c.Create(context.Background(), "payments", "checkout", []byte(body), "3f0a1c7e-8b52-4d6a-9f11-2c4e6a8b0d31")
		if err != nil {
			t.Fatal(err)
		}
		if got.Method != http.MethodPost || got.URL.Path != "/v1/namespaces/payments/services/checkout/collections" {
			t.Fatalf("request = %s %s, want POST the collections route", got.Method, got.URL.Path)
		}
		if got.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got.Header.Get("Content-Type"))
		}
		if keys := got.Header.Values("Idempotency-Key"); len(keys) != 1 || keys[0] != "3f0a1c7e-8b52-4d6a-9f11-2c4e6a8b0d31" {
			t.Fatalf("Idempotency-Key = %q, want the one key given", keys)
		}
		if string(gotBody) != body {
			t.Fatalf("body = %q, want %q", gotBody, body)
		}
		want := Created{ID: "7h2k9m4p6r8t0v1w3x5y", State: "pending", Status: http.StatusAccepted, Location: "/v1/collections/7h2k9m4p6r8t0v1w3x5y", Body: []byte(`{"id":"7h2k9m4p6r8t0v1w3x5y","state":"pending"}` + "\n")}
		if created.ID != want.ID || created.State != want.State || created.Status != want.Status || created.Location != want.Location || !bytes.Equal(created.Body, want.Body) {
			t.Fatalf("Created = %+v, want %+v", created, want)
		}
	})
	t.Run("200 replay carries id and state and no record fields", func(t *testing.T) {
		c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Location", "/v1/collections/7h2k9m4p6r8t0v1w3x5y")
			_, _ = w.Write([]byte(`{"id":"7h2k9m4p6r8t0v1w3x5y","state":"running"}`))
		}, tokenCredential("tok"), nil)
		created, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), "k")
		if err != nil {
			t.Fatal(err)
		}
		if created.ID != "7h2k9m4p6r8t0v1w3x5y" || created.State != "running" || created.Status != http.StatusOK {
			t.Fatalf("Created = %+v, want the replay's id, state, and 200", created)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(created.Body, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "id")
		delete(fields, "state")
		if len(fields) != 0 {
			t.Fatalf("the replay carries record fields: %v", fields)
		}
	})
	t.Run("a 2xx without an identifier is a status error", func(t *testing.T) {
		c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"state":"pending"}`))
		}, tokenCredential("tok"), nil)
		_, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), "k")
		var se *StatusError
		if !errors.As(err, &se) || se.Status != http.StatusAccepted {
			t.Fatalf("err = %v, want a StatusError 202", err)
		}
	})
	t.Run("a 2xx that is not JSON is a status error", func(t *testing.T) {
		c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html>ok</html>`))
		}, tokenCredential("tok"), nil)
		_, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), "k")
		var se *StatusError
		if !errors.As(err, &se) || se.Status != http.StatusOK {
			t.Fatalf("err = %v, want a StatusError 200", err)
		}
	})
	t.Run("a refusal comes back as its envelope", func(t *testing.T) {
		a := &answers{statuses: []int{http.StatusBadRequest}, bodies: []string{`{"error":"a collection request sets neither enabled nor schedule","code":"invalid_parameter"}`}}
		c, _ := pollClient(t, a.ServeHTTP)
		_, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), "k")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != "invalid_parameter" {
			t.Fatalf("err = %v, want the invalid_parameter envelope", err)
		}
	})
}

// script serves one handler per request in order, repeating the last, and
// records every request it saw so a test can compare their headers.
type script struct {
	steps    []http.HandlerFunc
	requests []*http.Request
}

func (s *script) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	step := s.steps[min(len(s.requests), len(s.steps)-1)]
	s.requests = append(s.requests, r)
	step(w, r)
}

// keys is the Idempotency-Key of every request the script saw.
func (s *script) keys() []string {
	got := make([]string, 0, len(s.requests))
	for _, r := range s.requests {
		got = append(got, r.Header.Get("Idempotency-Key"))
	}
	return got
}

// sameKey reports whether every request carried key and there was at least one.
func (s *script) sameKey(key string) bool {
	if len(s.requests) == 0 {
		return false
	}
	for _, got := range s.keys() {
		if got != key {
			return false
		}
	}
	return true
}

// dropConnection answers nothing at all: the connection closes before any byte of a response is written.
func dropConnection(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

// cutBody writes a status and its headers, promises a longer body than it
// writes, and stops: the answer's headers arrive and its body does not arrive whole.
func cutBody(status int, prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/v1/collections/7h2k9m4p6r8t0v1w3x5y")
		w.Header().Set("Content-Length", strconv.Itoa(len(prefix)+32))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(prefix))
	}
}

// answerJSON writes one complete JSON answer.
func answerJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "/v1/collections/7h2k9m4p6r8t0v1w3x5y")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const (
	acceptedJSON = `{"id":"7h2k9m4p6r8t0v1w3x5y","state":"pending"}`
	replayJSON   = `{"id":"7h2k9m4p6r8t0v1w3x5y","state":"running"}`
)

func TestCreateRetriesAnUnknownResult(t *testing.T) {
	const key = "3f0a1c7e-8b52-4d6a-9f11-2c4e6a8b0d31"
	t.Run("a transport failure then a 202", func(t *testing.T) {
		s := &script{steps: []http.HandlerFunc{dropConnection, answerJSON(http.StatusAccepted, acceptedJSON)}}
		c, _ := pollClient(t, s.ServeHTTP)
		created, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), key)
		if err != nil {
			t.Fatal(err)
		}
		if created.ID != "7h2k9m4p6r8t0v1w3x5y" || created.Status != http.StatusAccepted {
			t.Fatalf("Created = %+v, want the 202's identifier", created)
		}
		if len(s.requests) != 2 {
			t.Fatalf("%d requests, want the failure and the retry", len(s.requests))
		}
		if !s.sameKey(key) {
			t.Fatalf("Idempotency-Key = %q, want the one key on both requests", s.keys())
		}
	})
	t.Run("a 202 whose body is cut off then a 200 replay", func(t *testing.T) {
		s := &script{steps: []http.HandlerFunc{
			cutBody(http.StatusAccepted, `{"id":"7h2k9m4p`),
			answerJSON(http.StatusOK, replayJSON),
		}}
		c, _ := pollClient(t, s.ServeHTTP)
		created, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), key)
		if err != nil {
			t.Fatal(err)
		}
		if created.ID != "7h2k9m4p6r8t0v1w3x5y" || created.State != "running" || created.Status != http.StatusOK {
			t.Fatalf("Created = %+v, want the replay's identifier and state", created)
		}
		if len(s.requests) != 2 {
			t.Fatalf("%d requests, want the cut answer and the retry", len(s.requests))
		}
		if !s.sameKey(key) {
			t.Fatalf("Idempotency-Key = %q, want the one key on both requests, so the retry replays rather than creating a second Collection", s.keys())
		}
	})
	t.Run("a cut-off body is neither a transport failure nor a status", func(t *testing.T) {
		s := &script{steps: []http.HandlerFunc{cutBody(http.StatusAccepted, `{"id":"7h2k9m4p`)}}
		c, _ := pollClient(t, s.ServeHTTP)
		_, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), key)
		if !errors.Is(err, errAnswerIncomplete) {
			t.Fatalf("err = %v, want errAnswerIncomplete", err)
		}
		var te *TransportError
		if errors.As(err, &te) {
			t.Fatalf("err = %v is a TransportError: the answer's headers arrived, so a predicate over transport failures alone would miss it", err)
		}
		var se *StatusError
		if errors.As(err, &se) {
			t.Fatalf("err = %v is a StatusError: the gateway chose no refusing status", err)
		}
	})
	t.Run("a 503 then a 200 replay", func(t *testing.T) {
		s := &script{steps: []http.HandlerFunc{
			answerJSON(http.StatusServiceUnavailable, `{"error":"the store is unavailable","code":"pgo_unavailable"}`),
			answerJSON(http.StatusOK, replayJSON),
		}}
		c, clock := pollClient(t, s.ServeHTTP)
		created, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), key)
		if err != nil {
			t.Fatal(err)
		}
		if created.ID != "7h2k9m4p6r8t0v1w3x5y" || created.Status != http.StatusOK {
			t.Fatalf("Created = %+v, want the replay's identifier", created)
		}
		if len(clock.slept) != 1 || clock.slept[0] != time.Second {
			t.Fatalf("slept %v, want one second before the retry", clock.slept)
		}
	})
	t.Run("the schedule doubles from one second to eight inside the window", func(t *testing.T) {
		s := &script{steps: []http.HandlerFunc{func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		}}}
		c, clock := pollClient(t, s.ServeHTTP)
		start := clock.Now()
		_, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), key)
		var se *StatusError
		if !errors.As(err, &se) || se.Status != http.StatusBadGateway {
			t.Fatalf("err = %v, want the last 502 the window allowed", err)
		}
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 7 * time.Second}
		if !slices.Equal(clock.slept, want) {
			t.Fatalf("slept %v, want %v: one second doubling to eight, the last wait cut to the window", clock.slept, want)
		}
		if got := clock.Now().Sub(start); got != createRetryWindow {
			t.Fatalf("the clock advanced %s, want exactly %s", got, createRetryWindow)
		}
		if len(s.requests) != len(want)+1 {
			t.Fatalf("%d requests, want one per wait and the first", len(s.requests))
		}
		if !s.sameKey(key) {
			t.Fatalf("Idempotency-Key = %q, want the one key on every retry", s.keys())
		}
	})
}

func TestCreateDoesNotRetryAnAnswerThatArrivedWhole(t *testing.T) {
	const key = "3f0a1c7e-8b52-4d6a-9f11-2c4e6a8b0d31"
	tests := []struct {
		name string
		step http.HandlerFunc
	}{
		{name: "a 202 whose body is complete and does not decode", step: answerJSON(http.StatusAccepted, `<html>accepted</html>`)},
		{name: "429 collection_in_progress", step: answerJSON(http.StatusTooManyRequests, `{"error":"the service already has a live collection","code":"collection_in_progress"}`)},
		{name: "409 idempotency_mismatch", step: answerJSON(http.StatusConflict, `{"error":"the key names a different request","code":"idempotency_mismatch"}`)},
		{name: "400 invalid_parameter", step: answerJSON(http.StatusBadRequest, `{"error":"a collection request sets neither enabled nor schedule","code":"invalid_parameter"}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{steps: []http.HandlerFunc{tc.step}}
			c, clock := pollClient(t, s.ServeHTTP)
			if _, err := c.Create(context.Background(), "payments", "checkout", []byte(`{}`), key); err == nil {
				t.Fatal("no error from an answer that says something")
			}
			if len(s.requests) != 1 {
				t.Fatalf("%d requests, want one: an answer that arrived whole is not retried", len(s.requests))
			}
			if len(clock.slept) != 0 {
				t.Fatalf("slept %v, want nothing", clock.slept)
			}
		})
	}
}

func TestIsCollectionID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "twenty lowercase Crockford characters", id: "7h2k9m4p6r8t0v1w3x5y", want: true},
		{name: "all digits", id: "01234567890123456789", want: true},
		{name: "a Service address", id: "payments/checkout", want: false},
		{name: "nineteen characters", id: "7h2k9m4p6r8t0v1w3x5", want: false},
		{name: "twenty-one characters", id: "7h2k9m4p6r8t0v1w3x5y0", want: false},
		{name: "uppercase", id: "7H2K9M4P6R8T0V1W3X5Y", want: false},
		{name: "the letter i", id: "7h2k9m4p6r8t0v1w3x5i", want: false},
		{name: "the letter l", id: "7h2k9m4p6r8t0v1w3x5l", want: false},
		{name: "the letter o", id: "7h2k9m4p6r8t0v1w3x5o", want: false},
		{name: "the letter u", id: "7h2k9m4p6r8t0v1w3x5u", want: false},
		{name: "a traversal segment", id: "../../../../../../..", want: false},
		{name: "empty", id: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCollectionID(tc.id); got != tc.want {
				t.Fatalf("IsCollectionID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestCollectionPath(t *testing.T) {
	if got := CollectionPath("7h2k9m4p6r8t0v1w3x5y"); got != "/v1/collections/7h2k9m4p6r8t0v1w3x5y" {
		t.Fatalf("CollectionPath = %q, want /v1/collections/7h2k9m4p6r8t0v1w3x5y", got)
	}
}

// pollClient is a Client over handler on a fake clock, so a wait or a retry advances the clock instead of sleeping.
func pollClient(t *testing.T, handler http.HandlerFunc) (*Client, *fakeClock) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	clock := &fakeClock{now: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)}
	c, err := New(Options{Settings: settingsFor(t, srv.URL), Credential: tokenCredential("tok"), Now: clock.Now, Sleep: clock.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	return c, clock
}

// recordJSON is one record body with the fields Wait reads.
func recordJSON(state, reason string, round, rounds, ok, failed int) string {
	return fmt.Sprintf(`{"id":"7h2k9m4p6r8t0v1w3x5y","state":%q,"origin":"api","reason":%q,"progress":{"round":%d,"rounds":%d,"samplesOK":%d,"samplesFailed":%d}}`, state, reason, round, rounds, ok, failed)
}

// answers serves one JSON answer per request in order, repeating the last,
// and counts the requests it saw.
type answers struct {
	statuses []int
	bodies   []string
	requests []*http.Request
}

func (a *answers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i := min(len(a.requests), len(a.bodies)-1)
	a.requests = append(a.requests, r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.statuses[i])
	_, _ = w.Write([]byte(a.bodies[i]))
}

func ok(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = http.StatusOK
	}
	return s
}

func TestWait(t *testing.T) {
	t.Run("pending then running then completed polls at the interval", func(t *testing.T) {
		a := &answers{statuses: ok(3), bodies: []string{recordJSON("pending", "", 0, 0, 0, 0), recordJSON("running", "", 1, 2, 3, 0), recordJSON("completed", "", 2, 2, 6, 0)}}
		c, clock := pollClient(t, a.ServeHTTP)
		start := clock.Now()
		rec, body, err := c.Wait(context.Background(), "7h2k9m4p6r8t0v1w3x5y", 5*time.Second, time.Hour, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if rec.State != StateCompleted {
			t.Fatalf("state = %q, want completed", rec.State)
		}
		if string(body) != a.bodies[2] {
			t.Fatalf("body = %s, want the final record verbatim", body)
		}
		if len(a.requests) != 3 {
			t.Fatalf("%d requests, want three polls", len(a.requests))
		}
		for _, r := range a.requests {
			if r.Method != http.MethodGet || r.URL.Path != "/v1/collections/7h2k9m4p6r8t0v1w3x5y" {
				t.Fatalf("request = %s %s, want GET the record route", r.Method, r.URL.Path)
			}
		}
		if got := clock.Now().Sub(start); got != 10*time.Second {
			t.Fatalf("the clock advanced %s, want two intervals of 5s", got)
		}
	})
	t.Run("progress lines follow the record's progress", func(t *testing.T) {
		a := &answers{statuses: ok(4), bodies: []string{recordJSON("pending", "", 0, 0, 0, 0), recordJSON("running", "", 1, 2, 3, 0), recordJSON("running", "", 1, 2, 3, 0), recordJSON("completed", "", 2, 2, 6, 1)}}
		c, _ := pollClient(t, a.ServeHTTP)
		var progress bytes.Buffer
		if _, _, err := c.Wait(context.Background(), "7h2k9m4p6r8t0v1w3x5y", time.Second, time.Hour, &progress); err != nil {
			t.Fatal(err)
		}
		want := "round 1 of 2, 3 ok, 0 failed\nround 2 of 2, 6 ok, 1 failed\n"
		if progress.String() != want {
			t.Fatalf("progress = %q, want %q: one line per change, none before a round is claimed", progress.String(), want)
		}
	})
	for _, state := range []string{StateFailed, StateCancelled, StateExpired} {
		t.Run(state+" ends the wait as the record", func(t *testing.T) {
			a := &answers{statuses: ok(1), bodies: []string{recordJSON(state, "not_claimed", 0, 0, 0, 0)}}
			c, _ := pollClient(t, a.ServeHTTP)
			rec, _, err := c.Wait(context.Background(), "7h2k9m4p6r8t0v1w3x5y", time.Second, time.Hour, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if rec.State != state || rec.Reason != "not_claimed" {
				t.Fatalf("record = %+v, want state %s with its reason", rec, state)
			}
			if len(a.requests) != 1 {
				t.Fatalf("%d requests, want the one poll that saw the terminal state", len(a.requests))
			}
		})
	}
	t.Run("a state outside the seven stops the wait naming it", func(t *testing.T) {
		a := &answers{statuses: ok(1), bodies: []string{recordJSON("paused", "", 0, 0, 0, 0)}}
		c, _ := pollClient(t, a.ServeHTTP)
		_, _, err := c.Wait(context.Background(), "7h2k9m4p6r8t0v1w3x5y", time.Second, time.Hour, io.Discard)
		if !errors.Is(err, ErrUnknownState) || !strings.Contains(err.Error(), `"paused"`) {
			t.Fatalf("err = %v, want ErrUnknownState naming paused", err)
		}
		if len(a.requests) != 1 {
			t.Fatalf("%d requests, want one: an unknown state is not polled again", len(a.requests))
		}
	})
	t.Run("503 pgo_unavailable is retried", func(t *testing.T) {
		a := &answers{statuses: []int{http.StatusServiceUnavailable, http.StatusOK}, bodies: []string{`{"error":"the store is unavailable","code":"pgo_unavailable"}`, recordJSON("completed", "", 1, 1, 1, 0)}}
		c, clock := pollClient(t, a.ServeHTTP)
		start := clock.Now()
		rec, _, err := c.Wait(context.Background(), "7h2k9m4p6r8t0v1w3x5y", 3*time.Second, time.Hour, io.Discard)
		if err != nil || rec.State != StateCompleted {
			t.Fatalf("record = %+v, err = %v, want completed after the outage", rec, err)
		}
		if len(a.requests) != 2 || clock.Now().Sub(start) != 3*time.Second {
			t.Fatalf("%d requests and %s slept, want two polls one interval apart", len(a.requests), clock.Now().Sub(start))
		}
	})
	t.Run("404 collection_not_found ends the wait", func(t *testing.T) {
		a := &answers{statuses: []int{http.StatusNotFound}, bodies: []string{`{"error":"no collection 7h2k9m4p6r8t0v1w3x5y","code":"collection_not_found"}`}}
		c, _ := pollClient(t, a.ServeHTTP)
		_, _, err := c.Wait(context.Background(), "7h2k9m4p6r8t0v1w3x5y", time.Second, time.Hour, io.Discard)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != "collection_not_found" {
			t.Fatalf("err = %v, want the collection_not_found envelope", err)
		}
		if len(a.requests) != 1 {
			t.Fatalf("%d requests, want one: a missing record is not polled again", len(a.requests))
		}
	})
	t.Run("the timeout ends the wait on the clock", func(t *testing.T) {
		a := &answers{statuses: ok(1), bodies: []string{recordJSON("running", "", 1, 3, 2, 0)}}
		c, clock := pollClient(t, a.ServeHTTP)
		start := clock.Now()
		_, _, err := c.Wait(context.Background(), "7h2k9m4p6r8t0v1w3x5y", 10*time.Second, time.Minute, io.Discard)
		if !errors.Is(err, ErrWaitTimeout) {
			t.Fatalf("err = %v, want ErrWaitTimeout", err)
		}
		if got := clock.Now().Sub(start); got != time.Minute {
			t.Fatalf("the clock advanced %s, want exactly the timeout", got)
		}
		if len(a.requests) != 7 {
			t.Fatalf("%d requests, want seven: one at the start and one per interval through the deadline", len(a.requests))
		}
	})
	t.Run("a cancelled context ends the wait and sends nothing", func(t *testing.T) {
		a := &answers{statuses: ok(1), bodies: []string{recordJSON("running", "", 1, 3, 2, 0)}}
		c, _ := pollClient(t, a.ServeHTTP)
		ctx, cancel := context.WithCancel(context.Background())
		c.sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
		_, _, err := c.Wait(ctx, "7h2k9m4p6r8t0v1w3x5y", time.Second, time.Hour, io.Discard)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		for _, r := range a.requests {
			if r.Method != http.MethodGet {
				t.Fatalf("a %s %s was sent: an interrupt stops the watching, not the collecting", r.Method, r.URL.Path)
			}
		}
	})
}

func TestCancel(t *testing.T) {
	const initializing = `{"error":"the collection is not yet claimable","code":"collection_initializing"}`
	t.Run("posts with the JSON media type and no body", func(t *testing.T) {
		var got *http.Request
		var gotBody []byte
		c, _ := pollClient(t, func(w http.ResponseWriter, r *http.Request) {
			got = r
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(recordJSON("cancelled", "cancelled_by_api", 1, 3, 2, 0)))
		})
		body, err := c.Cancel(context.Background(), "7h2k9m4p6r8t0v1w3x5y")
		if err != nil {
			t.Fatal(err)
		}
		if got.Method != http.MethodPost || got.URL.Path != "/v1/collections/7h2k9m4p6r8t0v1w3x5y/cancel" {
			t.Fatalf("request = %s %s, want POST the cancel route", got.Method, got.URL.Path)
		}
		if got.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got.Header.Get("Content-Type"))
		}
		if len(gotBody) != 0 {
			t.Fatalf("body = %q, want none", gotBody)
		}
		rec, err := Decode[CollectionRecord](body)
		if err != nil || rec.State != StateCancelled {
			t.Fatalf("body = %s, err = %v, want the cancelled record", body, err)
		}
	})
	t.Run("409 collection_initializing is retried once a second for ten seconds", func(t *testing.T) {
		a := &answers{statuses: []int{http.StatusConflict}, bodies: []string{initializing}}
		c, clock := pollClient(t, a.ServeHTTP)
		start := clock.Now()
		_, err := c.Cancel(context.Background(), "7h2k9m4p6r8t0v1w3x5y")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != "collection_initializing" {
			t.Fatalf("err = %v, want the last collection_initializing envelope", err)
		}
		if got := clock.Now().Sub(start); got != 10*time.Second {
			t.Fatalf("the clock advanced %s, want ten seconds", got)
		}
		if len(a.requests) != 11 {
			t.Fatalf("%d requests, want the first and ten retries", len(a.requests))
		}
	})
	t.Run("409 collection_initializing then 200", func(t *testing.T) {
		a := &answers{statuses: []int{http.StatusConflict, http.StatusOK}, bodies: []string{initializing, recordJSON("cancelled", "cancelled_by_api", 0, 0, 0, 0)}}
		c, clock := pollClient(t, a.ServeHTTP)
		start := clock.Now()
		if _, err := c.Cancel(context.Background(), "7h2k9m4p6r8t0v1w3x5y"); err != nil {
			t.Fatal(err)
		}
		if len(a.requests) != 2 || clock.Now().Sub(start) != time.Second {
			t.Fatalf("%d requests and %s slept, want one retry after one second", len(a.requests), clock.Now().Sub(start))
		}
	})
	t.Run("409 collection_terminal is never retried", func(t *testing.T) {
		a := &answers{statuses: []int{http.StatusConflict}, bodies: []string{`{"error":"the collection already finished","code":"collection_terminal"}`}}
		c, clock := pollClient(t, a.ServeHTTP)
		start := clock.Now()
		_, err := c.Cancel(context.Background(), "7h2k9m4p6r8t0v1w3x5y")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != "collection_terminal" {
			t.Fatalf("err = %v, want the collection_terminal envelope", err)
		}
		if len(a.requests) != 1 || !clock.Now().Equal(start) {
			t.Fatalf("%d requests and %s slept, want one request and no wait", len(a.requests), clock.Now().Sub(start))
		}
	})
}
