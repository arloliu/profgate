package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
)

// podKind is the targetRef kind Profgate follows; the target model stops at the Pod.
const podKind = "Pod"

// Targets returns the currently eligible backends of a Service whose pprof port resolves under port.
// Order is unspecified.
// It reads only the informer caches and never calls the API server, which is why it ignores the context:
// its answer is as current as the caches are,
// and it cannot block.
// It reaches each endpoint's Pod by name and makes no namespace-wide read of the Pod cache;
// that read is Explain's alone.
func (c *Cluster) Targets(_ context.Context, namespace, service string, port PortSelection) ([]Target, error) {
	svc, selector, epSlices, err := c.serviceSlices(namespace, service)
	if err != nil {
		return nil, err
	}
	lookup := func(name string) (*corev1.Pod, bool) {
		pod, err := c.pods.Pods(namespace).Get(name)

		return pod, err == nil
	}
	targets, _ := c.resolve(svc, selector, epSlices, port, lookup)

	return targets, nil
}

// Explain returns the targets of a Service beside the reasons its other selected Pods were dropped,
// from one captured list of the namespace's selected Pods and the EndpointSlice pass Targets makes.
// It counts Pods and names none.
// The captured list is the population it counts and the map it resolves endpoints against,
// so both come from one snapshot of the Pod cache read once.
func (c *Cluster) Explain(_ context.Context, namespace, service string, port PortSelection) (Explanation, error) {
	svc, selector, epSlices, err := c.serviceSlices(namespace, service)
	if err != nil {
		return Explanation{}, err
	}
	pods, err := c.pods.Pods(namespace).List(selector)
	if err != nil {
		return Explanation{}, fmt.Errorf("read pods of %s/%s from cache: %w", namespace, service, err)
	}
	byName := make(map[string]*corev1.Pod, len(pods))
	for _, pod := range pods {
		byName[pod.Name] = pod
	}
	lookup := func(name string) (*corev1.Pod, bool) {
		pod, ok := byName[name]

		return pod, ok
	}
	targets, seen := c.resolve(svc, selector, epSlices, port, lookup)

	counts := make(map[string]int, len(exclusionReasons))
	for _, pod := range pods {
		facts := seen[string(pod.UID)]
		if facts != nil && facts.isTarget() {
			continue
		}
		counts[attribute(pod, facts)]++
	}
	var excluded []Exclusion
	for _, reason := range exclusionReasons {
		if n := counts[reason]; n > 0 {
			excluded = append(excluded, Exclusion{Reason: reason, Count: n})
		}
	}

	return Explanation{Targets: targets, SelectorMatched: len(pods), Excluded: excluded}, nil
}

// serviceSlices reads a Service and its EndpointSlices from the caches,
// answering the two sentinel errors a caller branches on.
func (c *Cluster) serviceSlices(namespace, service string) (*corev1.Service, labels.Selector, []*discoveryv1.EndpointSlice, error) {
	svc, err := c.services.Services(namespace).Get(service)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil, fmt.Errorf("service %s/%s: %w", namespace, service, ErrServiceNotFound)
		}

		return nil, nil, nil, fmt.Errorf("read service %s/%s from cache: %w", namespace, service, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return nil, nil, nil, fmt.Errorf("service %s/%s: %w", namespace, service, ErrServiceSelectorless)
	}
	selector := labels.SelectorFromSet(svc.Spec.Selector)

	owned := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: service})
	epSlices, err := c.endpointSlices.EndpointSlices(namespace).List(owned)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read endpointslices of %s/%s from cache: %w", namespace, service, err)
	}

	return svc, selector, epSlices, nil
}

// podFor resolves an endpoint's targetRef.name to the cached Pod behind it.
// Targets passes the lister's per-name read;
// Explain passes a lookup into the one selector-matched list it captured, so it reads the Pod cache once.
type podFor func(name string) (*corev1.Pod, bool)

// endpointFacts is what a Pod's trusted endpoints said about it, gathered over the endpoint pass.
// Each flag records that at least one trusted endpoint stopped at that rule;
// address and conflict are decided over the endpoints that passed the address rule.
type endpointFacts struct {
	notReady        bool   // an endpoint carries conditions.ready: false
	addressMismatch bool   // an endpoint has no address, or one the Pod's status.podIPs does not list
	address         string // the address of the first endpoint whose address the Pod holds
	conflict        bool   // a later such endpoint named a different address the Pod holds
	pod             string // the Pod's name, for the operator warning
	target          Target
	hasTarget       bool
}

// isTarget reports whether the Pod's endpoints made it a target: at least one eligible one, and no conflict.
func (f *endpointFacts) isTarget() bool { return f.hasTarget && !f.conflict }

// conflicted reports whether the Pod's exclusion is attributed to the address conflict.
// A Pod with an eligible endpoint is excluded by the conflict alone;
// any other conflicted Pod takes the first table reason that holds,
// and an unready or mismatched endpoint precedes the conflict there.
func (f *endpointFacts) conflicted() bool {
	return f.conflict && (f.hasTarget || (!f.notReady && !f.addressMismatch))
}

// resolve walks the Service's slices of the read address family and evaluates the eligibility rules once:
// the targets it yields, and, per Pod a trusted endpoint named, what its endpoints said about it.
// seen is keyed by Pod UID.
// A rule that does not hold makes the entry ineligible, never an error:
// an endpoint the gateway cannot vouch for is dropped,
// and the rest of the Service is still resolvable.
// Two endpoints that both pass the address rule and disagree on the address are a conflict:
// that Pod is excluded whether or not a pprof port resolves for it,
// and the conflict is logged when it is the reason the Pod carries.
func (c *Cluster) resolve(svc *corev1.Service, selector labels.Selector,
	epSlices []*discoveryv1.EndpointSlice, sel PortSelection, lookup podFor) ([]Target, map[string]*endpointFacts) {
	family := addressFamily(epSlices)
	seen := make(map[string]*endpointFacts)
	var order []string // Pod UIDs in the order their first trusted endpoint was met
	for _, es := range epSlices {
		if es.AddressType != family {
			continue
		}
		for i := range es.Endpoints {
			ep := &es.Endpoints[i]
			ref := ep.TargetRef
			if ref == nil || ref.Kind != podKind || ref.Namespace != svc.Namespace {
				continue
			}
			pod, ok := lookup(ref.Name)
			if !ok {
				continue
			}
			// A slice naming a recreated Pod by its predecessor's UID does not qualify.
			if pod.UID != ref.UID {
				continue
			}
			// A slice entry for a Pod the Service would not select is ignored.
			if !selector.Matches(labels.Set(pod.Labels)) {
				continue
			}
			uid := string(pod.UID)
			facts := seen[uid]
			if facts == nil {
				facts = &endpointFacts{pod: pod.Name}
				seen[uid] = facts
				order = append(order, uid)
			}
			// An unset endpoint readiness counts as ready, matching the EndpointSlice API contract.
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				facts.notReady = true

				continue
			}
			// The Pod's own state is attributed from the Pod object, not recorded per endpoint.
			if pod.Status.Phase != corev1.PodRunning || !podReady(pod) || pod.DeletionTimestamp != nil {
				continue
			}
			if len(ep.Addresses) == 0 || !hasPodIP(pod, ep.Addresses[0]) {
				facts.addressMismatch = true

				continue
			}
			address := ep.Addresses[0]
			switch {
			case facts.address == "":
				facts.address = address
			case facts.address != address:
				facts.conflict = true
			}
			port, ok := c.pprofPort(pod, sel)
			if !ok {
				continue
			}
			if facts.hasTarget {
				continue
			}
			facts.hasTarget = true
			facts.target = Target{
				Namespace: svc.Namespace,
				Service:   svc.Name,
				Pod:       pod.Name,
				Node:      pod.Spec.NodeName,
				PodIP:     address,
				Port:      port,
				Version:   pod.Labels[c.opts.VersionLabel],
				UID:       uid,
			}
		}
	}

	var targets []Target
	for _, uid := range order {
		facts := seen[uid]
		if facts.isTarget() {
			targets = append(targets, facts.target)
		}
		if facts.conflicted() {
			c.log.Warn("endpointslice address conflict, pod excluded",
				"namespace", svc.Namespace, "service", svc.Name, "pod", facts.pod)
		}
	}

	return targets, seen
}

// attribute names the reason a selected Pod that is not a target carries (spec: Eligibility).
// A Pod with at least one eligible trusted endpoint is excluded only by the address conflict, and carries it.
// Every other Pod takes the first reason of the table that holds for it:
// Pod state before endpoint state, because a terminating or unready Pod explains its own endpoint,
// then the endpoint reasons in table order, where the conflict follows an unready or mismatched endpoint.
// facts is nil for a Pod no trusted endpoint named.
func attribute(pod *corev1.Pod, facts *endpointFacts) string {
	switch {
	case pod.DeletionTimestamp != nil:
		return ReasonPodTerminating
	case pod.Status.Phase != corev1.PodRunning:
		return ReasonPodNotRunning
	case !podReady(pod):
		return ReasonPodNotReady
	case facts == nil:
		return ReasonEndpointMissing
	case facts.hasTarget:
		return ReasonEndpointAddressConflict
	case facts.notReady:
		return ReasonEndpointNotReady
	case facts.addressMismatch:
		return ReasonEndpointAddressMismatch
	case facts.conflict:
		return ReasonEndpointAddressConflict
	default:
		return ReasonPortNameNotDeclared
	}
}

// addressFamily is the address type Targets reads: IPv4 when the Service has any IPv4 slice,
// otherwise IPv6. A Service with neither has no address family and no targets.
func addressFamily(epSlices []*discoveryv1.EndpointSlice) discoveryv1.AddressType {
	family := discoveryv1.AddressType("")
	for _, es := range epSlices {
		if es.AddressType == discoveryv1.AddressTypeIPv4 {
			return discoveryv1.AddressTypeIPv4
		}
		if es.AddressType == discoveryv1.AddressTypeIPv6 {
			family = discoveryv1.AddressTypeIPv6
		}
	}

	return family
}

// podReady reports whether the Pod's Ready condition is True.
func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}

	return false
}

// hasPodIP reports whether address is one of the Pod's status.podIPs.
func hasPodIP(pod *corev1.Pod, address string) bool {
	for _, ip := range pod.Status.PodIPs {
		if ip.IP == address {
			return true
		}
	}

	return false
}

// pprofPort resolves the Pod's pprof port, the request's selection first and the configured default otherwise.
// A number is used for every Pod without checking its declarations;
// a name is the container port carrying it over TCP,
// and a Pod with no such port has no pprof port at all.
// The port resolves per Pod, never per endpoint,
// so all of a Pod's endpoints pass or fail the port rule together;
// attribute relies on that equivalence:
// a Pod whose endpoints passed the address checks without yielding a target has no pprof port,
// so no flag records the port rule.
func (c *Cluster) pprofPort(pod *corev1.Pod, sel PortSelection) (int32, bool) {
	switch {
	case sel.Port != 0:
		return sel.Port, true
	case sel.PortName != "":
		return namedPort(pod, sel.PortName)
	case c.opts.Port != 0:
		return c.opts.Port, true
	case c.opts.PortName != "":
		return namedPort(pod, c.opts.PortName)
	default:
		return 0, false
	}
}

// namedPort is the Pod's TCP container port of that name.
func namedPort(pod *corev1.Pod, name string) (int32, bool) {
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name != name {
				continue
			}
			// Kubernetes defaults an unset protocol to TCP.
			if port.Protocol == "" || port.Protocol == corev1.ProtocolTCP {
				return port.ContainerPort, true
			}
		}
	}

	return 0, false
}
