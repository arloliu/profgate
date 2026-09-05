package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
	"github.com/arloliu/profgate/internal/metrics"
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/pgo"
)

// artifactBytes is the stored profile every download fixture serves.
// It spans many reads, so a stream a test interrupts has somewhere to be
// interrupted.
var artifactBytes = bytes.Repeat([]byte("merged-profile-"), 256)

// completedRecord is a Collection that finished and stored its profile,
// with the manifest a reader gets back.
func (p *pgoHarness) completedRecord(t *testing.T, mutate ...func(*pgo.Record)) pgo.Record {
	t.Helper()

	finished := pgoFixtureNow.Add(2 * time.Minute)
	expires := finished.Add(2 * time.Hour)
	rec := p.newRecord(pgo.StateCompleted, func(r *pgo.Record) {
		r.Attempt = 1
		r.ResolvedVersion = "1.42.3"
		r.FinishedAt = &finished
		r.ExpiresAt = &expires
		r.Artifact = &pgo.ArtifactRef{Object: r.ID + "-1.pprof", Bytes: int64(len(artifactBytes))}
		r.Manifest = &pgo.Manifest{
			Collection:      r.ID,
			Namespace:       fixtureNamespace,
			Service:         fixtureService,
			Profile:         "cpu",
			ResolvedVersion: "1.42.3",
			VersionLabel:    "app.kubernetes.io/version",
			Attempt:         1,
			Gateway:         fixtureInstance,
			Samples: []pgo.Sample{
				{Round: 0, Pod: fixturePod, PodUID: fixtureUID, Node: fixtureNode, Result: "ok", Bytes: 48211},
			},
		}
	})
	for _, m := range mutate {
		m(&rec)
	}
	p.seedRecord(t, rec)
	p.nats.artifacts.put(rec.Artifact.Object, artifactBytes)

	return rec
}

// TestCollectionCreate is the on-demand publication: one record, one active
// key, and a Location naming the Collection.
func TestCollectionCreate(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{"sampling":{"duration":"10s","rounds":1}}`, jsonType())

	if got.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", got.Code, got.Body.String())
	}
	var body acceptedBody
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not readable: %v", got.Body.String(), err)
	}
	if body.State != pgo.StatePending {
		t.Errorf("state = %q, want %q", body.State, pgo.StatePending)
	}
	if location := got.Header().Get("Location"); location != "/v1/collections/"+body.ID {
		t.Errorf("Location = %q, want the collection's own path", location)
	}

	rec := h.nats.jobs.record(t, body.ID)
	switch {
	case rec.Origin != pgo.OriginAPI:
		t.Errorf("origin = %q, want %q", rec.Origin, pgo.OriginAPI)
	case rec.CreatedBy != "anonymous":
		t.Errorf("createdBy = %q, want the requesting principal", rec.CreatedBy)
	case rec.Policy.Sampling.Rounds != 1 || rec.Policy.Sampling.Duration.Duration() != 10*time.Second:
		t.Errorf("policy = %+v, want the body's fields over the effective policy", rec.Policy.Sampling)
	case rec.Policy.Sampling.MaxParallel != testPGODefaults().Sampling.MaxParallel:
		t.Errorf("maxParallel = %d, want the default the body did not replace", rec.Policy.Sampling.MaxParallel)
	case !rec.ClaimBy.Equal(pgoFixtureNow.Add(pgo.APIClaimGrace)):
		t.Errorf("claimBy = %v, want now plus the on-demand grace", rec.ClaimBy)
	}
	if n := h.nats.jobs.countKeys(activeKeyPrefix); n != 1 {
		t.Errorf("active keys = %d, want 1", n)
	}
	if n := h.nats.jobs.countKeys(slotKeyPrefix); n != 0 {
		t.Errorf("slot keys = %d, want none: an on-demand collection takes no slot", n)
	}
	h.expectPGOAudit(t, http.StatusAccepted, codeOK)
}

// TestCollectionCreateResolvesTheDefaultPort proves the advisory target
// resolution of a create never names a port: a Collection profiles the
// configured default and nothing a client could choose.
func TestCollectionCreateResolvesTheDefaultPort(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{"sampling":{"duration":"10s","rounds":1}}`, jsonType())
	if got.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", got.Code, got.Body.String())
	}
	seen := h.disc.selectionsSeen()
	if len(seen) != 1 || seen[0] != (k8s.PortSelection{}) {
		t.Errorf("selections = %v, want exactly one zero PortSelection", seen)
	}
}

// TestCollectionCreateCarriesTheStoredRevision proves the snapshot is layered
// on the override the watched cache holds, and that the Collection records the
// revision it came from.
func TestCollectionCreateCarriesTheStoredRevision(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rounds := 3
	revision := h.seedOverride(t, &pgo.PolicyOverride{Sampling: &pgo.SamplingOverride{Rounds: &rounds}})

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, jsonType())

	var body acceptedBody
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not readable: %v", got.Body.String(), err)
	}
	rec := h.nats.jobs.record(t, body.ID)
	if rec.ConfigRevision != revision {
		t.Errorf("configRevision = %d, want %d", rec.ConfigRevision, revision)
	}
	if rec.Policy.Sampling.Rounds != rounds {
		t.Errorf("rounds = %d, want the stored override's %d", rec.Policy.Sampling.Rounds, rounds)
	}
}

// TestCollectionCreateRefusesAPolicyOverACeiling proves the snapshot is
// measured before anything is written.
func TestCollectionCreateRefusesAPolicyOverACeiling(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{"sampling":{"rounds":99}}`, jsonType())

	h.expectPGOError(t, got, http.StatusBadRequest, "limit_exceeded", "limit_exceeded")
	if n := h.nats.jobs.countKeys(jobKeyPrefix); n != 0 {
		t.Errorf("job keys = %d, want none written", n)
	}
}

// TestCollectionCreateTakesItsTokenFirst is the handler's ordering rule.
// The bucket is emptied before anything else a request does, so a caller with
// pgo.collect across many Services cannot make the gateway resolve targets for
// requests it is not allowed to create records for.
func TestCollectionCreateTakesItsTokenFirst(t *testing.T) {
	const services = 50
	limits := testPGOLimits(func(l *config.PGOLimits) { l.OnDemandPerMinute = 10 })
	h := newPGOHarness(t, pgoOpts{limits: limits})

	accepted, limited := 0, 0
	for i := range services {
		path := fmt.Sprintf("/v1/namespaces/%s/services/svc-%02d/collections", fixtureNamespace, i)
		got := h.doPGO(t, http.MethodPost, path, `{}`, jsonType())
		switch got.Code {
		case http.StatusAccepted:
			accepted++
		case http.StatusTooManyRequests:
			if code, _ := errorBodyOf(t, got); code != "rate_limited" {
				t.Fatalf("code = %q, want rate_limited", code)
			}
			limited++
		default:
			t.Fatalf("status = %d, want 202 or 429 (body %q)", got.Code, got.Body.String())
		}
	}

	if accepted != limits.OnDemandPerMinute || limited != services-limits.OnDemandPerMinute {
		t.Errorf("accepted = %d, rate limited = %d, want %d and %d",
			accepted, limited, limits.OnDemandPerMinute, services-limits.OnDemandPerMinute)
	}
	if n := h.nats.jobs.countKeys(activeKeyPrefix); n != limits.OnDemandPerMinute {
		t.Errorf("active keys = %d, want %d: a refused request writes nothing", n, limits.OnDemandPerMinute)
	}
	// The refused requests resolved no targets, which is what puts the bucket
	// ahead of the discovery pre-check rather than behind it.
	if got := int(h.disc.targetsCalls.Load()); got != limits.OnDemandPerMinute {
		t.Errorf("Targets calls = %d, want %d", got, limits.OnDemandPerMinute)
	}
}

// TestCollectionCreateRefillsItsBucket proves the bucket refills at
// onDemandPerMinute rather than being a per-process quota.
func TestCollectionCreateRefillsItsBucket(t *testing.T) {
	limits := testPGOLimits(func(l *config.PGOLimits) { l.OnDemandPerMinute = 1 })
	h := newPGOHarness(t, pgoOpts{limits: limits})

	if got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, jsonType()); got.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", got.Code)
	}
	second := "/v1/namespaces/" + fixtureNamespace + "/services/other-api/collections"
	if got := h.doPGO(t, http.MethodPost, second, `{}`, jsonType()); got.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", got.Code)
	}

	h.clock.advance(time.Minute)

	if got := h.doPGO(t, http.MethodPost, second, `{}`, jsonType()); got.Code != http.StatusAccepted {
		t.Errorf("status after a minute = %d, want 202", got.Code)
	}
}

// TestCollectionCreateResolvesTargetsBeforeWriting is the second half of the
// ordering rule: both version answers are given before anything is written, so
// a request that cannot succeed leaves no key and no reservation behind.
func TestCollectionCreateResolvesTargetsBeforeWriting(t *testing.T) {
	cases := []struct {
		name    string
		targets []k8s.Target
		body    string
		code    string
	}{
		{"two versions", []k8s.Target{namedTarget("a", "1.0"), namedTarget("b", "2.0")}, `{}`, "version_conflict"},
		{"no version labels", []k8s.Target{namedTarget("a", "")}, `{}`, "version_missing"},
		{
			"a pinned version nothing carries",
			[]k8s.Target{namedTarget("a", "1.0")},
			`{"target":{"version":"9.9"}}`,
			"version_missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{}, tc.targets...)

			got := h.doPGO(t, http.MethodPost, collectionsPath, tc.body, jsonType())

			h.expectPGOError(t, got, http.StatusConflict, tc.code, tc.code)
			for _, prefix := range []string{jobKeyPrefix, activeKeyPrefix, slotKeyPrefix} {
				if n := h.nats.jobs.countKeys(prefix); n != 0 {
					t.Errorf("%s keys = %d, want none written", prefix, n)
				}
			}
			// A reservation taken before the pre-check would count against the
			// live-Collection ceiling for a request that wrote nothing.
			if held := h.pub.Reserved(); held != 0 {
				t.Errorf("reservations = %d, want none taken", held)
			}
		})
	}
}

// TestVersionMissingSaysWhichCaseItIs proves one refusal code, two texts:
// no Pod carries a version label at all,
// and none carries the version the policy pins.
// The code is the contract, so only the pinned version is asserted in the text.
func TestVersionMissingSaysWhichCaseItIs(t *testing.T) {
	cases := []struct {
		name    string
		targets []k8s.Target
		body    string
		want    string
	}{
		{"no version labels", []k8s.Target{namedTarget("a", "")}, `{}`, "version label"},
		{
			"a pinned version nothing carries",
			[]k8s.Target{namedTarget("a", "1.0")},
			`{"target":{"version":"9.9"}}`,
			"9.9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{}, tc.targets...)

			got := h.doPGO(t, http.MethodPost, collectionsPath, tc.body, jsonType())

			code, message := errorBodyOf(t, got)
			if code != "version_missing" {
				t.Fatalf("code = %q, want version_missing", code)
			}
			if !strings.Contains(message, tc.want) {
				t.Errorf("message = %q, want it to mention %q", message, tc.want)
			}
		})
	}
}

// TestCollectionCreateAnswersDiscoveryAsTheGatewayDoes proves the pre-check
// reports a missing or selectorless Service the way every other route does.
func TestCollectionCreateAnswersDiscoveryAsTheGatewayDoes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"a service that does not exist", k8s.ErrServiceNotFound, http.StatusNotFound, "service_not_found"},
		{
			"a service without a selector", k8s.ErrServiceSelectorless,
			http.StatusUnprocessableEntity, "service_selectorless",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			h.disc.err = tc.err

			got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, jsonType())

			h.expectPGOError(t, got, tc.status, tc.code, tc.code)
			if n := h.nats.jobs.countKeys(jobKeyPrefix); n != 0 {
				t.Errorf("job keys = %d, want none written", n)
			}
		})
	}
}

// TestCollectionCreateWhileOneIsLive proves the cached answer: a Service the
// caches already show as live is refused without a write.
func TestCollectionCreateWhileOneIsLive(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StateRunning))
	h.seedActive(t, fixtureNamespace, fixtureService, rec.ID)

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, jsonType())

	h.expectPGOError(t, got, http.StatusTooManyRequests, "collection_in_progress", "collection_in_progress")
	if n := h.nats.jobs.countKeys(jobKeyPrefix); n != 1 {
		t.Errorf("job keys = %d, want only the live collection's", n)
	}
}

// TestRefusalsNameTheNextStep proves that a refusal whose remedy lives at another route says so:
// each of the four messages below keeps its code and gains a clause naming where to look,
// so a caller does not have to already know the gateway's shape to find it.
func TestRefusalsNameTheNextStep(t *testing.T) {
	cases := []struct {
		name   string
		code   string
		clause string
		do     func(t *testing.T) *httptest.ResponseRecorder
	}{
		{
			name:   "port_not_allowed",
			code:   "port_not_allowed",
			clause: "GET /v1/limits lists the admitted selections",
			do: func(t *testing.T) *httptest.ResponseRecorder {
				h := newHarness(baseTarget())

				return h.do(t, http.MethodGet, profilePath+"heap?port=7000")
			},
		},
		{
			name:   "no_targets",
			code:   "no_targets",
			clause: "GET /v1/namespaces/{namespace}/services/{service}/targets?explain=true counts the reasons",
			do: func(t *testing.T) *httptest.ResponseRecorder {
				h := newHarness()

				return h.do(t, http.MethodGet, profilePath+"heap")
			},
		},
		{
			name:   "collection_in_progress",
			code:   "collection_in_progress",
			clause: "GET /v1/namespaces/{namespace}/services/{service}/collections lists it",
			do: func(t *testing.T) *httptest.ResponseRecorder {
				h := newPGOHarness(t, pgoOpts{})
				rec := h.seedRecord(t, h.newRecord(pgo.StateRunning))
				h.seedActive(t, fixtureNamespace, fixtureService, rec.ID)

				return h.doPGO(t, http.MethodPost, collectionsPath, `{}`, jsonType())
			},
		},
		{
			name:   "pgo_disabled",
			code:   "pgo_disabled",
			clause: "the gateway's pgo.enabled is false",
			do: func(t *testing.T) *httptest.ResponseRecorder {
				h := newPGOHarness(t, pgoOpts{})
				h.configure(func(cfg *config.Config) { cfg.PGO.Enabled = false })

				return h.doPGO(t, http.MethodPost, collectionsPath, `{}`, jsonType())
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.do(t)

			code, message := errorBodyOf(t, rec)
			if code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
			want := "; " + tc.clause
			if !strings.HasSuffix(message, want) {
				t.Errorf("message = %q, want it to end in %q", message, want)
			}
		})
	}
}

// TestCollectionCreateAtTheLiveCeiling proves the live-Collection ceiling is
// answered before any write.
func TestCollectionCreateAtTheLiveCeiling(t *testing.T) {
	limits := testPGOLimits(func(l *config.PGOLimits) { l.MaxLiveCollections = 2 })
	h := newPGOHarness(t, pgoOpts{limits: limits})
	for i := range limits.MaxLiveCollections {
		service := fmt.Sprintf("busy-%02d", i)
		rec := h.seedRecord(t, h.newRecord(pgo.StateRunning, func(r *pgo.Record) { r.Service = service }))
		h.seedActive(t, fixtureNamespace, service, rec.ID)
	}

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, jsonType())

	h.expectPGOError(t, got, http.StatusTooManyRequests, "capacity_exhausted", "capacity_exhausted")
	if n := h.nats.jobs.countKeys(jobKeyPrefix); n != limits.MaxLiveCollections {
		t.Errorf("job keys = %d, want only the seeded ones", n)
	}
}

// TestConcurrentCollectionCreates proves the active key is the decision and
// the cache decides nothing: with the caches frozen so every request believes
// the Service is free, the Create settles it and exactly one request wins.
func TestConcurrentCollectionCreates(t *testing.T) {
	const requests = 8
	h := newPGOHarness(t, pgoOpts{})
	// The watches deliver nothing from here on, so every request reserves
	// against a cache that shows the Service as free.
	h.nats.jobs.freeze(jobKeyPrefix)
	h.nats.jobs.freeze(activeKeyPrefix)

	handler := h.handler()
	start := make(chan struct{})
	statuses := make([]int, requests)
	codes := make([]string, requests)

	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, collectionsPath,
				strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(rec, req)
			statuses[i] = rec.Code
			var body struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			codes[i] = body.Code
		}()
	}
	close(start)
	wg.Wait()

	accepted, busy := 0, 0
	for i, status := range statuses {
		switch {
		case status == http.StatusAccepted:
			accepted++
		case status == http.StatusTooManyRequests && codes[i] == "collection_in_progress":
			busy++
		default:
			t.Errorf("request %d = %d (%s), want 202 or 429 collection_in_progress", i, status, codes[i])
		}
	}
	if accepted != 1 || busy != requests-1 {
		t.Errorf("accepted = %d, busy = %d, want 1 and %d", accepted, busy, requests-1)
	}
	if n := h.nats.jobs.countKeys(activeKeyPrefix); n != 1 {
		t.Errorf("active keys = %d, want 1", n)
	}
	if n := h.nats.jobs.countKeys(jobKeyPrefix); n != 1 {
		t.Errorf("job keys = %d, want the losers to have discarded their own records", n)
	}
}

// TestCollectionList answers from the watched cache, newest first.
func TestCollectionList(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	older := h.seedRecord(t, h.newRecord(pgo.StateCompleted, func(r *pgo.Record) {
		r.CreatedAt = pgoFixtureNow
		r.Attempt = 1
		r.ResolvedVersion = "1.41.0"
	}))
	newer := h.seedRecord(t, h.newRecord(pgo.StateRunning, func(r *pgo.Record) {
		r.CreatedAt = pgoFixtureNow.Add(time.Minute)
		r.Origin = pgo.OriginAPI
	}))
	h.seedRecord(t, h.newRecord(pgo.StatePending, func(r *pgo.Record) { r.Service = "other-api" }))

	got := h.doPGO(t, http.MethodGet, collectionsPath, "", nil)

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	var body collectionsBody
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not readable: %v", got.Body.String(), err)
	}
	if len(body.Collections) != 2 {
		t.Fatalf("collections = %d, want the two of this Service: %+v", len(body.Collections), body.Collections)
	}
	if body.Collections[0].ID != newer.ID || body.Collections[1].ID != older.ID {
		t.Errorf("order = %s then %s, want newest first", body.Collections[0].ID, body.Collections[1].ID)
	}
	if body.Collections[0].Origin != pgo.OriginAPI || body.Collections[1].ResolvedVersion != "1.41.0" {
		t.Errorf("entries = %+v, want each record's own fields", body.Collections)
	}
}

// listBody runs one listing request and reads the body it answered with.
func (p *pgoHarness) listBody(t *testing.T, query string) collectionsBody {
	t.Helper()

	got := p.doPGO(t, http.MethodGet, collectionsPath+query, "", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	var body collectionsBody
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not readable: %v", got.Body.String(), err)
	}

	return body
}

// listIDs is the identifiers one page carried, in the order it carried them.
func listIDs(body collectionsBody) []string {
	out := make([]string, 0, len(body.Collections))
	for _, v := range body.Collections {
		out = append(out, v.ID)
	}

	return out
}

// listRefusal runs one listing request that is expected to be refused,
// and checks the item it earned.
func (p *pgoHarness) listRefusal(t *testing.T, query string, want errorDetail) {
	t.Helper()

	got := p.doPGO(t, http.MethodGet, collectionsPath+query, "", nil)
	p.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
	expectDetails(t, got, "invalid_parameter", []errorDetail{want})
}

// listFixture is the four records the filter cases are read against:
// two states, two origins, and two instants.
func (p *pgoHarness) listFixture(t *testing.T) (running, completed, api, old pgo.Record) {
	t.Helper()

	running = p.seedRecord(t, p.newRecord(pgo.StateRunning, func(r *pgo.Record) {
		r.CreatedAt = pgoFixtureNow.Add(3 * time.Minute)
	}))
	completed = p.seedRecord(t, p.newRecord(pgo.StateCompleted, func(r *pgo.Record) {
		r.CreatedAt = pgoFixtureNow.Add(2 * time.Minute)
	}))
	api = p.seedRecord(t, p.newRecord(pgo.StatePending, func(r *pgo.Record) {
		r.CreatedAt = pgoFixtureNow.Add(time.Minute)
		r.Origin = pgo.OriginAPI
	}))
	old = p.seedRecord(t, p.newRecord(pgo.StatePending, func(r *pgo.Record) {
		r.CreatedAt = pgoFixtureNow
	}))

	return running, completed, api, old
}

// TestCollectionListFilters keeps the records each filter names and drops the
// rest, and several filters at once keep their intersection.
func TestCollectionListFilters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query func(running, completed, api, old pgo.Record) string
		want  func(running, completed, api, old pgo.Record) []string
	}{
		{
			name:  "no parameter at all",
			query: func(_, _, _, _ pgo.Record) string { return "" },
			want: func(running, completed, api, old pgo.Record) []string {
				return []string{running.ID, completed.ID, api.ID, old.ID}
			},
		},
		{
			name:  "one state",
			query: func(_, _, _, _ pgo.Record) string { return "?state=running" },
			want:  func(running, _, _, _ pgo.Record) []string { return []string{running.ID} },
		},
		{
			name:  "a state repeated",
			query: func(_, _, _, _ pgo.Record) string { return "?state=running&state=running" },
			want:  func(running, _, _, _ pgo.Record) []string { return []string{running.ID} },
		},
		{
			name:  "two states",
			query: func(_, _, _, _ pgo.Record) string { return "?state=running&state=completed" },
			want: func(running, completed, _, _ pgo.Record) []string {
				return []string{running.ID, completed.ID}
			},
		},
		{
			name: "since an instant",
			query: func(_, _, _, _ pgo.Record) string {
				return "?since=" + url.QueryEscape(pgoFixtureNow.Add(2*time.Minute).Format(time.RFC3339))
			},
			want: func(running, completed, _, _ pgo.Record) []string {
				return []string{running.ID, completed.ID}
			},
		},
		{
			name:  "origin schedule",
			query: func(_, _, _, _ pgo.Record) string { return "?origin=schedule" },
			want: func(running, completed, _, old pgo.Record) []string {
				return []string{running.ID, completed.ID, old.ID}
			},
		},
		{
			name:  "origin api",
			query: func(_, _, _, _ pgo.Record) string { return "?origin=api" },
			want:  func(_, _, api, _ pgo.Record) []string { return []string{api.ID} },
		},
		{
			name:  "a limit",
			query: func(_, _, _, _ pgo.Record) string { return "?limit=1" },
			want:  func(running, _, _, _ pgo.Record) []string { return []string{running.ID} },
		},
		{
			name: "several filters at once",
			query: func(_, _, _, _ pgo.Record) string {
				return "?origin=schedule&since=" +
					url.QueryEscape(pgoFixtureNow.Add(time.Minute).Format(time.RFC3339)) +
					"&state=completed&state=pending"
			},
			want: func(_, completed, _, _ pgo.Record) []string { return []string{completed.ID} },
		},
		{
			name:  "an intersection that keeps nothing",
			query: func(_, _, _, _ pgo.Record) string { return "?origin=api&state=running" },
			want:  func(_, _, _, _ pgo.Record) []string { return nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			running, completed, api, old := h.listFixture(t)

			got := listIDs(h.listBody(t, tc.query(running, completed, api, old)))

			if want := tc.want(running, completed, api, old); !slices.Equal(got, want) {
				t.Errorf("collections = %v, want %v", got, want)
			}
		})
	}
}

// TestCollectionListEntriesGainNoField pins the shape of one entry:
// the fields of the record's listing view and nothing else,
// and no cursor key on a response that reached the end of the listing.
func TestCollectionListEntriesGainNoField(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	finished := pgoFixtureNow.Add(time.Minute)
	h.seedRecord(t, h.newRecord(pgo.StateCompleted, func(r *pgo.Record) {
		r.ResolvedVersion = "1.42.3"
		r.FinishedAt = &finished
		r.ExpiresAt = &finished
	}))

	got := h.doPGO(t, http.MethodGet, collectionsPath, "", nil)

	var body struct {
		Collections []map[string]any `json:"collections"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not readable: %v", got.Body.String(), err)
	}
	if len(body.Collections) != 1 {
		t.Fatalf("collections = %d, want 1", len(body.Collections))
	}
	want := []string{"attempt", "createdAt", "expiresAt", "finishedAt", "id", "origin", "resolvedVersion", "state"}
	if fields := slices.Sorted(maps.Keys(body.Collections[0])); !slices.Equal(fields, want) {
		t.Errorf("entry fields = %v, want %v", fields, want)
	}
	if strings.Contains(got.Body.String(), "nextCursor") {
		t.Errorf("body %q carries a cursor, want none on the last page", got.Body.String())
	}
}

// TestCollectionListRefusals gives each fault the item it earns,
// and reports the first fault in name order for a query carrying several.
func TestCollectionListRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  errorDetail
	}{
		{"an unknown name", "?bogus=1", errorDetail{Field: "bogus", Code: detailUnknownParameter}},
		{"an empty value", "?state=", errorDetail{Field: "state", Code: detailEmptyParameter}},
		{"an empty limit", "?limit=", errorDetail{Field: "limit", Code: detailEmptyParameter}},
		{"states as one comma-separated value", "?state=running,pending",
			errorDetail{Field: "state", Code: detailMalformedParameter}},
		{"a state outside the set", "?state=nonsense", errorDetail{Field: "state", Code: detailMalformedParameter}},
		{"an origin outside the set", "?origin=on-demand",
			errorDetail{Field: "origin", Code: detailMalformedParameter}},
		{"a since that is not a timestamp", "?since=yesterday",
			errorDetail{Field: "since", Code: detailMalformedParameter}},
		{"a limit below the floor", "?limit=0", errorDetail{Field: "limit", Code: detailMalformedParameter}},
		{"a limit above the ceiling", "?limit=101", errorDetail{Field: "limit", Code: detailMalformedParameter}},
		{"a limit that is not a number", "?limit=abc", errorDetail{Field: "limit", Code: detailMalformedParameter}},
		{"a repeated limit", "?limit=1&limit=2", errorDetail{Field: "limit", Code: detailRepeatedParameter}},
		{"a repeated since", "?since=" + pgoFixtureNow.Format(time.RFC3339) + "&since=" +
			pgoFixtureNow.Format(time.RFC3339), errorDetail{Field: "since", Code: detailRepeatedParameter}},
		{"a cursor that does not decode", "?cursor=notatoken",
			errorDetail{Field: "cursor", Code: detailMalformedParameter}},
		{"several faults, the unknown name first in name order", "?bogus=1&limit=0",
			errorDetail{Field: "bogus", Code: detailUnknownParameter}},
		{"the same faults sent the other way round", "?limit=0&bogus=1",
			errorDetail{Field: "bogus", Code: detailUnknownParameter}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newPGOHarness(t, pgoOpts{}).listRefusal(t, tc.query, tc.want)
		})
	}
}

// TestCollectionListLimitCeiling holds the grammar sentence to the page the
// cache builds, which is the ceiling the sentence names.
func TestCollectionListLimitCeiling(t *testing.T) {
	if !strings.Contains(limitGrammar, strconv.Itoa(pgo.MaxListCollections)) {
		t.Errorf("limit grammar %q does not name the ceiling %d", limitGrammar, pgo.MaxListCollections)
	}
}

// TestCollectionListCursorGrammar refuses a token this build cannot read: one
// that is not the encoding, one carrying a version it does not know, and one
// decoding to a value outside its own grammar.
func TestCollectionListCursorGrammar(t *testing.T) {
	position := pgo.CollectionPosition{CreatedAt: pgoFixtureNow, ID: newFixtureID()}
	payload := func(version byte, body string) string {
		return base64.RawURLEncoding.EncodeToString(append([]byte{version}, body...))
	}
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"not the encoding at all", "not a token"},
		{"an empty token", ""},
		{"a version this build does not know", payload('9',
			pgoFixtureNow.Format(time.RFC3339Nano)+"\n"+position.ID+"\n\n\n")},
		{"too few values", payload('1', pgoFixtureNow.Format(time.RFC3339Nano)+"\n"+position.ID)},
		{"a createdAt that is not a timestamp", payload('1', "yesterday\n"+position.ID+"\n\n\n")},
		{"an identifier outside its grammar", payload('1',
			pgoFixtureNow.Format(time.RFC3339Nano)+"\nnot-an-id\n\n\n")},
		{"a state outside the closed set", payload('1',
			pgoFixtureNow.Format(time.RFC3339Nano)+"\n"+position.ID+"\nnonsense\n\n")},
		{"an origin outside the closed set", payload('1',
			pgoFixtureNow.Format(time.RFC3339Nano)+"\n"+position.ID+"\n\n\non-demand")},
		{"a since that is not a timestamp", payload('1',
			pgoFixtureNow.Format(time.RFC3339Nano)+"\n"+position.ID+"\n\nyesterday\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := "?cursor=" + url.QueryEscape(tc.token)
			want := errorDetail{Field: "cursor", Code: detailMalformedParameter}
			if tc.token == "" {
				want.Code = detailEmptyParameter
			}
			newPGOHarness(t, pgoOpts{}).listRefusal(t, query, want)
		})
	}
}

// TestCollectionListCursorCarriesItsFilters holds a position to the listing it is a place in:
// the same filters continue it, whatever their spelling,
// and another filter set is refused rather than read against a listing the position does not belong to.
func TestCollectionListCursorCarriesItsFilters(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	running, completed, _, _ := h.listFixture(t)

	minted := "?state=pending&state=running&limit=1"
	first := h.listBody(t, minted)
	if got := listIDs(first); !slices.Equal(got, []string{running.ID}) {
		t.Fatalf("first page = %v, want %v", got, []string{running.ID})
	}
	if first.NextCursor == "" {
		t.Fatal("first page carries no cursor, want one: entries stand behind it")
	}
	cursor := "cursor=" + url.QueryEscape(first.NextCursor)

	// The token is client-visible,
	// so what it carries is read back as bytes and as values.
	raw, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil {
		t.Fatalf("the cursor is not the encoding: %v", err)
	}
	if strings.Contains(string(raw), fixtureIP) || strings.Contains(string(raw), strconv.Itoa(fixturePort)) {
		t.Errorf("cursor payload %q names a Pod address", raw)
	}
	position, carried, ok := decodeCursor(first.NextCursor)
	if !ok || position.ID != running.ID || !position.CreatedAt.Equal(running.CreatedAt) {
		t.Errorf("cursor names %+v, want the last entry the page carried", position)
	}
	if !carried.equal(listFilters{States: []pgo.State{pgo.StatePending, pgo.StateRunning}}) {
		t.Errorf("cursor filters = %+v, want the two states the request carried", carried)
	}

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"the filters it was minted under", "?state=pending&state=running&" + cursor},
		{"the same filter set in another order", "?state=running&state=pending&" + cursor},
		{"the same filter set with a value repeated", "?state=running&state=pending&state=running&" + cursor},
		{"a different limit", "?state=pending&state=running&limit=2&" + cursor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := listIDs(h.listBody(t, tc.query))
			if len(got) == 0 || got[0] == running.ID {
				t.Errorf("page = %v, want the entries after the first one", got)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"one of the states alone", "?state=running&" + cursor},
		{"another state set", "?state=completed&" + cursor},
		{"no state at all", "?" + cursor},
		{"a since the token does not carry", "?state=pending&state=running&since=" +
			pgoFixtureNow.Format(time.RFC3339) + "&" + cursor},
		{"an origin the token does not carry", "?state=pending&state=running&origin=schedule&" + cursor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A cursor whose filters differ is refused before any record is read,
			// so the refusal needs no listing behind it.
			newPGOHarness(t, pgoOpts{}).listRefusal(t, tc.query,
				errorDetail{Field: "cursor", Code: detailMalformedParameter})
		})
	}

	// A cursor minted under since and origin is read back under the same two.
	filtered := "?origin=schedule&since=" + url.QueryEscape(pgoFixtureNow.Format(time.RFC3339)) + "&limit=1"
	page := h.listBody(t, filtered)
	if page.NextCursor == "" {
		t.Fatal("the filtered page carries no cursor, want one")
	}
	got := listIDs(h.listBody(t, filtered+"&cursor="+url.QueryEscape(page.NextCursor)))
	if len(got) == 0 || got[0] != completed.ID {
		t.Errorf("page after the filtered cursor = %v, want it to start at %s", got, completed.ID)
	}
}

// TestCollectionListCursorCrossesReplicas mints a token against one gateway
// and consumes it on another.
// Replicas share no signing material and no cursor state,
// so a token one writes has to be readable by every other.
func TestCollectionListCursorCrossesReplicas(t *testing.T) {
	minter := newPGOHarness(t, pgoOpts{})
	records := make([]pgo.Record, 0, 3)
	for i := range 3 {
		records = append(records, minter.newRecord(pgo.StateCompleted, func(r *pgo.Record) {
			r.CreatedAt = pgoFixtureNow.Add(time.Duration(i) * time.Minute)
		}))
	}
	for _, rec := range records {
		minter.seedRecord(t, rec)
	}

	first := minter.listBody(t, "?limit=1")
	if first.NextCursor == "" {
		t.Fatal("first page carries no cursor, want one")
	}

	other := newPGOHarness(t, pgoOpts{})
	for _, rec := range records {
		other.seedRecord(t, rec)
	}

	got := listIDs(other.listBody(t, "?limit=1&cursor="+url.QueryEscape(first.NextCursor)))
	want := []string{records[1].ID}
	if !slices.Equal(got, want) {
		t.Errorf("page from the other replica = %v, want %v", got, want)
	}
}

// TestCollectionListPagesThroughEveryRecord walks a listing longer than one page,
// including records that share an instant,
// and sees each of them once.
// The pairs are placed so that one of them straddles the first page's boundary,
// which is the case the second sort key exists for.
func TestCollectionListPagesThroughEveryRecord(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})

	const records = 250
	want := make(map[string]struct{}, records)
	for i := range records {
		rec := h.seedRecord(t, h.newRecord(pgo.StateCompleted, func(r *pgo.Record) {
			// Instants shared in pairs,
			// offset so a pair spans the boundary between the first page and the second.
			r.CreatedAt = pgoFixtureNow.Add(time.Duration((i+1)/2) * time.Minute)
		}))
		want[rec.ID] = struct{}{}
	}

	seen := make(map[string]struct{}, records)
	query := ""
	pages := 0
	for {
		body := h.listBody(t, query)
		pages++
		for _, id := range listIDs(body) {
			if _, twice := seen[id]; twice {
				t.Fatalf("collection %s appears on two pages", id)
			}
			seen[id] = struct{}{}
		}
		if body.NextCursor == "" {
			break
		}
		query = "?cursor=" + url.QueryEscape(body.NextCursor)
		if pages > 5 {
			t.Fatal("the walk does not end")
		}
	}

	if pages != 3 {
		t.Errorf("pages = %d, want 3 for %d records at a page of %d", pages, records, pgo.MaxListCollections)
	}
	if len(seen) != len(want) {
		t.Fatalf("collections seen = %d, want %d", len(seen), len(want))
	}
	for id := range want {
		if _, ok := seen[id]; !ok {
			t.Errorf("collection %s was skipped", id)
		}
	}
}

// TestCollectionListPageIsUndisturbedByAHeadInsertion continues a walk across
// a record created between two pages.
// The order promises stability over the records the listing already held, and
// a newer record belongs at the head the client has already passed.
func TestCollectionListPageIsUndisturbedByAHeadInsertion(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	running, completed, api, old := h.listFixture(t)

	first := h.listBody(t, "?limit=2")
	if got := listIDs(first); !slices.Equal(got, []string{running.ID, completed.ID}) {
		t.Fatalf("first page = %v, want the two newest", got)
	}

	inserted := h.seedRecord(t, h.newRecord(pgo.StatePending, func(r *pgo.Record) {
		r.CreatedAt = pgoFixtureNow.Add(time.Hour)
	}))

	got := listIDs(h.listBody(t, "?limit=2&cursor="+url.QueryEscape(first.NextCursor)))
	if !slices.Equal(got, []string{api.ID, old.ID}) {
		t.Errorf("second page = %v, want %v", got, []string{api.ID, old.ID})
	}
	if slices.Contains(got, inserted.ID) {
		t.Errorf("second page carries %s, which was created after the walk started", inserted.ID)
	}
}

// TestCollectionListPageAfterADeletedPosition pages on
// across the records the sweeper took away between two reads,
// the one the cursor itself names included.
// The pair is the position and nothing looks its record up.
func TestCollectionListPageAfterADeletedPosition(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	running, completed, api, old := h.listFixture(t)

	first := h.listBody(t, "?limit=2")
	if got := listIDs(first); !slices.Equal(got, []string{running.ID, completed.ID}) {
		t.Fatalf("first page = %v, want the two newest", got)
	}

	// The record the cursor names, and one from the tail behind it.
	h.dropRecord(t, completed.ID)
	h.dropRecord(t, api.ID)

	got := listIDs(h.listBody(t, "?limit=2&cursor="+url.QueryEscape(first.NextCursor)))
	if !slices.Equal(got, []string{old.ID}) {
		t.Errorf("second page = %v, want %v: the entries after the position", got, []string{old.ID})
	}
}

// TestCollectionRead answers the record as stored, and nothing in it names a
// Pod's address.
func TestCollectionRead(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)

	got := h.doPGO(t, http.MethodGet, collectionPath(rec.ID, ""), "", nil)

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	var stored pgo.Record
	if err := json.Unmarshal(got.Body.Bytes(), &stored); err != nil {
		t.Fatalf("body %q is not readable: %v", got.Body.String(), err)
	}
	switch {
	case stored.ID != rec.ID:
		t.Errorf("id = %q, want %q", stored.ID, rec.ID)
	case stored.Manifest == nil || len(stored.Manifest.Samples) != 1:
		t.Fatalf("manifest = %+v, want the stored one with its samples", stored.Manifest)
	case stored.Manifest.Samples[0].Pod != fixturePod:
		t.Errorf("sample pod = %q, want %q", stored.Manifest.Samples[0].Pod, fixturePod)
	}
	audit := h.expectPGOAudit(t, http.StatusOK, codeOK)
	if audit["collection"] != rec.ID {
		t.Errorf("audit collection = %v, want %q", audit["collection"], rec.ID)
	}
}

// TestCollectionNotFoundIsOpaque proves a Collection the realm denies and one
// that does not exist answer identically, byte for byte.
func TestCollectionNotFoundIsOpaque(t *testing.T) {
	denied := wideRealm()
	denied.Namespaces = []string{"billing"}
	denied.PGO = config.RealmPGO{Read: true, Collect: true}

	missing := newPGOHarness(t, pgoOpts{})
	absent := missing.doPGO(t, http.MethodGet, collectionPath("abcdefghjkmnpqrstv01", ""), "", nil)
	missing.expectPGOError(t, absent, http.StatusNotFound, "collection_not_found", "collection_not_found")

	refused := newPGOHarness(t, pgoOpts{realm: &denied})
	rec := refused.seedRecord(t, refused.newRecord(pgo.StateCompleted))
	forbidden := refused.doPGO(t, http.MethodGet, collectionPath(rec.ID, ""), "", nil)
	refused.expectPGOError(t, forbidden, http.StatusNotFound, "collection_not_found", "collection_not_found")

	if absent.Body.String() != forbidden.Body.String() {
		t.Errorf("bodies differ:\nmissing %q\ndenied  %q", absent.Body.String(), forbidden.Body.String())
	}
}

// TestCollectionDownload streams the stored profile with the headers a build
// needs to know what it got.
func TestCollectionDownload(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)

	got := h.doPGO(t, http.MethodGet, collectionPath(rec.ID, "/profile"), "", nil)

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if !bytes.Equal(got.Body.Bytes(), artifactBytes) {
		t.Errorf("body is %d bytes, want the stored object's %d", got.Body.Len(), len(artifactBytes))
	}
	header := got.Header()
	switch {
	case header.Get("Content-Type") != "application/octet-stream":
		t.Errorf("Content-Type = %q", header.Get("Content-Type"))
	case header.Get("Content-Disposition") != `attachment; filename="`+rec.ID+`.pprof"`:
		t.Errorf("Content-Disposition = %q", header.Get("Content-Disposition"))
	case header.Get("X-Pprof-Collection") != rec.ID:
		t.Errorf("X-Pprof-Collection = %q, want %q", header.Get("X-Pprof-Collection"), rec.ID)
	case header.Get("X-Pprof-Target-Version") != "1.42.3":
		t.Errorf("X-Pprof-Target-Version = %q, want the resolved version", header.Get("X-Pprof-Target-Version"))
	}
	h.expectPGOAudit(t, http.StatusOK, codeOK)
}

// TestCollectionDownloadStates is the download table: only a completed
// Collection with its object has a profile to give.
func TestCollectionDownloadStates(t *testing.T) {
	cases := []struct {
		state  pgo.State
		status int
		code   string
	}{
		{pgo.StateInitializing, http.StatusConflict, "collection_not_completed"},
		{pgo.StatePending, http.StatusConflict, "collection_not_completed"},
		{pgo.StateRunning, http.StatusConflict, "collection_not_completed"},
		{pgo.StateFailed, http.StatusConflict, "collection_not_completed"},
		{pgo.StateCancelled, http.StatusConflict, "collection_not_completed"},
		{pgo.StateExpired, http.StatusGone, "artifact_gone"},
	}

	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			rec := h.seedRecord(t, h.newRecord(tc.state))

			got := h.doPGO(t, http.MethodGet, collectionPath(rec.ID, "/profile"), "", nil)

			h.expectPGOError(t, got, tc.status, tc.code, tc.code)
			if state := h.nats.jobs.record(t, rec.ID).State; state != tc.state {
				t.Errorf("state = %q, want the record untouched", state)
			}
		})
	}
}

// TestCollectionDownloadFlipsAMissingArtifact proves the reader owns the
// transition it wins: a download does not protect its object from expiry, and
// whichever reader's conditional update wins emits the one log record and the
// one metric row for it.
func TestCollectionDownloadFlipsAMissingArtifact(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)
	if err := h.nats.artifacts.Delete(context.Background(), rec.Artifact.Object); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got := h.doPGO(t, http.MethodGet, collectionPath(rec.ID, "/profile"), "", nil)

	h.expectPGOError(t, got, http.StatusGone, "artifact_gone", "artifact_gone")
	if state := h.nats.jobs.record(t, rec.ID).State; state != pgo.StateExpired {
		t.Errorf("state = %q, want %q", state, pgo.StateExpired)
	}
	if rows := h.rec.collectionRows(); len(rows) != 1 || rows[0] != string(pgo.StateExpired) {
		t.Errorf("Collection rows = %v, want exactly one expired", rows)
	}
	if records := h.transitions(t); len(records) != 1 || records[0]["state"] != string(pgo.StateExpired) {
		t.Errorf("transition records = %v, want exactly one expired", records)
	}
}

// TestConcurrentDownloadsFlipOnce proves one owner per winning conditional
// update: two readers of one completed record whose object is gone both answer
// 410, and only the one whose update won records the transition.
func TestConcurrentDownloadsFlipOnce(t *testing.T) {
	const readers = 2
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)
	if err := h.nats.artifacts.Delete(context.Background(), rec.Artifact.Object); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Both readers take the same revision before either writes, which is the
	// race the ownership rule is about.
	var arrived sync.WaitGroup
	arrived.Add(readers)
	h.nats.jobs.afterGet = func() {
		arrived.Done()
		arrived.Wait()
	}

	handler := h.handler()
	var wg sync.WaitGroup
	statuses := make([]int, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
				collectionPath(rec.ID, "/profile"), nil)
			handler.ServeHTTP(out, req)
			statuses[i] = out.Code
		}()
	}
	wg.Wait()

	for i, status := range statuses {
		if status != http.StatusGone {
			t.Errorf("reader %d = %d, want 410", i, status)
		}
	}
	if rows := h.rec.collectionRows(); len(rows) != 1 {
		t.Errorf("Collection rows = %v, want exactly one from the winning update", rows)
	}
	if records := h.transitions(t); len(records) != 1 {
		t.Errorf("transition records = %d, want exactly one from the winning update", len(records))
	}
}

// TestCollectionDownloadStoreUnavailable proves a store failure before the
// headers is the client's 503, not a truncated body.
func TestCollectionDownloadStoreUnavailable(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)
	h.nats.artifacts.openErr = natskv.ErrUnavailable

	got := h.doPGO(t, http.MethodGet, collectionPath(rec.ID, "/profile"), "", nil)

	h.expectPGOError(t, got, http.StatusServiceUnavailable, "pgo_unavailable", "pgo_unavailable")
	if state := h.nats.jobs.record(t, rec.ID).State; state != pgo.StateCompleted {
		t.Errorf("state = %q, want the record untouched by an unreachable store", state)
	}
}

// TestCollectionDownloadFailingAfterHeaders proves the client sees a
// truncation rather than a cleanly finished body, and the operator sees why.
func TestCollectionDownloadFailingAfterHeaders(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)
	h.nats.artifacts.readErr = errors.New("the object expired mid-stream")
	h.nats.artifacts.failAfter = chunkBytes

	gateway := httptest.NewServer(h.handler())
	t.Cleanup(gateway.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		gateway.URL+collectionPath(rec.ID, "/profile"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200: the failure came after the headers", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ReadAll() error = %v (body %q), want io.ErrUnexpectedEOF", err, body)
	}
	waitForAudit(t, h.harness, codeArtifactStreamFail)
	h.expectMetricCode(t, codeArtifactStreamFail)
}

// TestCollectionDownloadClientGone proves a client that leaves mid-stream
// cancels the store read rather than being served to the end.
// The store parks after the first chunk and the test never releases it,
// so the server cannot finish the object before the cancellation lands.
func TestCollectionDownloadClientGone(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)
	h.nats.artifacts.gate = make(chan struct{})

	gateway := httptest.NewServer(h.handler())
	t.Cleanup(gateway.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL+collectionPath(rec.ID, "/profile"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, chunkBytes)); err != nil {
		t.Fatalf("ReadFull() error = %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	waitForAudit(t, h.harness, codeClientGone)
	reader := h.nats.artifacts.reader()
	if reader == nil || !reader.closed.Load() {
		t.Error("the store read was not released when the client left")
	}
	if got := int(reader.reads.Load()) * chunkBytes; got >= len(artifactBytes) {
		t.Errorf("the store read %d bytes of %d, want it to have stopped early", got, len(artifactBytes))
	}
}

// TestCollectionDownloadFollowsASlowClient proves a download is bounded by its request and nothing shorter:
// a client that drains the body slower than the socket fills it is served to the end,
// because the handler hands the store the request's context, which carries no deadline,
// and the store's own deadline covers each wait for a chunk rather than the transfer.
func TestCollectionDownloadFollowsASlowClient(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	body := bytes.Repeat([]byte("slow-client-"), (256<<10)/12)
	rec := h.completedRecord(t, func(r *pgo.Record) { r.Artifact.Bytes = int64(len(body)) })
	h.nats.artifacts.put(rec.Artifact.Object, body)

	gateway := httptest.NewServer(h.handler())
	t.Cleanup(gateway.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, gateway.URL+collectionPath(rec.ID, "/profile"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// 4 KiB every 20 milliseconds: the socket's buffers fill and the handler's copy waits on the client.
	got := make([]byte, 0, len(body))
	buf := make([]byte, 4<<10)
	for {
		n, err := resp.Body.Read(buf)
		got = append(got, buf[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read after %d bytes: %v", len(got), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body: got %d bytes, want %d", len(got), len(body))
	}

	reader := h.nats.artifacts.reader()
	if reader == nil {
		t.Fatal("the store handed out no reader")
	}
	if deadline, ok := reader.ctx.Deadline(); ok {
		t.Errorf("the store read ran under a deadline at %v, want the request's context and nothing shorter", deadline)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !reader.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the store read was not released after the body was served")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestADownloadCutByTheDrainIsTruncated proves the drain bound ending mid-download truncates the body
// and is audited as the cut it is, rather than as a client that left.
// The store parks after the first chunk and the test never releases it,
// so only the cut ends the stream.
func TestADownloadCutByTheDrainIsTruncated(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)
	h.nats.artifacts.gate = make(chan struct{})
	srv, cut := cutServer(t, h.handler())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+collectionPath(rec.ID, "/profile"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the cut lands after the headers", resp.StatusCode)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, chunkBytes)); err != nil {
		t.Fatalf("ReadFull() error = %v", err)
	}
	cut()

	rest, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ReadAll() error = %v (%d more bytes), want io.ErrUnexpectedEOF: the client must see a truncation, not a clean end", err, len(rest))
	}
	waitForAudit(t, h.harness, codeDrainExpired)
	h.expectMetricCode(t, codeDrainExpired)
	if reader := h.nats.artifacts.reader(); reader == nil || !reader.closed.Load() {
		t.Error("the store read was not released when the request was cut")
	}
}

// TestAStoreCallHeldAcrossTheDrainCutWritesNothing proves the cut reaches a request inside a store call.
// The cancelled call maps to a 503 the request is not answered:
// nothing is written, and the audit record carries the cut.
func TestAStoreCallHeldAcrossTheDrainCutWritesNothing(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))
	started := h.nats.jobs.blockGets()
	srv, cut := cutServer(t, h.handler())

	conn := dialRequest(t, srv, collectionPath(rec.ID, ""))
	// The cut lands with the request inside the store call, not before it reaches the store.
	select {
	case <-started:
	case <-time.After(heldOpenTimeout):
		t.Fatal("the request did not reach the store within the wait")
	}
	cut()

	expectNothingRead(t, conn)
	waitForAudit(t, h.harness, codeDrainExpired)
	h.expectPGOAudit(t, 0, codeDrainExpired)
	h.expectMetricCode(t, codeDrainExpired)
}

// TestAStoreCallHeldWhenTheClientLeavesIsClientGone is the ordinary-disconnect counterpart of the cut:
// the client closes its socket with the request inside the store call.
// The cancelled call maps to a 503 nobody is there to read;
// the record says the client left, and nothing is written.
func TestAStoreCallHeldWhenTheClientLeavesIsClientGone(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))
	started := h.nats.jobs.blockGets()
	counter := &answerCounter{Handler: h.handler()}
	srv, _ := cutServer(t, counter)

	conn := dialRequest(t, srv, collectionPath(rec.ID, ""))
	select {
	case <-started:
	case <-time.After(heldOpenTimeout):
		t.Fatal("the request did not reach the store within the wait")
	}
	_ = conn.Close()

	waitForAudit(t, h.harness, codeClientGone)
	h.expectPGOAudit(t, 0, codeClientGone)
	h.expectMetricCode(t, codeClientGone)
	if counter.answered.Load() {
		t.Error("the handler answered a client that had left: the cancelled store call must not become an envelope")
	}
}

// waitForAudit blocks until one audit record has been written and checks its code.
func waitForAudit(t *testing.T, h *harness, code string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		records := h.audits(t)
		if len(records) == 1 {
			if records[0]["code"] != code {
				t.Errorf("audit code = %v, want %q", records[0]["code"], code)
			}

			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit records = %d, want 1", len(records))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The two Service-scoped routes that answer for the newest completed Collection of a Service.
const (
	latestPath        = collectionsPath + "/latest"
	latestProfilePath = collectionsPath + "/latest/profile"
)

// completedAt is a completed Collection with its artifact stored, created at one instant.
// The instant is what orders the candidates a latest walk considers.
func (p *pgoHarness) completedAt(t *testing.T, created time.Time) pgo.Record {
	t.Helper()

	return p.completedRecord(t, func(r *pgo.Record) { r.CreatedAt = created })
}

// dropArtifact removes one Collection's stored object,
// leaving a completed record naming bytes the store no longer holds.
func (p *pgoHarness) dropArtifact(t *testing.T, rec pgo.Record) {
	t.Helper()

	if err := p.nats.artifacts.Delete(context.Background(), rec.Artifact.Object); err != nil {
		t.Fatalf("Delete %s: %v", rec.Artifact.Object, err)
	}
}

// latestID is the identifier the record route answered with.
func latestID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var stored pgo.Record
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("body %q is not readable: %v", rec.Body.String(), err)
	}

	return stored.ID
}

// TestLatestCollection answers for the newest completed Collection of a
// Service, past the newer records that have no profile to give.
func TestLatestCollection(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	h.completedAt(t, pgoFixtureNow)
	newest := h.completedAt(t, pgoFixtureNow.Add(time.Minute))
	for _, state := range []pgo.State{pgo.StateFailed, pgo.StateRunning, pgo.StateExpired} {
		h.seedRecord(t, h.newRecord(state, func(r *pgo.Record) {
			r.CreatedAt = pgoFixtureNow.Add(2 * time.Minute)
		}))
	}

	got := h.doPGO(t, http.MethodGet, latestPath, "", nil)

	if id := latestID(t, got); id != newest.ID {
		t.Errorf("id = %q, want the newest completed collection %q", id, newest.ID)
	}
	audit := h.expectPGOAudit(t, http.StatusOK, codeOK)
	if audit["collection"] != newest.ID {
		t.Errorf("audit collection = %v, want %q", audit["collection"], newest.ID)
	}
	h.expectMetric(t, metrics.EndpointCollection, labelCPU)
}

// TestLatestCollectionWritesTheRecordAsStored proves the record route writes the record as stored,
// exactly as GET /v1/collections/{id} writes it.
func TestLatestCollectionWritesTheRecordAsStored(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)

	latest := h.doPGO(t, http.MethodGet, latestPath, "", nil)
	direct := h.doPGO(t, http.MethodGet, collectionPath(rec.ID, ""), "", nil)

	if latest.Code != http.StatusOK || direct.Code != http.StatusOK {
		t.Fatalf("statuses = %d and %d, want 200 (bodies %q and %q)",
			latest.Code, direct.Code, latest.Body.String(), direct.Body.String())
	}
	if latest.Body.String() != direct.Body.String() {
		t.Errorf("bodies differ:\nlatest %q\ndirect %q", latest.Body.String(), direct.Body.String())
	}
}

// TestLatestProfileStreamsWhatTheIdentifierRouteStreams proves one thing:
// the profile route answers the bytes and the headers of the download it stands for.
func TestLatestProfileStreamsWhatTheIdentifierRouteStreams(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)

	latest := h.doPGO(t, http.MethodGet, latestProfilePath, "", nil)
	direct := h.doPGO(t, http.MethodGet, collectionPath(rec.ID, "/profile"), "", nil)

	if latest.Code != http.StatusOK || direct.Code != http.StatusOK {
		t.Fatalf("statuses = %d and %d, want 200", latest.Code, direct.Code)
	}
	if !bytes.Equal(latest.Body.Bytes(), artifactBytes) {
		t.Errorf("body is %d bytes, want the stored object's %d", latest.Body.Len(), len(artifactBytes))
	}
	for _, name := range []string{"Content-Type", "Content-Disposition", "X-Pprof-Collection", "X-Pprof-Target-Version"} {
		if got, want := latest.Header().Get(name), direct.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestLatestRoutesAnswerForOneCollection proves the two routes run one selection:
// the record one answers with is the record whose bytes the other streams,
// even when the newest completed record has lost its object.
func TestLatestRoutesAnswerForOneCollection(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	older := h.completedAt(t, pgoFixtureNow)
	newest := h.completedAt(t, pgoFixtureNow.Add(time.Minute))
	h.dropArtifact(t, newest)

	record := h.doPGO(t, http.MethodGet, latestPath, "", nil)
	profile := h.doPGO(t, http.MethodGet, latestProfilePath, "", nil)

	if profile.Code != http.StatusOK {
		t.Fatalf("profile status = %d, want 200 (body %q)", profile.Code, profile.Body.String())
	}
	streamed := profile.Header().Get("X-Pprof-Collection")
	if id := latestID(t, record); id != streamed || id != older.ID {
		t.Errorf("record answered %q and the profile streamed %q, want %q for both", id, streamed, older.ID)
	}
}

// TestLatestCollectionNotFound is the one answer for a Service with nothing to hand a build:
// no records at all,
// none completed,
// and one whose only completed Collection has expired.
func TestLatestCollectionNotFound(t *testing.T) {
	fixtures := []struct {
		name string
		seed func(t *testing.T, h *pgoHarness)
	}{
		{"no records at all", func(*testing.T, *pgoHarness) {}},
		{"no completed record", func(t *testing.T, h *pgoHarness) {
			h.seedRecord(t, h.newRecord(pgo.StateFailed))
			h.seedRecord(t, h.newRecord(pgo.StateRunning))
		}},
		{"the only completed record expired", func(t *testing.T, h *pgoHarness) {
			h.seedRecord(t, h.newRecord(pgo.StateExpired))
		}},
	}
	routes := map[string]string{"record": latestPath, "profile": latestProfilePath}

	for _, f := range fixtures {
		for route, path := range routes {
			t.Run(f.name+" "+route, func(t *testing.T) {
				h := newPGOHarness(t, pgoOpts{})
				f.seed(t, h)

				got := h.doPGO(t, http.MethodGet, path, "", nil)

				h.expectPGOError(t, got, http.StatusNotFound, "collection_not_found", "collection_not_found")
			})
		}
	}
}

// TestLatestCollectionWalksPastAGoneObject proves two things:
// the selection confirms the object rather than trusting the newest cached entry,
// and the reader that finds it gone owns the transition it commits.
func TestLatestCollectionWalksPastAGoneObject(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	older := h.completedAt(t, pgoFixtureNow)
	newest := h.completedAt(t, pgoFixtureNow.Add(time.Minute))
	h.dropArtifact(t, newest)

	got := h.doPGO(t, http.MethodGet, latestPath, "", nil)

	if id := latestID(t, got); id != older.ID {
		t.Errorf("id = %q, want the completed collection behind the gone one, %q", id, older.ID)
	}
	if state := h.nats.jobs.record(t, newest.ID).State; state != pgo.StateExpired {
		t.Errorf("state of the gone collection = %q, want %q", state, pgo.StateExpired)
	}
	if rows := h.rec.collectionRows(); len(rows) != 1 || rows[0] != string(pgo.StateExpired) {
		t.Errorf("Collection rows = %v, want exactly one expired", rows)
	}
	if records := h.transitions(t); len(records) != 1 {
		t.Errorf("transition records = %d, want exactly one", len(records))
	}
}

// TestLatestCollectionWalksPastTwoGoneObjects proves the walk keeps going:
// it passes two newest completed records that have both lost their bytes,
// and flips them both.
func TestLatestCollectionWalksPastTwoGoneObjects(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	intact := h.completedAt(t, pgoFixtureNow)
	first := h.completedAt(t, pgoFixtureNow.Add(time.Minute))
	second := h.completedAt(t, pgoFixtureNow.Add(2*time.Minute))
	h.dropArtifact(t, first)
	h.dropArtifact(t, second)

	got := h.doPGO(t, http.MethodGet, latestPath, "", nil)

	if id := latestID(t, got); id != intact.ID {
		t.Errorf("id = %q, want the collection behind both gone ones, %q", id, intact.ID)
	}
	for _, rec := range []pgo.Record{first, second} {
		if state := h.nats.jobs.record(t, rec.ID).State; state != pgo.StateExpired {
			t.Errorf("state of %s = %q, want %q", rec.ID, state, pgo.StateExpired)
		}
	}
	if rows := h.rec.collectionRows(); len(rows) != 2 {
		t.Errorf("Collection rows = %v, want one per flip", rows)
	}
}

// TestLatestCollectionSkipsAStaleCandidate proves the watched cache is a candidate filter
// and never the authority.
// The cache stops moving while the bucket does not,
// so its newest completed entry stands for a record that is no longer completed;
// the fresh read is what decides,
// and the walk answers with the Collection behind it.
func TestLatestCollectionSkipsAStaleCandidate(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	older := h.completedAt(t, pgoFixtureNow)
	newest := h.completedAt(t, pgoFixtureNow.Add(time.Minute))

	h.nats.jobs.freeze(jobKeyPrefix)
	expired := newest
	expired.State = pgo.StateExpired
	h.nats.jobs.put(t, jobKeyPrefix+newest.ID, expired)
	if state := h.cachedState(newest.ID); state != pgo.StateCompleted {
		t.Fatalf("the cache shows %q for the newest collection, want the stale %q", state, pgo.StateCompleted)
	}

	got := h.doPGO(t, http.MethodGet, latestPath, "", nil)

	if id := latestID(t, got); id != older.ID {
		t.Errorf("id = %q, want %q: the cached state is not the authority", id, older.ID)
	}
}

// TestLatestCollectionCostsOneReadPerCandidate proves the walk reads each candidate once
// and stops at the first that survives.
// The cache holds three completed entries and the bucket holds one,
// so the two it passes are the two the fresh reads discard.
func TestLatestCollectionCostsOneReadPerCandidate(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	survivor := h.completedAt(t, pgoFixtureNow)
	discarded := []pgo.Record{
		h.completedAt(t, pgoFixtureNow.Add(time.Minute)),
		h.completedAt(t, pgoFixtureNow.Add(2*time.Minute)),
	}

	h.nats.jobs.freeze(jobKeyPrefix)
	for _, rec := range discarded {
		rec.State = pgo.StateFailed
		h.nats.jobs.put(t, jobKeyPrefix+rec.ID, rec)
	}

	before := h.storeCalls()
	got := h.doPGO(t, http.MethodGet, latestPath, "", nil)
	calls := h.storeCalls() - before

	if id := latestID(t, got); id != survivor.ID {
		t.Errorf("id = %q, want %q", id, survivor.ID)
	}
	if calls != 3 {
		t.Errorf("store calls = %d, want one read per candidate the walk reached", calls)
	}
}

// TestLatestCollectionStoreUnavailable proves what a store the gateway cannot read says:
// nothing about which artifact is newest,
// so the walk refuses rather than falling through to an older Collection.
func TestLatestCollectionStoreUnavailable(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	h.completedAt(t, pgoFixtureNow)
	h.completedAt(t, pgoFixtureNow.Add(time.Minute))
	h.nats.jobs.setGetErr(natskv.ErrUnavailable)

	got := h.doPGO(t, http.MethodGet, latestPath, "", nil)

	h.expectPGOError(t, got, http.StatusServiceUnavailable, "pgo_unavailable", "pgo_unavailable")
}

// TestLatestCollectionReleasesTheReader proves the record route opens the object once,
// which is what confirms it,
// and closes it before it answers.
func TestLatestCollectionReleasesTheReader(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)

	got := h.doPGO(t, http.MethodGet, latestPath, "", nil)

	if id := latestID(t, got); id != rec.ID {
		t.Errorf("id = %q, want %q", id, rec.ID)
	}
	if opens := h.nats.artifacts.opens.Load(); opens != 1 {
		t.Errorf("object opens = %d, want the one the walk's confirmation costs", opens)
	}
	if reader := h.nats.artifacts.reader(); reader == nil || !reader.closed.Load() {
		t.Error("the reader the walk opened was not released")
	}
}

// TestLatestProfileStreamsTheConfirmedReader proves the profile route streams
// the reader the walk opened.
// The object goes the moment it has been opened,
// which a second open would answer 410 artifact_gone for
// while the completed Collection behind it still has its bytes.
func TestLatestProfileStreamsTheConfirmedReader(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)
	h.nats.artifacts.afterOpen = func(name string) {
		if err := h.nats.artifacts.Delete(context.Background(), name); err != nil {
			t.Errorf("Delete %s: %v", name, err)
		}
	}

	got := h.doPGO(t, http.MethodGet, latestProfilePath, "", nil)

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if !bytes.Equal(got.Body.Bytes(), artifactBytes) {
		t.Errorf("body is %d bytes, want the stored object's %d", got.Body.Len(), len(artifactBytes))
	}
	if opens := h.nats.artifacts.opens.Load(); opens != 1 {
		t.Errorf("object opens = %d, want the walk's one", opens)
	}
	if state := h.nats.jobs.record(t, rec.ID).State; state != pgo.StateCompleted {
		t.Errorf("state = %q, want the record left completed", state)
	}
	audit := h.expectPGOAudit(t, http.StatusOK, codeOK)
	if audit["collection"] != rec.ID {
		t.Errorf("audit collection = %v, want %q", audit["collection"], rec.ID)
	}
}

// TestLatestProfileFailingAfterHeaders is the ordinary truncation:
// an object that goes away mid-stream is what any other download sees.
func TestLatestProfileFailingAfterHeaders(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	h.completedRecord(t)
	h.nats.artifacts.readErr = errors.New("the object expired mid-stream")
	h.nats.artifacts.failAfter = chunkBytes

	gateway := httptest.NewServer(h.handler())
	t.Cleanup(gateway.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, gateway.URL+latestProfilePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200: the failure came after the headers", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ReadAll() error = %v (body %q), want io.ErrUnexpectedEOF", err, body)
	}
	waitForAudit(t, h.harness, codeArtifactStreamFail)
	h.expectMetricCode(t, codeArtifactStreamFail)
}

// TestLatestCollectionRealmDenied proves the realm decides first:
// both routes are checked on namespace, Service, and pgo.read before anything is read.
func TestLatestCollectionRealmDenied(t *testing.T) {
	realms := map[string]func(*config.Realm){
		"namespace": func(r *config.Realm) { r.Namespaces = []string{"billing"} },
		"service":   func(r *config.Realm) { r.Services = []string{"other-api"} },
		"pgo.read":  func(r *config.Realm) { r.PGO = config.RealmPGO{Collect: true, Configure: true} },
	}
	routes := map[string]string{"record": latestPath, "profile": latestProfilePath}

	for denied, narrow := range realms {
		for route, path := range routes {
			t.Run(denied+" "+route, func(t *testing.T) {
				realm := wideRealm()
				narrow(&realm)
				h := newPGOHarness(t, pgoOpts{realm: &realm})
				h.completedRecord(t)
				before := h.storeCalls()

				got := h.doPGO(t, http.MethodGet, path, "", nil)

				h.expectPGOError(t, got, http.StatusForbidden, "realm_denied", "realm_denied")
				if calls := h.storeCalls() - before; calls != 0 {
					t.Errorf("store calls = %d, want a denial that reads nothing", calls)
				}
				if opens := h.nats.artifacts.opens.Load(); opens != 0 {
					t.Errorf("object opens = %d, want a denial that opens nothing", opens)
				}
			})
		}
	}
}

// TestCollectionCancel ends a live Collection, releases its Service, and
// records the transition it committed exactly once.
func TestCollectionCancel(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))
	h.seedActive(t, fixtureNamespace, fixtureService, rec.ID)

	got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", jsonType())

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	var answered pgo.Record
	if err := json.Unmarshal(got.Body.Bytes(), &answered); err != nil {
		t.Fatalf("body %q is not readable: %v", got.Body.String(), err)
	}
	if answered.State != pgo.StateCancelled || answered.Reason != pgo.ReasonCancelledByAPI {
		t.Errorf("answered = %q/%q, want cancelled by the api", answered.State, answered.Reason)
	}

	stored := h.nats.jobs.record(t, rec.ID)
	switch {
	case stored.State != pgo.StateCancelled:
		t.Errorf("stored state = %q, want %q", stored.State, pgo.StateCancelled)
	case stored.FinishedAt == nil || !stored.FinishedAt.Equal(pgoFixtureNow):
		t.Errorf("finishedAt = %v, want the cancelling instant", stored.FinishedAt)
	case stored.Artifact != nil:
		t.Error("a cancelled collection names an artifact")
	}
	if n := h.nats.jobs.countKeys(activeKeyPrefix); n != 0 {
		t.Errorf("active keys = %d, want the Service released", n)
	}
	if rows := h.rec.collectionRows(); len(rows) != 1 || rows[0] != string(pgo.StateCancelled) {
		t.Errorf("Collection rows = %v, want exactly one cancelled", rows)
	}
	if records := h.transitions(t); len(records) != 1 || records[0]["state"] != string(pgo.StateCancelled) {
		t.Errorf("transition records = %v, want exactly one cancelled", records)
	}
}

// TestCollectionCancelKeepsAnotherCollectionsKey proves the release rule: a
// key that names a successor is left alone.
func TestCollectionCancelKeepsAnotherCollectionsKey(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StateRunning))
	h.seedActive(t, fixtureNamespace, fixtureService, "abcdefghjkmnpqrstv09")

	if got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", jsonType()); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.Code)
	}

	if n := h.nats.jobs.countKeys(activeKeyPrefix); n != 1 {
		t.Errorf("active keys = %d, want the successor's key kept", n)
	}
}

// TestCollectionCancelStates pins the two 409s: one nonterminal, one terminal,
// and a terminal answer only ever given from a read that shows one.
func TestCollectionCancelStates(t *testing.T) {
	cases := []struct {
		state pgo.State
		code  string
	}{
		{pgo.StateInitializing, "collection_initializing"},
		{pgo.StateCompleted, "collection_terminal"},
		{pgo.StateFailed, "collection_terminal"},
		{pgo.StateCancelled, "collection_terminal"},
		{pgo.StateExpired, "collection_terminal"},
	}

	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			rec := h.seedRecord(t, h.newRecord(tc.state))

			got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", jsonType())

			h.expectPGOError(t, got, http.StatusConflict, tc.code, tc.code)
			if state := h.nats.jobs.record(t, rec.ID).State; state != tc.state {
				t.Errorf("state = %q, want the record untouched", state)
			}
			if rows := h.rec.collectionRows(); len(rows) != 0 {
				t.Errorf("Collection rows = %v, want none for a refused cancel", rows)
			}
		})
	}
}

// TestCollectionCancelRetriesPastARenewal proves losing to the owner's renewal
// is not a terminal answer: the Collection is still live, so the loop reads
// again and the cancel wins on its retry.
func TestCollectionCancelRetriesPastARenewal(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	lease := pgoFixtureNow.Add(time.Minute)
	rec := h.seedRecord(t, h.newRecord(pgo.StateRunning, func(r *pgo.Record) {
		r.Attempt = 1
		r.LeaseUntil = &lease
	}))

	// The owner renews once, between the handler's first read and its update.
	var renewals atomic.Int32
	h.nats.jobs.afterGet = func() {
		if renewals.Add(1) != 1 {
			return
		}
		renewed := h.nats.jobs.record(t, rec.ID)
		next := lease.Add(time.Minute)
		renewed.LeaseUntil = &next
		h.nats.jobs.put(t, jobKeyPrefix+rec.ID, renewed)
	}

	got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", jsonType())

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on the retry (body %q)", got.Code, got.Body.String())
	}
	if state := h.nats.jobs.record(t, rec.ID).State; state != pgo.StateCancelled {
		t.Errorf("state = %q, want %q", state, pgo.StateCancelled)
	}
	if got := h.nats.jobs.updates.Load(); got != 2 {
		t.Errorf("Update calls = %d, want the lost one and the winning retry", got)
	}
}

// TestCollectionCancelExhaustsItsAttempts proves the loop is bounded: a record
// moving faster than the handler can read it is refused after five losses,
// with the operator told which failure it was.
func TestCollectionCancelExhaustsItsAttempts(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StateRunning))
	h.nats.jobs.updateMismatch = true

	got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", jsonType())

	h.expectPGOError(t, got, http.StatusServiceUnavailable, "pgo_unavailable", codeCASContended)
	// The bound is spelled out rather than read from the handler's own
	// constant, so raising it is a test failure and not a silent change.
	if attempts := h.nats.jobs.updates.Load(); attempts != 5 {
		t.Errorf("Update calls = %d, want 5", attempts)
	}
	if state := h.nats.jobs.record(t, rec.ID).State; state != pgo.StateRunning {
		t.Errorf("state = %q, want the record untouched", state)
	}
}

// TestCollectionCancelUnavailable proves a store that cannot answer is the
// client's 503, told apart from the contended one by its audit code.
func TestCollectionCancelUnavailable(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StateRunning))
	h.nats.jobs.updateErr = natskv.ErrUnavailable

	got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", jsonType())

	h.expectPGOError(t, got, http.StatusServiceUnavailable, "pgo_unavailable", "pgo_unavailable")
	if attempts := h.nats.jobs.updates.Load(); attempts != 1 {
		t.Errorf("Update calls = %d, want one: an unreachable store is not a lost race", attempts)
	}
}

// writeRoute is one of the two routes a POST must declare a JSON media type on.
type writeRoute struct {
	name string
	// path seeds whatever the route needs and answers with the path to post to.
	path func(t *testing.T, h *pgoHarness) string
	body string
	// accepted is the status the route answers once the media type passes.
	accepted int
}

// writeRoutes is both routes the media-type step covers, each with the request
// that succeeds on it, so every media-type row is driven through both.
func writeRoutes() []writeRoute {
	return []writeRoute{
		{
			"create",
			func(*testing.T, *pgoHarness) string { return collectionsPath },
			`{}`, http.StatusAccepted,
		},
		{
			"cancel",
			func(t *testing.T, h *pgoHarness) string {
				t.Helper()

				return collectionPath(h.seedRecord(t, h.newRecord(pgo.StatePending)).ID, "/cancel")
			},
			"", http.StatusOK,
		},
	}
}

// TestWriteRouteMediaType is the media type the two write routes require.
// The essence must be application/json,
// every parameter the parse returns is accepted and ignored,
// and a header that is absent, repeated, unparsable, or of another essence is refused,
// with nothing read or written.
func TestWriteRouteMediaType(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		// detail is the item the refusal carries, empty for a header the route accepts.
		detail string
	}{
		{"absent", nil, detailHeaderRequired},
		{"text", []string{"text/plain"}, detailHeaderMalformed},
		{"form", []string{"application/x-www-form-urlencoded"}, detailHeaderMalformed},
		{"multipart", []string{"multipart/form-data; boundary=x"}, detailHeaderMalformed},
		{"repeated", []string{"application/json", "application/json"}, detailHeaderMalformed},
		{"unparsable", []string{"application/json; charset"}, detailHeaderMalformed},
		{"json", []string{"application/json"}, ""},
		{"json with a charset", []string{"application/json; charset=utf-8"}, ""},
		{"json with another parameter", []string{"application/json; profile=x"}, ""},
	}

	for _, route := range writeRoutes() {
		for _, tc := range cases {
			t.Run(route.name+" with a "+tc.name+" media type", func(t *testing.T) {
				h := newPGOHarness(t, pgoOpts{})
				path := route.path(t, h)
				var header http.Header
				if tc.values != nil {
					header = http.Header{"Content-Type": tc.values}
				}
				before := h.storeCalls()

				got := h.doPGO(t, http.MethodPost, path, route.body, header)

				if tc.detail == "" {
					if got.Code != route.accepted {
						t.Fatalf("status = %d, want %d (body %q)", got.Code, route.accepted, got.Body.String())
					}

					return
				}
				h.expectPGOError(t, got, http.StatusBadRequest, CodeInvalidParameter, CodeInvalidParameter)
				expectDetails(t, got, CodeInvalidParameter, []errorDetail{{Field: "Content-Type", Code: tc.detail}})
				if after := h.storeCalls(); after != before {
					t.Errorf("store calls = %d, want %d: the refusal reached the store", after, before)
				}
			})
		}
	}
}

// fixtureReceiptKey is the receipt key of one idempotency key in the fixture
// scope: the anonymous principal, the fixture namespace, and the fixture Service.
func fixtureReceiptKey(key string) string {
	return pgo.ReceiptKey("anonymous", fixtureNamespace, fixtureService, key)
}

// fixtureSnapshotHash is the hash of the effective policy an empty body produces,
// with whatever a row needs moved in the operator's defaults.
func fixtureSnapshotHash(t *testing.T, mutate ...func(*config.PGODefaults)) string {
	t.Helper()

	defaults := testPGODefaults()
	for _, m := range mutate {
		m(&defaults)
	}
	policy, err := pgo.DefaultPolicy(defaults)
	if err != nil {
		t.Fatalf("DefaultPolicy: %v", err)
	}

	return pgo.SnapshotHash(policy)
}

// seedReceipt writes one receipt into the authoritative bucket, standing in
// for the create another request made.
func (p *pgoHarness) seedReceipt(t *testing.T, key string, r pgo.Receipt) {
	t.Helper()

	p.nats.jobs.put(t, key, r)
}

// boundRecord is a Collection an api request created under one idempotency key,
// in the given state, with the receipt that binds it.
func (p *pgoHarness) boundRecord(
	t *testing.T, state pgo.State, key, hash string, mutate ...func(*pgo.Record),
) pgo.Record {
	t.Helper()

	fields := append([]func(*pgo.Record){func(r *pgo.Record) {
		r.Origin = pgo.OriginAPI
		r.CreatedBy = "anonymous"
		r.IdempotencyKey = key
		r.SnapshotHash = hash
	}}, mutate...)
	rec := p.seedRecord(t, p.newRecord(state, fields...))
	p.seedReceipt(t, fixtureReceiptKey(key),
		pgo.Receipt{ID: rec.ID, SnapshotHash: hash, CreatedAt: pgoFixtureNow})

	return rec
}

// acceptedBodyOf decodes one create acknowledgement.
func acceptedBodyOf(t *testing.T, rec *httptest.ResponseRecorder) acceptedBody {
	t.Helper()

	var body acceptedBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not readable: %v", rec.Body.String(), err)
	}

	return body
}

// TestCollectionCreateAcceptsTheKeyGrammar proves both ends of the header's range are keys the route reads.
func TestCollectionCreateAcceptsTheKeyGrammar(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"one byte", "k"},
		{"128 bytes", strings.Repeat("a", 128)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})

			got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed(tc.key))

			if got.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (body %q)", got.Code, got.Body.String())
			}
			body := acceptedBodyOf(t, got)
			receipt := h.nats.jobs.receipt(t, fixtureReceiptKey(tc.key))
			if receipt.ID != body.ID {
				t.Errorf("receipt names %q, want the collection %q", receipt.ID, body.ID)
			}
			rec := h.nats.jobs.record(t, body.ID)
			if rec.IdempotencyKey != tc.key {
				t.Errorf("record key = %q, want the one the request sent", rec.IdempotencyKey)
			}
			if receipt.SnapshotHash != rec.SnapshotHash {
				t.Errorf("receipt hash = %q, want the record's %q", receipt.SnapshotHash, rec.SnapshotHash)
			}
		})
	}
}

// TestCollectionCreateRefusesAKeyItCannotRead proves a key outside the grammar is refused rather than replaced,
// and that the refusal writes nothing.
func TestCollectionCreateRefusesAKeyItCannotRead(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
	}{
		{"empty", keyed("")},
		{"129 bytes", keyed(strings.Repeat("a", 129))},
		{"a byte outside the set", keyed("bad key")},
		{"sent twice", http.Header{
			"Content-Type":       []string{"application/json"},
			idempotencyKeyHeader: []string{"one", "two"},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			before := h.storeCalls()

			got := h.doPGO(t, http.MethodPost, collectionsPath, `{"sampling":{"rounds":1}}`, tc.header)

			h.expectPGOError(t, got, http.StatusBadRequest, "invalid_parameter", "invalid_parameter")
			expectDetails(t, got, "invalid_parameter", []errorDetail{
				{Field: idempotencyKeyHeader, Code: detailHeaderMalformed},
			})
			for _, prefix := range []string{jobKeyPrefix, activeKeyPrefix, idemKeyPrefix} {
				if n := h.nats.jobs.countKeys(prefix); n != 0 {
					t.Errorf("%s keys = %d, want none written", prefix, n)
				}
			}
			if after := h.storeCalls(); after != before {
				t.Errorf("store calls = %d, want the %d the refusal started with", after, before)
			}
		})
	}
}

// TestCollectionCreateWithoutAKeyWritesNoReceipt is today's behavior: a create
// that carries no key binds nothing.
func TestCollectionCreateWithoutAKeyWritesNoReceipt(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, jsonType())

	if got.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", got.Code, got.Body.String())
	}
	if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 0 {
		t.Errorf("receipts = %d, want none", n)
	}
	rec := h.nats.jobs.record(t, acceptedBodyOf(t, got).ID)
	if rec.IdempotencyKey != "" {
		t.Errorf("record key = %q, want none", rec.IdempotencyKey)
	}
	if rec.SnapshotHash == "" {
		t.Error("record carries no snapshot hash, which every collection has")
	}
}

// TestCollectionCreateReplaysTheSameKey is the guarantee: a retry of a create
// whose answer was lost reads the identifier back and creates nothing.
func TestCollectionCreateReplaysTheSameKey(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})

	first := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body %q)", first.Code, first.Body.String())
	}
	created := acceptedBodyOf(t, first)

	second := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (body %q)", second.Code, second.Body.String())
	}
	replay := acceptedBodyOf(t, second)
	if replay.ID != created.ID {
		t.Errorf("replay names %q, want the first collection %q", replay.ID, created.ID)
	}
	if replay.State != pgo.StatePending {
		t.Errorf("replay state = %q, want the record's %q", replay.State, pgo.StatePending)
	}
	if location := second.Header().Get("Location"); location != "/v1/collections/"+created.ID {
		t.Errorf("Location = %q, want the collection's own path", location)
	}
	for prefix, want := range map[string]int{jobKeyPrefix: 1, activeKeyPrefix: 1, idemKeyPrefix: 1} {
		if n := h.nats.jobs.countKeys(prefix); n != want {
			t.Errorf("%s keys = %d, want %d", prefix, n, want)
		}
	}
}

// TestCollectionReplayReadsTheStateFromTheRecord proves the answer reports
// where the Collection is now rather than where it was accepted.
func TestCollectionReplayReadsTheStateFromTheRecord(t *testing.T) {
	for _, state := range []pgo.State{pgo.StateInitializing, pgo.StateRunning, pgo.StateCompleted} {
		t.Run(string(state), func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			rec := h.boundRecord(t, state, "k", fixtureSnapshotHash(t))

			got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

			if got.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
			}
			body := acceptedBodyOf(t, got)
			if body.ID != rec.ID || body.State != state {
				t.Errorf("replay = %+v, want the record %q in state %q", body, rec.ID, state)
			}
			h.expectPGOAudit(t, http.StatusOK, codeOK)
		})
	}
}

// TestCollectionReplayCarriesTheAcknowledgementAlone proves the thin answer:
// the identifier and the state, and no field of the record.
// POST /collections is a pgo.collect route and reading a record is a pgo.read one,
// so a full record here would hand a collect-only principal what its realm denies on the route that serves it.
func TestCollectionReplayCarriesTheAcknowledgementAlone(t *testing.T) {
	realm := wideRealm()
	realm.PGO = config.RealmPGO{Collect: true}
	h := newPGOHarness(t, pgoOpts{realm: &realm})
	rec := h.boundRecord(t, pgo.StateCompleted, "k", fixtureSnapshotHash(t))

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	var fields map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &fields); err != nil {
		t.Fatalf("body %q is not readable: %v", got.Body.String(), err)
	}
	if len(fields) != 2 || fields["id"] != rec.ID || fields["state"] != string(pgo.StateCompleted) {
		t.Errorf("replay body = %v, want the identifier and the state alone", fields)
	}

	read := h.doPGO(t, http.MethodGet, collectionPath(rec.ID, ""), "", nil)
	expectCode(t, read, http.StatusNotFound, "collection_not_found")
}

// TestCollectionReplayIsRefusedByTheRealmFirst proves the realm decides before the receipt is read:
// a principal the realm no longer admits learns nothing about the Collection its key names.
func TestCollectionReplayIsRefusedByTheRealmFirst(t *testing.T) {
	realm := wideRealm()
	realm.Services = []string{"other-*"}
	h := newPGOHarness(t, pgoOpts{realm: &realm})
	h.nats.jobs.put(t, fixtureReceiptKey("k"),
		pgo.Receipt{ID: newFixtureID(), SnapshotHash: fixtureSnapshotHash(t), CreatedAt: pgoFixtureNow})
	before := h.storeCalls()

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	h.expectPGOError(t, got, http.StatusForbidden, "realm_denied", "realm_denied")
	if after := h.storeCalls(); after != before {
		t.Errorf("store calls = %d, want the %d the refusal started with", after, before)
	}
}

// TestCollectionCreateScopesItsKey proves one key names one Collection per principal and per Service,
// and that neither scope tells a caller of the other.
func TestCollectionCreateScopesItsKey(t *testing.T) {
	t.Run("another principal", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		other := pgo.ReceiptKey("someone-else", fixtureNamespace, fixtureService, "k")
		h.seedReceipt(t, other,
			pgo.Receipt{ID: newFixtureID(), SnapshotHash: fixtureSnapshotHash(t), CreatedAt: pgoFixtureNow})

		got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

		if got.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (body %q)", got.Code, got.Body.String())
		}
		if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 2 {
			t.Errorf("receipts = %d, want one per principal", n)
		}
	})

	t.Run("another service", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		first := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))
		if first.Code != http.StatusAccepted {
			t.Fatalf("first status = %d, want 202 (body %q)", first.Code, first.Body.String())
		}

		other := "/v1/namespaces/" + fixtureNamespace + "/services/other-api/collections"
		second := h.doPGO(t, http.MethodPost, other, `{}`, keyed("k"))

		if second.Code != http.StatusAccepted {
			t.Fatalf("second status = %d, want 202 (body %q)", second.Code, second.Body.String())
		}
		if acceptedBodyOf(t, second).ID == acceptedBodyOf(t, first).ID {
			t.Error("the two services share a collection, want one each")
		}
		if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 2 {
			t.Errorf("receipts = %d, want one per service", n)
		}
	})
}

// TestCollectionCreateRefusesAKeyThatMeansSomethingElse proves the comparison is the effective policy
// and never the request bytes:
// identical JSON mismatches once the snapshot under it has moved.
func TestCollectionCreateRefusesAKeyThatMeansSomethingElse(t *testing.T) {
	t.Run("another body", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		rec := h.boundRecord(t, pgo.StateRunning, "k", fixtureSnapshotHash(t))

		got := h.doPGO(t, http.MethodPost, collectionsPath, `{"sampling":{"rounds":1}}`, keyed("k"))

		expectCode(t, got, http.StatusConflict, "idempotency_mismatch")
		if n := h.nats.jobs.countKeys(jobKeyPrefix); n != 1 {
			t.Errorf("job keys = %d, want only %s", n, rec.ID)
		}
		if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 1 {
			t.Errorf("receipts = %d, want the one the key already had", n)
		}
	})

	t.Run("the stored override moved", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		first := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))
		if first.Code != http.StatusAccepted {
			t.Fatalf("first status = %d, want 202 (body %q)", first.Code, first.Body.String())
		}
		rounds := 3
		h.seedOverride(t, &pgo.PolicyOverride{Sampling: &pgo.SamplingOverride{Rounds: &rounds}})

		got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

		expectCode(t, got, http.StatusConflict, "idempotency_mismatch")
	})

	t.Run("the operator defaults moved", func(t *testing.T) {
		h := newPGOHarness(t, pgoOpts{})
		// The receipt was written under defaults this replica no longer runs,
		// so the same body produces a different snapshot and a different hash.
		moved := fixtureSnapshotHash(t, func(d *config.PGODefaults) { d.Sampling.Rounds = 3 })
		h.boundRecord(t, pgo.StateRunning, "k", moved)

		got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

		expectCode(t, got, http.StatusConflict, "idempotency_mismatch")
	})
}

// TestCollectionCreateWithANewKeyWhileOneIsLive proves a key binds a create
// and decides nothing about another caller's Collection.
func TestCollectionCreateWithANewKeyWhileOneIsLive(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StateRunning))
	h.seedActive(t, fixtureNamespace, fixtureService, rec.ID)

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	h.expectPGOError(t, got, http.StatusTooManyRequests, "collection_in_progress", "collection_in_progress")
	if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 0 {
		t.Errorf("receipts = %d, want none: the request created nothing", n)
	}
}

// TestCollectionCreateNeverReplaysARecordWithNoReceipt proves the receipt is the only thing a key is answered from:
// a record carrying a key nothing bound is not a replay,
// and a scheduled Collection carries no key at all.
func TestCollectionCreateNeverReplaysARecordWithNoReceipt(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	h.seedRecord(t, h.newRecord(pgo.StateCompleted, func(r *pgo.Record) {
		r.Origin = pgo.OriginAPI
		r.CreatedBy = "anonymous"
		r.IdempotencyKey = "k"
		r.SnapshotHash = fixtureSnapshotHash(t)
	}))
	scheduled := h.seedRecord(t, h.newRecord(pgo.StateCompleted, func(r *pgo.Record) {
		r.SnapshotHash = fixtureSnapshotHash(t)
	}))
	if scheduled.IdempotencyKey != "" {
		t.Fatalf("a scheduled record carries key %q, want none", scheduled.IdempotencyKey)
	}

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if got.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", got.Code, got.Body.String())
	}
	if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 1 {
		t.Errorf("receipts = %d, want the one this create wrote", n)
	}
}

// TestCollectionReplayReadsPastTheCaches proves the lookup is authoritative:
// with every job the replica watches frozen since the first create,
// the retry still answers with the identifier the key names.
func TestCollectionReplayReadsPastTheCaches(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	first := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body %q)", first.Code, first.Body.String())
	}
	h.nats.jobs.freeze(jobKeyPrefix)
	h.nats.jobs.freeze(activeKeyPrefix)

	second := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (body %q)", second.Code, second.Body.String())
	}
	if got, want := acceptedBodyOf(t, second).ID, acceptedBodyOf(t, first).ID; got != want {
		t.Errorf("replay names %q, want the first collection %q", got, want)
	}
}

// TestCollectionUnreadableReceiptNamesItsRequest proves what the record a failed receipt read writes names:
// the request, and the Service whose Collection was being created,
// which join it to the audit record of the same request.
func TestCollectionUnreadableReceiptNamesItsRequest(t *testing.T) {
	const id = "unreadable-receipt-request"

	h := newPGOHarness(t, pgoOpts{})
	h.nats.jobs.setGetErr(natskv.ErrUnavailable)

	header := keyed("k")
	header.Set(requestIDHeader, id)
	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, header)
	expectCode(t, got, http.StatusServiceUnavailable, CodePGOUnavailable)

	records := h.logRecords(t, "pgo: idempotency receipt is not readable")
	if len(records) != 1 {
		t.Fatalf("unreadable receipt records = %d, want 1: %s", len(records), h.logs.String())
	}
	if v := records[0]["requestId"]; v != id {
		t.Errorf("the record names request %v, want the one the client sent, %q: %v", v, id, records[0])
	}
	if v := records[0]["service"]; v != fixtureService {
		t.Errorf("the record names service %v, want %q: %v", v, fixtureService, records[0])
	}
}

// TestCollectionCreateReplacesAStaleReceipt proves what a receipt outliving
// its record means: the record reached its retention, so the key names nothing
// and the request creates.
func TestCollectionCreateReplacesAStaleReceipt(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	gone := newFixtureID()
	h.seedReceipt(t, fixtureReceiptKey("k"),
		pgo.Receipt{ID: gone, SnapshotHash: fixtureSnapshotHash(t), CreatedAt: pgoFixtureNow})

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if got.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", got.Code, got.Body.String())
	}
	body := acceptedBodyOf(t, got)
	if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 1 {
		t.Fatalf("receipts = %d, want the stale one replaced", n)
	}
	if receipt := h.nats.jobs.receipt(t, fixtureReceiptKey("k")); receipt.ID != body.ID {
		t.Errorf("receipt names %q, want the new collection %q", receipt.ID, body.ID)
	}
}

// TestCollectionCreateAfterANotPublishedRecord proves the crash window the publication order opens is not a replay:
// a record the scan failed not_published never became claimable and never ran,
// so its key is free.
func TestCollectionCreateAfterANotPublishedRecord(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	failed := h.boundRecord(t, pgo.StateFailed, "k", fixtureSnapshotHash(t),
		func(r *pgo.Record) { r.Reason = pgo.ReasonNotPublished })

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if got.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", got.Code, got.Body.String())
	}
	body := acceptedBodyOf(t, got)
	if body.ID == failed.ID {
		t.Fatal("the answer names the record that never ran, want a new collection")
	}
	if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 1 {
		t.Fatalf("receipts = %d, want exactly one", n)
	}
	if receipt := h.nats.jobs.receipt(t, fixtureReceiptKey("k")); receipt.ID != body.ID {
		t.Errorf("receipt names %q, want the new collection %q", receipt.ID, body.ID)
	}
}

// TestCollectionCreateWritesNoReceiptWhenRefused proves the two ceilings after the lookup leave the key free:
// the request created nothing,
// so a retry creates rather than reading back a Collection that does not exist.
func TestCollectionCreateWritesNoReceiptWhenRefused(t *testing.T) {
	t.Run("rate limited", func(t *testing.T) {
		limits := testPGOLimits(func(l *config.PGOLimits) { l.OnDemandPerMinute = 1 })
		h := newPGOHarness(t, pgoOpts{limits: limits})
		other := "/v1/namespaces/" + fixtureNamespace + "/services/other-api/collections"
		if got := h.doPGO(t, http.MethodPost, other, `{}`, jsonType()); got.Code != http.StatusAccepted {
			t.Fatalf("first status = %d, want 202", got.Code)
		}

		refused := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

		expectCode(t, refused, http.StatusTooManyRequests, "rate_limited")
		if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 0 {
			t.Errorf("receipts = %d, want none: the request created nothing", n)
		}

		h.clock.advance(time.Minute)
		retry := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

		if retry.Code != http.StatusAccepted {
			t.Fatalf("retry status = %d, want 202 (body %q)", retry.Code, retry.Body.String())
		}
	})

	t.Run("capacity exhausted", func(t *testing.T) {
		limits := testPGOLimits(func(l *config.PGOLimits) { l.MaxLiveCollections = 1 })
		h := newPGOHarness(t, pgoOpts{limits: limits})
		busy := h.seedRecord(t, h.newRecord(pgo.StateRunning, func(r *pgo.Record) { r.Service = "other-api" }))
		h.seedActive(t, fixtureNamespace, "other-api", busy.ID)

		got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

		h.expectPGOError(t, got, http.StatusTooManyRequests, "capacity_exhausted", "capacity_exhausted")
		if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 0 {
			t.Errorf("receipts = %d, want none: the request created nothing", n)
		}
	})
}

// TestConcurrentKeyedCreatesNameOneCollection releases the loser only once the winner's record is claimable,
// which is the moment after which one key can only be answered from its receipt.
func TestConcurrentKeyedCreatesNameOneCollection(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	h.nats.jobs.freeze(jobKeyPrefix)
	h.nats.jobs.freeze(activeKeyPrefix)
	handler := h.handler()

	winner := httptest.NewRecorder()
	published := make(chan struct{})
	go func() {
		defer close(published)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, collectionsPath,
			strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(idempotencyKeyHeader, "k")
		handler.ServeHTTP(winner, req)
	}()
	<-published

	loser := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, collectionsPath,
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyKeyHeader, "k")
	handler.ServeHTTP(loser, req)

	if winner.Code != http.StatusAccepted {
		t.Fatalf("winner status = %d, want 202 (body %q)", winner.Code, winner.Body.String())
	}
	if loser.Code != http.StatusOK {
		t.Fatalf("loser status = %d, want 200 (body %q)", loser.Code, loser.Body.String())
	}
	if got, want := acceptedBodyOf(t, loser).ID, acceptedBodyOf(t, winner).ID; got != want {
		t.Errorf("the two answers name %q and %q, want one collection", got, want)
	}
	for prefix, want := range map[string]int{jobKeyPrefix: 1, activeKeyPrefix: 1, idemKeyPrefix: 1} {
		if n := h.nats.jobs.countKeys(prefix); n != want {
			t.Errorf("%s keys = %d, want %d", prefix, n, want)
		}
	}
	if rec := h.nats.jobs.record(t, acceptedBodyOf(t, winner).ID); rec.State != pgo.StatePending {
		t.Errorf("record state = %q, want %q: no initializing record is left behind", rec.State, pgo.StatePending)
	}
}

// TestKeyedLoserBeforeTheReceiptIsRefused holds the winner between its active create and its receipt create,
// which is the window in which the winner can still withdraw.
// The loser is refused rather than handed an identifier that may name nothing a moment later,
// and its retry past the window reads the receipt.
func TestKeyedLoserBeforeTheReceiptIsRefused(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	h.nats.jobs.freeze(jobKeyPrefix)
	h.nats.jobs.freeze(activeKeyPrefix)
	handler := h.handler()

	var arrived, released sync.Once
	held := make(chan struct{})
	release := make(chan struct{})
	lift := func() { released.Do(func() { close(release) }) }
	defer lift()
	receiptKey := fixtureReceiptKey("k")
	h.nats.jobs.setBefore(func(op, key string) {
		if op == "create" && key == receiptKey {
			arrived.Do(func() { close(held) })
			<-release
		}
	})

	winner := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, collectionsPath,
			strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(idempotencyKeyHeader, "k")
		handler.ServeHTTP(winner, req)
	}()
	<-held

	loser := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	expectCode(t, loser, http.StatusTooManyRequests, "collection_in_progress")
	if got := loser.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}

	lift()
	<-done
	if winner.Code != http.StatusAccepted {
		t.Fatalf("winner status = %d, want 202 (body %q)", winner.Code, winner.Body.String())
	}

	retry := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (body %q)", retry.Code, retry.Body.String())
	}
	if got, want := acceptedBodyOf(t, retry).ID, acceptedBodyOf(t, winner).ID; got != want {
		t.Errorf("retry names %q, want the winner's collection %q", got, want)
	}
	if n := h.nats.jobs.countKeys(jobKeyPrefix); n != 1 {
		t.Errorf("job keys = %d, want the loser to have discarded its own record", n)
	}
}

// TestConcurrentKeyedCreatesAnswerOneOfTwoWays states the disjunction the
// release point decides: before the winner's receipt exists a loser can only
// be refused, and after it a loser can only be answered from it.
// The retry is the assertion that holds either way.
func TestConcurrentKeyedCreatesAnswerOneOfTwoWays(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	h.nats.jobs.freeze(jobKeyPrefix)
	h.nats.jobs.freeze(activeKeyPrefix)
	handler := h.handler()

	answers := make([]*httptest.ResponseRecorder, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range answers {
		answers[i] = httptest.NewRecorder()
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, collectionsPath,
				strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(idempotencyKeyHeader, "k")
			<-start
			handler.ServeHTTP(answers[i], req)
		}()
	}
	close(start)
	wg.Wait()

	accepted := ""
	for i, got := range answers {
		switch got.Code {
		case http.StatusAccepted:
			accepted = acceptedBodyOf(t, got).ID
		case http.StatusOK:
		case http.StatusTooManyRequests:
			expectCode(t, got, http.StatusTooManyRequests, "collection_in_progress")
			if after := got.Header().Get("Retry-After"); after != "1" {
				t.Errorf("Retry-After = %q, want 1", after)
			}
		default:
			t.Fatalf("answer %d = %d (body %q), want 202, 200, or 429", i, got.Code, got.Body.String())
		}
	}
	if accepted == "" {
		t.Fatal("no request was accepted, want exactly one")
	}

	retry := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (body %q)", retry.Code, retry.Body.String())
	}
	if got := acceptedBodyOf(t, retry).ID; got != accepted {
		t.Errorf("retry names %q, want the accepted collection %q", got, accepted)
	}
}

// TestCollectionReplayOutrunsTheTokenBucket is where the lookup sits.
// The bucket bounds requests that would create something, and a replay creates nothing,
// so an empty bucket never withholds an identifier the caller already owns.
// The test fails when the lookup moves behind the bucket:
// the second request is then the one the bucket refuses.
func TestCollectionReplayOutrunsTheTokenBucket(t *testing.T) {
	limits := testPGOLimits(func(l *config.PGOLimits) { l.OnDemandPerMinute = 1 })
	h := newPGOHarness(t, pgoOpts{limits: limits})

	first := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body %q)", first.Code, first.Body.String())
	}

	// The one token of the minute is gone,
	// and the retry is the request whose answer was lost rather than a second creation.
	second := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200 with the bucket empty (body %q)", second.Code, second.Body.String())
	}
	if got, want := acceptedBodyOf(t, second).ID, acceptedBodyOf(t, first).ID; got != want {
		t.Errorf("replay names %q, want the first collection %q", got, want)
	}
}

// TestCollectionReplayReReadsAReceiptItLost proves the delete of a stale receipt is guarded by the revision it read.
// Another writer replaced the receipt in between,
// so this request's delete loses and it reads the key once more,
// which is where the Collection that now holds the key is answered from.
func TestCollectionReplayReReadsAReceiptItLost(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	hash := fixtureSnapshotHash(t)
	successor := h.boundRecord(t, pgo.StateRunning, "k", hash)
	// The receipt this request reads names a record that reached its retention,
	// which is the stale one it tries to delete.
	receiptKey := fixtureReceiptKey("k")
	h.nats.jobs.put(t, receiptKey,
		pgo.Receipt{ID: newFixtureID(), SnapshotHash: hash, CreatedAt: pgoFixtureNow})

	var reads atomic.Int32
	h.nats.jobs.afterGet = func() {
		if reads.Add(1) != 1 {
			return
		}
		h.nats.jobs.put(t, receiptKey,
			pgo.Receipt{ID: successor.ID, SnapshotHash: hash, CreatedAt: pgoFixtureNow})
	}

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, keyed("k"))

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if id := acceptedBodyOf(t, got).ID; id != successor.ID {
		t.Errorf("answer names %q, want the collection the key now holds, %q", id, successor.ID)
	}
	if n := h.nats.jobs.countKeys(idemKeyPrefix); n != 1 {
		t.Errorf("receipts = %d, want the successor's alone: a lost delete removes nothing", n)
	}
	if receipt := h.nats.jobs.receipt(t, receiptKey); receipt.ID != successor.ID {
		t.Errorf("receipt names %q, want the successor %q", receipt.ID, successor.ID)
	}
}

// waitPath is one Collection's read route carrying a query.
func waitPath(id, query string) string { return collectionPath(id, "") + "?" + query }

// expectNoElapsed fails when an answer carries the header only an accepted wait earns.
func expectNoElapsed(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if got := rec.Header().Get(waitElapsedHeader); got != "" {
		t.Errorf("%s = %q, want none: this request asked for no wait", waitElapsedHeader, got)
	}
}

// expectElapsed fails unless the answer carries the elapsed header,
// and returns the seconds it names.
func expectElapsed(t *testing.T, rec *httptest.ResponseRecorder) float64 {
	t.Helper()

	got := rec.Header().Get(waitElapsedHeader)
	if got == "" {
		t.Fatalf("%s is absent from an answer to an accepted wait", waitElapsedHeader)
	}
	seconds, err := strconv.ParseFloat(got, 64)
	if err != nil {
		t.Fatalf("%s = %q, want decimal seconds: %v", waitElapsedHeader, got, err)
	}
	if len(got) < 5 || got[len(got)-4] != '.' {
		t.Errorf("%s = %q, want millisecond precision", waitElapsedHeader, got)
	}

	return seconds
}

// stateOf reads the state out of one Collection answer,
// which is written as the record was stored.
func stateOf(t *testing.T, rec *httptest.ResponseRecorder) pgo.State {
	t.Helper()

	var body pgo.Record
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("collection body %q is not a record: %v", rec.Body.String(), err)
	}
	if body.Policy.Sampling.Rounds == 0 {
		t.Errorf("the answer carries no policy: it was written from the cache rather than from a read")
	}

	return body.State
}

// TestCollectionReadWithoutWaitAnswersAsItAlwaysDid pins what a client on a timer already gets:
// the record as read, no elapsed header, and no wait field on the audit record.
func TestCollectionReadWithoutWaitAnswersAsItAlwaysDid(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	got := h.doPGO(t, http.MethodGet, collectionPath(rec.ID, ""), "", nil)

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	expectNoElapsed(t, got)
	audit := h.expectPGOAudit(t, http.StatusOK, codeOK)
	if _, ok := audit["wait"]; ok {
		t.Errorf("audit record carries wait for a request that asked for none: %v", audit)
	}
}

// TestCollectionReadRefusesAWaitOutsideItsGrammar covers every refusal the parameter earns:
// each one names the input to change,
// none of them starts a wait,
// and none of them carries the elapsed header.
// A value above the grammar is refused rather than clamped.
func TestCollectionReadRefusesAWaitOutsideItsGrammar(t *testing.T) {
	cases := []struct {
		name  string
		query string
		field string
		code  string
	}{
		{"a name the route does not take", "bogus=1", "bogus", detailUnknownParameter},
		{"a wait of zero", "wait=0", waitParam, detailMalformedParameter},
		{"a negative wait", "wait=-1s", waitParam, detailMalformedParameter},
		{"a wait that is not a duration", "wait=abc", waitParam, detailMalformedParameter},
		{"a wait one second above the top", "wait=61s", waitParam, detailMalformedParameter},
		{"a wait far above the top", "wait=120s", waitParam, detailMalformedParameter},
		{"an empty wait", "wait=", waitParam, detailEmptyParameter},
		{"a repeated wait", "wait=5s&wait=5s", waitParam, detailRepeatedParameter},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			rec := h.seedRecord(t, h.newRecord(pgo.StatePending))
			before := h.storeCalls()

			got := h.doPGO(t, http.MethodGet, waitPath(rec.ID, tc.query), "", nil)

			h.expectPGOError(t, got, http.StatusBadRequest, CodeInvalidParameter, CodeInvalidParameter)
			expectDetails(t, got, CodeInvalidParameter, []errorDetail{{Field: tc.field, Code: tc.code}})
			expectNoElapsed(t, got)
			if delta := h.storeCalls() - before; delta != 0 {
				t.Errorf("store calls = %d, want 0: a refused wait reads nothing and registers nothing", delta)
			}
		})
	}
}

// TestOnlyTheCollectionReadTakesAParameter proves the other Collection-scoped routes take none:
// the wait belongs to the read alone,
// and every other route refuses a query naming what the client sent.
func TestOnlyTheCollectionReadTakesAParameter(t *testing.T) {
	for _, tc := range []struct{ name, suffix, method string }{
		{"the download", "/profile", http.MethodGet},
		{"the cancel", "/cancel", http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{})
			rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

			got := h.doPGO(t, tc.method, collectionPath(rec.ID, tc.suffix)+"?wait=5s", "", clientHeaders(tc.method))

			h.expectPGOError(t, got, http.StatusBadRequest, CodeInvalidParameter, CodeInvalidParameter)
			expectDetails(t, got, CodeInvalidParameter,
				[]errorDetail{{Field: waitParam, Code: detailUnknownParameter}})
			expectNoElapsed(t, got)
		})
	}
}

// TestWaitOnATerminalRecordAnswersAtOnce proves the wait ends on the first read
// when the record is one it can never leave,
// and that the audit record names the duration asked for
// while no metrics label is built from it.
func TestWaitOnATerminalRecordAnswersAtOnce(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StateCompleted))

	got := h.doPGO(t, http.MethodGet, waitPath(rec.ID, "wait=5s"), "", nil)

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if elapsed := expectElapsed(t, got); elapsed != 0 {
		t.Errorf("elapsed = %v, want 0: the wait never started", elapsed)
	}
	audit := h.expectPGOAudit(t, http.StatusOK, codeOK)
	if audit["wait"] != "5s" {
		t.Errorf("audit wait = %v, want the duration asked for", audit["wait"])
	}
	h.expectMetric(t, metrics.EndpointCollection, labelCPU)
}

// TestWaitRegistersBeforeItReads is the lost-wakeup case.
// The record moves the moment the handler's first authoritative read returns,
// and the pulse that transition produces has reached the caches before the
// handler goes on.
// A handler that registered first is woken by it and answers at once; one that
// reads first has nothing registered when the pulse is delivered and parks
// until its deadline over a change that had already happened.
func TestWaitRegistersBeforeItReads(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	var reads atomic.Int32
	h.nats.jobs.afterGet = func() {
		// The realm read is the first; the wait's own first read is the second.
		if reads.Add(1) != 2 {
			return
		}
		moved := h.nats.jobs.record(t, rec.ID)
		moved.State = pgo.StateRunning
		h.nats.jobs.put(t, jobKeyPrefix+rec.ID, moved)
		// The pulse has been delivered before the read returns.
		// This runs on the handler's own goroutine and so reports nothing itself:
		// a delivery that never landed shows as the answer below.
		for deadline := time.Now().Add(heldOpenTimeout); time.Now().Before(deadline); {
			if h.cachedState(rec.ID) == pgo.StateRunning {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}

	got := h.hold(t, waitPath(rec.ID, "wait=30s")).join(t)

	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if state := stateOf(t, got); state != pgo.StateRunning {
		t.Errorf("state = %q, want %q", state, pgo.StateRunning)
	}
	if elapsed := expectElapsed(t, got); elapsed != 0 {
		t.Errorf("elapsed = %v, want 0: the answer came from the pulse, not the deadline", elapsed)
	}
}

// TestWaitAnswersTheReadThatFollowsAPulse proves a pulse is a hint:
// the answer is the record the authoritative read returned,
// carrying what the cache never holds.
func TestWaitAnswersTheReadThatFollowsAPulse(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)

	moved := h.nats.jobs.record(t, rec.ID)
	moved.State = pgo.StateRunning
	h.nats.jobs.put(t, jobKeyPrefix+rec.ID, moved)

	got := held.join(t)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if state := stateOf(t, got); state != pgo.StateRunning {
		t.Errorf("state = %q, want %q", state, pgo.StateRunning)
	}
}

// TestWaitOutlivesTwoRenewals proves only a change of state ends a wait:
// an owner writes progress with every renewal,
// and a wait that answered on any write would answer every twenty seconds with a record that had not moved.
func TestWaitOutlivesTwoRenewals(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	lease := pgoFixtureNow.Add(time.Minute)
	rec := h.seedRecord(t, h.newRecord(pgo.StateRunning, func(r *pgo.Record) { r.LeaseUntil = &lease }))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)

	for i := range 2 {
		renewed := h.nats.jobs.record(t, rec.ID)
		next := lease.Add(time.Duration(i+1) * time.Minute)
		renewed.LeaseUntil = &next
		renewed.Progress = pgo.Progress{Round: i, Rounds: 2, SamplesOK: i + 1}
		h.nats.jobs.put(t, jobKeyPrefix+rec.ID, renewed)
		h.waitCache(t, "the renewal delivered", func() bool { return h.cachedState(rec.ID) == pgo.StateRunning })
	}
	if held.answered() {
		t.Fatal("a renewal that wrote no new state ended the wait")
	}

	finished := h.nats.jobs.record(t, rec.ID)
	finished.State = pgo.StateCancelled
	h.nats.jobs.put(t, jobKeyPrefix+rec.ID, finished)

	if state := stateOf(t, held.join(t)); state != pgo.StateCancelled {
		t.Errorf("state = %q, want %q: only a change of state ends a wait", state, pgo.StateCancelled)
	}
}

// TestWaitAnswers404WhenTheRecordGoes covers the record the sweeper deletes
// while a client is waiting on it: it answers exactly as a plain read of a
// deleted record does.
func TestWaitAnswers404WhenTheRecordGoes(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)
	h.nats.jobs.remove(t, jobKeyPrefix+rec.ID)

	got := held.join(t)
	expectCode(t, got, http.StatusNotFound, CodeCollectionNotFound)
	expectElapsed(t, got)
}

// TestWaitExpiresWithTheRecordAsRead proves the deadline answers the record its final read returned,
// with an elapsed value at least the duration asked for.
func TestWaitExpiresWithTheRecordAsRead(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)
	h.clock.advance(30 * time.Second)

	got := held.join(t)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if state := stateOf(t, got); state != pgo.StatePending {
		t.Errorf("state = %q, want %q", state, pgo.StatePending)
	}
	if elapsed := expectElapsed(t, got); elapsed < 30 {
		t.Errorf("elapsed = %v, want at least the 30 seconds asked for", elapsed)
	}
}

// TestTheDeadlineReads is the case a deadline answered from an earlier read gets wrong.
// The transition lands while the cache is frozen, so no pulse follows it,
// and the wait comes back only on its own timer:
// a handler that answered from the record it last read would report the state before the transition.
func TestTheDeadlineReads(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)

	h.nats.jobs.freeze(jobKeyPrefix)
	finished := h.nats.jobs.record(t, rec.ID)
	finished.State = pgo.StateCompleted
	h.nats.jobs.put(t, jobKeyPrefix+rec.ID, finished)
	h.clock.advance(30 * time.Second)

	got := held.join(t)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
	}
	if state := stateOf(t, got); state != pgo.StateCompleted {
		t.Errorf("state = %q, want %q: the deadline reads, so a dropped pulse costs latency and not the answer",
			state, pgo.StateCompleted)
	}
}

// TestTheDeadlineAnswers404ForARecordThatWent is the deadline ending for a record that went with no pulse.
func TestTheDeadlineAnswers404ForARecordThatWent(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)

	h.nats.jobs.freeze(jobKeyPrefix)
	h.nats.jobs.remove(t, jobKeyPrefix+rec.ID)
	h.clock.advance(30 * time.Second)

	expectCode(t, held.join(t), http.StatusNotFound, CodeCollectionNotFound)
}

// TestATransitionAndTheDeadlineTogetherAnswerTheTransition proves the two arms agree:
// whichever the select takes, the answer comes from a read taken after it.
func TestATransitionAndTheDeadlineTogetherAnswerTheTransition(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)

	finished := h.nats.jobs.record(t, rec.ID)
	finished.State = pgo.StateFailed
	finished.Reason = pgo.ReasonNoSamples
	h.nats.jobs.put(t, jobKeyPrefix+rec.ID, finished)
	h.clock.advance(30 * time.Second)

	if state := stateOf(t, held.join(t)); state != pgo.StateFailed {
		t.Errorf("state = %q, want %q whichever arm the select took", state, pgo.StateFailed)
	}
}

// TestAWaitThatLosesTheStoreIs503 proves what a final read the store cannot answer ends a wait with:
// the 503 every other store failure inside one earns.
func TestAWaitThatLosesTheStoreIs503(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)

	h.nats.jobs.setGetErr(natskv.ErrUnavailable)
	h.clock.advance(30 * time.Second)

	got := held.join(t)
	expectCode(t, got, http.StatusServiceUnavailable, CodePGOUnavailable)
	expectElapsed(t, got)
}

// TestAClientThatLeavesMidWaitIsAudited proves nothing is answered to a client
// that is gone, and that the operator sees why the request ended.
func TestAClientThatLeavesMidWaitIsAudited(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)
	held.cancel()

	got := held.join(t)
	if got.Body.Len() != 0 {
		t.Errorf("body = %q, want nothing written to a client that left", got.Body.String())
	}
	h.expectPGOAudit(t, 0, codeClientGone)
}

// TestTheGenerationMoveEndsAWait proves the mid-wait refusal is driven by the
// broadcast: a parked request cannot read Generation() again, so a handler
// that read it once would sit out the whole outage and answer from a view the
// store had moved past.
func TestTheGenerationMoveEndsAWait(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)
	h.disconnect()

	got := held.join(t)
	expectCode(t, got, http.StatusServiceUnavailable, CodePGOUnavailable)
	expectElapsed(t, got)
}

// TestTheGenerationMoveBetweenTheReadAndTheSelectEndsAWait is the window a
// looked-up broadcast loses.
// The connection drops the moment the handler's authoritative read returns,
// which is before it selects:
// the session holds the channel of its own generation,
// so the close it already missed is on the channel it holds.
// A handler that asked for the channel at select time would be handed the replacement,
// and park to its deadline over the outage.
func TestTheGenerationMoveBetweenTheReadAndTheSelectEndsAWait(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	var reads atomic.Int32
	h.nats.jobs.afterGet = func() {
		if reads.Add(1) == 2 {
			h.disconnect()
		}
	}

	held := h.hold(t, waitPath(rec.ID, "wait=30s"))
	h.waitTimer(t)
	// A wait that missed the move would come back only on its own deadline,
	// and answer 200 with a record read under a generation the store left.
	h.clock.advance(30 * time.Second)

	expectCode(t, held.join(t), http.StatusServiceUnavailable, CodePGOUnavailable)
}

// TestDrainingEndsEveryWait proves a poll cannot outlast the drain window the
// deployment sized: every waiting request answers with the record it last
// read, the moment readiness turns 503.
func TestDrainingEndsEveryWait(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))

	first := h.hold(t, waitPath(rec.ID, "wait=60s"))
	second := h.hold(t, waitPath(rec.ID, "wait=60s"))
	h.waitCache(t, "both waits parked", func() bool { return h.clock.timerCount() == 2 })

	h.startDrain()

	for _, held := range []*held{first, second} {
		got := held.join(t)
		if got.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", got.Code, got.Body.String())
		}
		if state := stateOf(t, got); state != pgo.StatePending {
			t.Errorf("state = %q, want the record as last read", state)
		}
		expectElapsed(t, got)
	}
}

// TestOneEntryWakesEveryWaitOnTheRecord proves what a hundred waiting clients cost:
// one channel each,
// one read each when the record moves,
// and no traffic to the store beyond the watches the replica already holds.
func TestOneEntryWakesEveryWaitOnTheRecord(t *testing.T) {
	const waiters = 50

	h := newPGOHarness(t, pgoOpts{})
	rec := h.seedRecord(t, h.newRecord(pgo.StatePending))
	watches := h.nats.jobs.watchCount()

	start := h.storeCalls()
	waiting := make([]*held, waiters)
	for i := range waiting {
		waiting[i] = h.hold(t, waitPath(rec.ID, "wait=60s"))
	}
	// Two reads each: the realm read and the first authoritative read.
	// Counting from before those land would read one of them as a wake-up.
	h.waitCache(t, "every wait past its first read", func() bool { return h.storeCalls() >= start+2*waiters })

	before := h.storeCalls()
	moved := h.nats.jobs.record(t, rec.ID)
	moved.State = pgo.StateRunning
	h.nats.jobs.put(t, jobKeyPrefix+rec.ID, moved)

	for i, held := range waiting {
		if state := stateOf(t, held.join(t)); state != pgo.StateRunning {
			t.Fatalf("wait %d state = %q, want %q", i, state, pgo.StateRunning)
		}
	}

	// The record read above is the one call beside the waits' own.
	if delta := h.storeCalls() - before - 1; delta != waiters {
		t.Errorf("store calls after one applied entry = %d, want one read per woken request (%d)", delta, waiters)
	}
	if got := h.nats.jobs.watchCount(); got != watches {
		t.Errorf("watches = %d, want the caches' own %d: a waiting request opens none", got, watches)
	}
}
