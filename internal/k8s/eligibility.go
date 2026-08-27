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
func (c *Cluster) Targets(_ context.Context, namespace, service string, port PortSelection) ([]Target, error) {
	svc, err := c.services.Services(namespace).Get(service)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("service %s/%s: %w", namespace, service, ErrServiceNotFound)
		}

		return nil, fmt.Errorf("read service %s/%s from cache: %w", namespace, service, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return nil, fmt.Errorf("service %s/%s: %w", namespace, service, ErrServiceSelectorless)
	}
	selector := labels.SelectorFromSet(svc.Spec.Selector)

	owned := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: service})
	epSlices, err := c.endpointSlices.EndpointSlices(namespace).List(owned)
	if err != nil {
		return nil, fmt.Errorf("read endpointslices of %s/%s from cache: %w", namespace, service, err)
	}
	family := addressFamily(epSlices)

	// Valid entries are collected in the order they are met and deduplicated by Pod UID.
	// Two valid entries for one UID that disagree on address are a conflict: that Pod is excluded,
	// because the gateway cannot tell which address belongs to it.
	var found []Target
	seen := make(map[string]int, len(epSlices))
	conflicted := make(map[string]bool)
	for _, es := range epSlices {
		if es.AddressType != family {
			continue
		}
		for i := range es.Endpoints {
			target, ok := c.eligible(svc, selector, &es.Endpoints[i], port)
			if !ok {
				continue
			}
			first, dup := seen[target.UID]
			if !dup {
				seen[target.UID] = len(found)
				found = append(found, target)

				continue
			}
			if conflicted[target.UID] || found[first].PodIP == target.PodIP {
				continue
			}
			conflicted[target.UID] = true
			c.log.Warn("endpointslice address conflict, pod excluded",
				"namespace", namespace, "service", service, "pod", target.Pod)
		}
	}
	if len(conflicted) == 0 {
		return found, nil
	}

	targets := make([]Target, 0, len(found))
	for _, t := range found {
		if !conflicted[t.UID] {
			targets = append(targets, t)
		}
	}

	return targets, nil
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

// eligible applies the spec's eligibility rules to one endpoint and the cached Pod behind it.
// A rule that does not hold makes the entry ineligible, never an error:
// an endpoint the gateway cannot vouch for is dropped,
// and the rest of the Service is still resolvable.
func (c *Cluster) eligible(svc *corev1.Service, selector labels.Selector, ep *discoveryv1.Endpoint, sel PortSelection) (Target, bool) {
	ref := ep.TargetRef
	if ref == nil || ref.Kind != podKind || ref.Namespace != svc.Namespace {
		return Target{}, false
	}
	pod, err := c.pods.Pods(svc.Namespace).Get(ref.Name)
	if err != nil {
		return Target{}, false
	}
	// A slice naming a recreated Pod by its predecessor's UID does not qualify.
	if pod.UID != ref.UID {
		return Target{}, false
	}
	if !selector.Matches(labels.Set(pod.Labels)) {
		return Target{}, false
	}
	// An unset endpoint readiness counts as ready, matching the EndpointSlice API contract.
	if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
		return Target{}, false
	}
	if pod.Status.Phase != corev1.PodRunning || !podReady(pod) || pod.DeletionTimestamp != nil {
		return Target{}, false
	}
	if len(ep.Addresses) == 0 {
		return Target{}, false
	}
	address := ep.Addresses[0]
	if !hasPodIP(pod, address) {
		return Target{}, false
	}
	port, ok := c.pprofPort(pod, sel)
	if !ok {
		return Target{}, false
	}

	return Target{
		Namespace: svc.Namespace,
		Service:   svc.Name,
		Pod:       pod.Name,
		Node:      pod.Spec.NodeName,
		PodIP:     address,
		Port:      port,
		Version:   pod.Labels[c.opts.VersionLabel],
		UID:       string(pod.UID),
	}, true
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
