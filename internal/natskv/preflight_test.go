package natskv

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const testInstanceID = "test0"

// preflight runs the exported Preflight against the fixture's server as the
// seam user and closes the returned client on success.
func (f *fixture) preflight(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*fixtureTimeout)
	defer cancel()
	cl, err := Preflight(ctx, Options{URL: f.url(fixtureClientUser), ConnectTimeout: callTimeout},
		testInstanceID, testLogger())
	if err == nil {
		cl.(*client).close()
	}
	return err
}

// wantsFatal asserts err names every wanted fragment and is not retryable.
func wantFatal(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("preflight passed, want a fatal error naming %v", wants)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("preflight error %v is ErrUnavailable; a caller would retry a fatal condition forever", err)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("preflight error %q does not name %q", err, want)
		}
	}
}

// assertNoProbeResidue fails when a probe key or object is still in a bucket.
func assertNoProbeResidue(t *testing.T, f *fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	for _, bucket := range []string{configBucket, jobsBucket} {
		kv, err := f.js.KeyValue(ctx, bucket)
		if err != nil {
			t.Fatalf("open %s as admin: %v", bucket, err)
		}
		if e, err := kv.Get(ctx, "probe."+testInstanceID); err == nil {
			t.Fatalf("bucket %s still holds the probe key at revision %d", bucket, e.Revision())
		}
	}
	obs, err := f.js.ObjectStore(ctx, artifactsBucket)
	if err != nil {
		t.Fatalf("open %s as admin: %v", artifactsBucket, err)
	}
	if _, err := obs.GetInfo(ctx, "probe-"+testInstanceID); err == nil {
		t.Fatalf("bucket %s still holds the probe object", artifactsBucket)
	}
}

func TestPreflightBuckets(t *testing.T) {
	t.Run("the provisioning commands' configuration passes", func(t *testing.T) {
		f := startServerFixture(t)
		if err := f.preflight(t); err != nil {
			t.Fatalf("preflight against conforming buckets: %v", err)
		}
		assertNoProbeResidue(t, f)
	})

	t.Run("a missing bucket is named", func(t *testing.T) {
		for _, bucket := range []string{configBucket, jobsBucket, artifactsBucket} {
			t.Run(bucket, func(t *testing.T) {
				f := startServerFixture(t)
				ctx := t.Context()
				var err error
				switch bucket {
				case artifactsBucket:
					err = f.js.DeleteObjectStore(ctx, bucket)
				default:
					err = f.js.DeleteKeyValue(ctx, bucket)
				}
				if err != nil {
					t.Fatalf("delete bucket %s: %v", bucket, err)
				}
				wantFatal(t, f.preflight(t), bucket)
			})
		}
	})

	t.Run("PROFGATE_ARTIFACTS created as a KV bucket is named", func(t *testing.T) {
		f := startServerFixture(t)
		ctx := t.Context()
		if err := f.js.DeleteObjectStore(ctx, artifactsBucket); err != nil {
			t.Fatalf("delete object store: %v", err)
		}
		if _, err := f.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: artifactsBucket, History: 1}); err != nil {
			t.Fatalf("recreate as KV: %v", err)
		}
		wantFatal(t, f.preflight(t), artifactsBucket)
	})

	t.Run("an unreachable server is ErrUnavailable", func(t *testing.T) {
		f := startServerFixture(t)
		f.stopServer()
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		_, err := Preflight(ctx, Options{URL: f.url(fixtureClientUser), ConnectTimeout: time.Second},
			testInstanceID, testLogger())
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("preflight against a stopped server: got %v, want ErrUnavailable", err)
		}
	})
}

func TestPreflightContract(t *testing.T) {
	recreateKV := func(t *testing.T, f *fixture, cfg jetstream.KeyValueConfig) {
		t.Helper()
		ctx := t.Context()
		if err := f.js.DeleteKeyValue(ctx, cfg.Bucket); err != nil {
			t.Fatalf("delete %s: %v", cfg.Bucket, err)
		}
		if cfg.History == 0 {
			cfg.History = 1
		}
		if _, err := f.js.CreateKeyValue(ctx, cfg); err != nil {
			t.Fatalf("recreate %s: %v", cfg.Bucket, err)
		}
	}

	cases := []struct {
		name   string
		mutate func(t *testing.T, f *fixture)
		bucket string
		field  string
	}{
		{
			name: "a 1-minute TTL",
			mutate: func(t *testing.T, f *fixture) {
				recreateKV(t, f, jetstream.KeyValueConfig{Bucket: configBucket, TTL: time.Minute})
			},
			bucket: configBucket,
			field:  "TTL",
		},
		{
			name: "memory storage",
			mutate: func(t *testing.T, f *fixture) {
				recreateKV(t, f, jetstream.KeyValueConfig{Bucket: jobsBucket, Storage: jetstream.MemoryStorage})
			},
			bucket: jobsBucket,
			field:  "storage",
		},
		{
			name: "discard old",
			mutate: func(t *testing.T, f *fixture) {
				ctx := t.Context()
				if err := f.js.DeleteKeyValue(ctx, configBucket); err != nil {
					t.Fatalf("delete %s: %v", configBucket, err)
				}
				// CreateKeyValue always writes Discard: new, so the stream is
				// built directly, shaped as a KV stream with discard old.
				_, err := f.js.CreateStream(ctx, jetstream.StreamConfig{
					Name:              "KV_" + configBucket,
					Subjects:          []string{"$KV." + configBucket + ".>"},
					MaxMsgsPerSubject: 1,
					Discard:           jetstream.DiscardOld,
					Storage:           jetstream.FileStorage,
					AllowRollup:       true,
					DenyDelete:        true,
					AllowDirect:       true,
				})
				if err != nil {
					t.Fatalf("create discard-old stream: %v", err)
				}
			},
			bucket: configBucket,
			field:  "discard",
		},
		{
			name: "a 1 MiB MaxBytes on a KV bucket",
			mutate: func(t *testing.T, f *fixture) {
				recreateKV(t, f, jetstream.KeyValueConfig{Bucket: jobsBucket, MaxBytes: 1 << 20})
			},
			bucket: jobsBucket,
			field:  "maxBytes",
		},
		{
			name: "a 1 MiB MaxBytes on the Object Store",
			mutate: func(t *testing.T, f *fixture) {
				ctx := t.Context()
				if err := f.js.DeleteObjectStore(ctx, artifactsBucket); err != nil {
					t.Fatalf("delete %s: %v", artifactsBucket, err)
				}
				_, err := f.js.CreateObjectStore(ctx, jetstream.ObjectStoreConfig{
					Bucket:   artifactsBucket,
					Storage:  jetstream.FileStorage,
					MaxBytes: 1 << 20,
				})
				if err != nil {
					t.Fatalf("recreate %s: %v", artifactsBucket, err)
				}
			},
			bucket: artifactsBucket,
			field:  "maxBytes",
		},
		{
			name: "a MaxValueSize below 512 KiB",
			mutate: func(t *testing.T, f *fixture) {
				recreateKV(t, f, jetstream.KeyValueConfig{Bucket: jobsBucket, MaxValueSize: 1024})
			},
			bucket: jobsBucket,
			field:  "maxValueSize",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := startServerFixture(t)
			tc.mutate(t, f)
			wantFatal(t, f.preflight(t), tc.bucket, tc.field)
		})
	}
}

func TestPreflightPermissions(t *testing.T) {
	t.Run("the account fragment's permissions suffice", func(t *testing.T) {
		f := startServerFixture(t, withUsers(fragmentPermissions()))
		if err := f.preflight(t); err != nil {
			t.Fatalf("preflight under the fragment's permissions: %v", err)
		}
		assertNoProbeResidue(t, f)
	})

	t.Run("publish denied on the jobs bucket", func(t *testing.T) {
		f := startServerFixture(t, withUsers(fragmentWithout(t, "publish", "$KV.PROFGATE_JOBS.>")))
		err := f.preflight(t)
		wantFatal(t, err, jobsBucket, "create")
		if !errors.Is(err, nats.ErrPermissionViolation) {
			t.Fatalf("preflight error %v does not carry the permission violation", err)
		}
		assertNoProbeResidue(t, f)
	})

	t.Run("publish denied on the artifact bucket", func(t *testing.T) {
		f := startServerFixture(t, withUsers(fragmentWithout(t, "publish", "$O.PROFGATE_ARTIFACTS.>")))
		err := f.preflight(t)
		wantFatal(t, err, artifactsBucket, "put")
		if !errors.Is(err, nats.ErrPermissionViolation) {
			t.Fatalf("preflight error %v does not carry the permission violation", err)
		}
		assertNoProbeResidue(t, f)
	})

	t.Run("consumer create denied on the artifact stream", func(t *testing.T) {
		f := startServerFixture(t, withUsers(fragmentWithout(t, "publish", "$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>")))
		err := f.preflight(t)
		wantFatal(t, err, artifactsBucket)
		if !errors.Is(err, nats.ErrPermissionViolation) {
			t.Fatalf("preflight error %v does not carry the permission violation", err)
		}
		assertNoProbeResidue(t, f)
	})

	// The spec's account fragment grants subscribe on $KV.PROFGATE_JOBS.>
	// and $O.PROFGATE_ARTIFACTS.>, and the spec expects preflight to fail
	// without them.
	// In fact every watch is delivered through an ordered consumer whose deliver subject is an inbox,
	// and every object read is fetched from the seam's own pull consumer into an inbox,
	// so only the granted _INBOX.> subscription carries data and preflight passes;
	// the grants are unexercised by this client version.
	// These subtests pin that fact so a client or server change that starts
	// exercising them is caught here.
	t.Run("subscribe denied on the jobs bucket still passes", func(t *testing.T) {
		f := startServerFixture(t, withUsers(fragmentWithout(t, "subscribe", "$KV.PROFGATE_JOBS.>")))
		if err := f.preflight(t); err != nil {
			t.Fatalf("preflight: %v — inbox-delivered watches should not need the $KV subscription", err)
		}
		assertNoProbeResidue(t, f)
	})

	t.Run("subscribe denied on the artifact bucket still passes", func(t *testing.T) {
		f := startServerFixture(t, withUsers(fragmentWithout(t, "subscribe", "$O.PROFGATE_ARTIFACTS.>")))
		if err := f.preflight(t); err != nil {
			t.Fatalf("preflight: %v — inbox-delivered object reads should not need the $O subscription", err)
		}
		assertNoProbeResidue(t, f)
	})
}

func TestPreflightWatchDelivery(t *testing.T) {
	t.Run("withheld watch deliveries fail preflight at its deadline", func(t *testing.T) {
		f := startServerFixture(t)
		c := f.connectClient()
		c.probeDeadline = 500 * time.Millisecond
		ctx := t.Context()

		// testInterceptProbeWatch swallows every delivery on the jobs
		// bucket's probe watch while the writes themselves succeed.
		c.testInterceptProbeWatch = func(bucket string, _ Entry) bool {
			return bucket == jobsBucket
		}
		start := time.Now()
		err := c.runPreflight(ctx, testInstanceID)
		elapsed := time.Since(start)
		wantFatal(t, err, jobsBucket, "watch", "did not deliver")
		if elapsed < c.probeDeadline {
			t.Fatalf("preflight failed after %s without blocking until the %s deadline", elapsed, c.probeDeadline)
		}
		assertNoProbeResidue(t, f)

		// The same run with the interceptor released succeeds.
		c.testInterceptProbeWatch = nil
		if err := c.runPreflight(ctx, testInstanceID); err != nil {
			t.Fatalf("preflight with the interceptor released: %v", err)
		}
		assertNoProbeResidue(t, f)
	})
}

func TestPreflightConnectionCallback(t *testing.T) {
	t.Run("OnConnectionChange sees connect, outage, reconnect", func(t *testing.T) {
		f := startServerFixture(t)
		var mu sync.Mutex
		var seq []bool
		record := func(up bool) {
			mu.Lock()
			defer mu.Unlock()
			seq = append(seq, up)
		}
		snapshot := func() []bool {
			mu.Lock()
			defer mu.Unlock()
			return append([]bool(nil), seq...)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*fixtureTimeout)
		defer cancel()
		cl, err := Preflight(ctx, Options{
			URL:                f.url(fixtureClientUser),
			ConnectTimeout:     callTimeout,
			OnConnectionChange: record,
		}, testInstanceID, testLogger())
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		t.Cleanup(cl.(*client).close)

		if got := snapshot(); len(got) != 1 || !got[0] {
			t.Fatalf("after preflight: callback saw %v, want [true]", got)
		}
		f.stopServer()
		waitFor(t, "the disconnect callback", func() bool {
			got := snapshot()
			return len(got) == 2 && !got[1]
		})
		f.restartServer()
		waitFor(t, "the reconnect callback", func() bool {
			got := snapshot()
			return len(got) == 3 && got[2]
		})
	})

	t.Run("OnGenerationMove runs for a watch cut and for a disconnect", func(t *testing.T) {
		f := startServerFixture(t)
		var mu sync.Mutex
		var conns []bool
		moves := 0
		recordConn := func(up bool) {
			mu.Lock()
			defer mu.Unlock()
			conns = append(conns, up)
		}
		recordMove := func() {
			mu.Lock()
			defer mu.Unlock()
			moves++
		}
		connSnapshot := func() []bool {
			mu.Lock()
			defer mu.Unlock()
			return append([]bool(nil), conns...)
		}
		moveCount := func() int {
			mu.Lock()
			defer mu.Unlock()
			return moves
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*fixtureTimeout)
		defer cancel()
		cl, err := Preflight(ctx, Options{
			URL:                f.url(fixtureClientUser),
			ConnectTimeout:     callTimeout,
			OnConnectionChange: recordConn,
			OnGenerationMove:   recordMove,
		}, testInstanceID, testLogger())
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		c, ok := cl.(*client)
		if !ok {
			t.Fatalf("Preflight returned %T, not *client", cl)
		}
		t.Cleanup(c.close)

		tap := newWatcherTap()
		c.testWatchOpened = tap.record
		stores, err := cl.View(cl.Generation())
		if err != nil {
			t.Fatalf("view: %v", err)
		}
		ch, err := stores.Jobs.Watch(ctx, "g.")
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		drainToMarker(t, ch)

		// Preflight's own probe watches end with the context they were opened under,
		// which is this process closing a watch rather than a cut, so nothing has moved yet.
		if n := moveCount(); n != 0 {
			t.Fatalf("move callbacks before anything moved the generation: got %d, want 0", n)
		}
		gen := cl.Generation()

		tap.cut(t, "g.")
		waitFor(t, "the move callback for the watch cut", func() bool { return moveCount() == 1 })
		if got := cl.Generation(); got != gen+1 {
			t.Fatalf("generation after the cut: got %d, want %d", got, gen+1)
		}
		if got := connSnapshot(); len(got) != 1 || !got[0] {
			t.Fatalf("the connection callback saw %v across a watch cut, want [true]", got)
		}

		f.stopServer()
		waitFor(t, "the move callback for the disconnect", func() bool { return moveCount() == 2 })
		waitFor(t, "the disconnect callback", func() bool {
			got := connSnapshot()
			return len(got) == 2 && !got[1]
		})
	})
}

// subjectTap is a nats.CustomDialer that records every subject the
// connection publishes to by reading PUB and HPUB frames off the wire —
// the NATS analogue of the gateway's recording transport.
// On demand it also withholds every frame the server sends to an inbox,
// which is how a store that stops acknowledging looks from the client while the connection stays up.
type subjectTap struct {
	mu        sync.Mutex
	subjects  map[string]struct{}
	dropInbox bool
}

// setDropInbox turns the withholding of inbox frames on or off.
// While it is on, a MSG or HMSG frame whose subject begins with _INBOX. is dropped whole, payload included,
// and every other frame passes; a JetStream acknowledgement is such a frame, addressed to the publisher's inbox.
func (s *subjectTap) setDropInbox(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropInbox = on
}

func (s *subjectTap) droppingInbox() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropInbox
}

// connectTapped connects the fixture's seam client through a subjectTap and returns the tap.
func (f *fixture) connectTapped() *subjectTap {
	f.t.Helper()
	tap := &subjectTap{}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	c, err := connect(ctx, connectConfig{
		url:            f.url(fixtureClientUser),
		connectTimeout: callTimeout,
		reconnectWait:  fixtureReconnectWait,
		name:           "natskv-tap",
		dialer:         tap,
	}, testLogger())
	if err != nil {
		f.t.Fatalf("seam connect through the tap: %v", err)
	}
	f.t.Cleanup(c.close)
	f.c = c
	return tap
}

func (s *subjectTap) Dial(network, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: fixtureTimeout}
	conn, err := d.DialContext(context.Background(), network, address)
	if err != nil {
		return nil, err
	}
	return &tapConn{Conn: conn, tap: s}, nil
}

func (s *subjectTap) record(subject string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subjects == nil {
		s.subjects = map[string]struct{}{}
	}
	s.subjects[subject] = struct{}{}
}

// anyWithPrefix reports whether a subject recorded so far begins with prefix.
func (s *subjectTap) anyWithPrefix(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for subj := range s.subjects {
		if strings.HasPrefix(subj, prefix) {
			return true
		}
	}
	return false
}

func (s *subjectTap) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.subjects))
	for subj := range s.subjects {
		out = append(out, subj)
	}
	return out
}

// tapConn parses the client-to-server protocol stream: control lines are
// scanned for PUB and HPUB subjects, payload bytes are skipped by the length
// each header announces, so payload content is never mistaken for a frame.
// The server-to-client stream is parsed the same way,
// so a MSG or HMSG frame can be dropped whole while the tap withholds inbox frames.
// nats.go reads the connection from one goroutine, so the read side needs no lock.
type tapConn struct {
	net.Conn
	tap *subjectTap

	mu      sync.Mutex
	pending []byte
	skip    int

	in      []byte // the server's bytes not yet parsed
	inSkip  int    // payload bytes of the current frame still to pass or drop
	inDrop  bool   // whether the current frame is being dropped
	out     []byte // parsed bytes not yet handed to the client
	readBuf [64 << 10]byte
}

func (c *tapConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.parse(b)
	c.mu.Unlock()
	return c.Conn.Write(b)
}

// Read hands the client the server's frames minus the ones the tap withholds.
func (c *tapConn) Read(b []byte) (int, error) {
	for len(c.out) == 0 {
		n, err := c.Conn.Read(c.readBuf[:])
		if n > 0 {
			c.parseIn(c.readBuf[:n])
		}
		if err != nil && len(c.out) == 0 {
			return 0, err
		}
	}
	n := copy(b, c.out)
	c.out = c.out[n:]
	if len(c.out) == 0 {
		c.out = nil
	}
	return n, nil
}

// parseIn is parse for the server's stream: MSG and HMSG frames are passed or dropped whole,
// every other control line passes.
func (c *tapConn) parseIn(b []byte) {
	data := append(c.in, b...)
	i := 0
	for i < len(data) {
		if c.inSkip > 0 {
			n := min(c.inSkip, len(data)-i)
			if !c.inDrop {
				c.out = append(c.out, data[i:i+n]...)
			}
			i += n
			c.inSkip -= n
			continue
		}
		j := bytes.Index(data[i:], []byte("\r\n"))
		if j < 0 {
			break
		}
		line := data[i : i+j+2]
		fields := strings.Fields(string(data[i : i+j]))
		i += j + 2
		c.inDrop = false
		if len(fields) >= 4 {
			switch strings.ToUpper(fields[0]) {
			case "MSG", "HMSG": // MSG <subject> <sid> [reply] <bytes> / HMSG <subject> <sid> [reply] <hdr> <total>
				if size, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
					c.inSkip = size + 2 // payload and its trailing CRLF
					c.inDrop = c.tap.droppingInbox() && strings.HasPrefix(fields[1], "_INBOX.")
				}
			}
		}
		if !c.inDrop {
			c.out = append(c.out, line...)
		}
	}
	c.in = append(c.in[:0], data[i:]...)
}

func (c *tapConn) parse(b []byte) {
	data := append(c.pending, b...)
	i := 0
	for i < len(data) {
		if c.skip > 0 {
			n := min(c.skip, len(data)-i)
			i += n
			c.skip -= n
			continue
		}
		j := bytes.Index(data[i:], []byte("\r\n"))
		if j < 0 {
			break
		}
		fields := strings.Fields(string(data[i : i+j]))
		i += j + 2
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "PUB", "HPUB": // PUB <subject> [reply] <bytes> / HPUB <subject> [reply] <hdr> <total>
			if len(fields) < 3 {
				continue
			}
			c.tap.record(fields[1])
			if size, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
				c.skip = size + 2 // payload and its trailing CRLF
			}
		}
	}
	c.pending = append(c.pending[:0], data[i:]...)
}

// subjectMatches implements NATS subject matching: * is one token, a
// trailing > is one or more.
func subjectMatches(pattern, subject string) bool {
	pt := strings.Split(pattern, ".")
	st := strings.Split(subject, ".")
	for i, p := range pt {
		if p == ">" {
			return i < len(st)
		}
		if i >= len(st) || (p != "*" && p != st[i]) {
			return false
		}
	}
	return len(pt) == len(st)
}

func TestPublishedSubjects(t *testing.T) {
	t.Run("every published subject is inside the account fragment", func(t *testing.T) {
		f := startServerFixture(t)
		tap := &subjectTap{}
		ctx, cancel := context.WithTimeout(context.Background(), 2*fixtureTimeout)
		defer cancel()

		c, err := connect(ctx, connectConfig{
			url:            f.url(fixtureClientUser),
			connectTimeout: callTimeout,
			reconnectWait:  fixtureReconnectWait,
			name:           "natskv-tap",
			dialer:         tap,
		}, testLogger())
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(c.close)

		// Preflight exercises watch, create, update, get, delete, put,
		// object get, list, object delete, and status on every bucket.
		if err := c.runPreflight(ctx, testInstanceID); err != nil {
			t.Fatalf("preflight: %v", err)
		}
		// Keys is the one seam operation preflight does not run.
		stores, err := c.View(c.Generation())
		if err != nil {
			t.Fatalf("view: %v", err)
		}
		if _, err := stores.Config.Keys(ctx, ""); err != nil {
			t.Fatalf("keys: %v", err)
		}
		if _, err := stores.Jobs.Keys(ctx, ""); err != nil {
			t.Fatalf("keys: %v", err)
		}
		// The probe's object is one chunk; three chunks put the filtered create,
		// one fetch per chunk, and the delete of the seam's consumer in the tap.
		data := make([]byte, 300<<10)
		if err := stores.Artifacts.Put(ctx, "three.pprof", bytes.NewReader(data)); err != nil {
			t.Fatalf("put: %v", err)
		}
		r, err := stores.Artifacts.Get(ctx, "three.pprof")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got, err := io.ReadAll(r); err != nil || len(got) != len(data) {
			t.Fatalf("read: %d bytes, %v", len(got), err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		// The consumer's delete is sent after Close has returned,
		// so the snapshot waits for it to reach the wire and holds every step of the chunk read.
		consumerAPI := "$JS.API.CONSUMER."
		consumerSuffix := ".OBJ_PROFGATE_ARTIFACTS." + chunkConsumerName
		waitFor(t, "the consumer's delete", func() bool {
			return tap.anyWithPrefix(consumerAPI + "DELETE" + consumerSuffix)
		})
		for _, op := range []string{"CREATE", "MSG.NEXT", "DELETE"} {
			if !tap.anyWithPrefix(consumerAPI + op + consumerSuffix) {
				t.Errorf("no published subject under %q: the chunk read's %s is not in the snapshot", consumerAPI+op+consumerSuffix, op)
			}
		}

		allowed := fragmentPermissions().Publish.Allow
		for _, subject := range tap.all() {
			ok := false
			for _, pattern := range allowed {
				if subjectMatches(pattern, subject) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("published subject %q is outside the account fragment", subject)
			}
		}
	})
}

func TestPreflightProbeOrder(t *testing.T) {
	t.Run("a watch that lags the writes on a history-1 bucket still passes", func(t *testing.T) {
		f := startServerFixture(t)
		c := f.connectClient()
		c.probeDeadline = 2 * time.Second
		ctx := t.Context()

		// The fixture's buckets keep one revision per key, so the server
		// drops a revision the moment the next write lands.
		// Whether the watch's consumer loaded it first is a timing question
		// that cannot be forced on the server, so the interceptor decides
		// it the slow way at the point the probe reads: an entry the bucket
		// no longer holds when the probe reads it is swallowed, the way a
		// consumer that loaded after the next write never sees it.
		// A probe that writes back-to-back and reads afterwards then sees
		// only the tombstone; a probe that awaits each revision before the
		// next write sees all three.
		c.testInterceptProbeWatch = func(bucket string, e Entry) bool {
			if e.Synced || e.Value == nil {
				return false
			}
			gctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
			defer cancel()
			kv, err := f.js.KeyValue(gctx, bucket)
			if err != nil {
				t.Errorf("open %s as admin: %v", bucket, err)
				return false
			}
			head, err := kv.Get(gctx, e.Key)
			return err != nil || head.Revision() != e.Revision
		}
		if err := c.runPreflight(ctx, testInstanceID); err != nil {
			t.Fatalf("preflight with a watch that lags the writes: %v", err)
		}
		assertNoProbeResidue(t, f)
	})
}
