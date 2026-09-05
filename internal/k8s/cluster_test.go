package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// klogRecordTimeout bounds how long a subtest waits for client-go to write the record it expects.
const klogRecordTimeout = 5 * time.Second

// jsonLog is a JSON slog handler over a buffer that the reflector goroutine and the test both touch.
type jsonLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newJSONLog() (*jsonLog, *slog.Logger) {
	l := &jsonLog{}

	return l, slog.New(slog.NewJSONHandler(&lockedWriter{l: l}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lockedWriter serializes the handler's writes with the test's reads.
type lockedWriter struct{ l *jsonLog }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.l.mu.Lock()
	defer w.l.mu.Unlock()

	return w.l.buf.Write(p)
}

// records decodes every complete line written so far.
func (l *jsonLog) records(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}

	return out
}

// waitRecord polls until a record with level and msg is present, and returns it.
func (l *jsonLog) waitRecord(t *testing.T, level, msg string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(klogRecordTimeout)
	for {
		for _, rec := range l.records(t) {
			if rec["level"] == level && rec["msg"] == msg {
				return rec
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no record with level %q and msg %q within %s; records: %v", level, msg, klogRecordTimeout, l.records(t))
		}
		time.Sleep(cachePollInterval)
	}
}

// runCluster builds the runtime over cs, which installs the logger as klog's sink, and runs its cluster.
// The returned stop cancels the informers and waits for Run to return,
// so the next runtime is built while no informer goroutine logs.
func runCluster(t *testing.T, cs *fake.Clientset, opts Options) (c *Cluster, stop func()) {
	t.Helper()

	c = NewRuntimeWithClientset(cs, opts).Cluster()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		c.Run(ctx)
	}()

	return c, func() {
		cancel()
		<-stopped
	}
}

// TestClusterLogsThroughSlog proves client-go's reflector lines reach the gateway's logger as JSON,
// at the level client-go emits, rather than stderr as text.
// klog's sink is package state that the runtime constructor installs,
// so this package's tests never run in parallel,
// and each subtest stops its informers before the next one installs a sink of its own.
func TestClusterLogsThroughSlog(t *testing.T) {
	t.Run("a refused list is an error record", func(t *testing.T) {
		cs := fake.NewClientset(baseline().objects()...)
		refused := errors.New("pods are refused by the test")
		cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, refused
		})
		log, logger := newJSONLog()
		opts := baseOptions()
		opts.Logger = logger

		c, stop := runCluster(t, cs, opts)
		defer stop()

		rec := log.waitRecord(t, "ERROR", "Failed to watch")
		if got, _ := rec["err"].(string); !strings.Contains(got, refused.Error()) {
			t.Fatalf("err = %q, want it to contain %q", got, refused.Error())
		}
		if c.HasSynced() {
			t.Fatal("HasSynced() = true with every Pod list refused, want false")
		}
	})

	t.Run("a watch that ends is an info record", func(t *testing.T) {
		cs := fake.NewClientset(baseline().objects()...)
		cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			w := watch.NewFake()
			w.Stop()

			return true, w, nil
		})
		log, logger := newJSONLog()
		opts := baseOptions()
		opts.Logger = logger

		c, stop := runCluster(t, cs, opts)
		defer stop()

		waitCache(t, c.HasSynced)
		log.waitRecord(t, "INFO", "Warning: watch ended with error")
	})
}
