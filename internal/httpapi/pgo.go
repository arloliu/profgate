package httpapi

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

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
	codeCASContended       = "cas_contended"
	codeArtifactStreamFail = "artifact_stream_failed"
	codeClientGone         = "client_gone"
)

// The errors more than one PGO route answers with.
var (
	// errPGOUnavailable is every way the stores cannot be decided from:
	// the runtime is not bound yet, the connection is down, the watches have
	// not replayed under this generation, or a call reported ErrUnavailable.
	errPGOUnavailable = &requestError{
		status:  http.StatusServiceUnavailable,
		code:    CodePGOUnavailable,
		message: "pgo state is unavailable",
	}
	// errCollectionNotFound is the one answer for a Collection that does not
	// exist and for one the realm denies.
	// The identifier is opaque, so telling them apart would leak nothing the
	// realm hides and telling them alike costs nothing.
	errCollectionNotFound = &requestError{
		status:  http.StatusNotFound,
		code:    CodeCollectionNotFound,
		message: "no such collection",
	}
	errArtifactGone = &requestError{
		status:  http.StatusGone,
		code:    CodeArtifactGone,
		message: "the profile of this collection is no longer stored",
	}
)

// servePGOService dispatches the six Service-scoped PGO routes, past the
// realm check their namespace and Service were evaluated against.
func (s *server) servePGOService(
	w http.ResponseWriter, r *http.Request, q *request, cfg *config.Config, sess *pgo.Session, principal string,
) {
	// The Collection listing is the one Service-scoped route that takes query parameters;
	// every other one refuses a query as it always has.
	listing := q.route.kind == kindCollections && r.Method == http.MethodGet
	var query pgo.CollectionQuery
	if listing {
		var perr *requestError
		if query, perr = parseCollectionList(r.URL.RawQuery); perr != nil {
			q.fail(w, perr)

			return
		}
	} else if r.URL.RawQuery != "" {
		q.fail(w, noParameters(r.URL.RawQuery))

		return
	}

	switch {
	case q.route.kind == kindPGOPolicy && r.Method == http.MethodGet:
		s.servePolicyRead(w, r, q, sess)
	case q.route.kind == kindPGOPolicy && r.Method == http.MethodPut:
		s.servePolicyWrite(w, r, q, cfg, sess, principal)
	case q.route.kind == kindPGOPolicy:
		s.servePolicyDelete(w, r, q, cfg, sess)
	case q.route.kind == kindCollectionLatest, q.route.kind == kindCollectionLatestProfile:
		s.serveLatestCollection(w, r, q, sess)
	case listing:
		s.serveCollectionList(w, q, sess, query)
	default:
		s.serveCollectionCreate(w, r, q, sess, principal)
	}
}

// servePGOCollection reads the Collection first and evaluates the realm
// against the record's own namespace and Service, then dispatches.
// A record the realm denies and a record that does not exist answer alike.
func (s *server) servePGOCollection(
	w http.ResponseWriter, r *http.Request, q *request, _ *config.Config,
	sess *pgo.Session, realm config.Realm,
) {
	// The Collection read is the one route here that takes a parameter;
	// the download and the cancel take none and refuse any query as they always have.
	var wait time.Duration
	if q.route.kind == kindCollection {
		var perr *requestError
		if wait, perr = parseWait(r.URL.RawQuery); perr != nil {
			q.fail(w, perr)

			return
		}
		q.audit.wait = wait
		if wait > 0 {
			// Every answer to an accepted wait carries the header, including
			// the ones given before the wait itself begins.
			w.Header().Set(waitElapsedHeader, waitElapsed(0))
		}
	} else if r.URL.RawQuery != "" {
		q.fail(w, noParameters(r.URL.RawQuery))

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
	if !realmAllows(realm, scoped, r.Method) {
		q.fail(w, errCollectionNotFound)

		return
	}

	switch q.route.kind {
	case kindCollection:
		s.serveCollectionRead(w, r, q, sess, stored, wait)
	case kindCollectionProfile:
		s.serveCollectionDownload(w, r, q, sess, stored)
	case kindCollectionCancel:
		s.serveCollectionCancel(w, r, q, sess, stored)
	case kindTargets, kindProfile, kindPGOPolicy, kindCollections, kindCollectionLatest,
		kindCollectionLatestProfile, kindNamespaces, kindServices,
		kindWhoami, kindLimits, kindAuth, kindAuthLogin, kindAuthCallback, kindAuthLogout,
		kindOpenAPI, kindConsole:
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
//
// The unknown field is located before the value is decoded, because the
// standard decoder reports a field name with no path and a create body nests:
// sampling.rounds and {"bogus": 1} would otherwise answer alike.
func (s *server) decodeBody(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) *requestError {
	// A body that arrives one byte at a time held this read for as long as the client chose.
	// A read that fails leaves the deadline armed:
	// net/http reads the rest of an unread body after the handler returns, up to 256 KiB,
	// and the same deadline ends that read at once instead of letting the same client stall it.
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(s.bodyReadTimeout))
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return s.bodyUnreadable(err)
	}
	_ = control.SetReadDeadline(time.Time{})
	if allowEmpty && len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if pointer, found := firstUnknownField(raw, reflect.TypeOf(v)); found {
		return invalidParameter(fmt.Sprintf("the request body carries no field %s", pointer),
			bodyFault(detailUnknownField, pointer, "this route accepts no field at that path"))
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bodyMalformed(fmt.Sprintf("the request body is not valid: %v", err))
	}
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return bodyMalformed("the request body carries more than one value")
	}

	return nil
}

// bodyMalformed refuses a body no field of which can be named at fault:
// one that is not JSON, one over the size limit, or one carrying a value the
// target type cannot hold.
func bodyMalformed(message string) *requestError {
	return invalidParameter(message, bodyFault(detailBodyMalformed, "", message))
}

// bodyUnreadable refuses a body whose read failed.
// The deadline's error names both socket addresses,
// so it is answered with a fixed message that names the bound and nothing else.
func (s *server) bodyUnreadable(err error) *requestError {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return bodyMalformed(fmt.Sprintf("the request body did not arrive within %s", s.bodyReadTimeout))
	case errors.As(err, &tooLarge):
		return bodyMalformed(fmt.Sprintf("the request body is larger than %d bytes", maxBodyBytes))
	default:
		return bodyMalformed(fmt.Sprintf("the request body is not readable: %v", err))
	}
}

// rejectBody refuses a request that carries one where the route accepts none.
// The probe read takes the same deadline decodeBody's read does,
// and a probe that fails is answered the way an unreadable body is;
// only a probe that reached the body's end, finding nothing, clears it.
func (s *server) rejectBody(w http.ResponseWriter, r *http.Request) *requestError {
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(s.bodyReadTimeout))
	var probe [1]byte
	n, err := io.ReadFull(http.MaxBytesReader(w, r.Body, 1), probe[:])
	switch {
	case n > 0:
		const message = "this endpoint takes no request body"

		return invalidParameter(message, bodyFault(detailBodyNotAllowed, "", message))
	case errors.Is(err, io.EOF):
		_ = control.SetReadDeadline(time.Time{})

		return nil
	default:
		return s.bodyUnreadable(err)
	}
}

// The two interfaces that make a type decode itself.
// Their inside is not a set of declared JSON names,
// so the walk stops there and leaves a value they refuse to the strict decode,
// which answers body_malformed.
var (
	jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// firstUnknownField walks the body in document order against the JSON names t declares,
// and returns a pointer to the first name it does not.
// A body that is not a JSON object is left to the strict decode,
// and so is a value whose shape the target type cannot hold.
func firstUnknownField(raw []byte, t reflect.Type) (string, bool) {
	return walkObject(raw, walkableStruct(t), "")
}

// walkObject reads one JSON object against the struct it decodes into,
// descending into a field that is itself a struct of declared names.
func walkObject(raw []byte, t reflect.Type, prefix string) (string, bool) {
	if t == nil {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return "", false
		}
		name, ok := key.(string)
		if !ok {
			return "", false
		}
		pointer := prefix + "/" + escapePointer(name)
		field, declared := declaredField(t, name)
		if !declared {
			return pointer, true
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return "", false
		}
		if inner, found := walkObject(value, walkableStruct(field), pointer); found {
			return inner, true
		}
	}

	return "", false
}

// walkableStruct is the struct type a JSON object may be walked against:
// the type with its pointers removed, and nothing that decodes itself.
func walkableStruct(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	for _, iface := range []reflect.Type{jsonUnmarshalerType, textUnmarshalerType} {
		if t.Implements(iface) || reflect.PointerTo(t).Implements(iface) {
			return nil
		}
	}

	return t
}

// declaredField finds the field a body name decodes into,
// matching the way encoding/json does: the declared name exactly, then without regard to case.
func declaredField(t reflect.Type, name string) (reflect.Type, bool) {
	for _, fold := range []bool{false, true} {
		for i := range t.NumField() {
			f := t.Field(i)
			declared, ok := declaredName(f)
			if !ok {
				continue
			}
			if declared == name || (fold && strings.EqualFold(declared, name)) {
				return f.Type, true
			}
		}
	}

	return nil, false
}

// declaredName is the JSON name of one struct field, and false for a field
// encoding/json never fills from a body.
func declaredName(f reflect.StructField) (string, bool) {
	if !f.IsExported() {
		return "", false
	}
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return f.Name, true
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return "", false
	}
	if name == "" {
		return f.Name, true
	}

	return name, true
}

// escapePointer writes one body field name as a JSON pointer token.
func escapePointer(name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
}

// writeJSON writes one PGO response body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encode adds the trailing newline; a failure here is the client's connection going away.
	_ = json.NewEncoder(w).Encode(body)
}
