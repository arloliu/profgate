package k8s

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
)

const (
	// resyncPeriod is how often the shared informers replay their caches to their handlers.
	resyncPeriod = 10 * time.Minute
	// informerCount is the number of informers New registers: Services, Pods, EndpointSlices.
	informerCount = 3
)

// Cluster resolves Services to their backend Pods from cluster-wide shared informer caches,
// and confirms one of them against the API server before it is dialed.
type Cluster struct {
	cs             kubernetes.Interface
	factory        informers.SharedInformerFactory
	services       corelisters.ServiceLister
	pods           corelisters.PodLister
	endpointSlices discoverylisters.EndpointSliceLister
	opts           Options
	log            *slog.Logger
	synced         atomic.Bool
}

// New constructs a Cluster over cs.
// Asking the factory for each lister is what registers that informer, so the three
// informers exist before Run starts them; the caches stay empty until then.
// The confirmation read shares this clientset with the informers, so it shares
// their client-side rate limit as well.
func New(cs kubernetes.Interface, opts Options) *Cluster {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	factory := informers.NewSharedInformerFactory(cs, resyncPeriod)

	return &Cluster{
		cs:             cs,
		factory:        factory,
		services:       factory.Core().V1().Services().Lister(),
		pods:           factory.Core().V1().Pods().Lister(),
		endpointSlices: factory.Discovery().V1().EndpointSlices().Lister(),
		opts:           opts,
		log:            log,
	}
}

// Run starts the informers and blocks until ctx is done.
// It returns once every informer goroutine has stopped.
func (c *Cluster) Run(ctx context.Context) {
	c.factory.Start(ctx.Done())

	// A canceled context ends the wait with unsynced informers, and a factory
	// nobody registered an informer with reports an empty result: both leave
	// HasSynced false rather than claiming an empty cache is a filled one.
	result := c.factory.WaitForCacheSync(ctx.Done())
	synced := len(result) == informerCount
	for _, ok := range result {
		synced = synced && ok
	}
	c.synced.Store(synced)

	<-ctx.Done()
	c.factory.Shutdown()
}

// HasSynced reports whether every informer has completed its initial list.
func (c *Cluster) HasSynced() bool { return c.synced.Load() }
