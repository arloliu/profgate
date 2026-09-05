// Package natskv is the NATS seam: the only non-test package that imports
// github.com/nats-io/nats.go.
// Its exported interface is the complete set of things Profgate can do to
// NATS — three buckets reached through generation-bound views, so a caller
// never acts on a result that belongs to a connection it no longer has.
package natskv

import (
	"context"
	"errors"
	"io"
	"time"
)

// Sentinel errors, matched with errors.Is.
var (
	// ErrKeyNotFound is returned by Get on an absent or deleted key.
	ErrKeyNotFound = errors.New("key not found")
	// ErrKeyExists is returned when Create lost: another actor wrote the key first.
	ErrKeyExists = errors.New("key exists")
	// ErrRevisionMismatch is returned when Update or Delete lost:
	// the key moved past the expected revision.
	ErrRevisionMismatch = errors.New("revision mismatch")
	// ErrObjectNotFound is returned by Get on an absent object.
	ErrObjectNotFound = errors.New("object not found")
	// ErrUnavailable is returned when the connection is down, the request
	// timed out, or the view's generation is no longer current.
	ErrUnavailable = errors.New("nats unavailable")
)

// Entry is one KV value with the revision that produced it.
type Entry struct {
	Key        string
	Value      []byte
	Revision   uint64
	Created    time.Time // server timestamp of this revision, KeyValueEntry.Created()
	Synced     bool      // true on the one marker entry that ends the initial replay; Key is empty
	Generation uint64    // the store generation this entry was delivered under
}

// KV is one bucket.
type KV interface {
	// Get returns the latest entry; ErrKeyNotFound when absent or deleted.
	Get(ctx context.Context, key string) (Entry, error)
	// Create stores value only when key is absent; ErrKeyExists otherwise.
	Create(ctx context.Context, key string, value []byte) (revision uint64, err error)
	// Update stores value only when the key's current revision equals expected;
	// ErrRevisionMismatch otherwise.
	Update(ctx context.Context, key string, value []byte, expected uint64) (revision uint64, err error)
	// Delete removes the key only when its current revision equals expected.
	Delete(ctx context.Context, key string, expected uint64) error
	// Keys lists live keys under prefix.
	Keys(ctx context.Context, prefix string) ([]string, error)
	// Watch delivers every live entry under prefix, then every later change, until ctx ends.
	// A deleted key arrives as an Entry with a nil Value.
	// The end of the initial replay arrives as one Entry with Synced set and no Key;
	// every Entry after it is a live change.
	Watch(ctx context.Context, prefix string) (<-chan Entry, error)
}

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Name    string
	Size    uint64
	ModTime time.Time // set by the NATS server when the object was put
}

// Objects is one Object Store bucket.
type Objects interface {
	Put(ctx context.Context, name string, r io.Reader) error
	// Get returns a reader for the object's bytes; ErrObjectNotFound when absent.
	// The reader follows ctx: it ends when ctx ends or when it is closed, whichever comes first,
	// and a pending Read returns then with the cause.
	// The call deadline bounds opening the object and each wait for a chunk, never the transfer,
	// so a store that stops delivering fails a Read one deadline into the wait with ErrUnavailable.
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// Delete removes the object; an absent name is success.
	Delete(ctx context.Context, name string) error
	// List returns every live object in the bucket.
	List(ctx context.Context) ([]ObjectInfo, error)
}

// Status is the server-side configuration of one bucket,
// read from its stream configuration.
type Status struct {
	TTL          time.Duration // 0 when none
	MaxValueSize int64         // -1 when unlimited; KV only
	MaxBytes     int64         // -1 when unlimited
	Storage      string        // "file" or "memory"
	Discard      string        // "old" or "new"
}

// Statused is implemented by every KV and Objects value the seam returns.
type Statused interface {
	Status(ctx context.Context) (Status, error)
}

// Stores is a view of the three buckets bound to one store generation.
// Every method of its KV and Objects values compares the view's generation
// with the client's current generation before issuing the call and again
// when the result arrives, and returns ErrUnavailable on either mismatch;
// a mismatched mutation is indeterminate, like any other ErrUnavailable.
// There is no unbound accessor: the only way to reach a bucket is through a view.
type Stores struct {
	Config    KV
	Jobs      KV
	Artifacts Objects
}

// Client is the connection; Preflight returns it and the rest of the gateway consumes it.
type Client interface {
	// Connected reports whether the underlying connection is currently up.
	Connected() bool
	// Generation returns the store generation:
	// a counter the seam increments in the nats.go disconnected callback,
	// and again when a watch's subscription closes under a live connection,
	// never in the reconnected one.
	Generation() uint64
	// Synced reports whether every watch opened by the PGO runtime has delivered
	// its initial-replay marker under generation gen.
	Synced(gen uint64) bool
	// View returns the stores bound to gen.
	// It is ErrUnavailable when gen is not the current generation.
	View(gen uint64) (Stores, error)
}

// Options carries what Preflight needs to reach the NATS cluster.
type Options struct {
	URL            string
	CredsFile      string
	ConnectTimeout time.Duration
	// OnConnectionChange, when set, runs once with true after the initial
	// connection succeeds inside Preflight, then in the disconnected callback
	// with false and the reconnected callback with true; the caller wires it to
	// metrics.Recorder.NATSConnected so natskv never imports internal/metrics.
	// Without the initial call the gauge would read zero until the first reconnect.
	OnConnectionChange func(up bool)

	// OnGenerationMove, when set, runs after either event that moves the store generation:
	// the disconnected callback, or a watch whose subscription closed under a live connection.
	// The caller wires it to the runtime call that ends every request parked under the generation just left behind.
	// It is a separate field from OnConnectionChange because a watch cut moves the generation
	// while the connection is still up.
	OnGenerationMove func()
}
