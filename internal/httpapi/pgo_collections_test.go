package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
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
	if rows := h.rec.collectionRows(); len(rows) != 1 || rows[0] != codeCollectionCancelled {
		t.Errorf("Collection rows = %v, want exactly one cancelled", rows)
	}
	if records := h.transitions(t); len(records) != 1 || records[0]["state"] != codeCollectionCancelled {
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
