package k8s

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	// recreatedUID is the UID of the replacement Pod that takes the fixture Pod's name and address.
	recreatedUID = "u2"
	// secondPodName, secondUID, and secondIPv4 describe a second backend added to a running fixture.
	secondPodName = "payment-api-2"
	secondUID     = "u3"
	secondIPv4    = "10.0.0.6"
	// movedIPv4 is an address the fixture Pod never held.
	movedIPv4 = "10.0.0.7"

	// loadWorkers, loadDuration, and loadDelay describe the confirmation load the cache keeps up under.
	loadWorkers  = 64
	loadDuration = 2 * time.Second
	loadDelay    = 20 * time.Millisecond
)

// onlyTarget resolves the baseline Service and returns its single Target.
func onlyTarget(t *testing.T, c *Cluster) Target {
	t.Helper()

	targets, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, PortSelection{})
	if err != nil || len(targets) != 1 {
		t.Fatalf("Targets() = %+v, %v, want exactly one target and no error", targets, err)
	}

	return targets[0]
}

// livePod reads the fixture Pod from the fake API server, bypassing the informer caches.
func livePod(t *testing.T, cs *fake.Clientset) *corev1.Pod {
	t.Helper()

	pod, err := cs.CoreV1().Pods(fixtureNamespace).Get(context.Background(), fixturePod, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	return pod
}

// updateLive applies mutate to the fixture Pod on the fake API server.
func updateLive(t *testing.T, cs *fake.Clientset, mutate func(*corev1.Pod)) {
	t.Helper()

	pod := livePod(t, cs)
	mutate(pod)
	if _, err := cs.CoreV1().Pods(fixtureNamespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
}

// updateLiveStatus applies mutate to the fixture Pod's status on the fake API server.
func updateLiveStatus(t *testing.T, cs *fake.Clientset, mutate func(*corev1.Pod)) {
	t.Helper()

	pod := livePod(t, cs)
	mutate(pod)
	_, err := cs.CoreV1().Pods(fixtureNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}
}

// deleteLive removes the fixture Pod from the fake API server.
func deleteLive(t *testing.T, cs *fake.Clientset) {
	t.Helper()

	if err := cs.CoreV1().Pods(fixtureNamespace).Delete(context.Background(), fixturePod, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

// recreateLive replaces the fixture Pod with one of the same name and address under a new UID,
// and points the EndpointSlice at the replacement, the way a controller replaces a Pod.
func recreateLive(t *testing.T, cs *fake.Clientset) {
	t.Helper()

	deleteLive(t, cs)
	pod := baseline().pod
	pod.UID = types.UID(recreatedUID)
	if _, err := cs.CoreV1().Pods(fixtureNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	slices := cs.DiscoveryV1().EndpointSlices(fixtureNamespace)
	es, err := slices.Get(context.Background(), baseline().slices[0].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	es.Endpoints[0].TargetRef.UID = types.UID(recreatedUID)
	if _, err := slices.Update(context.Background(), es, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
}

// secondPod is another eligible backend of the baseline Service.
func secondPod() *corev1.Pod {
	pod := baseline().pod
	pod.Name = secondPodName
	pod.UID = types.UID(secondUID)
	pod.Status.PodIPs = []corev1.PodIP{{IP: secondIPv4}}

	return pod
}

// secondSlice is the EndpointSlice carrying secondPod.
func secondSlice() *discoveryv1.EndpointSlice {
	es := newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, secondIPv4)
	es.Endpoints[0].TargetRef.Name = secondPodName
	es.Endpoints[0].TargetRef.UID = types.UID(secondUID)

	return es
}

// podGets is every get the fake API server received against pods.
func podGets(t *testing.T, cs *fake.Clientset) []k8stesting.GetAction {
	t.Helper()

	var gets []k8stesting.GetAction
	for _, a := range cs.Actions() {
		if a.GetVerb() != "get" || a.GetResource().Resource != "pods" {
			continue
		}
		ga, ok := a.(k8stesting.GetAction)
		if !ok {
			t.Fatalf("get action has type %T, want GetAction", a)
		}
		gets = append(gets, ga)
	}

	return gets
}

// wantOnly asserts that err matches want and not the other sentinel.
// The API listener picks its reason string by branching on which one it is,
// so an error carrying both would answer with whichever it happened to test first.
func wantOnly(t *testing.T, err error, want error) {
	t.Helper()

	if !errors.Is(err, want) {
		t.Fatalf("Confirm() = %v, want %v: the gateway must not dial a target the API server no longer vouches for", err, want)
	}
	other := ErrTargetChanged
	if errors.Is(want, ErrTargetChanged) {
		other = ErrDiscoveryUnavailable
	}
	if errors.Is(err, other) {
		t.Fatalf("Confirm() = %v, which also matches %v: the two outcomes carry different response codes", err, other)
	}
}

func TestConfirm(t *testing.T) {
	// Each row starts from the baseline, captures the Target the caches resolve, stops the informers,
	// and then changes the live object:
	// what Confirm sees is the API server alone.
	tests := []struct {
		name    string
		live    func(*testing.T, *fake.Clientset)
		wantErr error
	}{
		{
			name: "unchanged",
		},
		{
			name:    "deleted",
			live:    deleteLive,
			wantErr: ErrTargetChanged,
		},
		{
			name:    "recreated same name and ip",
			live:    recreateLive,
			wantErr: ErrTargetChanged,
		},
		{
			name: "terminating",
			live: func(t *testing.T, cs *fake.Clientset) {
				updateLive(t, cs, func(pod *corev1.Pod) { pod.DeletionTimestamp = ptr(metav1.Now()) })
			},
			wantErr: ErrTargetChanged,
		},
		{
			name: "not ready",
			live: func(t *testing.T, cs *fake.Clientset) {
				updateLiveStatus(t, cs, func(pod *corev1.Pod) {
					pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
				})
			},
			wantErr: ErrTargetChanged,
		},
		{
			name: "not running",
			live: func(t *testing.T, cs *fake.Clientset) {
				updateLiveStatus(t, cs, func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodSucceeded })
			},
			wantErr: ErrTargetChanged,
		},
		{
			name: "ip moved",
			live: func(t *testing.T, cs *fake.Clientset) {
				updateLiveStatus(t, cs, func(pod *corev1.Pod) {
					pod.Status.PodIPs = []corev1.PodIP{{IP: movedIPv4}}
				})
			},
			wantErr: ErrTargetChanged,
		},
		{
			name: "api error",
			live: func(_ *testing.T, cs *fake.Clientset) {
				cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("boom")
				})
			},
			wantErr: ErrDiscoveryUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs, c, cancel := startFixture(t, baseOptions(), baseline().objects()...)
			target := onlyTarget(t, c)
			cancel()
			if tc.live != nil {
				tc.live(t, cs)
			}

			err := c.Confirm(context.Background(), target)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Confirm() = %v, want nil: an unchanged Pod is still safe to dial", err)
				}

				return
			}
			wantOnly(t, err, tc.wantErr)
		})
	}

	t.Run("api timeout", func(t *testing.T) {
		cs, c, cancel := startFixture(t, baseOptions(), baseline().objects()...)
		target := onlyTarget(t, c)
		cancel()

		release := make(chan struct{})
		defer close(release)
		cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			<-release

			return true, nil, nil
		})

		ctx, cancelCall := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancelCall()
		done := make(chan error, 1)
		go func() { done <- c.Confirm(ctx, target) }()
		select {
		case err := <-done:
			wantOnly(t, err, ErrDiscoveryUnavailable)
		case <-time.After(time.Second):
			t.Fatal("Confirm() did not return within 1s: a client that ignores its context would stall the request forever")
		}
	})

	t.Run("caller cancelled", func(t *testing.T) {
		cs, c, cancel := startFixture(t, baseOptions(), baseline().objects()...)
		target := onlyTarget(t, c)
		cancel()

		// entered is closed the first time the API server is asked; release holds that call open.
		entered := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		var once sync.Once
		cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			once.Do(func() { close(entered) })
			<-release

			return true, nil, nil
		})

		ctx, cancelCall := context.WithCancel(context.Background())
		defer cancelCall()
		done := make(chan error, 1)
		go func() { done <- c.Confirm(ctx, target) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("the API server was not asked within 1s")
		}
		cancelCall()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Confirm() = %v, want context.Canceled: a caller that left is not an API server that could not answer", err)
			}
			if errors.Is(err, ErrDiscoveryUnavailable) {
				t.Fatalf("Confirm() = %v, which also matches ErrDiscoveryUnavailable: the two outcomes carry different audit codes", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Confirm() did not return within 1s of its context being cancelled")
		}
	})

	t.Run("one call only", func(t *testing.T) {
		cs, c, cancel := startFixture(t, baseOptions(), baseline().objects()...)
		target := onlyTarget(t, c)
		cancel()

		if err := c.Confirm(context.Background(), target); err != nil {
			t.Fatalf("Confirm() = %v, want nil", err)
		}
		gets := podGets(t, cs)
		if len(gets) != 1 {
			t.Fatalf("Confirm() issued %d gets, want exactly 1: one connection costs the API server one read", len(gets))
		}
		if gets[0].GetNamespace() != fixtureNamespace || gets[0].GetName() != fixturePod {
			t.Fatalf("Confirm() read %s/%s, want %s/%s", gets[0].GetNamespace(), gets[0].GetName(), fixtureNamespace, fixturePod)
		}
	})

	t.Run("cache unused", func(t *testing.T) {
		// The informers keep running, so the cache still holds the Pod when Confirm runs.
		cs, c, _ := startFixture(t, baseOptions(), baseline().objects()...)
		target := onlyTarget(t, c)
		deleteLive(t, cs)

		if err := c.Confirm(context.Background(), target); !errors.Is(err, ErrTargetChanged) {
			t.Fatalf("Confirm() = %v, want ErrTargetChanged: the live read decides, not a cache that has not caught up", err)
		}
		if n := len(podGets(t, cs)); n != 1 {
			t.Fatalf("Confirm() issued %d gets, want exactly 1: a confirmation that reads the cache proves nothing", n)
		}
	})

	t.Run("uid from target", func(t *testing.T) {
		cs, c, _ := startFixture(t, baseOptions(), baseline().objects()...)
		target := onlyTarget(t, c)
		recreateLive(t, cs)

		// The replacement Pod reaches the cache under the same name and address.
		waitCache(t, func() bool {
			targets, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, PortSelection{})

			return err == nil && len(targets) == 1 && targets[0].UID == recreatedUID
		})

		if err := c.Confirm(context.Background(), target); !errors.Is(err, ErrTargetChanged) {
			t.Fatalf("Confirm() = %v, want ErrTargetChanged: the UID captured at selection is what the read is checked against", err)
		}
	})

	t.Run("tuples across the lifecycle", func(t *testing.T) {
		cs, c, cancel := startFixture(t, baseOptions(), baseline().objects()...)
		target := onlyTarget(t, c)
		cancel()

		if err := c.Confirm(context.Background(), target); err != nil {
			t.Fatalf("Confirm() = %v, want nil", err)
		}

		for _, a := range cs.Actions() {
			if !isGranted(a.GetVerb(), a.GetResource().Resource) {
				t.Fatalf("discovery issued %q; the ClusterRole grants seven read tuples and nothing else",
					a.GetVerb()+" "+a.GetResource().Resource)
			}
		}
		if n := len(podGets(t, cs)); n != 1 {
			t.Fatalf("the lifecycle issued %d gets, want exactly 1: only the confirmation reads a named Pod", n)
		}
	})

	t.Run("relist continues under load", func(t *testing.T) {
		cs, c, _ := startFixture(t, baseOptions(), baseline().objects()...)
		target := onlyTarget(t, c)
		cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			time.Sleep(loadDelay)

			return false, nil, nil
		})

		ctx, stop := context.WithTimeout(context.Background(), loadDuration)
		defer stop()
		var (
			confirmed atomic.Int64
			wg        sync.WaitGroup
		)
		for range loadWorkers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					if err := c.Confirm(context.Background(), target); err != nil {
						t.Errorf("Confirm() = %v, want nil while the caches keep working", err)

						return
					}
					confirmed.Add(1)
				}
			}()
		}

		// The informers deliver a new backend while every confirmation slot is busy.
		if err := cs.Tracker().Add(secondPod()); err != nil {
			t.Fatalf("Tracker().Add() error = %v", err)
		}
		if err := cs.Tracker().Add(secondSlice()); err != nil {
			t.Fatalf("Tracker().Add() error = %v", err)
		}
		waitCache(t, func() bool {
			targets, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, PortSelection{})

			return err == nil && len(targets) == 2
		})

		wg.Wait()
		if confirmed.Load() == 0 {
			t.Fatal("no confirmation completed: the load the cache kept up under was not applied")
		}
	})
}
