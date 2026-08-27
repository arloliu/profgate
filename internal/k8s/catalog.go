package k8s

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// ServiceRef names one Service in the cache.
type ServiceRef struct {
	Namespace, Name string
}

// Catalog implements Discovery over the Service lister and nothing else.
// It lists the Services with a non-empty selector, sorted by namespace then name;
// an empty namespace means every namespace, and a namespace the cache lacks is an empty list.
// spec.type is not read: selector presence is the only criterion, as it is for Targets.
// It reads only the informer cache and cannot block, which is why it ignores the context.
func (c *Cluster) Catalog(_ context.Context, namespace string) ([]ServiceRef, error) {
	var (
		svcs []*corev1.Service
		err  error
	)
	if namespace != "" {
		svcs, err = c.services.Services(namespace).List(labels.Everything())
	} else {
		svcs, err = c.services.List(labels.Everything())
	}
	if err != nil {
		return nil, fmt.Errorf("read services from cache: %w", err)
	}

	refs := make([]ServiceRef, 0, len(svcs))
	for _, svc := range svcs {
		if len(svc.Spec.Selector) == 0 {
			continue
		}
		refs = append(refs, ServiceRef{Namespace: svc.Namespace, Name: svc.Name})
	}
	slices.SortFunc(refs, func(a, b ServiceRef) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})

	return refs, nil
}
