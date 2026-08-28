// Package k8s is the gateway's only seam to the Kubernetes API.
// The methods it exposes are the set of things Profgate can do to the cluster:
// observe Services, Pods, and EndpointSlices, read one named Pod, and list the Services it holds.
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
	// ErrDiscoveryUnavailable reports that discovery cannot answer:
	// the informer caches have not synced, or the confirmation read failed against the API server.
	ErrDiscoveryUnavailable = errors.New("discovery unavailable")
)

// PortSelection is the client's port choice for one request (spec: Port resolution);
// the zero value means the configured default.
// Port and PortName are never both set: the HTTP layer refuses that before discovery.
type PortSelection struct {
	Port     int32
	PortName string
}

// Exclusion counts the Pods one reason kept out of a Service's target list.
type Exclusion struct {
	Reason string // one of ExclusionReasons
	Count  int
}

// Explanation is what Explain reports about one Service.
type Explanation struct {
	Targets         []Target    // what Targets returns for the same arguments
	SelectorMatched int         // Pods in the namespace whose labels match spec.selector
	Excluded        []Exclusion // the reasons with a non-zero count, in vocabulary order
}

// The exclusion vocabulary (spec: Eligibility), in the order a report carries it.
const (
	ReasonPodTerminating          = "pod_terminating"
	ReasonPodNotRunning           = "pod_not_running"
	ReasonPodNotReady             = "pod_not_ready"
	ReasonEndpointMissing         = "endpoint_missing"
	ReasonEndpointNotReady        = "endpoint_not_ready"
	ReasonEndpointAddressMismatch = "endpoint_address_mismatch"
	ReasonEndpointAddressConflict = "endpoint_address_conflict"
	ReasonPortNameNotDeclared     = "port_name_not_declared"
	ReasonVersionMismatch         = "version_mismatch"
	ReasonPodNameMismatch         = "pod_name_mismatch"
)

// exclusionReasons is the vocabulary in report order; ExclusionReasons hands out copies of it.
var exclusionReasons = [...]string{
	ReasonPodTerminating,
	ReasonPodNotRunning,
	ReasonPodNotReady,
	ReasonEndpointMissing,
	ReasonEndpointNotReady,
	ReasonEndpointAddressMismatch,
	ReasonEndpointAddressConflict,
	ReasonPortNameNotDeclared,
	ReasonVersionMismatch,
	ReasonPodNameMismatch,
}

// ExclusionReasons returns the exclusion vocabulary in the order a report carries it.
// The first eight are decided here, from the caches;
// the last two, version_mismatch and pod_name_mismatch, are decided by the request's filters
// and are never produced by Explain.
func ExclusionReasons() []string {
	return append([]string(nil), exclusionReasons[:]...)
}

// Discovery resolves a Service to its backend Pods and confirms one before proxying.
type Discovery interface {
	// Targets returns the currently eligible backends of a Service
	// whose pprof port resolves under port.
	// Order is unspecified.
	Targets(ctx context.Context, namespace, service string, port PortSelection) ([]Target, error)
	HasSynced() bool
	Confirm(ctx context.Context, t Target) error
	// Catalog lists the Services with a non-empty selector from the cache,
	// sorted by namespace then name.
	// An empty namespace means every namespace; a namespace the cache lacks is an empty list, not an error.
	// It issues no request; an error means the lister could not be read.
	Catalog(ctx context.Context, namespace string) ([]ServiceRef, error)
	// Explain returns the targets of a Service beside the reasons its other selected Pods were dropped,
	// from one captured list of the namespace's selected Pods and the EndpointSlice pass Targets makes.
	// It counts Pods and names none.
	Explain(ctx context.Context, namespace, service string, port PortSelection) (Explanation, error)
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
