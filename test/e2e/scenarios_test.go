//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	// testAppName is the Deployment and Service name in test/e2e/testapp/deployment.yaml.
	testAppName = "testapp"
	// testAppManifest is applied into every scenario namespace; paths resolve from the module root.
	testAppManifest = "test/e2e/testapp/deployment.yaml"
	// testAppLabel is the label the test-app Deployment and Service select on.
	testAppLabel = "app.kubernetes.io/name"
	// versionLabel is the Pod label the gateway reads Target.Version from.
	versionLabel = "app.kubernetes.io/version"
	// managedByLabel marks the EndpointSlices the scenarios create by hand.
	managedByLabel = "endpointslice.kubernetes.io/managed-by"
	managedBy      = "profgate-e2e"
	// serviceNameLabel ties an EndpointSlice to its Service.
	serviceNameLabel = "kubernetes.io/service-name"

	// convergenceDeadline bounds a convergence measurement, including the three stable polls.
	convergenceDeadline = 10 * time.Second
	// stablePolls is how many consecutive polls must return the expected set.
	stablePolls = 3
	// eligibilityDeadline bounds how long a Pod may stay in targets after it stops being eligible.
	eligibilityDeadline = 10 * time.Second
	// settleDeadline bounds how long the gateways may take to reflect a freshly applied object.
	settleDeadline = 30 * time.Second
	// reconnectDeadline bounds the gateway's recovery after the API outage ends.
	reconnectDeadline = 30 * time.Second
	// crashDeadline bounds the wait for a reduced-RBAC gateway to reach CrashLoopBackOff.
	crashDeadline = 2 * time.Minute
	// versionRequests is how many selections the version filter is proven over.
	versionRequests = 100
	// wrongAddress is an address no Pod in a kind cluster holds.
	wrongAddress = "10.255.255.255"
	// testAppPortNumber is testAppPort as an int32 for manifests.
	testAppPortNumber = 6060
)

// profiles lists every profile the gateway serves; trace is the one that is not a pprof proto.
var profiles = [...]string{"cpu", "trace", "heap", "allocs", "goroutine", "mutex", "block", "threadcreate"}

// target mirrors the targets endpoint's entry: what the gateway says about a backend.
type target struct {
	Pod     string `json:"pod"`
	Node    string `json:"node"`
	Version string `json:"version"`
}

// targetsResponse mirrors the targets endpoint's body.
type targetsResponse struct {
	Namespace string   `json:"namespace"`
	Service   string   `json:"service"`
	Targets   []target `json:"targets"`
}

// errorResponse mirrors the gateway's error envelope.
type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// gatewayURL builds an API path; the harness clients reach the API listener for any host.
func gatewayURL(ns, service, tail string) string {
	return fmt.Sprintf("http://gateway/v1/namespaces/%s/services/%s/%s", ns, service, tail)
}

// response is a finished HTTP exchange: the body has been read and closed.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

// get performs one GET and returns the response with its body read and closed.
func get(t *testing.T, c *http.Client, rawURL string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", rawURL, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read %s: %v", rawURL, err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: body}
}

// post performs one POST with no body and fails unless the status is 2xx.
func post(t *testing.T, rawURL string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, rawURL, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", rawURL, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", rawURL, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("POST %s: status %d", rawURL, resp.StatusCode)
	}
}

// expectError asserts a gateway-generated error with the given status and code and returns its body.
func expectError(t *testing.T, c *http.Client, rawURL string, status int, code string) []byte {
	t.Helper()
	resp := get(t, c, rawURL)
	var e errorResponse
	if err := json.Unmarshal(resp.Body, &e); err != nil {
		t.Fatalf("GET %s: status %d, body %q is not an error envelope: %v", rawURL, resp.Status, resp.Body, err)
	}
	if resp.Status != status || e.Code != code {
		t.Fatalf("GET %s: got %d %s, want %d %s", rawURL, resp.Status, e.Code, status, code)
	}
	return resp.Body
}

// expectErrorSoon is expectError for an object the gateway's cache may not hold yet:
// it polls until the status changes from 404 service_not_found, then asserts.
func expectErrorSoon(t *testing.T, c *http.Client, rawURL string, status int, code string) {
	t.Helper()
	err := poll(t.Context(), settleDeadline, func(_ context.Context) (bool, error) {
		resp := get(t, c, rawURL)
		var e errorResponse
		_ = json.Unmarshal(resp.Body, &e)
		return resp.Status != http.StatusNotFound || e.Code != "service_not_found", nil
	})
	if err != nil {
		t.Fatalf("GET %s never left service_not_found: %v", rawURL, err)
	}
	expectError(t, c, rawURL, status, code)
}

// targetNames lists the Pod names the gateway behind c reports for ns/service, sorted.
func targetNames(t *testing.T, c *http.Client, ns, service string) []string {
	t.Helper()
	names, err := tryTargetNames(t.Context(), c, ns, service)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// tryTargetNames is targetNames for callers that poll and want the error instead of a failure.
func tryTargetNames(ctx context.Context, c *http.Client, ns, service string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayURL(ns, service, "targets"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("targets %s/%s: %w", ns, service, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("targets %s/%s: %w", ns, service, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("targets %s/%s: status %d: %s", ns, service, resp.StatusCode, body)
	}
	var tr targetsResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("targets %s/%s: %w", ns, service, err)
	}
	names := make([]string, 0, len(tr.Targets))
	for _, tg := range tr.Targets {
		names = append(names, tg.Pod)
	}
	slices.Sort(names)
	return names, nil
}

// waitTargets polls until every gateway reports exactly want for ns/service,
// which is how a scenario knows the informers have caught up with what it applied.
func waitTargets(t *testing.T, h *Harness, ns, service string, want []string) {
	t.Helper()
	want = slices.Clone(want)
	slices.Sort(want)
	var last [gatewayReplicas][]string
	var lastErr error
	err := poll(t.Context(), settleDeadline, func(ctx context.Context) (bool, error) {
		for i, c := range h.Gateways {
			names, err := tryTargetNames(ctx, c, ns, service)
			if err != nil {
				// A Service the gateway's own watch has not delivered yet
				// answers 404, and a Service just created is exactly what this
				// wait is for: the convergence this measures is the informer
				// catching up, so an answer that is not the wanted one keeps
				// the wait going rather than ending it.
				lastErr = err

				return false, nil
			}
			last[i] = names
		}
		return slices.Equal(last[0], want) && slices.Equal(last[1], want), nil
	})
	if err != nil {
		t.Fatalf("gateways never reported targets %v for %s/%s: %v (last %v, last error %v)",
			want, ns, service, err, last, lastErr)
	}
}

// deployTestApp applies the test app into ns, waits for its rollout, and waits until both gateways list its Pods;
// it returns the ready Pods sorted by name.
func deployTestApp(t *testing.T, h *Harness, ns string) []corev1.Pod {
	t.Helper()
	ctx := t.Context()
	if err := h.kubectl(ctx, "apply", "-n", ns, "-f", testAppManifest); err != nil {
		t.Fatal(err)
	}
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+testAppName, "-n", ns, "--timeout="+podTimeout.String()); err != nil {
		t.Fatal(err)
	}
	pods := readyPods(t, h, ns, testAppLabel+"="+testAppName)
	waitTargets(t, h, ns, testAppName, podNames(pods))
	return pods
}

// readyPods lists the ready, non-terminating Pods matching selector, sorted by name.
func readyPods(t *testing.T, h *Harness, ns, selector string) []corev1.Pod {
	t.Helper()
	list, err := h.Client.CoreV1().Pods(ns).List(t.Context(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("list pods %s: %v", selector, err)
	}
	var ready []corev1.Pod
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil && podReady(&p) {
			ready = append(ready, p)
		}
	}
	slices.SortFunc(ready, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	return ready
}

func podNames(pods []corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	return names
}

// controllerSlice returns the EndpointSlice the endpoint controller manages for ns/service.
func controllerSlice(t *testing.T, h *Harness, ns, service string) *discoveryv1.EndpointSlice {
	t.Helper()
	list, err := h.Client.DiscoveryV1().EndpointSlices(ns).List(t.Context(), metav1.ListOptions{LabelSelector: serviceNameLabel + "=" + service})
	if err != nil {
		t.Fatalf("list endpointslices of %s/%s: %v", ns, service, err)
	}
	for i := range list.Items {
		if list.Items[i].Labels[managedByLabel] == "endpointslice-controller.k8s.io" {
			return &list.Items[i]
		}
	}
	t.Fatalf("no controller-managed EndpointSlice for %s/%s", ns, service)
	return nil
}

// manualSlice builds a manually managed EndpointSlice for service with one endpoint per Pod.
// address, when set, replaces every Pod's real address, which is how a scenario lies about one.
func manualSlice(name, ns, service, address string, pods ...corev1.Pod) *discoveryv1.EndpointSlice {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{managedByLabel: managedBy, serviceNameLabel: service},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{{
			Name:     ptr("pprof"),
			Port:     ptr(int32(testAppPortNumber)),
			Protocol: ptr(corev1.ProtocolTCP),
		}},
	}
	for _, p := range pods {
		addr := p.Status.PodIP
		if address != "" {
			addr = address
		}
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{addr},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
			NodeName:   ptr(p.Spec.NodeName),
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: p.Name, UID: p.UID},
		})
	}
	return slice
}

func ptr[T any](v T) *T { return &v }

// createSlice creates slice and fails the test if that does not work.
func createSlice(t *testing.T, h *Harness, slice *discoveryv1.EndpointSlice) {
	t.Helper()
	if _, err := h.Client.DiscoveryV1().EndpointSlices(slice.Namespace).Create(t.Context(), slice, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create endpointslice %s: %v", slice.Name, err)
	}
}

// testAppDeployment builds a Deployment of the test app named name whose Pods carry the app label value app and,
// when version is not empty, the version label.
// It mirrors test/e2e/testapp/deployment.yaml for scenarios that need more than one Deployment.
func testAppDeployment(name, ns, app, version string) *appsv1.Deployment {
	labels := map[string]string{testAppLabel: app}
	podLabels := map[string]string{testAppLabel: app, "profgate-e2e/deployment": name}
	if version != "" {
		podLabels[versionLabel] = version
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"profgate-e2e/deployment": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr(int64(60)),
					Containers: []corev1.Container{{
						Name:  testAppName,
						Image: testAppImage,
						Ports: []corev1.ContainerPort{{Name: "pprof", ContainerPort: testAppPortNumber, Protocol: corev1.ProtocolTCP}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("pprof")}},
							PeriodSeconds:    1,
							FailureThreshold: 1,
						},
						Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"/ko-app/testapp", "sleep", "30"}}}},
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             ptr(true),
							AllowPrivilegeEscalation: ptr(false),
							ReadOnlyRootFilesystem:   ptr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

// testAppService builds a Service named name selecting Pods with the app label value app.
// An empty app makes the Service selectorless.
func testAppService(name, ns, app string, publishNotReady bool) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Ports:                    []corev1.ServicePort{{Name: "pprof", Port: testAppPortNumber, TargetPort: intstr.FromString("pprof"), Protocol: corev1.ProtocolTCP}},
			PublishNotReadyAddresses: publishNotReady,
		},
	}
	if app != "" {
		svc.Spec.Selector = map[string]string{testAppLabel: app}
	}
	return svc
}

func createDeployment(t *testing.T, h *Harness, d *appsv1.Deployment) {
	t.Helper()
	if _, err := h.Client.AppsV1().Deployments(d.Namespace).Create(t.Context(), d, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment %s: %v", d.Name, err)
	}
}

func createService(t *testing.T, h *Harness, s *corev1.Service) {
	t.Helper()
	if _, err := h.Client.CoreV1().Services(s.Namespace).Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service %s: %v", s.Name, err)
	}
}

// waitDeploymentReady waits for every replica of the named Deployment and returns its ready Pods.
func waitDeploymentReady(t *testing.T, h *Harness, ns, name string) []corev1.Pod {
	t.Helper()
	if err := h.kubectl(t.Context(), "rollout", "status", "deployment/"+name, "-n", ns, "--timeout="+podTimeout.String()); err != nil {
		t.Fatal(err)
	}
	return readyPods(t, h, ns, "profgate-e2e/deployment="+name)
}

// labelPod adds labels to a Pod so a Service can select it alone.
func labelPod(t *testing.T, h *Harness, ns, name string, labels map[string]string) {
	t.Helper()
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if _, err := h.Client.CoreV1().Pods(ns).Patch(t.Context(), name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("label pod %s: %v", name, err)
	}
}

// deletePod deletes a Pod with the default grace period, so it stays Terminating through its preStop sleep.
func deletePod(t *testing.T, h *Harness, ns, name string) {
	t.Helper()
	if err := h.Client.CoreV1().Pods(ns).Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod %s: %v", name, err)
	}
}

// orphanPods deletes the test-app Deployment and its ReplicaSets without their Pods,
// so a deleted Pod is not replaced and an expected target set stays stable.
func orphanPods(t *testing.T, h *Harness, ns string) {
	t.Helper()
	ctx := t.Context()
	orphan := metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationOrphan)}
	if err := h.Client.AppsV1().Deployments(ns).Delete(ctx, testAppName, orphan); err != nil {
		t.Fatalf("orphan deployment: %v", err)
	}
	if err := h.Client.AppsV1().ReplicaSets(ns).DeleteCollection(ctx, orphan, metav1.ListOptions{LabelSelector: testAppLabel + "=" + testAppName}); err != nil {
		t.Fatalf("orphan replicasets: %v", err)
	}
	err := poll(ctx, podTimeout, func(ctx context.Context) (bool, error) {
		list, err := h.Client.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{LabelSelector: testAppLabel + "=" + testAppName})
		if err != nil {
			return false, err
		}
		return len(list.Items) == 0, nil
	})
	if err != nil {
		t.Fatalf("replicasets still present: %v", err)
	}
}

// converge polls both gateways every pollInterval
// until each has returned exactly want stablePolls times in a row, and fails
// unless that happens within convergenceDeadline of start.
func converge(t *testing.T, h *Harness, ns, service string, want []string, start time.Time) {
	t.Helper()
	want = slices.Clone(want)
	slices.Sort(want)
	ctx, cancel := context.WithDeadline(t.Context(), start.Add(convergenceDeadline))
	defer cancel()
	var stable [gatewayReplicas]int
	for {
		for i, c := range h.Gateways {
			names, err := tryTargetNames(ctx, c, ns, service)
			if err != nil {
				t.Fatalf("gateway %d did not converge on %v within %s: %v", i, want, convergenceDeadline, err)
			}
			if slices.Equal(names, want) {
				stable[i]++
			} else {
				stable[i] = 0
			}
		}
		if stable[0] >= stablePolls && stable[1] >= stablePolls {
			t.Logf("both gateways converged on %v in %s", want, time.Since(start).Round(time.Millisecond))
			return
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			t.Fatalf("gateways did not converge on %v within %s (stable polls %v)", want, convergenceDeadline, stable)
		}
	}
}

// waitPodGoneFromTargets polls until no gateway lists pod for ns/service.
func waitPodGoneFromTargets(t *testing.T, h *Harness, ns, service, pod string, within time.Duration) {
	t.Helper()
	err := poll(t.Context(), within, func(ctx context.Context) (bool, error) {
		for _, c := range h.Gateways {
			names, err := tryTargetNames(ctx, c, ns, service)
			if err != nil {
				return false, err
			}
			if slices.Contains(names, pod) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("pod %s still a target of %s/%s after %s: %v", pod, ns, service, within, err)
	}
}

// watchUntil drains events until match returns true for a Pod named pod, and returns when it did.
func watchUntil(t *testing.T, events <-chan watch.Event, pod string, match func(*corev1.Pod, watch.EventType) bool) time.Time {
	t.Helper()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("pod watch closed before %s matched", pod)
			}
			p, isPod := ev.Object.(*corev1.Pod)
			if !isPod || p.Name != pod {
				continue
			}
			if match(p, ev.Type) {
				return time.Now()
			}
		case <-t.Context().Done():
			t.Fatalf("pod watch: %v", t.Context().Err())
		}
	}
}

// scenarioDedupe proves rules 7 and deduplication of the eligibility algorithm on a real Service.
func scenarioDedupe(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	controlled := controllerSlice(t, h, ns, testAppName)

	// A manual copy of the controller's endpoints: every Pod is now listed twice.
	dup := manualSlice("testapp-duplicate", ns, testAppName, "", pods...)
	dup.AddressType = controlled.AddressType
	createSlice(t, h, dup)
	// A slice that names the real Pods at an address none of them holds.
	createSlice(t, h, manualSlice("testapp-wrong-address", ns, testAppName, wrongAddress, pods...))

	// The gateways give no signal for "I have seen the new slices",
	// so the assertion holds over repeated polls long enough for an informer to deliver them.
	want := podNames(pods)
	for i := 0; i < 6; i++ {
		for g, c := range h.Gateways {
			if got := targetNames(t, c, ns, testAppName); !slices.Equal(got, want) {
				t.Fatalf("gateway %d targets %v, want each Pod exactly once: %v", g, got, want)
			}
		}
		time.Sleep(pollInterval)
	}
	for g, c := range h.Gateways {
		resp := get(t, c, gatewayURL(ns, testAppName, "profiles/heap"))
		if resp.Status != http.StatusOK {
			t.Fatalf("gateway %d heap: status %d: %s", g, resp.Status, resp.Body)
		}
		if _, err := profile.ParseData(resp.Body); err != nil {
			t.Fatalf("gateway %d heap does not parse: %v", g, err)
		}
	}
}

// scenarioIneligiblePods proves that not-ready, terminating, and publishNotReadyAddresses Pods are never targets.
func scenarioIneligiblePods(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	failing, terminating := pods[0], pods[1]

	// Not ready: the readiness probe fails and the Pod leaves targets.
	app := h.ForwardTestApp(t, ns, failing.Name)
	post(t, app+"/healthz/fail")
	waitPodGoneFromTargets(t, h, ns, testAppName, failing.Name, eligibilityDeadline)

	// Terminating: the preStop sleep keeps the Pod alive while targets must already exclude it.
	deletePod(t, h, ns, terminating.Name)
	waitPodGoneFromTargets(t, h, ns, testAppName, terminating.Name, eligibilityDeadline)
	p, err := h.Client.CoreV1().Pods(ns).Get(t.Context(), terminating.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", terminating.Name, err)
	}
	if p.DeletionTimestamp == nil || !podReady(p) {
		t.Fatalf("%s should still be a ready, Terminating Pod while excluded (deletionTimestamp=%v ready=%v)", p.Name, p.DeletionTimestamp, podReady(p))
	}

	// publishNotReadyAddresses: the slice lists the failing Pod; the gateway must not.
	labelPod(t, h, ns, failing.Name, map[string]string{testAppLabel + "-notready": "true"})
	svc := testAppService("testapp-notready", ns, "", true)
	svc.Spec.Selector = map[string]string{testAppLabel + "-notready": "true"}
	createService(t, h, svc)
	err = poll(t.Context(), settleDeadline, func(ctx context.Context) (bool, error) {
		list, err := h.Client.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{LabelSelector: serviceNameLabel + "=" + svc.Name})
		if err != nil {
			return false, err
		}
		for _, s := range list.Items {
			for _, e := range s.Endpoints {
				if e.TargetRef != nil && e.TargetRef.Name == failing.Name {
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("the publishNotReadyAddresses Service never listed %s: %v", failing.Name, err)
	}
	for i := 0; i < 6; i++ {
		for g, c := range h.Gateways {
			if got := targetNames(t, c, ns, svc.Name); len(got) != 0 {
				t.Fatalf("gateway %d lists %v for a publishNotReadyAddresses Service over a failing Pod", g, got)
			}
		}
		time.Sleep(pollInterval)
	}
}

// scenarioConvergenceOnDelete proves both replicas drop a deleted Pod within the deadline.
func scenarioConvergenceOnDelete(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	orphanPods(t, h, ns)
	victim := pods[0]
	expected := podNames(pods[1:])

	events := h.WatchPods(t, ns)
	deletePod(t, h, ns, victim.Name)
	// The clock starts when the watch shows the deletion: the deletionTimestamp is what the gateway reacts to,
	// and the DELETED event follows
	// once the preStop sleep lets the Pod go.
	start := watchUntil(t, events, victim.Name, func(p *corev1.Pod, typ watch.EventType) bool {
		return typ == watch.Deleted || p.DeletionTimestamp != nil
	})
	converge(t, h, ns, testAppName, expected, start)
}

// scenarioConvergenceOnReady proves both replicas add a Pod that becomes ready within the deadline.
func scenarioConvergenceOnReady(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	flipped := pods[0]
	app := h.ForwardTestApp(t, ns, flipped.Name)

	post(t, app+"/healthz/fail")
	waitPodGoneFromTargets(t, h, ns, testAppName, flipped.Name, eligibilityDeadline)
	expected := podNames(pods)

	events := h.WatchPods(t, ns)
	post(t, app+"/healthz/pass")
	start := watchUntil(t, events, flipped.Name, func(p *corev1.Pod, _ watch.EventType) bool { return podReady(p) })
	converge(t, h, ns, testAppName, expected, start)
}

// scenarioProfilesParse proves every profile fetched through the gateway is what pprof expects.
func scenarioProfilesParse(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	deployTestApp(t, h, ns)
	c := h.Gateways[0]

	for _, name := range profiles {
		tail := "profiles/" + name
		if name == "cpu" || name == "trace" {
			tail += "?seconds=2"
		}
		started := time.Now()
		resp := get(t, c, gatewayURL(ns, testAppName, tail))
		took := time.Since(started)
		if resp.Status != http.StatusOK {
			t.Fatalf("%s: status %d: %s", name, resp.Status, resp.Body)
		}
		for _, hdr := range []string{"X-Pprof-Target-Pod", "X-Pprof-Target-Node", "X-Pprof-Target-Version"} {
			if _, ok := resp.Header[hdr]; !ok {
				t.Fatalf("%s: response lacks %s", name, hdr)
			}
		}
		if resp.Header.Get("X-Pprof-Target-Pod") == "" {
			t.Fatalf("%s: empty X-Pprof-Target-Pod", name)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("%s: Cache-Control %q, want no-store", name, cc)
		}
		switch name {
		case "trace":
			if !bytes.HasPrefix(resp.Body, []byte("go 1.")) {
				t.Fatalf("trace body does not start with \"go 1.\": %q", truncate(resp.Body, 16))
			}
		default:
			if _, err := profile.ParseData(resp.Body); err != nil {
				t.Fatalf("%s: profile.Parse: %v", name, err)
			}
		}
		if name == "cpu" && (took < 2*time.Second || took > 5*time.Second) {
			t.Fatalf("cpu?seconds=2 took %s, want between 2s and 5s", took)
		}
		t.Logf("%s: %d bytes in %s", name, len(resp.Body), took.Round(time.Millisecond))
	}
}

func truncate(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// scenarioErrors proves the error contract against a gateway whose realm admits one namespace.
func scenarioErrors(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	c := deployScopedGateway(t, h, ns, "errors-gateway", "profgate-errors")

	other := createNamespace(t, h, ns+"-other")
	createService(t, h, testAppService(testAppName, other, testAppName, false))
	waitTargets(t, h, other, testAppName, nil)

	// Outside the realm: the same denial whether or not the Service exists.
	existing := expectError(t, c, gatewayURL(other, testAppName, "targets"), http.StatusForbidden, "realm_denied")
	missing := expectError(t, c, gatewayURL(other, "missing", "targets"), http.StatusForbidden, "realm_denied")
	if !bytes.Equal(existing, missing) {
		t.Fatalf("realm denial differs between an existing and a missing Service:\n%s\n%s", existing, missing)
	}

	expectError(t, c, gatewayURL(ns, "missing", "targets"), http.StatusNotFound, "service_not_found")

	// A selectorless Service with a manual slice over a real Pod is refused as a whole.
	createService(t, h, testAppService("manual", ns, "", false))
	createSlice(t, h, manualSlice("manual-1", ns, "manual", "", pods[0]))
	expectErrorSoon(t, c, gatewayURL(ns, "manual", "targets"), http.StatusUnprocessableEntity, "service_selectorless")

	// A Pod of another Service is not a target of this one, however real it is.
	createDeployment(t, h, testAppDeployment("otherapp", ns, "otherapp", ""))
	createService(t, h, testAppService("otherapp", ns, "otherapp", false))
	otherPods := waitDeploymentReady(t, h, ns, "otherapp")
	waitTargets(t, h, ns, "otherapp", podNames(otherPods))
	expectError(t, c, gatewayURL(ns, testAppName, "profiles/heap?pod="+otherPods[0].Name), http.StatusNotFound, "pod_not_found")
}

// createNamespace creates a labeled namespace that is deleted when the test ends.
func createNamespace(t *testing.T, h *Harness, name string) string {
	t.Helper()
	if err := h.createNamespace(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.deleteNamespace(context.Background(), name); err != nil {
			t.Errorf("delete namespace %s: %v", name, err)
		}
	})
	return name
}

// deployScopedGateway applies the overlay under test/e2e/overlays into ns with its realm limited to ns,
// waits for its Pod, and returns a client that reaches its API listener.
// name is the resource name every object in the overlay shares.
// The realm is fixed in the ConfigMap before the Pod starts, so no restart is needed.
func deployScopedGateway(t *testing.T, h *Harness, ns, overlay, name string) *http.Client {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	overlayDir := filepath.Join(h.root, "test", "e2e", "overlays", overlay)
	target, err := filepath.Rel(dir, overlayDir)
	if err != nil {
		t.Fatal(err)
	}
	configMap, err := os.ReadFile(filepath.Join(overlayDir, "configmap.yaml")) //nolint:gosec // the overlay path is composed from the module root
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(configMap), `namespaces: ["placeholder"]`, fmt.Sprintf("namespaces: [%q]", ns), 1)
	if patched == string(configMap) {
		t.Fatalf("%s configmap.yaml has no realm placeholder to patch", overlay)
	}
	if err := os.WriteFile(filepath.Join(dir, "configmap.yaml"), []byte(patched), 0o600); err != nil { //nolint:gosec // dir is the test's temporary directory
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: %s\nresources:\n  - %s\npatches:\n  - path: configmap.yaml\n", ns, target)
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(wrapper), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.kubectl(ctx, "apply", "-k", dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The namespace deletion takes the rest; the ClusterRoleBinding is cluster-scoped.
		err := h.Client.RbacV1().ClusterRoleBindings().Delete(context.Background(), name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete clusterrolebinding %s: %v", name, err)
		}
	})
	selector := "app.kubernetes.io/name=" + name
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+name, "-n", ns, "--timeout="+rolloutTimeout.String()); err != nil {
		_ = h.kubectl(ctx, "logs", "-n", ns, "-l", selector, "--tail=50")
		t.Fatal(err)
	}
	pods := readyPods(t, h, ns, selector)
	if len(pods) != 1 {
		t.Fatalf("%d ready %s pods, want 1", len(pods), name)
	}
	ports, stop, err := h.forward(ctx, ns, pods[0].Name, []string{"0:" + gatewayAPIPort})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0])))
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, local)
		},
	}}
	// The realm admits ns, so the test app must be visible before any negative assertion runs.
	err = poll(ctx, settleDeadline, func(ctx context.Context) (bool, error) {
		names, err := tryTargetNames(ctx, c, ns, testAppName)
		return err == nil && len(names) > 0, nil
	})
	if err != nil {
		t.Fatalf("%s gateway never listed the test app: %v", name, err)
	}
	return c
}

// scenarioVersionFilter proves ?version= selects only Pods carrying that label value.
func scenarioVersionFilter(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	const app = "multi"
	versions := map[string]string{"app-v1": "1.0.0", "app-v2": "2.0.0", "app-unlabeled": ""}
	for name, version := range versions {
		createDeployment(t, h, testAppDeployment(name, ns, app, version))
	}
	createService(t, h, testAppService(app, ns, app, false))
	var want []string
	for name := range versions {
		want = append(want, podNames(waitDeploymentReady(t, h, ns, name))...)
	}
	waitTargets(t, h, ns, app, want)

	for i := 0; i < versionRequests; i++ {
		c := h.Gateways[i%gatewayReplicas]
		resp := get(t, c, gatewayURL(ns, app, "profiles/heap?version=2.0.0"))
		if resp.Status != http.StatusOK {
			t.Fatalf("request %d: status %d: %s", i, resp.Status, resp.Body)
		}
		if v := resp.Header.Get("X-Pprof-Target-Version"); v != "2.0.0" {
			t.Fatalf("request %d: X-Pprof-Target-Version %q, want 2.0.0", i, v)
		}
	}
	expectError(t, h.Gateways[0], gatewayURL(ns, app, "profiles/heap?version=3.0.0"), http.StatusServiceUnavailable, "no_targets")
}

// scenarioRBAC proves the shipped ClusterRole is exactly enough: the main gateway runs, and a
// ClusterRole missing watch or get on Pods makes the gateway exit naming the denied tuple.
func scenarioRBAC(t *testing.T, h *Harness) {
	ctx := t.Context()
	if _, err := h.gatewayPods(ctx); err != nil {
		t.Fatalf("main gateway: %v", err)
	}

	variants := []struct{ overlay, name, verb string }{
		{"reduced-no-watch", "profgate-no-watch", "watch"},
		{"reduced-no-get", "profgate-no-get", "get"},
	}
	for _, v := range variants {
		h.Apply(t, gatewayNamespace, v.overlay)
		t.Cleanup(func() { deleteReducedGateway(t, h, v.name) })
	}
	for _, v := range variants {
		pod := waitCrashLoop(t, h, gatewayNamespace, testAppLabel+"="+v.name)
		logs := podLogs(t, h, gatewayNamespace, pod)
		want := `"resource":"pods"`
		verb := fmt.Sprintf(`"verb":%q`, v.verb)
		if !strings.Contains(logs, want) || !strings.Contains(logs, verb) {
			t.Fatalf("%s logs lack %s with %s:\n%s", v.name, want, verb, logs)
		}
	}

	if h.Lane.Name == "1.24" {
		secrets, err := h.Client.CoreV1().Secrets(gatewayNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list secrets: %v", err)
		}
		for _, s := range secrets.Items {
			if s.Type == corev1.SecretTypeServiceAccountToken && s.Annotations[corev1.ServiceAccountNameKey] == gatewayDeployment {
				t.Fatalf("secret %s is a token for the gateway ServiceAccount; the projected token alone should serve it", s.Name)
			}
		}
	}
}

// deleteReducedGateway removes everything a reduced overlay created, by name.
func deleteReducedGateway(t *testing.T, h *Harness, name string) {
	t.Helper()
	ctx := context.Background()
	opts := metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationBackground)}
	check := func(what string, err error) {
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete %s %s: %v", what, name, err)
		}
	}
	check("deployment", h.Client.AppsV1().Deployments(gatewayNamespace).Delete(ctx, name, opts))
	check("configmap", h.Client.CoreV1().ConfigMaps(gatewayNamespace).Delete(ctx, name, opts))
	check("serviceaccount", h.Client.CoreV1().ServiceAccounts(gatewayNamespace).Delete(ctx, name, opts))
	check("clusterrolebinding", h.Client.RbacV1().ClusterRoleBindings().Delete(ctx, name, opts))
	check("clusterrole", h.Client.RbacV1().ClusterRoles().Delete(ctx, name, opts))
}

// waitCrashLoop polls until a Pod matching selector is crash-looping and returns its name.
//
// Looping is read as: the container has been restarted more than once, its last
// run ended non-zero, and it is not sitting in a finished run right now —
// it is either running again or held back by the kubelet.
// The CrashLoopBackOff waiting reason alone is not something to wait for,
// because a container that lives a few seconds before it exits spends its whole
// backoff reported as terminated and never carries that reason.
// Waiting for a moment when the container is not terminated is also what the
// callers need: they read the previous run's log, and while the current run is
// itself finished that run is one further back than the node keeps.
func waitCrashLoop(t *testing.T, h *Harness, ns, selector string) string {
	t.Helper()
	var name string
	err := poll(t.Context(), crashDeadline, func(ctx context.Context) (bool, error) {
		list, err := h.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err
		}
		for _, p := range list.Items {
			for _, cs := range p.Status.ContainerStatuses {
				last := cs.LastTerminationState.Terminated
				if cs.RestartCount > 1 && last != nil && last.ExitCode != 0 && cs.State.Terminated == nil {
					name = p.Name
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		_ = h.kubectl(t.Context(), "get", "pods", "-n", ns, "-l", selector, "-o", "wide")
		t.Fatalf("no Pod matching %s crash-looped within %s: %v", selector, crashDeadline, err)
	}
	return name
}

// podLogs returns the logs of the last terminated run of the Pod's only container.
func podLogs(t *testing.T, h *Harness, ns, name string) string {
	t.Helper()
	out, err := h.Client.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{Previous: true}).DoRaw(t.Context())
	if err != nil {
		t.Fatalf("logs of %s: %v", name, err)
	}
	return string(out)
}

// scenarioReplicasAgree proves both replicas describe the same cluster.
func scenarioReplicasAgree(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	a := targetNames(t, h.Gateways[0], ns, testAppName)
	b := targetNames(t, h.Gateways[1], ns, testAppName)
	if !slices.Equal(a, b) {
		t.Fatalf("replicas disagree: %v vs %v", a, b)
	}
	for _, p := range pods {
		var headers [gatewayReplicas]http.Header
		for i, c := range h.Gateways {
			resp := get(t, c, gatewayURL(ns, testAppName, "profiles/heap?pod="+p.Name))
			if resp.Status != http.StatusOK {
				t.Fatalf("gateway %d heap?pod=%s: status %d: %s", i, p.Name, resp.Status, resp.Body)
			}
			headers[i] = targetHeaders(resp.Header)
		}
		if headers[0].Get("X-Pprof-Target-Pod") != p.Name {
			t.Fatalf("X-Pprof-Target-Pod %q, want %s", headers[0].Get("X-Pprof-Target-Pod"), p.Name)
		}
		if fmt.Sprint(headers[0]) != fmt.Sprint(headers[1]) {
			t.Fatalf("target headers differ for %s: %v vs %v", p.Name, headers[0], headers[1])
		}
	}
}

// targetHeaders keeps only the gateway's X-Pprof-Target-* headers.
func targetHeaders(hdr http.Header) http.Header {
	out := http.Header{}
	for k, v := range hdr {
		if strings.HasPrefix(k, "X-Pprof-Target-") {
			out[k] = v
		}
	}
	return out
}

// scenarioAPIOutage proves a gateway cut off from the API server keeps serving its cache,
// refuses to proxy, and recovers once the network policy is gone.
func scenarioAPIOutage(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	app := h.ForwardTestApp(t, ns, pods[0].Name)
	before := hits(t, app)
	cached := targetNames(t, h.Gateways[0], ns, testAppName)

	h.Apply(t, gatewayNamespace, "api-outage")
	severAPIConnections(t, h)
	applied := time.Now()
	policyGone := false
	deletePolicy := func() {
		if policyGone {
			return
		}
		policyGone = true
		err := h.Client.NetworkingV1().NetworkPolicies(gatewayNamespace).Delete(context.Background(), "profgate-api-outage", metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete api-outage policy: %v", err)
		}
	}
	t.Cleanup(deletePolicy)

	// The policy takes effect in the data plane a moment after the object exists: wait
	// until the first gateway's confirmation read fails before asserting on both.
	err := poll(t.Context(), settleDeadline, func(_ context.Context) (bool, error) {
		resp := get(t, h.Gateways[0], gatewayURL(ns, testAppName, "profiles/heap"))
		var e errorResponse
		_ = json.Unmarshal(resp.Body, &e)
		return resp.Status == http.StatusServiceUnavailable && e.Code == "discovery_unavailable", nil
	})
	if err != nil {
		t.Fatalf("the gateway kept proxying after the API outage began: %v", err)
	}
	t.Logf("outage felt %s after the policy was applied", time.Since(applied).Round(time.Millisecond))
	for i, c := range h.Gateways {
		if resp := get(t, c, "http://gateway:9090/readyz"); resp.Status != http.StatusOK {
			t.Fatalf("gateway %d /readyz during outage: %d %s", i, resp.Status, resp.Body)
		}
		if got := targetNames(t, c, ns, testAppName); !slices.Equal(got, cached) {
			t.Fatalf("gateway %d targets during outage %v, want cached %v", i, got, cached)
		}
		expectError(t, c, gatewayURL(ns, testAppName, "profiles/heap"), http.StatusServiceUnavailable, "discovery_unavailable")
	}
	if after := hits(t, app); after != before {
		t.Fatalf("test app saw %d profile requests during the outage (was %d)", after-before, before)
	}

	deletePolicy()
	lifted := time.Now()
	err = poll(t.Context(), reconnectDeadline, func(_ context.Context) (bool, error) {
		for _, c := range h.Gateways {
			resp := get(t, c, gatewayURL(ns, testAppName, "profiles/heap"))
			if resp.Status != http.StatusOK {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("profiles/heap did not recover within %s of the outage ending: %v", reconnectDeadline, err)
	}
	t.Logf("both gateways proxied again %s after the policy was deleted", time.Since(lifted).Round(time.Millisecond))
}

// severAPIConnections drops the node's conntrack entries for every gateway Pod.
// kind's NetworkPolicy enforcer accepts packets of a connection it already admitted,
// so a policy applied after the gateway connected to the API server would never be felt on that long-lived connection.
// A real outage severs it; this does the same, and the next packet is evaluated as a new flow against the policy.
func severAPIConnections(t *testing.T, h *Harness) {
	t.Helper()
	pods, err := h.gatewayPods(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pods {
		// conntrack exits non-zero when nothing matched, and a gateway Pod always has API flows.
		if err := h.run(t.Context(), nil, "docker", "exec", h.Cluster+"-control-plane", "conntrack", "-D", "--orig-src", p.Status.PodIP); err != nil {
			t.Fatalf("flush conntrack for %s: %v", p.Name, err)
		}
	}
}

// hitsResponse mirrors the test app's /hits body: the profile request total and the same count per listen address.
type hitsResponse struct {
	Pprof int64            `json:"pprof"`
	Hits  map[string]int64 `json:"hits"`
}

// readHits fetches and decodes the test app's /hits body.
func readHits(t *testing.T, app string) hitsResponse {
	t.Helper()
	resp := get(t, http.DefaultClient, app+"/hits")
	if resp.Status != http.StatusOK {
		t.Fatalf("/hits: status %d", resp.Status)
	}
	var counts hitsResponse
	if err := json.Unmarshal(resp.Body, &counts); err != nil {
		t.Fatalf("/hits: %v", err)
	}
	return counts
}

// hits reads the test app's profile request counter across every listener.
func hits(t *testing.T, app string) int64 {
	t.Helper()
	return readHits(t, app).Pprof
}

// listenerHits reads the test app's profile request counter for one listen address, such as ":6061".
func listenerHits(t *testing.T, app, addr string) int64 {
	t.Helper()
	return readHits(t, app).Hits[addr]
}

// altPort is the test app's second pprof listener, container port pprof-alt;
// it is not the configured default, so a request must name it.
const (
	altPort     = "6061"
	altPortName = "pprof-alt"
	altAddr     = ":" + altPort
)

// scenarioPortSelection proves that, against the default gateway whose allowlists are empty,
// a request naming the test app's second port by number or by name is served from that port
// and nothing the gateway generates carries the number.
func scenarioPortSelection(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	app := h.ForwardTestApp(t, ns, pods[0].Name)
	c := h.Gateways[0]

	before := listenerHits(t, app, altAddr)
	for _, query := range []string{"port=" + altPort, "portName=" + altPortName} {
		resp := get(t, c, gatewayURL(ns, testAppName, "profiles/heap?"+query+"&pod="+pods[0].Name))
		if resp.Status != http.StatusOK {
			t.Fatalf("heap?%s: status %d: %s", query, resp.Status, resp.Body)
		}
		if _, err := profile.ParseData(resp.Body); err != nil {
			t.Fatalf("heap?%s: profile.Parse: %v", query, err)
		}
		for name, values := range resp.Header {
			if !strings.HasPrefix(name, "X-Pprof-Target-") {
				continue
			}
			for _, v := range values {
				if strings.Contains(v, altPort) {
					t.Fatalf("heap?%s: header %s %q carries the port", query, name, v)
				}
			}
		}
		after := listenerHits(t, app, altAddr)
		if after != before+1 {
			t.Fatalf("heap?%s: %s hits went %d -> %d, want one more", query, altAddr, before, after)
		}
		before = after
	}

	resp := get(t, c, gatewayURL(ns, testAppName, "targets?portName="+altPortName))
	if resp.Status != http.StatusOK {
		t.Fatalf("targets?portName=%s: status %d: %s", altPortName, resp.Status, resp.Body)
	}
	var tr targetsResponse
	if err := json.Unmarshal(resp.Body, &tr); err != nil {
		t.Fatalf("targets?portName=%s: %v", altPortName, err)
	}
	var got []string
	for _, tg := range tr.Targets {
		got = append(got, tg.Pod)
	}
	slices.Sort(got)
	if want := podNames(pods); !slices.Equal(got, want) {
		t.Fatalf("targets?portName=%s = %v, want %v", altPortName, got, want)
	}
}

// scenarioPortSelectionRefused proves that a gateway whose allowlists exclude the test app's
// second port and name refuses both before discovery, so the test app never sees the request,
// while the configured default still passes.
func scenarioPortSelectionRefused(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	pods := deployTestApp(t, h, ns)
	c := deployScopedGateway(t, h, ns, "ports-gateway", "profgate-ports")
	app := h.ForwardTestApp(t, ns, pods[0].Name)

	before := listenerHits(t, app, altAddr)
	for _, tc := range []struct{ query, sent string }{
		{query: "port=" + altPort, sent: altPort},
		{query: "portName=" + altPortName, sent: altPortName},
	} {
		body := expectError(t, c, gatewayURL(ns, testAppName, "profiles/heap?"+tc.query), http.StatusBadRequest, "port_not_allowed")
		if !bytes.Contains(body, []byte(tc.sent)) {
			t.Fatalf("heap?%s: body %s does not name %q", tc.query, body, tc.sent)
		}
	}
	if after := listenerHits(t, app, altAddr); after != before {
		t.Fatalf("%s hits went %d -> %d after refused requests", altAddr, before, after)
	}

	resp := get(t, c, gatewayURL(ns, testAppName, "profiles/heap?port="+testAppPort))
	if resp.Status != http.StatusOK {
		t.Fatalf("heap?port=%s: status %d: %s", testAppPort, resp.Status, resp.Body)
	}
}
