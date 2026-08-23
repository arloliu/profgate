package k8s

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	defaultNamespaceFile        = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	defaultPreflightCallTimeout = 10 * time.Second
	preflightPodName            = "profgate-preflight"

	clientQPS   = 20
	clientBurst = 50
)

// NewClientset uses in-cluster config, or KUBECONFIG when set, with QPS 20 and Burst 50.
func NewClientset() (kubernetes.Interface, error) {
	var (
		cfg *rest.Config
		err error
	)
	if path := os.Getenv("KUBECONFIG"); path != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	cfg.QPS = clientQPS
	cfg.Burst = clientBurst
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes clientset: %w", err)
	}
	return cs, nil
}

// OwnNamespace reads Options.NamespaceFile and returns its trimmed content.
func OwnNamespace(opts Options) (string, error) {
	path := opts.NamespaceFile
	if path == "" {
		path = defaultNamespaceFile
	}
	b, err := os.ReadFile(path) //nolint:gosec // path is the ServiceAccount namespace file or a test fixture; reading it is the purpose
	if err != nil {
		return "", fmt.Errorf("read namespace file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// ErrForbidden reports a 403 on one of the preflight's (resource, verb) tuples.
type ErrForbidden struct{ Resource, Verb string }

func (e ErrForbidden) Error() string { return fmt.Sprintf("forbidden: %s %s", e.Verb, e.Resource) }

// preflightCall is one list or watch against a resource, run under its own deadline.
type preflightCall struct {
	resource string
	list     func(context.Context, metav1.ListOptions) error
	watch    func(context.Context, metav1.ListOptions) (watch.Interface, error)
}

// deadlineGuard runs fn under ctx and returns as soon as either fn returns or ctx is done.
// The deadline is enforced here as well as passed down,
// so a transport that ignores its context still cannot stall the caller.
// The error is returned unclassified: what a failure means is the caller's decision, not this function's.
func deadlineGuard(ctx context.Context, fn func(context.Context) error) error {
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Preflight performs the seven calls from the spec's "Startup preflight" section.
// A 403 on any of them returns ErrForbidden; any other error is returned wrapped.
func Preflight(ctx context.Context, cs kubernetes.Interface, opts Options, ownNamespace string) error {
	timeout := opts.PreflightCallTimeout
	if timeout == 0 {
		timeout = defaultPreflightCallTimeout
	}
	call := func(verb, resource string, fn func(context.Context) error) error {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		err := deadlineGuard(callCtx, fn)
		switch {
		case err == nil:
			return nil
		case apierrors.IsForbidden(err):
			return ErrForbidden{Resource: resource, Verb: verb}
		default:
			return fmt.Errorf("preflight %s %s: %w", verb, resource, err)
		}
	}

	services := cs.CoreV1().Services("")
	pods := cs.CoreV1().Pods("")
	slices := cs.DiscoveryV1().EndpointSlices("")
	calls := []preflightCall{
		{
			resource: "services",
			list: func(c context.Context, o metav1.ListOptions) error {
				_, err := services.List(c, o)
				return err
			},
			watch: services.Watch,
		},
		{
			resource: "pods",
			list: func(c context.Context, o metav1.ListOptions) error {
				_, err := pods.List(c, o)
				return err
			},
			watch: pods.Watch,
		},
		{
			resource: "endpointslices",
			list: func(c context.Context, o metav1.ListOptions) error {
				_, err := slices.List(c, o)
				return err
			},
			watch: slices.Watch,
		},
	}

	for _, pc := range calls {
		err := call("list", pc.resource, func(c context.Context) error {
			return pc.list(c, metav1.ListOptions{Limit: 1})
		})
		if err != nil {
			return err
		}
		err = call("watch", pc.resource, func(c context.Context) error {
			one := int64(1)
			w, err := pc.watch(c, metav1.ListOptions{TimeoutSeconds: &one})
			if err != nil {
				return err
			}
			w.Stop()
			return nil
		})
		if err != nil {
			return err
		}
	}

	return call("get", "pods", func(c context.Context) error {
		_, err := cs.CoreV1().Pods(ownNamespace).Get(c, preflightPodName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	})
}
