//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const (
	// gatewayNamespace is where TestMain deploys the shared gateway.
	gatewayNamespace = "profgate"
	// gatewayDeployment is the shipped Deployment's name.
	gatewayDeployment = "profgate"
	// gatewaySelector selects the shared gateway Pods.
	gatewaySelector = "app.kubernetes.io/name=profgate"
	// gatewayReplicas is how many gateway Pods the shipped base runs; the harness port-forwards to each.
	gatewayReplicas = 2
	// gatewayImage and testAppImage are the references ko builds and kind loads.
	gatewayImage = "ko.local/profgate:e2e"
	testAppImage = "ko.local/testapp:e2e"
	// testAppNamespaceLabel marks every namespace the harness creates,
	// so the api-outage NetworkPolicy can allow egress to test apps without knowing their names.
	testAppNamespaceLabel = "profgate-e2e/test-app"

	gatewayAPIPort = "8080"
	gatewayOpsPort = "9090"
	testAppPort    = "6060"

	// rolloutTimeout bounds the wait for the gateway Deployment, including image start on a cold node.
	rolloutTimeout = 3 * time.Minute
	// podTimeout bounds WaitPodReady and WaitPodGone.
	podTimeout = 2 * time.Minute
	// pollInterval is how often the waits re-read a Pod.
	pollInterval = 500 * time.Millisecond
	// namespaceMaxLength is the Kubernetes limit on a namespace name.
	namespaceMaxLength = 63
)

// Harness is the running cluster a scenario works against.
type Harness struct {
	Lane     Lane
	Cluster  string               // kind cluster name
	Client   kubernetes.Interface // tester kubeconfig
	Gateways [2]*http.Client      // through standing port-forwards opened in TestMain
	scenario *Scenario            // set by TestScenarios before Run

	root       string // module root, where ko and kustomize paths resolve
	kubeconfig string // the tester's kubeconfig, written by kind
	restConfig *rest.Config
	log        *slog.Logger
}

// harness is the one test-only package variable: TestMain fills it, TestScenarios reads it.
// It is the named exception to the global-state rule for the e2e package.
var harness *Harness

// runners returns the implementation for every scenario name.
// It is a function, not package state.
func runners() map[string]func(t *testing.T, h *Harness) {
	return map[string]func(t *testing.T, h *Harness){
		"dedupe and wrong-address slice": scenarioDedupe,
		"ineligible pods":                scenarioIneligiblePods,
		"convergence on delete":          scenarioConvergenceOnDelete,
		"convergence on ready":           scenarioConvergenceOnReady,
		"profiles parse":                 scenarioProfilesParse,
		"errors":                         scenarioErrors,
		"version filter":                 scenarioVersionFilter,
		"rbac":                           scenarioRBAC,
		"replicas agree":                 scenarioReplicasAgree,
		"api outage":                     scenarioAPIOutage,
	}
}

func TestMain(m *testing.M) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx := context.Background()
	lane, err := laneFromEnv()
	if err != nil {
		logger.Error("lane", "error", err)
		os.Exit(1)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		logger.Error("module root", "error", err)
		os.Exit(1)
	}
	h := &Harness{Lane: lane, Cluster: "profgate-" + lane.Name, root: root, log: logger}
	keep := os.Getenv("PROFGATE_E2E_KEEP") != ""

	fail := func(step string, err error) {
		logger.Error(step, "error", err)
		os.Exit(1)
	}
	exists, matches, err := clusterState(ctx, h)
	if err != nil {
		fail("inspect cluster", err)
	}
	reused := keep && exists && matches
	if exists && !reused {
		logger.Info("deleting cluster", "cluster", h.Cluster, "reason", "not kept or image mismatch")
		if err := h.kind(ctx, "delete", "cluster", "--name", h.Cluster); err != nil {
			fail("kind delete", err)
		}
	}
	if !reused {
		logger.Info("creating cluster", "cluster", h.Cluster, "kind", lane.Kind, "image", registry()+"/"+lane.Image)
		if err := h.kind(ctx, "create", "cluster", "--name", h.Cluster, "--image", registry()+"/"+lane.Image); err != nil {
			fail("kind create", err)
		}
	} else {
		logger.Info("reusing cluster", "cluster", h.Cluster)
	}

	if err := h.buildImages(ctx); err != nil {
		fail("ko build", err)
	}
	if err := h.kind(ctx, "load", "docker-image", "--name", h.Cluster, gatewayImage, testAppImage); err != nil {
		fail("kind load", err)
	}

	kubeconfigDir, err := os.MkdirTemp("", "profgate-e2e-")
	if err != nil {
		fail("kubeconfig dir", err)
	}
	if err := h.connect(ctx, filepath.Join(kubeconfigDir, "kubeconfig")); err != nil {
		fail("connect", err)
	}
	if err := h.deployGateway(ctx); err != nil {
		h.dumpGateway(ctx)
		fail("deploy gateway", err)
	}
	stopForwards, err := h.forwardGateways(ctx)
	if err != nil {
		fail("port-forward gateways", err)
	}

	harness = h
	code := m.Run()

	stopForwards()
	_ = os.RemoveAll(kubeconfigDir)
	if !keep {
		if err := h.kind(ctx, "delete", "cluster", "--name", h.Cluster); err != nil {
			logger.Error("kind delete", "error", err)
		}
	}
	os.Exit(code)
}

func TestScenarios(t *testing.T) {
	rs := runners()
	for _, s := range Scenarios() {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			run, ok := rs[s.Name]
			if !ok {
				t.Fatalf("scenario %q has no runner", s.Name)
			}
			if skip, why := s.Skips(harness.Lane); skip {
				t.Log(why)
				t.Skip(why)
			}
			harness.scenario = &s
			run(t, harness)
		})
	}
}

// laneFromEnv returns the lane named by PROFGATE_E2E_LANE, or "current" when unset.
func laneFromEnv() (Lane, error) {
	name := os.Getenv("PROFGATE_E2E_LANE")
	if name == "" {
		name = "current"
	}
	lanes, err := LoadLanes("versions.yaml")
	if err != nil {
		return Lane{}, err
	}
	for _, l := range lanes {
		if l.Name == name {
			return l, nil
		}
	}
	return Lane{}, fmt.Errorf("PROFGATE_E2E_LANE=%q is not one of %v", name, LaneNames(lanes, false))
}

// registry is the prefix the lane image is pulled from: PROFGATE_E2E_REGISTRY
// (for example "ghcr.io/arloliu"), or "docker.io" by default.
func registry() string {
	if r := os.Getenv("PROFGATE_E2E_REGISTRY"); r != "" {
		return r
	}
	return "docker.io"
}

// clusterState reports whether a kind cluster with the harness's name exists and
// whether its control-plane node runs the lane's image digest.
func clusterState(ctx context.Context, h *Harness) (exists, matches bool, err error) {
	out, err := h.output(ctx, "mise", "x", "kind@"+h.Lane.Kind, "--", "kind", "get", "clusters")
	if err != nil {
		return false, false, err
	}
	for _, name := range strings.Fields(out) {
		if name == h.Cluster {
			exists = true
		}
	}
	if !exists {
		return false, false, nil
	}
	image, err := h.output(ctx, "docker", "inspect", "--format", "{{.Config.Image}}", h.Cluster+"-control-plane")
	if err != nil {
		return true, false, nil //nolint:nilerr // a node container that cannot be inspected is a mismatch, not a failure
	}
	_, digest, _ := strings.Cut(h.Lane.Image, "@")
	return true, strings.Contains(image, digest), nil
}

// buildImages builds the gateway and the test app into the local Docker daemon.
// With --bare ko names the image exactly KO_DOCKER_REPO plus the tag,
// so each build sets the repository to the reference kind will load.
func (h *Harness) buildImages(ctx context.Context) error {
	builds := []struct{ repo, importPath string }{
		{"ko.local/profgate", "./cmd/profgate"},
		{"ko.local/testapp", "./test/e2e/testapp"},
	}
	for _, b := range builds {
		env := []string{"KO_DOCKER_REPO=" + b.repo, "VERSION=e2e"}
		if err := h.run(ctx, env, "ko", "build", "--local", "--bare", "--tags", "e2e", b.importPath); err != nil {
			return fmt.Errorf("build %s: %w", b.importPath, err)
		}
	}
	return nil
}

// connect writes the cluster's kubeconfig to path and builds the tester's client from it.
func (h *Harness) connect(ctx context.Context, path string) error {
	kubeconfig, err := h.output(ctx, "mise", "x", "kind@"+h.Lane.Kind, "--", "kind", "get", "kubeconfig", "--name", h.Cluster)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	h.kubeconfig = path
	h.restConfig, err = clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	h.Client, err = kubernetes.NewForConfig(h.restConfig)
	if err != nil {
		return fmt.Errorf("clientset: %w", err)
	}
	return nil
}

// deployGateway applies the default overlay into the gateway namespace and waits for the rollout.
// A Deployment that already existed gets a restart so its Pods run the image just loaded,
// since re-applying the same tag changes nothing the Deployment notices.
func (h *Harness) deployGateway(ctx context.Context) error {
	if err := h.createNamespace(ctx, gatewayNamespace); err != nil {
		return err
	}
	_, err := h.Client.AppsV1().Deployments(gatewayNamespace).Get(ctx, gatewayDeployment, metav1.GetOptions{})
	existed := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get deployment %s: %w", gatewayDeployment, err)
	}
	if err := h.apply(ctx, gatewayNamespace, "default"); err != nil {
		return err
	}
	if existed {
		if err := h.kubectl(ctx, "rollout", "restart", "deployment/"+gatewayDeployment, "-n", gatewayNamespace); err != nil {
			return err
		}
	}
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+gatewayDeployment, "-n", gatewayNamespace,
		"--timeout="+rolloutTimeout.String()); err != nil {
		return err
	}
	// The rollout completes while replaced Pods are still terminating; wait until only the new ones remain.
	return poll(ctx, rolloutTimeout, func(ctx context.Context) (bool, error) {
		_, err := h.gatewayPods(ctx)
		return err == nil, nil
	})
}

// dumpGateway prints the gateway Pods and their logs so a failed deployment is diagnosable.
func (h *Harness) dumpGateway(ctx context.Context) {
	_ = h.kubectl(ctx, "get", "pods", "-n", gatewayNamespace, "-o", "wide")
	_ = h.kubectl(ctx, "describe", "pods", "-n", gatewayNamespace, "-l", gatewaySelector)
	_ = h.kubectl(ctx, "logs", "-n", gatewayNamespace, "-l", gatewaySelector, "--tail=50", "--prefix")
}

// gatewayPods returns the ready, non-terminating gateway Pods sorted by name,
// and fails unless there are exactly two.
func (h *Harness) gatewayPods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := h.Client.CoreV1().Pods(gatewayNamespace).List(ctx, metav1.ListOptions{LabelSelector: gatewaySelector})
	if err != nil {
		return nil, fmt.Errorf("list gateway pods: %w", err)
	}
	var ready []corev1.Pod
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil && podReady(&p) {
			ready = append(ready, p)
		}
	}
	if len(ready) != gatewayReplicas {
		return nil, fmt.Errorf("%d ready gateway pods, want %d", len(ready), gatewayReplicas)
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })
	return ready, nil
}

// forwardGateways opens one standing port-forward per gateway Pod and builds the clients that dial through them.
// Requests to port 9090 reach the ops listener; every other port reaches the API listener,
// so "http://gateway/..." and "http://gateway:9090/readyz" both work.
func (h *Harness) forwardGateways(ctx context.Context) (func(), error) {
	pods, err := h.gatewayPods(ctx)
	if err != nil {
		return nil, err
	}
	var stops []func()
	stopAll := func() {
		for _, s := range stops {
			s()
		}
	}
	for i, p := range pods {
		ports, stop, err := h.forward(ctx, p.Namespace, p.Name, []string{"0:" + gatewayAPIPort, "0:" + gatewayOpsPort})
		if err != nil {
			stopAll()
			return nil, fmt.Errorf("port-forward %s: %w", p.Name, err)
		}
		stops = append(stops, stop)
		h.log.Info("gateway port-forward", "pod", p.Name, "api", ports[0], "ops", ports[1])
		api, ops := ports[0], ports[1]
		h.Gateways[i] = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				local := api
				if _, port, err := net.SplitHostPort(addr); err == nil && port == gatewayOpsPort {
					local = ops
				}
				var d net.Dialer
				return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(local))))
			},
		}}
	}
	return stopAll, nil
}

// forward opens a port-forward to pod and returns the local ports in the order requested.
// The forward runs until stop is called; ctx only bounds opening it.
func (h *Harness) forward(ctx context.Context, ns, pod string, ports []string) ([]uint16, func(), error) {
	req := h.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward")
	transport, upgrader, err := spdy.RoundTripperFor(h.restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("spdy transport: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())
	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	var errOut bytes.Buffer
	pf, err := portforward.New(dialer, ports, stopCh, readyCh, io.Discard, &errOut)
	if err != nil {
		return nil, nil, fmt.Errorf("port-forward: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- pf.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-done:
		return nil, nil, fmt.Errorf("port-forward %s/%s: %w (%s)", ns, pod, err, errOut.String())
	case <-ctx.Done():
		close(stopCh)
		return nil, nil, fmt.Errorf("port-forward %s/%s: %w", ns, pod, ctx.Err())
	}
	forwarded, err := pf.GetPorts()
	if err != nil {
		close(stopCh)
		return nil, nil, fmt.Errorf("forwarded ports: %w", err)
	}
	local := make([]uint16, len(forwarded))
	for i, fp := range forwarded {
		local[i] = fp.Local
	}
	var once bool
	stop := func() {
		if !once {
			once = true
			close(stopCh)
		}
	}
	return local, stop, nil
}

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

// Apply renders the named overlay under test/e2e/overlays into namespace ns and applies it.
// Cluster-scoped objects keep their names, so two tests applying the same overlay would collide;
// the reduced overlays are named apart from the base for that reason.
func (h *Harness) Apply(t *testing.T, ns, overlay string) {
	t.Helper()
	if err := h.apply(t.Context(), ns, overlay); err != nil {
		t.Fatal(err)
	}
}

// apply wraps the overlay in a kustomization that sets the namespace,
// which also rewrites the ServiceAccount namespace in the overlay's ClusterRoleBinding subjects.
func (h *Harness) apply(ctx context.Context, ns, overlay string) error {
	dir, err := os.MkdirTemp("", "profgate-overlay-")
	if err != nil {
		return fmt.Errorf("overlay dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// kustomize only accepts relative resource paths.
	target, err := filepath.Rel(dir, filepath.Join(h.root, "test", "e2e", "overlays", overlay))
	if err != nil {
		return fmt.Errorf("overlay path: %w", err)
	}
	wrapper := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: %s\nresources:\n  - %s\n", ns, target)
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(wrapper), 0o600); err != nil {
		return fmt.Errorf("write kustomization: %w", err)
	}
	if err := h.kubectl(ctx, "apply", "-k", dir); err != nil {
		return fmt.Errorf("apply overlay %s into %s: %w", overlay, ns, err)
	}
	return nil
}

// ForwardTestApp opens a port-forward to a test-app Pod's pprof port and returns the local base URL.
// Only a scenario that declares NeedsPodReach may call it,
// so a degraded lane skips exactly the scenarios that would fail there.
func (h *Harness) ForwardTestApp(t *testing.T, ns, pod string) string {
	t.Helper()
	if h.scenario == nil || !h.scenario.NeedsPodReach {
		t.Fatalf("ForwardTestApp called by a scenario that does not declare NeedsPodReach")
	}
	ports, stop, err := h.forward(t.Context(), ns, pod, []string{"0:" + testAppPort})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0])))
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

// kind runs the lane's pinned kind binary through mise.
func (h *Harness) kind(ctx context.Context, args ...string) error {
	return h.run(ctx, nil, "mise", append([]string{"x", "kind@" + h.Lane.Kind, "--", "kind"}, args...)...)
}

// kubectl runs kubectl against the harness kubeconfig.
func (h *Harness) kubectl(ctx context.Context, args ...string) error {
	return h.run(ctx, []string{"KUBECONFIG=" + h.kubeconfig}, "kubectl", args...)
}

// run executes a command from the module root with its output on stderr,
// so the test log shows what kind, ko, and kubectl reported.
func (h *Harness) run(ctx context.Context, env []string, name string, args ...string) error {
	h.log.Info("run", "command", name+" "+strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the harness drives kind, ko, and kubectl with arguments it composes itself
	cmd.Dir = h.root
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// output executes a command from the module root and returns its stdout.
func (h *Harness) output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the harness drives kind and docker with arguments it composes itself
	cmd.Dir = h.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, stderr.String())
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
