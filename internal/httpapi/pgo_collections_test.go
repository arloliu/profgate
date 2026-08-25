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

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{"sampling":{"duration":"10s","rounds":1}}`, nil)

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
	case rec.CreatedBy != anonymousPrincipal:
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

// TestCollectionCreateCarriesTheStoredRevision proves the snapshot is layered
// on the override the watched cache holds, and that the Collection records the
// revision it came from.
func TestCollectionCreateCarriesTheStoredRevision(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rounds := 3
	revision := h.seedOverride(t, &pgo.PolicyOverride{Sampling: &pgo.SamplingOverride{Rounds: &rounds}})

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, nil)

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

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{"sampling":{"rounds":99}}`, nil)

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
		got := h.doPGO(t, http.MethodPost, path, `{}`, nil)
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

	if got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, nil); got.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", got.Code)
	}
	second := "/v1/namespaces/" + fixtureNamespace + "/services/other-api/collections"
	if got := h.doPGO(t, http.MethodPost, second, `{}`, nil); got.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", got.Code)
	}

	h.clock.advance(time.Minute)

	if got := h.doPGO(t, http.MethodPost, second, `{}`, nil); got.Code != http.StatusAccepted {
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
		code    string
	}{
		{"two versions", []k8s.Target{namedTarget("a", "1.0"), namedTarget("b", "2.0")}, "version_conflict"},
		{"no version labels", []k8s.Target{namedTarget("a", "")}, "version_missing"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPGOHarness(t, pgoOpts{}, tc.targets...)

			got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, nil)

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

			got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, nil)

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

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, nil)

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

	got := h.doPGO(t, http.MethodPost, collectionsPath, `{}`, nil)

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
func TestCollectionDownloadClientGone(t *testing.T) {
	h := newPGOHarness(t, pgoOpts{})
	rec := h.completedRecord(t)

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

	got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", nil)

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

	if got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", nil); got.Code != http.StatusOK {
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

			got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", nil)

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

	got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", nil)

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

	got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", nil)

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

	got := h.doPGO(t, http.MethodPost, collectionPath(rec.ID, "/cancel"), "", nil)

	h.expectPGOError(t, got, http.StatusServiceUnavailable, "pgo_unavailable", "pgo_unavailable")
	if attempts := h.nats.jobs.updates.Load(); attempts != 1 {
		t.Errorf("Update calls = %d, want one: an unreachable store is not a lost race", attempts)
	}
}
