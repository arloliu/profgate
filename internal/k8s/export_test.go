package k8s

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// cachePollInterval is how often waitCache re-evaluates its predicate.
const cachePollInterval = 5 * time.Millisecond

// cacheWaitTimeout bounds how long a fixture waits for the informers to reach a state.
const cacheWaitTimeout = 5 * time.Second

// startFixture creates a fake clientset pre-loaded with objs, constructs a Cluster, runs it,
// and waits for HasSynced. cancel stops informer delivery; the clientset stays usable for live calls.
//
// Every object is loaded before the informers start, so a synced cache holds all of them:
// a test that mutates objects does so on the literals it passes here, never on a running fixture.
func startFixture(t *testing.T, opts Options, objs ...runtime.Object) (cs *fake.Clientset, c *Cluster, cancel context.CancelFunc) {
	t.Helper()

	cs = fake.NewClientset(objs...)
	c = New(cs, opts)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	waitCache(t, c.HasSynced)

	return cs, c, cancel
}

// waitCache polls until pred is true or 5s pass, then fails the test.
func waitCache(t *testing.T, pred func() bool) {
	t.Helper()

	deadline := time.Now().Add(cacheWaitTimeout)
	for {
		if pred() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the informer cache did not reach the expected state within %s", cacheWaitTimeout)
		}
		time.Sleep(cachePollInterval)
	}
}
