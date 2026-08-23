package k8s

import (
	"context"

	"k8s.io/client-go/kubernetes"
)

// Runtime owns the one Kubernetes client the process shares between its startup preflight
// and its discovery caches, so the packages that use it never name a client-go type.
type Runtime interface {
	// OwnNamespace is the namespace the gateway itself runs in.
	OwnNamespace() (string, error)
	// Preflight exercises the granted tuples, over the runtime's own namespace.
	Preflight(ctx context.Context) error
	// Cluster is the discovery, constructed and not yet running.
	// Every call returns the same one:
	// the caller that runs the informers and the caller that reads them share a single set of caches.
	Cluster() *Cluster
}

// clusterRuntime is a Runtime over one clientset.
type clusterRuntime struct {
	cs      kubernetes.Interface
	opts    Options
	cluster *Cluster
}

// NewRuntime builds the in-cluster client and the discovery over it.
func NewRuntime(opts Options) (Runtime, error) {
	cs, err := NewClientset()
	if err != nil {
		return nil, err
	}

	return NewRuntimeWithClientset(cs, opts), nil
}

// NewRuntimeWithClientset builds a Runtime over a client the caller supplies.
// It is exported for tests in other packages, which may name kubernetes.Interface from their _test.go files.
func NewRuntimeWithClientset(cs kubernetes.Interface, opts Options) Runtime {
	return &clusterRuntime{cs: cs, opts: opts, cluster: New(cs, opts)}
}

func (r *clusterRuntime) OwnNamespace() (string, error) { return OwnNamespace(r.opts) }

func (r *clusterRuntime) Preflight(ctx context.Context) error {
	ownNamespace, err := r.OwnNamespace()
	if err != nil {
		return err
	}

	return Preflight(ctx, r.cs, r.opts, ownNamespace)
}

func (r *clusterRuntime) Cluster() *Cluster { return r.cluster }
