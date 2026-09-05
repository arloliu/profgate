package k8s

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// confirmCallTimeout bounds the single read a confirmation costs.
const confirmCallTimeout = 5 * time.Second

// A Cluster is the discovery the API listener runs against.
var _ Discovery = (*Cluster)(nil)

// Confirm re-reads the Pod behind t from the API server and checks that it is still the Pod that was selected:
// same UID, not terminating, running and ready, and still holding the selected address.
// Any mismatch, and a Pod that is gone, is ErrTargetChanged; any other failure of the read is ErrDiscoveryUnavailable,
// because a gateway that cannot check identity does not connect.
// A caller that cancels ctx while the read is in flight gets context.Canceled and neither sentinel:
// nothing about the target is known, and nothing is claimed.
// A deadline that passes, the confirmation's own or the caller's, is ErrDiscoveryUnavailable.
//
// The UID comes from t, captured at selection,
// so a replacement Pod that has since taken the same name cannot satisfy the check.
func (c *Cluster) Confirm(ctx context.Context, t Target) error {
	callCtx, cancel := context.WithTimeout(ctx, confirmCallTimeout)
	defer cancel()

	// pod is written by the call and read only after it has returned successfully,
	// which the deadline guard's channel orders.
	var pod *corev1.Pod
	err := deadlineGuard(callCtx, func(ctx context.Context) error {
		got, err := c.cs.CoreV1().Pods(t.Namespace).Get(ctx, t.Pod, metav1.GetOptions{})
		pod = got

		return err
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("pod %s/%s is gone: %w", t.Namespace, t.Pod, ErrTargetChanged)
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			// The caller left before the API server answered; nothing about the target is known, and nothing is claimed.
			return fmt.Errorf("confirm pod %s/%s: %w", t.Namespace, t.Pod, context.Canceled)
		}

		return fmt.Errorf("%w: confirm pod %s/%s: %s", ErrDiscoveryUnavailable, t.Namespace, t.Pod, err.Error())
	}

	switch {
	case string(pod.UID) != t.UID:
		return fmt.Errorf("pod %s/%s was replaced: %w", t.Namespace, t.Pod, ErrTargetChanged)
	case pod.DeletionTimestamp != nil:
		return fmt.Errorf("pod %s/%s is terminating: %w", t.Namespace, t.Pod, ErrTargetChanged)
	case pod.Status.Phase != corev1.PodRunning:
		return fmt.Errorf("pod %s/%s is in phase %s: %w", t.Namespace, t.Pod, pod.Status.Phase, ErrTargetChanged)
	case !podReady(pod):
		return fmt.Errorf("pod %s/%s is not ready: %w", t.Namespace, t.Pod, ErrTargetChanged)
	case !hasPodIP(pod, t.PodIP):
		return fmt.Errorf("pod %s/%s no longer holds the selected address: %w", t.Namespace, t.Pod, ErrTargetChanged)
	}

	return nil
}
