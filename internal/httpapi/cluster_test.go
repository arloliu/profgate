package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
)

const (
	clusterPod       = "payment-api-1"
	clusterSecondPod = "payment-api-2"
	clusterSecondIP  = "10.0.0.6"
	clusterPortName  = "pprof"
	versionLabelKey  = "app.kubernetes.io/version"

	// confirmDelay is how long each fake Pod read takes, so in-flight reads overlap.
	confirmDelay = 20 * time.Millisecond
	// concurrentRequests is how many profile requests race for the slots at once.
	concurrentRequests = 64
	// slotCap is the admission cap under test.
	slotCap = 4
	// cacheWaitTimeout bounds how long the test waits for the informers.
	cacheWaitTimeout = 5 * time.Second
)

// clusterPodObject is one ready Pod of the fixture Service, as the internal/k8s fixtures build it.
func clusterPodObject(name, uid, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixtureNamespace,
			Name:      name,
			UID:       types.UID(uid),
			Labels:    map[string]string{"app": "payment", versionLabelKey: fixtureVersion},
		},
		Spec: corev1.PodSpec{
			NodeName: fixtureNode,
			Containers: []corev1.Container{{
				Name:  "app",
				Ports: []corev1.ContainerPort{{Name: clusterPortName, ContainerPort: fixturePort, Protocol: corev1.ProtocolTCP}},
			}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			PodIPs:     []corev1.PodIP{{IP: ip}},
		},
	}
}

// clusterSliceObject is an EndpointSlice of the fixture Service holding one endpoint for the named Pod.
func clusterSliceObject(name, pod, uid, ip string) *discoveryv1.EndpointSlice {
	ready := true

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixtureNamespace,
			Name:      name,
			Labels:    map[string]string{discoveryv1.LabelServiceName: fixtureService},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{ip},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef: &corev1.ObjectReference{
				Kind:      "Pod",
				Namespace: fixtureNamespace,
				Name:      pod,
				UID:       types.UID(uid),
			},
		}},
	}
}

// clusterBaseline is one Service with one ready Pod behind one IPv4 EndpointSlice.
func clusterBaseline() []runtime.Object {
	return []runtime.Object{
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNamespace, Name: fixtureService},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "payment"}},
		},
		clusterPodObject(clusterPod, fixtureUID, fixtureIP),
		clusterSliceObject("payment-api-abc", clusterPod, fixtureUID, fixtureIP),
	}
}

// startCluster runs a real Cluster over a fake clientset holding the baseline objects
// and returns once its caches have synced.
func startCluster(t *testing.T) (*fake.Clientset, *k8s.Cluster) {
	t.Helper()

	cs := fake.NewClientset(clusterBaseline()...)
	cluster := k8s.NewRuntimeWithClientset(cs, k8s.Options{VersionLabel: versionLabelKey, PortName: clusterPortName}).Cluster()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		cluster.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	waitFor(t, cluster.HasSynced)

	return cs, cluster
}

// waitFor polls pred until it holds or the cache wait timeout passes.
func waitFor(t *testing.T, pred func() bool) {
	t.Helper()

	deadline := time.Now().Add(cacheWaitTimeout)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached within %s", cacheWaitTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAdmissionBoundsAPIReads(t *testing.T) {
	cs, cluster := startCluster(t)

	var inFlight, peak atomic.Int32
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(confirmDelay)

		// Falling through lets the tracker answer, so Confirm sees the real object.
		return false, nil, nil
	})

	h := newHarness()
	h.discovery = cluster
	h.configure(func(cfg *config.Config) { cfg.Limits.MaxConcurrentProfiles = slotCap })
	handler := h.handler()

	start := make(chan struct{})
	var (
		wg       sync.WaitGroup
		statuses [concurrentRequests]int
	)
	for i := range concurrentRequests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, profilePath+"heap", nil))
			assertNoLeak(t, rec)
			statuses[i] = rec.Code
		}()
	}
	close(start)

	// The informers deliver a new backend while the confirmation slots are busy.
	if err := cs.Tracker().Add(clusterPodObject(clusterSecondPod, "u2", clusterSecondIP)); err != nil {
		t.Fatalf("Tracker().Add() error = %v", err)
	}
	if err := cs.Tracker().Add(clusterSliceObject("payment-api-def", clusterSecondPod, "u2", clusterSecondIP)); err != nil {
		t.Fatalf("Tracker().Add() error = %v", err)
	}
	waitFor(t, func() bool {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, targetsPath, nil))
		var body struct {
			Targets []struct {
				Pod string `json:"pod"`
			} `json:"targets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			return false
		}
		for _, target := range body.Targets {
			if target.Pod == clusterSecondPod {
				return true
			}
		}

		return false
	})
	wg.Wait()

	if got := peak.Load(); got > slotCap {
		t.Errorf("peak in-flight Pod reads = %d, want at most %d: the admission cap must bound confirmations", got, slotCap)
	}
	var ok, rejected int
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			rejected++
		default:
			t.Errorf("unexpected status %d", status)
		}
	}
	if ok == 0 || rejected == 0 {
		t.Errorf("ok = %d, rejected = %d; want both: %d requests over %d slots", ok, rejected, concurrentRequests, slotCap)
	}
	if _, _, inFlight := h.rec.snapshot(); inFlight != 0 {
		t.Errorf("ProfilesInFlight net = %d, want 0", inFlight)
	}
}
