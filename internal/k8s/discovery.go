// Package k8s is the gateway's only seam to the Kubernetes API.
// The methods it exposes are the set of things Profgate can do to the cluster:
// observe Services, Pods, and EndpointSlices, and read one named Pod.
package k8s

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Target is one backend Pod of a Service, resolved from the informer caches.
type Target struct {
	Namespace, Service, Pod, Node, PodIP string
	Port                                 int32
	Version                              string
	UID                                  string
}

var (
	// ErrServiceNotFound reports that the named Service is absent from the cache.
	ErrServiceNotFound = errors.New("service not found")
	// ErrServiceSelectorless reports a Service without a selector, which has no Pods to profile.
	ErrServiceSelectorless = errors.New("service has no selector")
	// ErrTargetChanged reports that the confirmation read no longer matches the selected Target.
	ErrTargetChanged = errors.New("target changed")
	// ErrDiscoveryUnavailable reports that discovery cannot answer: the informer caches
	// have not synced, or the confirmation read failed against the API server.
	ErrDiscoveryUnavailable = errors.New("discovery unavailable")
)

// Discovery resolves a Service to its backend Pods and confirms one before proxying.
type Discovery interface {
	Targets(ctx context.Context, namespace, service string) ([]Target, error)
	HasSynced() bool
	Confirm(ctx context.Context, t Target) error
}

// Options configures the package's constructors and Preflight.
type Options struct {
	VersionLabel         string
	Port                 int32
	PortName             string
	NamespaceFile        string        // default "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	PreflightCallTimeout time.Duration // default 10s when zero
	Logger               *slog.Logger
}
