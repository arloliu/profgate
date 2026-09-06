//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/watch"
)

// Namespace creates a namespace named after the test, labeled as a test-app namespace,
// and deletes it when the test ends.
// A leftover of the same name from an interrupted run is waited out first,
// because a Terminating namespace rejects creation.
func (h *Harness) Namespace(t *testing.T) string {
	t.Helper()
	name := namespaceName(t.Name())
	ctx := t.Context()
	err := h.createNamespace(ctx, name)
	if apierrors.IsAlreadyExists(err) {
		t.Logf("namespace %s exists from an earlier run; deleting and waiting for it", name)
		if err := h.deleteNamespace(ctx, name); err != nil {
			t.Fatal(err)
		}
		if err := h.waitNamespaceGone(ctx, name); err != nil {
			t.Fatal(err)
		}
		err = h.createNamespace(ctx, name)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.deleteNamespace(context.Background(), name); err != nil {
			t.Errorf("delete namespace %s: %v", name, err)
		}
	})
	return name
}

// namespaceName turns a test name into a DNS label:
// lower case, runs of other characters collapsed to one hyphen, trimmed, and capped at the Kubernetes limit.
func namespaceName(testName string) string {
	name := strings.ToLower(testName)
	name = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > namespaceMaxLength {
		name = strings.TrimRight(name[:namespaceMaxLength], "-")
	}
	return name
}

// createNamespace creates name with the test-app label.
// The gateway namespace may already exist on a reused cluster; any other collision is an error.
func (h *Harness) createNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{testAppNamespaceLabel: "true"},
	}}
	_, err := h.Client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && (name != gatewayNamespace || !apierrors.IsAlreadyExists(err)) {
		return fmt.Errorf("create namespace %s: %w", name, err)
	}
	return nil
}

func (h *Harness) deleteNamespace(ctx context.Context, name string) error {
	err := h.Client.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", name, err)
	}
	return nil
}

func (h *Harness) waitNamespaceGone(ctx context.Context, name string) error {
	return poll(ctx, podTimeout, func(ctx context.Context) (bool, error) {
		_, err := h.Client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// WaitPodReady blocks until the Pod's Ready condition is True.
func (h *Harness) WaitPodReady(t *testing.T, ns, name string) {
	t.Helper()
	err := poll(t.Context(), podTimeout, func(ctx context.Context) (bool, error) {
		p, err := h.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return podReady(p), nil
	})
	if err != nil {
		t.Fatalf("wait for %s/%s ready: %v", ns, name, err)
	}
}

// WaitPodGone blocks until the Pod no longer exists.
func (h *Harness) WaitPodGone(t *testing.T, ns, name string) {
	t.Helper()
	err := poll(t.Context(), podTimeout, func(ctx context.Context) (bool, error) {
		_, err := h.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		t.Fatalf("wait for %s/%s gone: %v", ns, name, err)
	}
}

// WatchPods returns the event stream for every Pod in ns, stopped when the test ends.
func (h *Harness) WatchPods(t *testing.T, ns string) <-chan watch.Event {
	t.Helper()
	w, err := h.Client.CoreV1().Pods(ns).Watch(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("watch pods in %s: %v", ns, err)
	}
	t.Cleanup(w.Stop)
	return w.ResultChan()
}

// podReady reports whether the Pod's Ready condition is True.
func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// poll calls check every pollInterval until it returns true, an error, or timeout passes.
func poll(ctx context.Context, timeout time.Duration, check func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		done, err := check(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// CrashGateway stops the named gateway Pod's container without letting the
// gateway drain.
// Deleting the Pod does not stop the gateway: it is a SIGTERM the gateway
// answers by draining, and a drain keeps the worker running, so an owner whose
// kubelet lets the drain run its course finishes or fails its in-flight
// Collection instead of leaving a live lease behind.
// The container's exit is the crash the reclaim path is about.
func (h *Harness) CrashGateway(t *testing.T, pod string) {
	t.Helper()
	ctx := t.Context()
	p, err := h.Client.CoreV1().Pods(gatewayNamespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read pod %s: %v", pod, err)
	}
	// kind runs a node in a Docker container of the node's own name, so the
	// Pod's node names the container its crictl is reached through.
	node := p.Spec.NodeName
	ids, err := h.output(ctx, "docker", "exec", node, "crictl", "ps", "--quiet",
		"--label", "io.kubernetes.pod.name="+pod,
		"--label", "io.kubernetes.container.name="+gatewayContainer)
	if err != nil {
		t.Fatalf("list containers of %s on %s: %v", pod, node, err)
	}
	// An empty list is a container that is already gone, which is the state the
	// caller asked for.
	for _, id := range strings.Fields(ids) {
		if err := h.run(ctx, nil, "docker", "exec", node, "crictl", "stop", "--timeout", "0", id); err != nil {
			t.Fatalf("stop container %s of %s: %v", id, pod, err)
		}
	}
}

// waitOnePod blocks until exactly one ready, non-terminating Pod matches
// selector and returns its name.
func (h *Harness) waitOnePod(ctx context.Context, ns, selector string) (string, error) {
	var name string
	err := poll(ctx, rolloutTimeout, func(ctx context.Context) (bool, error) {
		list, err := h.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err
		}
		var ready []string
		for i := range list.Items {
			if list.Items[i].DeletionTimestamp == nil && podReady(&list.Items[i]) {
				ready = append(ready, list.Items[i].Name)
			}
		}
		if len(ready) != 1 {
			return false, nil
		}
		name = ready[0]
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("wait for one ready pod matching %s in %s: %w", selector, ns, err)
	}
	return name, nil
}
