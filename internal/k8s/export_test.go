package k8s

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
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

// countingPods is a PodLister over the informer's that counts namespace-wide List calls
// and per-name Get calls, so a test can assert which method paid for which read.
// listErr, when set, fails every namespace-wide List the way a broken cache would.
type countingPods struct {
	corelisters.PodLister
	lists   atomic.Int32
	gets    atomic.Int32
	listErr error
}

func (p *countingPods) Pods(namespace string) corelisters.PodNamespaceLister {
	return countingNamespacePods{p: p, PodNamespaceLister: p.PodLister.Pods(namespace)}
}

type countingNamespacePods struct {
	corelisters.PodNamespaceLister
	p *countingPods
}

func (n countingNamespacePods) List(selector labels.Selector) ([]*corev1.Pod, error) {
	n.p.lists.Add(1)
	if n.p.listErr != nil {
		return nil, n.p.listErr
	}

	return n.PodNamespaceLister.List(selector)
}

func (n countingNamespacePods) Get(name string) (*corev1.Pod, error) {
	n.p.gets.Add(1)

	return n.PodNamespaceLister.Get(name)
}

// countPodReads swaps a counting lister onto a started Cluster's Pod lister and returns it.
func countPodReads(c *Cluster) *countingPods {
	p := &countingPods{PodLister: c.pods}
	c.pods = p

	return p
}
