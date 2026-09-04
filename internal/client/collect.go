package client

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Created is what a create answered: the identifier and the state, the
// status that carried them (202 for a new Collection, 200 for a replay of
// the key), the Location header, and the body verbatim for --output json.
type Created struct {
	ID       string
	State    string
	Status   int
	Location string
	Body     []byte
}

// createdBody is the two fields a create or its replay carries; a replay
// carries no record fields, and any other field is left unread.
type createdBody struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// errAnswerIncomplete marks an answer whose headers arrived and whose body was cut off before it could be read.
// The status says the gateway acted and the body that would have named what it did never arrived,
// so this is neither a transport failure nor a status the gateway chose.
// CreateIndeterminate is what a caller outside this package asks instead.
var errAnswerIncomplete = errors.New("the answer did not arrive whole")

// createRetryFirst, createRetryMax, and createRetryWindow pace the retry of a create whose result is unknown:
// one second doubling to eight, for at most thirty seconds.
const (
	createRetryFirst  = time.Second
	createRetryMax    = 8 * time.Second
	createRetryWindow = 30 * time.Second
)

// Create posts one Collection under key and returns the identifier and state the answer carried.
// The gateway records the key, so the same key posted again replays the
// Collection it created rather than starting a second one.
// Create therefore retries whenever the result of a create is unknown,
// on the injected clock and sleeper, at one second doubling to eight for at most thirty seconds.
// An answer that arrived whole and says something is returned as it came,
// every 4xx included, and the caller decides what it means.
func (c *Client) Create(ctx context.Context, ns, svc string, body []byte, key string) (Created, error) {
	if body == nil {
		body = []byte("{}")
	}
	req := Request{
		Method: http.MethodPost,
		Path:   "/v1/namespaces/" + url.PathEscape(ns) + "/services/" + url.PathEscape(svc) + "/collections",
		Body:   body,
		Header: http.Header{
			"Content-Type":    {"application/json"},
			"Accept":          {"application/json"},
			"Idempotency-Key": {key},
		},
	}
	deadline := c.now().Add(createRetryWindow)
	wait := createRetryFirst
	for {
		created, err := c.create(ctx, req)
		if err == nil || !CreateIndeterminate(err) {
			return created, err
		}
		remaining := deadline.Sub(c.now())
		if remaining <= 0 {
			return Created{}, err
		}
		// An interrupted sleep returns the attempt's own failure rather than
		// the interruption, because that failure is what says whether a
		// Collection may be running.
		if c.sleep(ctx, min(wait, remaining)) != nil {
			return Created{}, err
		}
		wait = min(wait*2, createRetryMax)
	}
}

// create posts one Collection once.
// The response is obtained first and its body read afterwards,
// so a body that is cut off after the headers arrived is neither a transport failure nor a status the gateway chose.
func (c *Client) create(ctx context.Context, req Request) (Created, error) {
	resp, err := c.Do(ctx, req)
	if err != nil {
		return Created{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := readBounded(resp.Body)
	switch {
	case errors.Is(err, errResponseTooLarge):
		return Created{}, fmt.Errorf("%s: %w", c.settings.Origin, err)
	case err != nil:
		return Created{}, fmt.Errorf("%s: %w: %w", c.settings.Origin, errAnswerIncomplete, err)
	}
	var answer createdBody
	if !json.Valid(raw) || decodeOne(raw, &answer) != nil {
		return Created{}, &StatusError{Status: resp.StatusCode}
	}
	if answer.ID == "" {
		return Created{}, &StatusError{Status: resp.StatusCode, Detail: "the response carries no id"}
	}
	return Created{
		ID:       answer.ID,
		State:    answer.State,
		Status:   resp.StatusCode,
		Location: resp.Header.Get("Location"),
		Body:     raw,
	}, nil
}

// CreateIndeterminate reports a create whose result its failure does not settle,
// which is what the same key is posted again for.
// Three failures leave the result unknown:
// no answer arrived at all, the gateway could not complete one, and an answer's body did not arrive whole.
// Everything else arrived whole and says something: every 4xx, and a 2xx whose body this client would not read.
func CreateIndeterminate(err error) bool {
	var te *TransportError
	var se *StatusError
	var ae *APIError
	switch {
	case errors.Is(err, errAnswerIncomplete):
		return true
	case errors.As(err, &te):
		return true
	case errors.As(err, &se):
		return se.Status >= http.StatusInternalServerError
	case errors.As(err, &ae):
		return ae.Status >= http.StatusInternalServerError
	}
	return false
}

// collectionIDAlphabet is Crockford base32 in lowercase, the alphabet of a
// Collection identifier: the digits and the letters that cannot be confused with them, so i, l, o, and u are absent.
const collectionIDAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// collectionIDLength is the character count of a Collection identifier.
const collectionIDLength = 20

// IsCollectionID reports whether s is the Collection identifier grammar exactly:
// 20 lowercase Crockford base32 characters, the grammar the
// gateway's route matching accepts and nothing else.
// The singular verbs refuse anything else before a request is built,
// which is what keeps a Service address out of the record route.
func IsCollectionID(s string) bool {
	if len(s) != collectionIDLength {
		return false
	}
	for i := range len(s) {
		if !strings.ContainsRune(collectionIDAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}

// CollectionPath is /v1/collections/{id}.
func CollectionPath(id string) string {
	return "/v1/collections/" + url.PathEscape(id)
}

// NewKey is one UUIDv4 from the injected random source, RFC 9562's version
// and variant bits set.
func NewKey(random io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", fmt.Errorf("idempotency key: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// The states of a Collection record, a closed set: the first three continue
// a wait and the last four end it.
const (
	StateInitializing = "initializing"
	StatePending      = "pending"
	StateRunning      = "running"
	StateCompleted    = "completed"
	StateFailed       = "failed"
	StateCancelled    = "cancelled"
	StateExpired      = "expired"
)

// ErrWaitTimeout marks a wait that reached its timeout before the record reached a terminal state.
var ErrWaitTimeout = errors.New("the wait timed out")

// ErrUnknownState marks a record whose state is none of the seven the
// gateway defines; a client that treated it as non-terminal would poll until its deadline for no reason.
var ErrUnknownState = errors.New("unknown collection state")

// cancelRetryInterval and cancelRetryWindow pace the retry of a cancel that met 409 collection_initializing,
// which means "not yet claimable, retry".
const (
	cancelRetryInterval = time.Second
	cancelRetryWindow   = 10 * time.Second
)

// Wait polls the record on the injected clock and sleeper until a terminal state, the timeout, or a cancelled context, writing one progress line to w
// each time the record's progress changes.
// It returns the terminal record with its body verbatim; the caller decides what each terminal state means.
// A 503 pgo_unavailable is retried on the same interval,
// because the record outlives a NATS outage;
// any other refusal ends the wait.
func (c *Client) Wait(ctx context.Context, id string, interval, timeout time.Duration, w io.Writer) (CollectionRecord, []byte, error) {
	deadline := c.now().Add(timeout)
	var last CollectionProgress
	for {
		body, _, err := c.JSON(ctx, Request{Method: http.MethodGet, Path: CollectionPath(id)})
		switch {
		case err == nil:
			rec, err := Decode[CollectionRecord](body)
			if err != nil {
				return CollectionRecord{}, nil, err
			}
			if rec.Progress != last && rec.Progress.Rounds > 0 {
				last = rec.Progress
				_, _ = fmt.Fprintf(w, "round %d of %d, %d ok, %d failed\n", last.Round, last.Rounds, last.SamplesOK, last.SamplesFailed)
			}
			switch rec.State {
			case StateCompleted, StateFailed, StateCancelled, StateExpired:
				return rec, body, nil
			case StateInitializing, StatePending, StateRunning:
			default:
				return CollectionRecord{}, nil, fmt.Errorf("%w: %q", ErrUnknownState, rec.State)
			}
		case isCode(err, http.StatusServiceUnavailable, "pgo_unavailable"):
		default:
			return CollectionRecord{}, nil, err
		}
		remaining := deadline.Sub(c.now())
		if remaining <= 0 {
			return CollectionRecord{}, nil, fmt.Errorf("%w after %s", ErrWaitTimeout, timeout)
		}
		if err := c.sleep(ctx, min(interval, remaining)); err != nil {
			return CollectionRecord{}, nil, err
		}
	}
}

// Cancel posts the cancel with the JSON media type and no body, retrying
// 409 collection_initializing once a second for ten seconds on the injected clock, and returns the updated record's body.
// 409 collection_terminal and every other refusal are returned as they came.
func (c *Client) Cancel(ctx context.Context, id string) ([]byte, error) {
	deadline := c.now().Add(cancelRetryWindow)
	for {
		body, _, err := c.JSON(ctx, Request{
			Method: http.MethodPost,
			Path:   CollectionPath(id) + "/cancel",
			Header: http.Header{"Content-Type": {"application/json"}},
		})
		if err == nil {
			return body, nil
		}
		if !isCode(err, http.StatusConflict, "collection_initializing") || !c.now().Before(deadline) {
			return nil, err
		}
		if err := c.sleep(ctx, cancelRetryInterval); err != nil {
			return nil, err
		}
	}
}

// isCode reports whether err is the gateway's envelope with this status and code.
func isCode(err error, status int, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status && ae.Code == code
}
