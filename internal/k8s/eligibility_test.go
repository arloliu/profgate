package k8s

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	fixtureNamespace = "payment"
	fixtureService   = "payment-api"
	fixturePod       = "payment-api-1"
	fixtureUID       = "u1"
	fixtureNode      = "worker-1"
	fixtureIPv4      = "10.0.0.5"
	fixtureIPv6      = "fd00::5"
	fixtureVersion   = "1.2.3"
	versionLabelKey  = "app.kubernetes.io/version"
	portName         = "pprof"
)

func ptr[T any](v T) *T { return &v }

// fixture holds the objects a subtest loads into its own fake clientset.
// A subtest mutates these literals before the informers ever see them.
type fixture struct {
	svc    *corev1.Service
	pod    *corev1.Pod
	slices []*discoveryv1.EndpointSlice
}

// baseline is one Service with one ready Pod behind one IPv4 EndpointSlice.
func baseline() *fixture {
	return &fixture{
		svc: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNamespace, Name: fixtureService},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "payment"}},
		},
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: fixtureNamespace,
				Name:      fixturePod,
				UID:       types.UID(fixtureUID),
				Labels:    map[string]string{"app": "payment", versionLabelKey: fixtureVersion},
			},
			Spec: corev1.PodSpec{
				NodeName: fixtureNode,
				Containers: []corev1.Container{{
					Name:  "app",
					Ports: []corev1.ContainerPort{{Name: portName, ContainerPort: 6060, Protocol: corev1.ProtocolTCP}},
				}},
			},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				PodIPs:     []corev1.PodIP{{IP: fixtureIPv4}},
			},
		},
		slices: []*discoveryv1.EndpointSlice{newSlice("payment-api-abc", discoveryv1.AddressTypeIPv4, fixtureIPv4)},
	}
}

// newSlice is one EndpointSlice of the baseline Service holding a single endpoint for the baseline Pod.
func newSlice(name string, family discoveryv1.AddressType, address string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixtureNamespace,
			Name:      name,
			Labels:    map[string]string{discoveryv1.LabelServiceName: fixtureService},
		},
		AddressType: family,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{address},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
			TargetRef: &corev1.ObjectReference{
				Kind:      "Pod",
				Namespace: fixtureNamespace,
				Name:      fixturePod,
				UID:       types.UID(fixtureUID),
			},
		}},
	}
}

// endpoint is the single endpoint of the fixture's first slice.
func (f *fixture) endpoint() *discoveryv1.Endpoint { return &f.slices[0].Endpoints[0] }

func (f *fixture) objects() []runtime.Object {
	objs := make([]runtime.Object, 0, 2+len(f.slices))
	if f.svc != nil {
		objs = append(objs, f.svc)
	}
	if f.pod != nil {
		objs = append(objs, f.pod)
	}
	for _, s := range f.slices {
		objs = append(objs, s)
	}

	return objs
}

func baseOptions() Options {
	return Options{VersionLabel: versionLabelKey, PortName: portName}
}

// wantTarget is the Target the baseline fixture resolves to, after any adjustment.
func wantTarget(adjust func(*Target)) []Target {
	t := Target{
		Namespace: fixtureNamespace,
		Service:   fixtureService,
		Pod:       fixturePod,
		Node:      fixtureNode,
		PodIP:     fixtureIPv4,
		Port:      6060,
		Version:   fixtureVersion,
		UID:       fixtureUID,
	}
	if adjust != nil {
		adjust(&t)
	}

	return []Target{t}
}

// sink collects log records so a test can assert on what was logged.
type sink struct {
	mu      sync.Mutex
	records []string
}

func (s *sink) Enabled(context.Context, slog.Level) bool { return true }

func (s *sink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r.Message)

	return nil
}

func (s *sink) WithAttrs([]slog.Attr) slog.Handler { return s }

func (s *sink) WithGroup(string) slog.Handler { return s }

// logged reports whether any record's message contains sub.
func (s *sink) logged(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.records {
		if strings.Contains(m, sub) {
			return true
		}
	}

	return false
}

func TestTargets(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*fixture)
		options func(*Options)
		want    []Target
		wantErr error
		wantLog string
	}{
		{
			name: "baseline",
			want: wantTarget(nil),
		},
		{
			name:    "service missing",
			mutate:  func(f *fixture) { f.svc = nil },
			wantErr: ErrServiceNotFound,
		},
		{
			name:    "selectorless",
			mutate:  func(f *fixture) { f.svc.Spec.Selector = nil },
			wantErr: ErrServiceSelectorless,
		},
		{
			name:   "selector mismatch",
			mutate: func(f *fixture) { f.pod.Labels["app"] = "other" },
		},
		{
			name:   "stale uid",
			mutate: func(f *fixture) { f.endpoint().TargetRef.UID = "u0" },
		},
		{
			name:   "wrong namespace in targetRef",
			mutate: func(f *fixture) { f.endpoint().TargetRef.Namespace = "other" },
		},
		{
			name:   "targetRef not Pod",
			mutate: func(f *fixture) { f.endpoint().TargetRef.Kind = "Node" },
		},
		{
			name:   "ready false",
			mutate: func(f *fixture) { f.endpoint().Conditions.Ready = ptr(false) },
		},
		{
			name:   "ready nil",
			mutate: func(f *fixture) { f.endpoint().Conditions.Ready = nil },
			want:   wantTarget(nil),
		},
		{
			name:   "pod pending",
			mutate: func(f *fixture) { f.pod.Status.Phase = corev1.PodPending },
		},
		{
			name: "pod not ready",
			mutate: func(f *fixture) {
				f.pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
			},
		},
		{
			name:   "terminating",
			mutate: func(f *fixture) { f.pod.DeletionTimestamp = ptr(metav1.Now()) },
		},
		{
			name:   "address not in podIPs",
			mutate: func(f *fixture) { f.endpoint().Addresses = []string{"10.0.0.9"} },
		},
		{
			name:   "named port missing",
			mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports = nil },
		},
		{
			name:   "named port UDP",
			mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolUDP },
		},
		{
			name:   "named port protocol unset",
			mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports[0].Protocol = "" },
			want:   wantTarget(nil),
		},
		{
			name:   "numeric port mode",
			mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports = nil },
			options: func(o *Options) {
				o.Port = 7070
				o.PortName = ""
			},
			want: wantTarget(func(tg *Target) { tg.Port = 7070 }),
		},
		{
			name:   "version absent",
			mutate: func(f *fixture) { delete(f.pod.Labels, versionLabelKey) },
			want:   wantTarget(func(tg *Target) { tg.Version = "" }),
		},
		{
			name: "two slices same pod",
			mutate: func(f *fixture) {
				f.slices = append(f.slices, newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, fixtureIPv4))
			},
			want: wantTarget(nil),
		},
		{
			name: "valid plus invalid duplicate",
			mutate: func(f *fixture) {
				f.slices = append(f.slices, newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, "10.0.0.9"))
			},
			want: wantTarget(nil),
		},
		{
			name: "two valid conflicting",
			mutate: func(f *fixture) {
				f.pod.Status.PodIPs = []corev1.PodIP{{IP: fixtureIPv4}, {IP: "10.0.0.6"}}
				f.slices = append(f.slices, newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, "10.0.0.6"))
			},
			wantLog: "conflict",
		},
		{
			name: "dual stack",
			mutate: func(f *fixture) {
				f.pod.Status.PodIPs = []corev1.PodIP{{IP: fixtureIPv4}, {IP: fixtureIPv6}}
				f.slices = append(f.slices, newSlice("payment-api-v6", discoveryv1.AddressTypeIPv6, fixtureIPv6))
			},
			want: wantTarget(nil),
		},
		{
			name: "ipv6 only",
			mutate: func(f *fixture) {
				f.pod.Status.PodIPs = []corev1.PodIP{{IP: fixtureIPv6}}
				f.slices = []*discoveryv1.EndpointSlice{newSlice("payment-api-v6", discoveryv1.AddressTypeIPv6, fixtureIPv6)}
			},
			want: wantTarget(func(tg *Target) { tg.PodIP = fixtureIPv6 }),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := baseline()
			if tc.mutate != nil {
				tc.mutate(f)
			}
			opts := baseOptions()
			if tc.options != nil {
				tc.options(&opts)
			}
			log := &sink{}
			opts.Logger = slog.New(log)

			_, c, _ := startFixture(t, opts, f.objects()...)

			got, err := c.Targets(context.Background(), fixtureNamespace, fixtureService)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Targets() error = %v, want %v", err, tc.wantErr)
				}

				return
			}
			if err != nil {
				t.Fatalf("Targets() error = %v, want nil", err)
			}
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Targets() = %+v, want %+v", got, tc.want)
			}
			if tc.wantLog != "" && !log.logged(tc.wantLog) {
				t.Fatalf("Targets() logged %v, want a record containing %q: an operator has to be able to see why the Pod was dropped", log.records, tc.wantLog)
			}
		})
	}

	t.Run("not synced", func(t *testing.T) {
		c := New(fake.NewClientset(), baseOptions())
		if c.HasSynced() {
			t.Fatal("HasSynced() = true before Run: the API listener answers 503 not_ready until the caches are filled")
		}
	})

	t.Run("cache follows deletes", func(t *testing.T) {
		f := baseline()
		cs, c, _ := startFixture(t, baseOptions(), f.objects()...)

		got, err := c.Targets(context.Background(), fixtureNamespace, fixtureService)
		if err != nil || len(got) != 1 {
			t.Fatalf("Targets() = %+v, %v, want one target and no error", got, err)
		}

		err = cs.CoreV1().Pods(fixtureNamespace).Delete(context.Background(), fixturePod, metav1.DeleteOptions{})
		if err != nil {
			t.Fatalf("Delete() error = %v", err)
		}

		waitCache(t, func() bool {
			targets, err := c.Targets(context.Background(), fixtureNamespace, fixtureService)

			return err == nil && len(targets) == 0
		})
	})
}
