package k8s

import (
	"context"
	"log/slog"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
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
	installKlog(opts.Logger)
	cs, err := NewClientset()
	if err != nil {
		return nil, err
	}

	return NewRuntimeWithClientset(cs, opts), nil
}

// NewRuntimeWithClientset builds a Runtime over a client the caller supplies.
// It is exported for tests in other packages, which may name kubernetes.Interface from their _test.go files.
func NewRuntimeWithClientset(cs kubernetes.Interface, opts Options) Runtime {
	installKlog(opts.Logger)

	return &clusterRuntime{cs: cs, opts: opts, cluster: New(cs, opts)}
}

// installKlog routes client-go's klog output through log,
// so reflector and watch failures reach stdout as JSON at the level client-go emits.
// klog's setter mutates unsynchronised package state and must run while no informer logs,
// so it runs where the runtime is built, before any client or informer exists,
// and writes nothing when log's handler is already the one klog holds.
func installKlog(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if logr.ToSlogHandler(klog.Background()) == log.Handler() {
		return
	}
	klog.SetSlogLogger(log)
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
