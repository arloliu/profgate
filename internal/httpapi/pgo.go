package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/natskv"
	"github.com/arloliu/profgate/internal/pgo"
)

// maxBodyBytes is the largest request body any PGO route reads.
// A policy override and a Collection request are both a few hundred bytes, so
// the bound is generous and still refuses a body that would cost memory.
const maxBodyBytes = 64 << 10

// configAPIDisabled is the pgo.configAPI value that closes the two policy writes.
const configAPIDisabled = "disabled"

// The audit-only codes of the spec's "Errors" table: outcomes with a code of
// their own that are never an HTTP status of their own.
const (
	codeCASContended        = "cas_contended"
	codeArtifactStreamFail  = "artifact_stream_failed"
	codeClientGone          = "client_gone"
	codeCollectionCancelled = "cancelled"
)

// The errors more than one PGO route answers with.
var (
	// errPGOUnavailable is every way the stores cannot be decided from:
	// the runtime is not bound yet, the connection is down, the watches have
	// not replayed under this generation, or a call reported ErrUnavailable.
	errPGOUnavailable = &requestError{
		status:  http.StatusServiceUnavailable,
		code:    "pgo_unavailable",
		message: "pgo state is unavailable",
	}
	// errCollectionNotFound is the one answer for a Collection that does not
	// exist and for one the realm denies.
	// The identifier is opaque, so telling them apart would leak nothing the
	// realm hides and telling them alike costs nothing.
	errCollectionNotFound = &requestError{
		status:  http.StatusNotFound,
		code:    "collection_not_found",
		message: "no such collection",
	}
	errArtifactGone = &requestError{
		status:  http.StatusGone,
		code:    "artifact_gone",
		message: "the profile of this collection is no longer stored",
	}
)

// servePGOService dispatches the four Service-scoped PGO routes, past the
// realm check their namespace and Service were evaluated against.
func (s *server) servePGOService(
	w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config, sess *pgo.Session, principal string,
) {
	if r.URL.RawQuery != "" {
		q.fail(w, invalidParameter("this endpoint takes no parameters"))

		return
	}

	switch {
	case q.route.kind == kindPGOPolicy && r.Method == http.MethodGet:
		s.servePolicyRead(w, r, q, sess)
	case q.route.kind == kindPGOPolicy && r.Method == http.MethodPut:
		s.servePolicyWrite(w, r, q, cfg, sess, principal)
	case q.route.kind == kindPGOPolicy:
		s.servePolicyDelete(w, r, q, cfg, sess)
	case r.Method == http.MethodGet:
		s.serveCollectionList(w, q, sess)
	default:
		s.serveCollectionCreate(w, r, q, sess, principal)
	}
}

// servePGOCollection reads the Collection first and evaluates the realm
// against the record's own namespace and Service, then dispatches.
// A record the realm denies and a record that does not exist answer alike.
func (s *server) servePGOCollection(
	w http.ResponseWriter, r *http.Request, q *request, _ *config.Config,
	sess *pgo.Session, realm config.Realm, realmOK bool,
) {
	if r.URL.RawQuery != "" {
		q.fail(w, invalidParameter("this endpoint takes no parameters"))

		return
	}

	stored, rerr := s.readCollection(r.Context(), q, sess)
	if rerr != nil {
		q.fail(w, rerr)

		return
	}
	rec := stored.Record
	q.audit.namespace = rec.Namespace
	q.audit.service = rec.Service

	scoped := q.route
	scoped.namespace = rec.Namespace
	scoped.service = rec.Service
	if !realmOK || !realmAllows(realm, scoped, r.Method) {
		q.fail(w, errCollectionNotFound)

		return
	}

	switch q.route.kind {
	case kindCollection:
		s.serveCollectionRead(w, q, stored)
	case kindCollectionProfile:
		s.serveCollectionDownload(w, r, q, sess, stored)
	case kindCollectionCancel:
		s.serveCollectionCancel(w, r, q, sess, stored)
	case kindTargets, kindProfile, kindPGOPolicy, kindCollections:
		q.fail(w, errCollectionNotFound)
	}
}

// readCollection reads one Collection record fresh and maps the seam's errors.
// The read is what makes the revision the request's later conditional write is
// made at; it is not a pre-check.
func (s *server) readCollection(
	ctx context.Context, q *request, sess *pgo.Session,
) (pgo.StoredRecord, *requestError) {
	stored, err := sess.ReadRecord(ctx, q.route.collection)
	switch {
	case err == nil:
		return stored, nil
	case errors.Is(err, natskv.ErrKeyNotFound):
		return pgo.StoredRecord{}, errCollectionNotFound
	case errors.Is(err, natskv.ErrUnavailable):
		return pgo.StoredRecord{}, errPGOUnavailable
	default:
		// The record is there and the gateway cannot read it, which is not the
		// client's to fix and not a Collection it can be told does not exist.
		s.deps.Logger.Warn("pgo: collection record is not readable",
			"collection", q.route.collection, "error", err)

		return pgo.StoredRecord{}, errPGOUnavailable
	}
}

// storeError maps a store call the routes have no more specific answer for.
func storeError(err error) *requestError {
	if errors.Is(err, natskv.ErrKeyNotFound) {
		return errCollectionNotFound
	}

	return errPGOUnavailable
}

// decodeBody reads at most maxBodyBytes of JSON into v, rejecting a field the
// type does not declare and anything after the first value.
// allowEmpty accepts a request that sent no body at all as the empty object,
// which is what POST /collections means by "every field is optional".
func decodeBody(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) *requestError {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()

	switch err := dec.Decode(v); {
	case err == nil:
	case errors.Is(err, io.EOF) && allowEmpty:
		return nil
	default:
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return invalidParameter(fmt.Sprintf("the request body is larger than %d bytes", maxBodyBytes))
		}

		return invalidParameter(fmt.Sprintf("the request body is not valid: %v", err))
	}

	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return invalidParameter("the request body carries more than one value")
	}

	return nil
}

// rejectBody refuses a request that carries one where the route accepts none.
func rejectBody(w http.ResponseWriter, r *http.Request) *requestError {
	var probe [1]byte
	if n, _ := io.ReadFull(http.MaxBytesReader(w, r.Body, 1), probe[:]); n > 0 {
		return invalidParameter("this endpoint takes no request body")
	}

	return nil
}

// writeJSON writes one PGO response body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encode adds the trailing newline; a failure here is the client's connection going away.
	_ = json.NewEncoder(w).Encode(body)
}
