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

// Handle records the message followed by its attributes as key=value pairs,
// so a test can assert on what a line names as well as on what it says.
func (s *sink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()

		return true
	})
	s.records = append(s.records, line)

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

// count is the number of records whose line contains sub.
func (s *sink) count(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.records {
		if strings.Contains(m, sub) {
			n++
		}
	}

	return n
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

// wantExcluded is the one-reason report an explain row expects: the reason with a count of one and no other.
func wantExcluded(reason string) []Exclusion {
	return []Exclusion{{Reason: reason, Count: 1}}
}

// checkExplainSum asserts the invariant every report holds:
// each selected Pod is either a target or counted under exactly one reason.
func checkExplainSum(t *testing.T, ex Explanation) {
	t.Helper()

	sum := len(ex.Targets)
	for _, e := range ex.Excluded {
		if e.Count <= 0 {
			t.Fatalf("Explain() reported %q with count %d; a report carries only reasons with a non-zero count", e.Reason, e.Count)
		}
		sum += e.Count
	}
	if sum != ex.SelectorMatched {
		t.Fatalf("Explain() = %d targets and %+v over %d selected Pods; every selected Pod is a target or carries one reason",
			len(ex.Targets), ex.Excluded, ex.SelectorMatched)
	}
}

// checkNoRequest asserts the fixture issued nothing the seven read tuples do not grant.
func checkNoRequest(t *testing.T, cs *fake.Clientset) {
	t.Helper()

	for _, a := range cs.Actions() {
		if !isGranted(a.GetVerb(), a.GetResource().Resource) {
			t.Fatalf("discovery issued %q; the ClusterRole grants seven read tuples and nothing else",
				a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
}

func TestExplainReasons(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*fixture)
		options     func(*Options)
		sel         PortSelection
		wantMatched int
		wantTargets int
		wantExcl    []Exclusion
		wantErr     error
		wantLog     string // a warning line every row without it must not log
	}{
		{
			name:        "all targets",
			mutate:      func(f *fixture) { f.addPod(secondPodName, secondUID, secondIPv4) },
			wantMatched: 2,
			wantTargets: 2,
		},
		{
			name: "terminating and unready is terminating",
			mutate: func(f *fixture) {
				f.pod.DeletionTimestamp = ptr(metav1.Now())
				f.pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
			},
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonPodTerminating),
		},
		{
			name:        "pod pending",
			mutate:      func(f *fixture) { f.pod.Status.Phase = corev1.PodPending },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonPodNotRunning),
		},
		{
			name: "pod unready with endpoint unready is pod not ready",
			mutate: func(f *fixture) {
				f.pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
				f.endpoint().Conditions.Ready = ptr(false)
			},
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonPodNotReady),
		},
		{
			name:        "no slice entry",
			mutate:      func(f *fixture) { f.slices[0].Endpoints = nil },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointMissing),
		},
		{
			name:        "nil targetRef",
			mutate:      func(f *fixture) { f.endpoint().TargetRef = nil },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointMissing),
		},
		{
			name:        "targetRef not Pod",
			mutate:      func(f *fixture) { f.endpoint().TargetRef.Kind = "Node" },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointMissing),
		},
		{
			name:        "wrong namespace in targetRef",
			mutate:      func(f *fixture) { f.endpoint().TargetRef.Namespace = "other" },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointMissing),
		},
		{
			name:        "targetRef names no cached Pod",
			mutate:      func(f *fixture) { f.endpoint().TargetRef.Name = "payment-api-gone" },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointMissing),
		},
		{
			name:        "stale uid",
			mutate:      func(f *fixture) { f.endpoint().TargetRef.UID = "u0" },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointMissing),
		},
		{
			name:        "endpoint ready false",
			mutate:      func(f *fixture) { f.endpoint().Conditions.Ready = ptr(false) },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointNotReady),
		},
		{
			name:        "endpoint without address",
			mutate:      func(f *fixture) { f.endpoint().Addresses = nil },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointAddressMismatch),
		},
		{
			name:        "address not in podIPs",
			mutate:      func(f *fixture) { f.endpoint().Addresses = []string{"10.0.0.9"} },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointAddressMismatch),
		},
		{
			name: "two valid conflicting",
			mutate: func(f *fixture) {
				f.pod.Status.PodIPs = []corev1.PodIP{{IP: fixtureIPv4}, {IP: "10.0.0.6"}}
				f.slices = append(f.slices, newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, "10.0.0.6"))
			},
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointAddressConflict),
			wantLog:     "conflict",
		},
		{
			// The conflict is decided without the port rule, so a portless conflicted Pod is conflicted,
			// and the operator warning fires for it as for any other conflict.
			name: "conflicting and portless is conflicted",
			mutate: func(f *fixture) {
				f.pod.Status.PodIPs = []corev1.PodIP{{IP: fixtureIPv4}, {IP: "10.0.0.6"}}
				f.pod.Spec.Containers[0].Ports = nil
				f.slices = append(f.slices, newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, "10.0.0.6"))
			},
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonEndpointAddressConflict),
			wantLog:     "conflict",
		},
		{
			name:        "named port missing",
			mutate:      func(f *fixture) { f.pod.Spec.Containers[0].Ports = nil },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonPortNameNotDeclared),
		},
		{
			name:        "named port UDP",
			mutate:      func(f *fixture) { f.pod.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolUDP },
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonPortNameNotDeclared),
		},
		{
			name:        "request portName absent",
			sel:         PortSelection{PortName: "nowhere"},
			wantMatched: 1,
			wantExcl:    wantExcluded(ReasonPortNameNotDeclared),
		},
		{
			name:        "selector matches no Pod",
			mutate:      func(f *fixture) { f.svc.Spec.Selector = map[string]string{"app": "nobody"} },
			wantMatched: 0,
		},
		{
			name:        "unselected Pod in a slice is counted nowhere",
			mutate:      func(f *fixture) { f.pod.Labels["app"] = "other" },
			wantMatched: 0,
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

			got, err := c.Explain(context.Background(), fixtureNamespace, fixtureService, tc.sel)
			checkNoRequest(t, cs)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Explain() error = %v, want %v", err, tc.wantErr)
				}

				return
			}
			if err != nil {
				t.Fatalf("Explain() error = %v, want nil", err)
			}
			if got.SelectorMatched != tc.wantMatched {
				t.Fatalf("Explain().SelectorMatched = %d, want %d", got.SelectorMatched, tc.wantMatched)
			}
			if len(got.Targets) != tc.wantTargets {
				t.Fatalf("Explain().Targets = %+v, want %d targets", got.Targets, tc.wantTargets)
			}
			if len(got.Excluded) == 0 {
				got.Excluded = nil
			}
			if !reflect.DeepEqual(got.Excluded, tc.wantExcl) {
				t.Fatalf("Explain().Excluded = %+v, want %+v", got.Excluded, tc.wantExcl)
			}
			checkExplainSum(t, got)

			lines := log.count("conflict")
			if tc.wantLog != "" {
				if lines != 1 {
					t.Fatalf("Explain() logged %v; a conflicted Pod is warned about exactly once", log.records)
				}
				if !log.logged("namespace="+fixtureNamespace) || !log.logged("service="+fixtureService) || !log.logged("pod="+fixturePod) {
					t.Fatalf("Explain() logged %v; the operator warning names the namespace, Service, and Pod", log.records)
				}
			} else if lines != 0 {
				t.Fatalf("Explain() logged %v; only a conflict is warned about", log.records)
			}
		})
	}
}

// TestExplainAgreesWithTargets runs every Targets row through Explain as well:
// the attribution a rejection earns, the sum invariant, and the same target set under either Pod lookup.
func TestExplainAgreesWithTargets(t *testing.T) {
	rows := []struct {
		name    string
		mutate  func(*fixture)
		options func(*Options)
		sel     PortSelection
		reason  string // the reason the baseline Pod earns, "" when it is a target or unselected
	}{
		{name: "baseline"},
		{name: "selector mismatch", mutate: func(f *fixture) { f.pod.Labels["app"] = "other" }},
		{name: "stale uid", mutate: func(f *fixture) { f.endpoint().TargetRef.UID = "u0" }, reason: ReasonEndpointMissing},
		{name: "wrong namespace in targetRef", mutate: func(f *fixture) { f.endpoint().TargetRef.Namespace = "other" }, reason: ReasonEndpointMissing},
		{name: "targetRef not Pod", mutate: func(f *fixture) { f.endpoint().TargetRef.Kind = "Node" }, reason: ReasonEndpointMissing},
		{name: "ready false", mutate: func(f *fixture) { f.endpoint().Conditions.Ready = ptr(false) }, reason: ReasonEndpointNotReady},
		{name: "ready nil", mutate: func(f *fixture) { f.endpoint().Conditions.Ready = nil }},
		{name: "pod pending", mutate: func(f *fixture) { f.pod.Status.Phase = corev1.PodPending }, reason: ReasonPodNotRunning},
		{
			name: "pod not ready",
			mutate: func(f *fixture) {
				f.pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
			},
			reason: ReasonPodNotReady,
		},
		{name: "terminating", mutate: func(f *fixture) { f.pod.DeletionTimestamp = ptr(metav1.Now()) }, reason: ReasonPodTerminating},
		{name: "address not in podIPs", mutate: func(f *fixture) { f.endpoint().Addresses = []string{"10.0.0.9"} }, reason: ReasonEndpointAddressMismatch},
		{name: "named port missing", mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports = nil }, reason: ReasonPortNameNotDeclared},
		{name: "named port UDP", mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolUDP }, reason: ReasonPortNameNotDeclared},
		{name: "named port protocol unset", mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports[0].Protocol = "" }},
		{name: "numeric port mode", mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports = nil }, options: numericOptions(7070)},
		{name: "numeric selection ignores declarations", mutate: func(f *fixture) { f.pod.Spec.Containers[0].Ports = nil }, sel: PortSelection{Port: 7070}},
		{name: "name selection resolves per Pod", mutate: func(f *fixture) { altPort(f.pod, corev1.ProtocolTCP) }, options: numericOptions(6060), sel: PortSelection{PortName: "pprof-alt"}},
		{
			name: "name selection on one Pod of two",
			mutate: func(f *fixture) {
				altPort(f.pod, corev1.ProtocolTCP)
				f.addPod(secondPodName, secondUID, secondIPv4)
			},
			sel:    PortSelection{PortName: "pprof-alt"},
			reason: ReasonPortNameNotDeclared, // the second Pod, which does not declare the name
		},
		{name: "name absent from every Pod", sel: PortSelection{PortName: "nowhere"}, reason: ReasonPortNameNotDeclared},
		{name: "name selection requires TCP", mutate: func(f *fixture) { altPort(f.pod, corev1.ProtocolUDP) }, sel: PortSelection{PortName: "pprof-alt"}, reason: ReasonPortNameNotDeclared},
		{name: "version absent", mutate: func(f *fixture) { delete(f.pod.Labels, versionLabelKey) }},
		{
			name: "two slices same pod",
			mutate: func(f *fixture) {
				f.slices = append(f.slices, newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, fixtureIPv4))
			},
		},
		{
			name: "valid plus invalid duplicate",
			mutate: func(f *fixture) {
				f.slices = append(f.slices, newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, "10.0.0.9"))
			},
		},
		{
			name: "two valid conflicting",
			mutate: func(f *fixture) {
				f.pod.Status.PodIPs = []corev1.PodIP{{IP: fixtureIPv4}, {IP: "10.0.0.6"}}
				f.slices = append(f.slices, newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, "10.0.0.6"))
			},
			reason: ReasonEndpointAddressConflict,
		},
		{
			name: "dual stack",
			mutate: func(f *fixture) {
				f.pod.Status.PodIPs = []corev1.PodIP{{IP: fixtureIPv4}, {IP: fixtureIPv6}}
				f.slices = append(f.slices, newSlice("payment-api-v6", discoveryv1.AddressTypeIPv6, fixtureIPv6))
			},
		},
		{
			name: "ipv6 only",
			mutate: func(f *fixture) {
				f.pod.Status.PodIPs = []corev1.PodIP{{IP: fixtureIPv6}}
				f.slices = []*discoveryv1.EndpointSlice{newSlice("payment-api-v6", discoveryv1.AddressTypeIPv6, fixtureIPv6)}
			},
		},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			f := baseline()
			if tc.mutate != nil {
				tc.mutate(f)
			}
			opts := baseOptions()
			if tc.options != nil {
				tc.options(&opts)
			}
			opts.Logger = slog.New(&sink{})
			cs, c, _ := startFixture(t, opts, f.objects()...)

			targets, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, tc.sel)
			if err != nil {
				t.Fatalf("Targets() error = %v, want nil", err)
			}
			got, err := c.Explain(context.Background(), fixtureNamespace, fixtureService, tc.sel)
			if err != nil {
				t.Fatalf("Explain() error = %v, want nil", err)
			}
			checkNoRequest(t, cs)
			checkExplainSum(t, got)

			if !sameTargets(targets, got.Targets) {
				t.Fatalf("Explain().Targets = %+v, Targets() = %+v; the two reach Pods differently and must agree", got.Targets, targets)
			}
			var want []Exclusion
			if tc.reason != "" {
				want = wantExcluded(tc.reason)
			}
			if len(got.Excluded) == 0 {
				got.Excluded = nil
			}
			if !reflect.DeepEqual(got.Excluded, want) {
				t.Fatalf("Explain().Excluded = %+v, want %+v: a rule tested for exclusion is tested for its explanation here", got.Excluded, want)
			}
		})
	}
}

// sameTargets reports whether two target lists hold the same Targets in any order.
func sameTargets(a, b []Target) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[Target]int, len(a))
	for _, t := range a {
		seen[t]++
	}
	for _, t := range b {
		seen[t]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}

	return true
}

// mixedFixture is one Service over five Pods carrying, in an order the vocabulary does not use,
// a missing port, a terminating Pod, an unready endpoint, a target, and an address mismatch.
func mixedFixture() *fixture {
	f := baseline()
	f.pod.Spec.Containers[0].Ports = nil // port_name_not_declared
	f.addPod("payment-api-2", "u2", "10.0.0.6").DeletionTimestamp = ptr(metav1.Now())
	f.addPod("payment-api-3", "u3", "10.0.0.7")
	f.slices[0].Endpoints[2].Conditions.Ready = ptr(false)
	f.addPod("payment-api-4", "u4", "10.0.0.8")
	f.addPod("payment-api-5", "u5", "10.0.0.9")
	f.slices[0].Endpoints[4].Addresses = []string{"10.0.0.99"}

	return f
}

// mixedWant is the report mixedFixture earns, in vocabulary order.
func mixedWant() Explanation {
	return Explanation{
		SelectorMatched: 5,
		Targets: []Target{{
			Namespace: fixtureNamespace, Service: fixtureService, Pod: "payment-api-4", Node: fixtureNode,
			PodIP: "10.0.0.8", Port: 6060, Version: fixtureVersion, UID: "u4",
		}},
		Excluded: []Exclusion{
			{Reason: ReasonPodTerminating, Count: 1},
			{Reason: ReasonEndpointNotReady, Count: 1},
			{Reason: ReasonEndpointAddressMismatch, Count: 1},
			{Reason: ReasonPortNameNotDeclared, Count: 1},
		},
	}
}

func TestExplainOrder(t *testing.T) {
	reverseEndpoints := func(f *fixture) {
		eps := f.slices[0].Endpoints
		for i, j := 0, len(eps)-1; i < j; i, j = i+1, j-1 {
			eps[i], eps[j] = eps[j], eps[i]
		}
	}
	// splitSlices moves every endpoint after the first into a second slice, so the slice order matters.
	splitSlices := func(f *fixture) {
		second := newSlice("payment-api-def", discoveryv1.AddressTypeIPv4, fixtureIPv4)
		second.Endpoints = f.slices[0].Endpoints[1:]
		f.slices[0].Endpoints = f.slices[0].Endpoints[:1]
		f.slices = append(f.slices, second)
	}
	reverseSlices := func(f *fixture) {
		splitSlices(f)
		f.slices[0], f.slices[1] = f.slices[1], f.slices[0]
	}
	reversePods := func(f *fixture) {
		pods := append([]*corev1.Pod{f.pod}, f.more...)
		for i, j := 0, len(pods)-1; i < j; i, j = i+1, j-1 {
			pods[i], pods[j] = pods[j], pods[i]
		}
		f.pod, f.more = pods[0], pods[1:]
	}

	tests := []struct {
		name    string
		reorder func(*fixture)
	}{
		{name: "as inserted"},
		{name: "slices reversed", reorder: reverseSlices},
		{name: "endpoints reversed", reorder: reverseEndpoints},
		{name: "pods reversed", reorder: reversePods},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := mixedFixture()
			if tc.reorder != nil {
				tc.reorder(f)
			}
			cs, c, _ := startFixture(t, baseOptions(), f.objects()...)

			first, err := c.Explain(context.Background(), fixtureNamespace, fixtureService, PortSelection{})
			if err != nil {
				t.Fatalf("Explain() error = %v, want nil", err)
			}
			checkNoRequest(t, cs)
			checkExplainSum(t, first)
			want := mixedWant()
			if !reflect.DeepEqual(first.Excluded, want.Excluded) {
				t.Fatalf("Explain().Excluded = %+v, want %+v in vocabulary order whatever the caches' order", first.Excluded, want.Excluded)
			}
			if first.SelectorMatched != want.SelectorMatched || !reflect.DeepEqual(first.Targets, want.Targets) {
				t.Fatalf("Explain() = %+v, want %+v", first, want)
			}

			second, err := c.Explain(context.Background(), fixtureNamespace, fixtureService, PortSelection{})
			if err != nil {
				t.Fatalf("Explain() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(first.Excluded, second.Excluded) {
				t.Fatalf("two Explain() calls over identical caches = %+v then %+v; replicas must answer identically", first.Excluded, second.Excluded)
			}
		})
	}
}

// TestExplainReads pins which method pays for which Pod-cache read:
// Targets reads one Pod per endpoint by name, and Explain lists the namespace once under the selector.
func TestExplainReads(t *testing.T) {
	// otherPod is a Pod of the namespace the selector does not match, with no slice entry.
	otherPod := func() *corev1.Pod {
		pod := baseline().pod
		pod.Name = "billing-1"
		pod.UID = "u9"
		pod.Labels = map[string]string{"app": "billing"}

		return pod
	}

	t.Run("Targets reads per endpoint", func(t *testing.T) {
		f := baseline()
		f.addPod("payment-api-2", "u2", "10.0.0.6")
		f.addPod("payment-api-3", "u3", "10.0.0.7")
		_, c, _ := startFixture(t, baseOptions(), f.objects()...)
		reads := countPodReads(c)

		targets, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, PortSelection{})
		if err != nil || len(targets) != 3 {
			t.Fatalf("Targets() = %+v, %v, want three targets", targets, err)
		}
		if lists, gets := reads.lists.Load(), reads.gets.Load(); lists != 0 || gets != 3 {
			t.Fatalf("Targets() made %d namespace-wide List and %d Get calls, want 0 and 3: the listing that does not ask for counts does not pay for them", lists, gets)
		}
	})

	t.Run("Explain lists once", func(t *testing.T) {
		f := baseline()
		f.addPod("payment-api-2", "u2", "10.0.0.6")
		f.addPod("payment-api-3", "u3", "10.0.0.7")
		_, c, _ := startFixture(t, baseOptions(), append(f.objects(), otherPod())...)
		reads := countPodReads(c)

		got, err := c.Explain(context.Background(), fixtureNamespace, fixtureService, PortSelection{})
		if err != nil {
			t.Fatalf("Explain() error = %v, want nil", err)
		}
		if lists, gets := reads.lists.Load(), reads.gets.Load(); lists != 1 || gets != 0 {
			t.Fatalf("Explain() made %d namespace-wide List and %d Get calls, want 1 and 0: one captured list is the snapshot it counts and resolves against", lists, gets)
		}
		if got.SelectorMatched != 3 || len(got.Targets) != 3 || len(got.Excluded) != 0 {
			t.Fatalf("Explain() = %+v; the list is filtered by the selector, so a Pod of another Service is absent", got)
		}
	})

	t.Run("failed Pod read", func(t *testing.T) {
		f := baseline()
		_, c, _ := startFixture(t, baseOptions(), f.objects()...)
		reads := countPodReads(c)
		reads.listErr = errors.New("cache broken")

		if _, err := c.Explain(context.Background(), fixtureNamespace, fixtureService, PortSelection{}); err == nil {
			t.Fatal("Explain() error = nil; a failed Pod-cache read is an error, never an empty count")
		}
		targets, err := c.Targets(context.Background(), fixtureNamespace, fixtureService, PortSelection{})
		if err != nil || len(targets) != 1 {
			t.Fatalf("Targets() = %+v, %v; it never makes the namespace-wide read and answers as before", targets, err)
		}
	})
}
