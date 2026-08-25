package k8s_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/arloliu/profgate/internal/k8s"
)

const ownNS = "profgate-system"

// forbidden is the 403 the API server returns for a missing RBAC tuple.
func forbidden(resource string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", nil)
}

// denyVerb installs a reactor that answers verb on resource with a 403.
func denyVerb(cs *fake.Clientset, verb, resource string) {
	cs.PrependReactor(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbidden(resource)
	})
}

// denyWatch installs a watch reactor that answers watches on resource with a 403.
func denyWatch(cs *fake.Clientset, resource string) {
	cs.PrependWatchReactor(resource, func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, forbidden(resource)
	})
}

func opts() k8s.Options {
	return k8s.Options{PreflightCallTimeout: time.Second}
}

// TestNewClientsetOutOfCluster proves the failure names the way out.
// The in-cluster error alone names only the service variables,
// and reads as if a cluster were the only place the gateway runs.
func TestNewClientsetOutOfCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	_, err := k8s.NewClientset()
	if err == nil {
		t.Fatal("NewClientset() = nil error, want one")
	}
	if !strings.Contains(err.Error(), "KUBECONFIG") {
		t.Errorf("error = %q, want it to mention KUBECONFIG", err)
	}
}

func TestPreflight(t *testing.T) {
	t.Run("all allowed", func(t *testing.T) {
		cs := fake.NewClientset()
		if err := k8s.Preflight(context.Background(), cs, opts(), ownNS); err != nil {
			t.Fatalf("Preflight() = %v, want nil", err)
		}
	})

	forbiddenCases := []struct {
		name  string
		setup func(*fake.Clientset)
		want  k8s.ErrForbidden
	}{
		{
			name:  "watch pods forbidden",
			setup: func(cs *fake.Clientset) { denyWatch(cs, "pods") },
			want:  k8s.ErrForbidden{Resource: "pods", Verb: "watch"},
		},
		{
			name:  "get pods forbidden",
			setup: func(cs *fake.Clientset) { denyVerb(cs, "get", "pods") },
			want:  k8s.ErrForbidden{Resource: "pods", Verb: "get"},
		},
		{
			name:  "list endpointslices forbidden",
			setup: func(cs *fake.Clientset) { denyVerb(cs, "list", "endpointslices") },
			want:  k8s.ErrForbidden{Resource: "endpointslices", Verb: "list"},
		},
	}
	for _, tc := range forbiddenCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			tc.setup(cs)
			err := k8s.Preflight(context.Background(), cs, opts(), ownNS)
			var got k8s.ErrForbidden
			if !errors.As(err, &got) {
				t.Fatalf("Preflight() = %v, want ErrForbidden", err)
			}
			if got != tc.want {
				t.Fatalf("Preflight() = %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("get not found", func(t *testing.T) {
		cs := fake.NewClientset()
		cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "profgate-preflight")
		})
		if err := k8s.Preflight(context.Background(), cs, opts(), ownNS); err != nil {
			t.Fatalf("Preflight() = %v, want nil: a 404 means authorization already passed", err)
		}
	})

	t.Run("generic list error", func(t *testing.T) {
		cs := fake.NewClientset()
		boom := errors.New("boom")
		cs.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, boom
		})
		err := k8s.Preflight(context.Background(), cs, opts(), ownNS)
		if !errors.Is(err, boom) {
			t.Fatalf("Preflight() = %v, want it to wrap boom", err)
		}
		var fb k8s.ErrForbidden
		if errors.As(err, &fb) {
			t.Fatalf("Preflight() = %v, must not be ErrForbidden: a transient error is retried, not fatal", err)
		}
	})

	t.Run("blocking call", func(t *testing.T) {
		cs := fake.NewClientset()
		release := make(chan struct{})
		defer close(release)
		cs.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
			<-release
			return true, nil, nil
		})
		done := make(chan error, 1)
		go func() {
			done <- k8s.Preflight(context.Background(), cs, k8s.Options{PreflightCallTimeout: 50 * time.Millisecond}, ownNS)
		}()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Preflight() = %v, want it to wrap context.DeadlineExceeded", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Preflight() did not return within 1s: the per-call timeout is not enforced")
		}
	})

	t.Run("exact tuples", func(t *testing.T) {
		cs := fake.NewClientset()
		if err := k8s.Preflight(context.Background(), cs, opts(), ownNS); err != nil {
			t.Fatalf("Preflight() = %v, want nil", err)
		}
		var got []string
		for _, a := range cs.Actions() {
			got = append(got, a.GetVerb()+" "+a.GetResource().Resource)
			if a.GetVerb() == "get" {
				ga, ok := a.(k8stesting.GetAction)
				if !ok {
					t.Fatalf("get action has type %T, want GetAction", a)
				}
				if ga.GetNamespace() != ownNS || ga.GetName() != "profgate-preflight" {
					t.Fatalf("get names %s/%s, want %s/profgate-preflight", ga.GetNamespace(), ga.GetName(), ownNS)
				}
			}
		}
		want := []string{
			"get pods",
			"list endpointslices", "list pods", "list services",
			"watch endpointslices", "watch pods", "watch services",
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("preflight actions = %v, want exactly %v: the ClusterRole grants these seven tuples and nothing else", got, want)
		}
	})
}

func TestOwnNamespace(t *testing.T) {
	t.Run("trimmed content", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "namespace")
		if err := os.WriteFile(path, []byte("payment\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := k8s.OwnNamespace(k8s.Options{NamespaceFile: path})
		if err != nil {
			t.Fatalf("OwnNamespace() error = %v", err)
		}
		if got != "payment" {
			t.Fatalf("OwnNamespace() = %q, want %q", got, "payment")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent")
		if _, err := k8s.OwnNamespace(k8s.Options{NamespaceFile: path}); err == nil {
			t.Fatal("OwnNamespace() = nil error, want one for a missing file")
		}
	})
}
