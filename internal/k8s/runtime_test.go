package k8s_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/arloliu/profgate/internal/k8s"
)

// runtimeOptions is the preflight options with a namespace file holding ownNS.
func runtimeOptions(t *testing.T) k8s.Options {
	t.Helper()

	path := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(path, []byte(ownNS+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := opts()
	o.NamespaceFile = path

	return o
}

func TestRuntime(t *testing.T) {
	t.Run("cluster is constructed but not running", func(t *testing.T) {
		rt := k8s.NewRuntimeWithClientset(fake.NewClientset(), runtimeOptions(t))

		c := rt.Cluster()
		if c == nil {
			t.Fatal("Cluster() = nil, want a constructed Cluster")
		}
		if c.HasSynced() {
			t.Fatal("HasSynced() = true before Run: the caller runs the Cluster, and the API listener answers 503 not_ready until it is synced")
		}
		if rt.Cluster() != c {
			t.Fatal("Cluster() returned a second Cluster: whoever runs the informers and whoever reads them must share one set")
		}
	})

	t.Run("own namespace", func(t *testing.T) {
		rt := k8s.NewRuntimeWithClientset(fake.NewClientset(), runtimeOptions(t))

		got, err := rt.OwnNamespace()
		if err != nil {
			t.Fatalf("OwnNamespace() error = %v", err)
		}
		if got != ownNS {
			t.Fatalf("OwnNamespace() = %q, want %q", got, ownNS)
		}
	})

	t.Run("preflight forbidden", func(t *testing.T) {
		options := runtimeOptions(t)

		direct := fake.NewClientset()
		denyWatch(direct, "pods")
		var want k8s.ErrForbidden
		if err := k8s.Preflight(context.Background(), direct, options, ownNS); !errors.As(err, &want) {
			t.Fatalf("Preflight() = %v, want ErrForbidden", err)
		}

		through := fake.NewClientset()
		denyWatch(through, "pods")
		var got k8s.ErrForbidden
		if err := k8s.NewRuntimeWithClientset(through, options).Preflight(context.Background()); !errors.As(err, &got) {
			t.Fatalf("Runtime.Preflight() = %v, want ErrForbidden", err)
		}
		if got != want {
			t.Fatalf("Runtime.Preflight() = %+v, want %+v: the runtime runs the same preflight over its own namespace", got, want)
		}
	})
}
