package httpapi

import (
	"context"
	"errors"
	"io"
	"mime"
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
		code:    CodeCollectionInProgress,
		message: "the service already has a live collection",
	}
	errRateLimited = &requestError{
		status:  http.StatusTooManyRequests,
		code:    CodeRateLimited,
		message: "too many on-demand collections; retry shortly",
	}
	errCapacityExhausted = &requestError{
		status:  http.StatusTooManyRequests,
		code:    CodeCapacityExhausted,
		message: "the gateway is running its limit of live collections",
	}
	errCollectionNotCompleted = &requestError{
		status:  http.StatusConflict,
		code:    CodeCollectionNotCompleted,
		message: "the collection has no stored profile",
	}
	errCollectionInitializing = &requestError{
		status:  http.StatusConflict,
		code:    CodeCollectionInitializing,
		message: "the collection is still being published; retry shortly",
	}
	errCollectionTerminal = &requestError{
		status:  http.StatusConflict,
		code:    CodeCollectionTerminal,
		message: "the collection has already finished",
	}
	errIdempotencyMismatch = &requestError{
		status:  http.StatusConflict,
		code:    CodeIdempotencyMismatch,
		message: "this idempotency key already stands for another collection request",
	}
	errCASContended = &requestError{
		status:    http.StatusServiceUnavailable,
		code:      CodePGOUnavailable,
		message:   "the collection is changing faster than it can be cancelled; retry",
		auditCode: codeCASContended,
	}
)

// contentTypeHeader is the header a POST to a write route declares its media type in,
// and jsonMediaType is the one essence those routes accept.
const (
	contentTypeHeader = "Content-Type"
	jsonMediaType     = "application/json"
)

// idempotencyKeyHeader is the header a create binds itself to one Collection with.
const idempotencyKeyHeader = "Idempotency-Key"

// idempotencyKey is the key one create carries,
// and the empty string for a request that carried none.
// The grammar is the request identifier's, 1 to 128 bytes drawn from [A-Za-z0-9._-].
// The two headers differ in what a value outside that set earns:
// an identifier is replaced because it decides nothing,
// while a key the gateway cannot read is refused,
// because this header decides whether a Collection is created.
func idempotencyKey(h http.Header) (string, *requestError) {
	const message = "the idempotency key is not one this route can read"

	values := h.Values(idempotencyKeyHeader)
	switch {
	case len(values) == 0:
		return "", nil
	case len(values) > 1:
		return "", invalidParameter(message,
			headerFault(detailHeaderMalformed, idempotencyKeyHeader, "send this header once"))
	case !echoable(values[0]):
		return "", invalidParameter(message,
			headerFault(detailHeaderMalformed, idempotencyKeyHeader,
				"the key is 1 to 128 bytes drawn from [A-Za-z0-9._-]"))
	}

	return values[0], nil
}

// mediaTypeFault reports why a write route refuses this request's Content-Type,
// or nil when the header is one it accepts.
// The essence must be application/json,
// and every parameter mime.ParseMediaType returns is accepted and ignored, charset among them,
// so no client is refused over a parameter the route does not read.
// A header sent twice is refused rather than resolved:
// which of the two values the route would act on is not the gateway's to pick.
// The answer is decided by the request's own headers alone,
// which is what lets the step run before readiness, before a credential is read, and before the realm.
func mediaTypeFault(h http.Header) *requestError {
	const message = "this route requires a json request media type"

	values := h.Values(contentTypeHeader)
	switch {
	case len(values) == 0:
		return invalidParameter(message,
			headerFault(detailHeaderRequired, contentTypeHeader, "declare "+jsonMediaType))
	case len(values) > 1:
		return invalidParameter(message,
			headerFault(detailHeaderMalformed, contentTypeHeader, "send this header once"))
	}

	essence, _, err := mime.ParseMediaType(values[0])
	if err != nil || essence != jsonMediaType {
		return invalidParameter(message,
			headerFault(detailHeaderMalformed, contentTypeHeader, "the media type must be "+jsonMediaType))
	}

	return nil
}

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
// The idempotency key is read first,
// so a key the gateway cannot read is refused whatever the body carries.
// The receipt lookup comes as soon as the snapshot its comparison needs exists,
// and before every refusal that writes nothing.
// A replay creates nothing,
// so none of the bounds those refusals hold apply to it,
// and a ceiling that moved between two requests cannot refuse a Collection that is already running.
// The token bucket comes next,
// so a caller with pgo.collect across many Services cannot make the gateway work for requests it cannot create.
// The advisory target resolution follows,
// so both version answers are given before anything is written
// and a request that cannot succeed leaves no key behind;
// the round is still the authority, so a Collection can fail for the same reason later.
// Only then is a reservation taken and the publication's writes made.
func (s *server) serveCollectionCreate(
	w http.ResponseWriter, r *http.Request, q *request, sess *pgo.Session, principal string,
) {
	key, kerr := idempotencyKey(r.Header)
	if kerr != nil {
		q.fail(w, kerr)

		return
	}

	var body pgo.PolicyOverride
	if berr := decodeBody(w, r, &body, true); berr != nil {
		q.fail(w, berr)

		return
	}
	// The two fields a policy override may set and a Collection request may not,
	// named in the order the policy declares them.
	var notApplicable []errorDetail
	if body.Enabled != nil {
		notApplicable = append(notApplicable,
			bodyFault(detailFieldNotApplicable, "/enabled", "a collection request does not set enabled"))
	}
	if body.Schedule != nil {
		notApplicable = append(notApplicable,
			bodyFault(detailFieldNotApplicable, "/schedule", "a collection request does not set schedule"))
	}
	if len(notApplicable) > 0 {
		q.fail(w, invalidParameter("a collection request sets neither enabled nor schedule", notApplicable...))

		return
	}

	rt := q.route
	// The override comes from the watched cache, which is read only behind the
	// replay barrier, so configRevision is never 0 for a Service that has one.
	stored, configRevision := sess.CachedOverride(rt.namespace, rt.service)
	snapshot, violations := sess.Effective(stored, &body)

	// What one key already stands for, read from the store on every request.
	// The hash is of the snapshot rather than of the body, so identical JSON
	// whose effective policy has moved is a different request.
	var receiptKey, hash string
	if key != "" {
		receiptKey = pgo.ReceiptKey(principal, rt.namespace, rt.service, key)
		hash = pgo.SnapshotHash(snapshot)
		bound, lerr := s.lookupReceipt(r.Context(), sess, receiptKey, hash, true)
		if lerr != nil {
			q.fail(w, lerr)

			return
		}
		if s.answerFromReceipt(w, q, bound) {
			return
		}
	}

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
		q.fail(w, versionRefusal(reason, rt.namespace, rt.service, snapshot.Target.Version))

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
		IdempotencyKey: key,
	})
	switch {
	case err != nil:
		s.deps.Logger.Warn("pgo: publishing an on-demand collection failed",
			"namespace", rt.namespace, "service", rt.service, "collection", id, "error", err)
		q.fail(w, errPGOUnavailable)
	case outcome == pgo.OutcomeBusy:
		s.refuseLoser(w, r, q, sess, receiptKey, hash, key)
	default:
		q.audit.collection = id
		q.audit.status = http.StatusAccepted
		q.audit.code = codeOK
		w.Header().Set("Location", collectionLocation(id))
		writeJSON(w, http.StatusAccepted, acceptedBody{ID: id, State: pgo.StatePending})
	}
}

// receiptLookup is what a keyed request's receipt names.
// Both fields empty is a key with nothing to answer from,
// which creates before the publication and is refused after a lost one.
type receiptLookup struct {
	// record is the Collection the key is bound to, when the binding stands.
	record *pgo.Record
	// mismatch reports a key that already stands for a different request.
	mismatch bool
}

// lookupReceipt reads the receipt one key is bound by and reports what it names.
// The read is authoritative:
// no watch is opened on the prefix and no cache holds it,
// so a miss is the store's answer and never replay lag.
//
// A receipt whose record is gone,
// and one whose record the scan failed not_published,
// both name a Collection that no longer stands:
// the first reached its retention,
// and the second never became claimable and never ran.
// clearStale deletes such a receipt at the revision this read returned,
// which frees the key for the create that follows;
// losing that delete means another request cleaned it or a create wrote a newer one,
// so the receipt is read once more.
// A caller that cannot create — a publication that already lost the active key —
// leaves a stale receipt alone and is refused instead.
func (s *server) lookupReceipt(
	ctx context.Context, sess *pgo.Session, receiptKey, hash string, clearStale bool,
) (receiptLookup, *requestError) {
	for range receiptReads {
		receipt, revision, err := sess.ReadReceipt(ctx, receiptKey)
		switch {
		case errors.Is(err, natskv.ErrKeyNotFound):
			return receiptLookup{}, nil
		case err != nil:
			s.deps.Logger.Warn("pgo: idempotency receipt is not readable", "error", err)

			return receiptLookup{}, errPGOUnavailable
		}

		stored, rerr := sess.ReadRecord(ctx, receipt.ID)
		switch {
		case rerr == nil:
			rec := stored.Record
			if rec.State != pgo.StateFailed || rec.Reason != pgo.ReasonNotPublished {
				if receipt.SnapshotHash != hash {
					return receiptLookup{mismatch: true}, nil
				}

				return receiptLookup{record: &rec}, nil
			}
		case errors.Is(rerr, natskv.ErrKeyNotFound):
		default:
			s.deps.Logger.Warn("pgo: collection record of an idempotency receipt is not readable",
				"collection", receipt.ID, "error", rerr)

			return receiptLookup{}, errPGOUnavailable
		}

		if !clearStale {
			return receiptLookup{}, nil
		}
		if derr := sess.DeleteReceipt(ctx, receiptKey, revision); derr == nil {
			return receiptLookup{}, nil
		}
	}

	// The receipt moved under both reads;
	// the publication's own receipt write clears what is left of it.
	return receiptLookup{}, nil
}

// receiptReads is how many times one request reads a receipt:
// the read it decides from, and one more after a delete another writer won.
const receiptReads = 2

// answerFromReceipt writes the answer a bound key earns and reports whether it wrote one.
// A replay carries the create acknowledgement the first answer carried and nothing else:
// this route needs pgo.collect and reading a record needs pgo.read,
// so a full record here would hand a collect-only principal what its realm denies on the route that serves it.
func (s *server) answerFromReceipt(w http.ResponseWriter, q *request, bound receiptLookup) bool {
	switch {
	case bound.mismatch:
		q.fail(w, errIdempotencyMismatch)

		return true
	case bound.record != nil:
		q.audit.collection = bound.record.ID
		q.audit.status = http.StatusOK
		q.audit.code = codeOK
		w.Header().Set("Location", collectionLocation(bound.record.ID))
		writeJSON(w, http.StatusOK, acceptedBody{ID: bound.record.ID, State: bound.record.State})

		return true
	default:
		return false
	}
}

// refuseLoser answers a publication whose active create lost.
// A keyed loser reads its receipt once more:
// the winner may have published under the same key while this request was writing,
// and the answer is then the winner's identifier.
// Without a binding to answer from it is refused for one second,
// whatever the winner's record holds.
// An identifier taken from a record that is not yet claimable could name a Collection that the winner withdraws,
// and the caller's retry reads the receipt or creates.
func (s *server) refuseLoser(
	w http.ResponseWriter, r *http.Request, q *request, sess *pgo.Session, receiptKey, hash, key string,
) {
	if key == "" {
		q.fail(w, errCollectionInProgress)

		return
	}

	bound, lerr := s.lookupReceipt(r.Context(), sess, receiptKey, hash, false)
	if lerr != nil {
		q.fail(w, lerr)

		return
	}
	if s.answerFromReceipt(w, q, bound) {
		return
	}

	w.Header().Set("Retry-After", "1")
	q.fail(w, errCollectionInProgress)
}

// collectionLocation is the path of one Collection, which a create and a
// replay both name.
func collectionLocation(id string) string { return "/v1/collections/" + id }

// versionRefusal is the advisory version refusal a create earns,
// one error per reason, each naming the Service and never a Pod's address.
// One missing-version refusal covers two situations,
// so the text says which one the caller is in:
// no Pod carries a version label at all,
// or none carries the version the effective policy pins.
// A conflict never carries a pin,
// since the pin filter leaves at most one version to disagree over.
func versionRefusal(reason, namespace, service, pin string) *requestError {
	where := " of service " + service + " in namespace " + namespace
	if reason == pgo.ReasonVersionMissing {
		message := "no pod" + where + " carries a version label"
		if pin != "" {
			message = "no pod" + where + " carries version " + pin
		}

		return &requestError{status: http.StatusConflict, code: CodeVersionMissing, message: message}
	}

	return &requestError{
		status:  http.StatusConflict,
		code:    CodeVersionConflict,
		message: "the pods" + where + " carry more than one version",
	}
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
