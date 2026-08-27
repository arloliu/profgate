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

// isGranted reports whether the ClusterRole permits a verb-resource pair.
// These seven are the only ones discovery may issue.
func isGranted(verb, resource string) bool {
	switch verb + " " + resource {
	case "list services", "watch services",
		"list pods", "watch pods", "get pods",
		"list endpointslices", "watch endpointslices":
		return true
	default:
		return false
	}
}

// fixture holds the objects a subtest loads into its own fake clientset.
// A subtest mutates these literals before the informers ever see them.
type fixture struct {
	svc    *corev1.Service
	pod    *corev1.Pod
	more   []*corev1.Pod // further Pods added by addPod, each with an endpoint in the first slice
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

// addPod appends another ready Pod of the baseline Service, a copy of the baseline Pod under its own
// name, UID, and address, and an endpoint for it in the first slice. It returns the Pod so a row can
// give it different port declarations.
func (f *fixture) addPod(name, uid, address string) *corev1.Pod {
	pod := baseline().pod
	pod.Name = name
	pod.UID = types.UID(uid)
	pod.Status.PodIPs = []corev1.PodIP{{IP: address}}
	f.more = append(f.more, pod)
	f.slices[0].Endpoints = append(f.slices[0].Endpoints, discoveryv1.Endpoint{
		Addresses:  []string{address},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
		TargetRef: &corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: fixtureNamespace,
			Name:      name,
			UID:       types.UID(uid),
		},
	})

	return pod
}

func (f *fixture) objects() []runtime.Object {
	objs := make([]runtime.Object, 0, 2+len(f.more)+len(f.slices))
	if f.svc != nil {
		objs = append(objs, f.svc)
	}
	if f.pod != nil {
		objs = append(objs, f.pod)
	}
	for _, p := range f.more {
		objs = append(objs, p)
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

// altPort declares a second TCP container port named pprof-alt on the Pod.
func altPort(pod *corev1.Pod, protocol corev1.Protocol) {
	pod.Spec.Containers[0].Ports = append(pod.Spec.Containers[0].Ports,
		corev1.ContainerPort{Name: "pprof-alt", ContainerPort: 6061, Protocol: protocol})
}

// numericOptions is the numeric default port mode.
func numericOptions(port int32) func(*Options) {
	return func(o *Options) {
		o.Port = port
		o.PortName = ""
	}
}

func TestTargets(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*fixture)
		options func(*Options)
		sel     PortSelection
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
			name:    "numeric port mode",
			mutate:  func(f *fixture) { f.pod.Spec.Containers[0].Ports = nil },
			options: numericOptions(7070),
			want:    wantTarget(func(tg *Target) { tg.Port = 7070 }),
		},
		{
			// A request port is used for every Pod without checking its declarations.
			name:   "numeric selection ignores declarations",
			mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports = nil },
			sel:    PortSelection{Port: 7070},
			want:   wantTarget(func(tg *Target) { tg.Port = 7070 }),
		},
		{
			name:    "numeric selection over numeric default",
			options: numericOptions(6060),
			sel:     PortSelection{Port: 6061},
			want:    wantTarget(func(tg *Target) { tg.Port = 6061 }),
		},
		{
			name:    "name selection resolves per Pod",
			mutate:  func(f *fixture) { altPort(f.pod, corev1.ProtocolTCP) },
			options: numericOptions(6060),
			sel:     PortSelection{PortName: "pprof-alt"},
			want:    wantTarget(func(tg *Target) { tg.Port = 6061 }),
		},
		{
			// The Pod that declares the requested name is a target; the one without it is ineligible.
			name: "name selection on one Pod of two",
			mutate: func(f *fixture) {
				altPort(f.pod, corev1.ProtocolTCP)
				f.addPod(secondPodName, secondUID, secondIPv4)
			},
			sel:  PortSelection{PortName: "pprof-alt"},
			want: wantTarget(func(tg *Target) { tg.Port = 6061 }),
		},
		{
			name: "name absent from every Pod",
			sel:  PortSelection{PortName: "nowhere"},
		},
		{
			name:   "name selection requires TCP",
			mutate: func(f *fixture) { altPort(f.pod, corev1.ProtocolUDP) },
			sel:    PortSelection{PortName: "pprof-alt"},
		},
		{
			name:   "name selection protocol unset",
			mutate: func(f *fixture) { altPort(f.pod, "") },
			sel:    PortSelection{PortName: "pprof-alt"},
			want:   wantTarget(func(tg *Target) { tg.Port = 6061 }),
		},
		{
			name: "zero selection keeps the default name",
			sel:  PortSelection{},
			want: wantTarget(nil),
		},
		{
			name:    "zero selection keeps the default number",
			mutate:  func(f *fixture) { f.pod.Spec.Containers[0].Ports = nil },
			options: numericOptions(7070),
			sel:     PortSelection{},
			want:    wantTarget(func(tg *Target) { tg.Port = 7070 }),
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

			cs, c, _ := startFixture(t, opts, f.objects()...)

			got, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, tc.sel)
			// Whatever the selection, resolving reads only the caches:
			// the seven granted read tuples are all the API server ever sees.
			for _, a := range cs.Actions() {
				if !isGranted(a.GetVerb(), a.GetResource().Resource) {
					t.Fatalf("discovery issued %q; the ClusterRole grants seven read tuples and nothing else",
						a.GetVerb()+" "+a.GetResource().Resource)
				}
			}
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

	t.Run("selection reads no API", func(t *testing.T) {
		f := baseline()
		altPort(f.pod, corev1.ProtocolTCP)
		cs, c, _ := startFixture(t, baseOptions(), f.objects()...)

		// Filling the informers is the only traffic the fixture is allowed:
		// every resolution after this line has to come out of the caches.
		cs.ClearActions()

		for _, sel := range []PortSelection{{Port: 7070}, {PortName: "pprof-alt"}} {
			got, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, sel)
			if err != nil {
				t.Fatalf("Targets(%+v) error = %v, want nil", sel, err)
			}
			if len(got) != 1 {
				t.Fatalf("Targets(%+v) = %d targets, want 1", sel, len(got))
			}
			acts := cs.Actions()
			if len(acts) == 0 {
				continue
			}
			tuples := make([]string, 0, len(acts))
			for _, a := range acts {
				tuples = append(tuples, a.GetVerb()+" "+a.GetResource().Resource)
			}
			t.Fatalf("Targets(%+v) issued %v; a selection resolves from the informer caches and never reaches the API server", sel, tuples)
		}
	})

	t.Run("not synced", func(t *testing.T) {
		c := New(fake.NewClientset(), baseOptions())
		if c.HasSynced() {
			t.Fatal("HasSynced() = true before Run: the API listener answers 503 not_ready until the caches are filled")
		}
	})

	t.Run("cache follows deletes", func(t *testing.T) {
		f := baseline()
		cs, c, _ := startFixture(t, baseOptions(), f.objects()...)

		got, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, PortSelection{})
		if err != nil || len(got) != 1 {
			t.Fatalf("Targets() = %+v, %v, want one target and no error", got, err)
		}

		err = cs.CoreV1().Pods(fixtureNamespace).Delete(context.Background(), fixturePod, metav1.DeleteOptions{})
		if err != nil {
			t.Fatalf("Delete() error = %v", err)
		}

		waitCache(t, func() bool {
			targets, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, PortSelection{})

			return err == nil && len(targets) == 0
		})
	})
}
