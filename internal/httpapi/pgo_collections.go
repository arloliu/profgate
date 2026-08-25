package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/pgo"
)

// cancelAttempts bounds the cancel loop.
// Losing to a renewal is the common race and the loop simply reads again;
// five losses in a row means the record is moving faster than the handler can
// read it, which does not happen at the renewal rate, so the bound exists only
// so the handler cannot spin.
const cancelAttempts = 5

// The errors the Collection routes answer with.
var (
	errCollectionInProgress = &requestError{
		status:  http.StatusTooManyRequests,
		code:    "collection_in_progress",
		message: "the service already has a live collection",
	}
	errRateLimited = &requestError{
		status:  http.StatusTooManyRequests,
		code:    "rate_limited",
		message: "too many on-demand collections; retry shortly",
	}
	errCapacityExhausted = &requestError{
		status:  http.StatusTooManyRequests,
		code:    "capacity_exhausted",
		message: "the gateway is running its limit of live collections",
	}
	errCollectionNotCompleted = &requestError{
		status:  http.StatusConflict,
		code:    "collection_not_completed",
		message: "the collection has no stored profile",
	}
	errCollectionInitializing = &requestError{
		status:  http.StatusConflict,
		code:    "collection_initializing",
		message: "the collection is still being published; retry shortly",
	}
	errCollectionTerminal = &requestError{
		status:  http.StatusConflict,
		code:    "collection_terminal",
		message: "the collection has already finished",
	}
	errCASContended = &requestError{
		status:    http.StatusServiceUnavailable,
		code:      "pgo_unavailable",
		message:   "the collection is changing faster than it can be cancelled; retry",
		auditCode: codeCASContended,
	}
)

// collectionView is one entry of the Collection listing.
type collectionView struct {
	ID              string     `json:"id"`
	Origin          pgo.Origin `json:"origin"`
	State           pgo.State  `json:"state"`
	Attempt         int        `json:"attempt"`
	ResolvedVersion string     `json:"resolvedVersion,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
}

// collectionsBody is the Collection listing of one Service.
type collectionsBody struct {
	Namespace   string           `json:"namespace"`
	Service     string           `json:"service"`
	Collections []collectionView `json:"collections"`
}

// acceptedBody is what a created Collection answers with, alongside its Location.
type acceptedBody struct {
	ID    string    `json:"id"`
	State pgo.State `json:"state"`
}

// serveCollectionList answers one Service's Collections from the watched
// cache, newest first, with no pagination behind it.
func (s *server) serveCollectionList(w http.ResponseWriter, q *request, sess *pgo.Session) {
	rt := q.route
	views := sess.Collections(rt.namespace, rt.service)
	body := collectionsBody{Namespace: rt.namespace, Service: rt.service, Collections: make([]collectionView, 0, len(views))}
	for _, v := range views {
		body.Collections = append(body.Collections, collectionView{
			ID:              v.ID,
			Origin:          v.Origin,
			State:           v.State,
			Attempt:         v.Attempt,
			ResolvedVersion: v.ResolvedVersion,
			CreatedAt:       v.CreatedAt,
			FinishedAt:      v.FinishedAt,
			ExpiresAt:       v.ExpiresAt,
		})
	}

	q.audit.status = http.StatusOK
	q.audit.code = codeOK
	writeJSON(w, http.StatusOK, body)
}

// serveCollectionCreate publishes an on-demand Collection.
//
// The order is the point of the handler.
// The token bucket comes first, so a caller with pgo.collect across many
// Services cannot make the gateway do work per request it is not allowed to
// make records for.
// The advisory target resolution comes next, so both version answers are given
// before anything is written and a request that cannot succeed leaves no key
// behind; the round is still the authority, so a Collection can fail for the
// same reason later.
// Only then is a reservation taken and the publication's three writes made.
func (s *server) serveCollectionCreate(
	w http.ResponseWriter, r *http.Request, q *request, sess *pgo.Session, principal string,
) {
	var body pgo.PolicyOverride
	if berr := decodeBody(w, r, &body, true); berr != nil {
		q.fail(w, berr)

		return
	}
	if body.Enabled != nil || body.Schedule != nil {
		q.fail(w, invalidParameter("a collection request sets neither enabled nor schedule"))

		return
	}

	rt := q.route
	// The override comes from the watched cache, which is read only behind the
	// replay barrier, so configRevision is never 0 for a Service that has one.
	stored, configRevision := sess.CachedOverride(rt.namespace, rt.service)
	snapshot, violations := sess.Effective(stored, &body)
	if len(violations) > 0 {
		q.fail(w, limitExceeded(violations))

		return
	}

	if !sess.TakeToken() {
		q.fail(w, errRateLimited)

		return
	}

	targets, derr := s.discover(r.Context(), q)
	if derr != nil {
		q.fail(w, derr)

		return
	}
	if _, reason := pgo.ResolveVersion(targets, snapshot.Target.Version); reason != "" {
		q.fail(w, &requestError{
			status:  http.StatusConflict,
			code:    reason,
			message: versionMessage(reason, rt.namespace, rt.service, snapshot.Target.Version),
		})

		return
	}

	// The cached check only spares a write that would lose the active create;
	// the create is the decision, and a request whose cache lags simply loses it.
	if sess.Live(rt.namespace, rt.service) {
		q.fail(w, errCollectionInProgress)

		return
	}

	res, ok := sess.Reserve(rt.namespace, rt.service)
	if !ok {
		q.fail(w, errCapacityExhausted)

		return
	}

	id, outcome, err := sess.Publish(r.Context(), res, pgo.PublishInput{
		Namespace:      rt.namespace,
		Service:        rt.service,
		Origin:         pgo.OriginAPI,
		ClaimBy:        sess.Now().Add(pgo.APIClaimGrace),
		ConfigRevision: configRevision,
		Policy:         snapshot,
		CreatedBy:      principal,
	})
	switch {
	case err != nil:
		s.deps.Logger.Warn("pgo: publishing an on-demand collection failed",
			"namespace", rt.namespace, "service", rt.service, "collection", id, "error", err)
		q.fail(w, errPGOUnavailable)
	case outcome == pgo.OutcomeBusy:
		q.fail(w, errCollectionInProgress)
	default:
		q.audit.collection = id
		q.audit.status = http.StatusAccepted
		q.audit.code = codeOK
		w.Header().Set("Location", "/v1/collections/"+id)
		writeJSON(w, http.StatusAccepted, acceptedBody{ID: id, State: pgo.StatePending})
	}
}

// versionMessage is the envelope text for an advisory version refusal;
// it names the Service and never a Pod's address.
// One missing-version refusal covers two situations,
// so the text says which one the caller is in:
// no Pod carries a version label at all,
// or none carries the version the effective policy pins.
// A conflict never carries a pin,
// since the pin filter leaves at most one version to disagree over.
func versionMessage(reason, namespace, service, pin string) string {
	where := " of service " + service + " in namespace " + namespace
	if reason == pgo.ReasonVersionMissing {
		if pin != "" {
			return "no pod" + where + " carries version " + pin
		}

		return "no pod" + where + " carries a version label"
	}

	return "the pods" + where + " carry more than one version"
}

// serveCollectionRead answers the Collection record as the bucket holds it.
func (s *server) serveCollectionRead(w http.ResponseWriter, q *request, stored pgo.StoredRecord) {
	q.audit.status = http.StatusOK
	q.audit.code = codeOK
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// The record is written back as stored, so a reader sees exactly what the
	// gateway decides from.
	// A failed write is the client's connection going away.
	_, _ = w.Write(append(stored.Value, '\n'))
}

// serveCollectionDownload streams the merged profile out of the Object Store.
// The body is streamed rather than buffered, so the outcome after headers is
// classified the way an upstream stream is: the connection is dropped and the
// audit record carries what happened.
func (s *server) serveCollectionDownload(
	w http.ResponseWriter, r *http.Request, q *request, sess *pgo.Session, stored pgo.StoredRecord,
) {
	rec := stored.Record
	switch rec.State {
	case pgo.StateCompleted:
	case pgo.StateExpired:
		q.fail(w, errArtifactGone)

		return
	case pgo.StateInitializing, pgo.StatePending, pgo.StateRunning, pgo.StateFailed, pgo.StateCancelled:
		q.fail(w, errCollectionNotCompleted)

		return
	default:
		q.fail(w, errCollectionNotCompleted)

		return
	}

	object := ""
	if rec.Artifact != nil {
		object = rec.Artifact.Object
	}
	if object == "" {
		s.expireCollection(r, q, sess, stored)
		q.fail(w, errArtifactGone)

		return
	}

	body, err := sess.OpenArtifact(r.Context(), object)
	switch {
	case err == nil:
	case errors.Is(err, natskv.ErrObjectNotFound):
		// A download does not protect its object from expiry, so a completed
		// record whose object is gone is flipped by whichever reader gets
		// there first, and that reader owns the transition.
		s.expireCollection(r, q, sess, stored)
		q.fail(w, errArtifactGone)

		return
	default:
		q.fail(w, storeError(err))

		return
	}
	defer func() {
		//nolint:errcheck // the read is over either way; closing releases it
		_ = body.Close()
	}()

	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Disposition", `attachment; filename="`+rec.ID+`.pprof"`)
	header.Set("X-Pprof-Collection", rec.ID)
	header.Set("X-Pprof-Target-Version", rec.ResolvedVersion)
	w.WriteHeader(http.StatusOK)
	q.audit.status = http.StatusOK
	q.audit.code = codeOK

	// The headers go out before the first byte of the object, so a failure
	// part-way through is a truncation of a started response rather than a
	// response the client never saw.
	control := http.NewResponseController(w)
	//nolint:errcheck // a ResponseWriter that cannot flush still delivers the body
	_ = control.Flush()

	if _, err := io.CopyBuffer(flushing{w: w, control: control}, body, make([]byte, downloadChunkBytes)); err != nil {
		if r.Context().Err() != nil {
			// The client left; the store read was cancelled with it and there
			// is nobody to answer.
			q.audit.code = codeClientGone

			return
		}
		q.audit.code = codeArtifactStreamFail
		// The deferred audit and metrics run during the unwind; net/http then
		// drops the connection without a stack trace, so the client sees a
		// truncated body rather than a cleanly finished one.
		panic(http.ErrAbortHandler)
	}
}

// downloadChunkBytes is how much of the object one write carries.
const downloadChunkBytes = 32 << 10

// flushing pushes each chunk to the client as it arrives, so a download is a
// stream and not a response the client sees only once it has all been read.
type flushing struct {
	w       http.ResponseWriter
	control *http.ResponseController
}

func (f flushing) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	//nolint:errcheck // a ResponseWriter that cannot flush still delivers the body
	_ = f.control.Flush()

	return n, nil
}

// expireCollection flips a completed record whose object is gone.
// The conditional update is what decides: the reader that wins it owns the
// transition's log record and its metric row, exactly as the sweeper owns the
// same transition on its own path, so one flip is never counted twice.
func (s *server) expireCollection(r *http.Request, q *request, sess *pgo.Session, stored pgo.StoredRecord) {
	rec := stored.Record
	rec.State = pgo.StateExpired
	if err := sess.WriteRecord(r.Context(), rec, stored.Revision); err != nil {
		return
	}
	q.audit.collection = rec.ID
	sess.RecordTransition(rec)
}

// serveCollectionCancel ends a live Collection.
// The read is what makes the revision the update is conditional on, and losing
// to the owner's renewal is the common race: the Collection is still live, so
// the loop reads again and retries.
// A terminal answer is only ever given from a read that shows a terminal
// state, never inferred from a lost update.
func (s *server) serveCollectionCancel(
	w http.ResponseWriter, r *http.Request, q *request, sess *pgo.Session, stored pgo.StoredRecord,
) {
	if berr := rejectBody(w, r); berr != nil {
		q.fail(w, berr)

		return
	}

	for attempt := range cancelAttempts {
		if attempt > 0 {
			next, rerr := s.readCollection(r.Context(), q, sess)
			if rerr != nil {
				q.fail(w, rerr)

				return
			}
			stored = next
		}

		rec := stored.Record
		switch rec.State {
		case pgo.StateInitializing:
			// Nonterminal: the publication is still in flight and the client retries.
			q.fail(w, errCollectionInitializing)

			return
		case pgo.StatePending, pgo.StateRunning:
		case pgo.StateCompleted, pgo.StateFailed, pgo.StateCancelled, pgo.StateExpired:
			q.fail(w, errCollectionTerminal)

			return
		default:
			q.fail(w, errCollectionTerminal)

			return
		}

		now := sess.Now()
		rec.State = pgo.StateCancelled
		rec.Reason = pgo.ReasonCancelledByAPI
		rec.FinishedAt = &now

		err := sess.WriteRecord(r.Context(), rec, stored.Revision)
		switch {
		case err == nil:
			sess.RecordTransition(rec)
			sess.ReleaseActive(r.Context(), rec)
			q.audit.status = http.StatusOK
			q.audit.code = codeOK
			writeJSON(w, http.StatusOK, rec)

			return
		case errors.Is(err, natskv.ErrRevisionMismatch):
			continue
		default:
			q.fail(w, storeError(err))

			return
		}
	}

	q.fail(w, errCASContended)
}
