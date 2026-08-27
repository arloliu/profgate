package k8s

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// catalogService is one Service of the catalog fixture; selector nil makes it selectorless.
func catalogService(namespace, name string, selector map[string]string, kind corev1.ServiceType) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.ServiceSpec{Selector: selector, Type: kind},
	}
}

// catalogFixture is three selector-bearing Services across two namespaces
// and two selectorless ones, one of them ExternalName.
func catalogFixture() []runtime.Object {
	app := map[string]string{"app": "x"}

	return []runtime.Object{
		catalogService("a", "one", app, corev1.ServiceTypeClusterIP),
		catalogService("a", "two", app, corev1.ServiceTypeClusterIP),
		catalogService("b", "three", app, corev1.ServiceTypeClusterIP),
		catalogService("b", "four", nil, corev1.ServiceTypeClusterIP),
		catalogService("c", "five", nil, corev1.ServiceTypeExternalName),
	}
}

func ref(namespace, name string) ServiceRef { return ServiceRef{Namespace: namespace, Name: name} }

func TestCatalog(t *testing.T) {
	tests := []struct {
		name      string
		objects   []runtime.Object
		namespace string
		want      []ServiceRef
	}{
		{
			name:      "every namespace",
			objects:   catalogFixture(),
			namespace: "",
			want:      []ServiceRef{ref("a", "one"), ref("a", "two"), ref("b", "three")},
		},
		{
			name:      "one namespace",
			objects:   catalogFixture(),
			namespace: "b",
			want:      []ServiceRef{ref("b", "three")},
		},
		{
			// The gateway observes no Namespace objects, so an absent namespace
			// and an empty one are the same fact: an empty list, not an error.
			name:      "absent namespace",
			objects:   catalogFixture(),
			namespace: "zzz",
			want:      []ServiceRef{},
		},
		{
			// A selectorless Service is guaranteed to fail a profile request,
			// so it is omitted whatever its type; spec.type is never read.
			name: "selectorless whatever the type",
			objects: []runtime.Object{
				catalogService("b", "four", nil, corev1.ServiceTypeClusterIP),
				catalogService("c", "five", nil, corev1.ServiceTypeExternalName),
				catalogService("d", "six", nil, corev1.ServiceTypeNodePort),
			},
			namespace: "",
			want:      []ServiceRef{},
		},
		{
			name: "sorted",
			objects: []runtime.Object{
				catalogService("b", "z", map[string]string{"app": "z"}, corev1.ServiceTypeClusterIP),
				catalogService("a", "y", map[string]string{"app": "y"}, corev1.ServiceTypeClusterIP),
				catalogService("a", "x", map[string]string{"app": "x"}, corev1.ServiceTypeClusterIP),
			},
			namespace: "",
			want:      []ServiceRef{ref("a", "x"), ref("a", "y"), ref("b", "z")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, c, _ := startFixture(t, baseOptions(), tc.objects...)

			got, err := c.Catalog(context.Background(), tc.namespace)
			if err != nil {
				t.Fatalf("Catalog(%q) error = %v, want nil", tc.namespace, err)
			}
			if got == nil {
				t.Fatalf("Catalog(%q) = nil, want a non-nil slice: the listing routes encode it as a JSON array", tc.namespace)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Catalog(%q) = %v, want %v", tc.namespace, got, tc.want)
			}
		})
	}

	t.Run("reads only the cache", func(t *testing.T) {
		cs, c, _ := startFixture(t, baseOptions(), catalogFixture()...)

		// Filling the informers is the only traffic the fixture is allowed:
		// every listing after this line has to come out of the cache.
		cs.ClearActions()

		for _, namespace := range []string{"", "a"} {
			if _, err := c.Catalog(context.Background(), namespace); err != nil {
				t.Fatalf("Catalog(%q) error = %v, want nil", namespace, err)
			}
			acts := cs.Actions()
			if len(acts) == 0 {
				continue
			}
			tuples := make([]string, 0, len(acts))
			for _, a := range acts {
				tuples = append(tuples, a.GetVerb()+" "+a.GetResource().Resource)
			}
			t.Fatalf("Catalog(%q) issued %v; the catalog is read from the Service lister and never reaches the API server", namespace, tuples)
		}
	})

	t.Run("stays within the tuples", func(t *testing.T) {
		cs, c, _ := startFixture(t, baseOptions(), catalogFixture()...)

		for _, namespace := range []string{"", "a"} {
			if _, err := c.Catalog(context.Background(), namespace); err != nil {
				t.Fatalf("Catalog(%q) error = %v, want nil", namespace, err)
			}
		}
		// From fixture start through the listings, the seven granted read tuples
		// are all the API server ever sees: the catalog adds no RBAC tuple.
		for _, a := range cs.Actions() {
			if !isGranted(a.GetVerb(), a.GetResource().Resource) {
				t.Fatalf("discovery issued %q; the ClusterRole grants seven read tuples and nothing else",
					a.GetVerb()+" "+a.GetResource().Resource)
			}
		}
	})

	t.Run("reflects an added Service", func(t *testing.T) {
		cs, c, _ := startFixture(t, baseOptions(), catalogFixture()...)

		six := catalogService("a", "six", map[string]string{"app": "six"}, corev1.ServiceTypeClusterIP)
		if _, err := cs.CoreV1().Services("a").Create(context.Background(), six, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}

		var got []ServiceRef
		waitCache(t, func() bool {
			refs, err := c.Catalog(context.Background(), "a")
			got = refs

			return err == nil && len(refs) == 3
		})
		want := []ServiceRef{ref("a", "one"), ref("a", "six"), ref("a", "two")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Catalog(\"a\") = %v, want %v", got, want)
		}
	})

	t.Run("reflects a deleted Service", func(t *testing.T) {
		cs, c, _ := startFixture(t, baseOptions(), catalogFixture()...)

		if err := cs.CoreV1().Services("a").Delete(context.Background(), "one", metav1.DeleteOptions{}); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}

		waitCache(t, func() bool {
			refs, err := c.Catalog(context.Background(), "a")

			return err == nil && reflect.DeepEqual(refs, []ServiceRef{ref("a", "two")})
		})
	})
}
