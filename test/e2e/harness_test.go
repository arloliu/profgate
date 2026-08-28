//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
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
	// gatewayConfigMap is the shipped ConfigMap the Deployment reads its
	// configuration from.
	gatewayConfigMap = "profgate-config"
	// gatewayContainer is the container the shipped Deployment runs the gateway in.
	gatewayContainer = "profgate"
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

	// natsImage is the JetStream server the PGO scenarios keep their state in.
	// TestMain pulls it and loads it onto the node itself, so no node pulls it
	// and no lane depends on a registry for it.
	natsImage = "nats:2.11-alpine"
	// natsName is the Deployment, Service, and label value in nats.yaml.
	natsName = "nats"
	// natsManifest is applied into a namespace by the harness; paths resolve
	// from the module root.
	natsManifest = "test/e2e/nats.yaml"
	// natsConfigMap is the ConfigMap nats.yaml mounts the server configuration
	// from; the harness writes it, because it holds the run's account.
	natsConfigMap = "nats-config"

	natsClientPort  = "4222"
	natsMonitorPort = "8222"

	// dexImage is the OpenID Connect issuer the auth scenarios log in
	// through; TestMain loads it the way it loads the NATS server.
	dexImage = "ghcr.io/dexidp/dex:v2.45.1"
	// keycloakImage is the second issuer, the one docs/keycloak-realm.json
	// was verified against; the Keycloak scenario imports that realm.
	keycloakImage = "quay.io/keycloak/keycloak:26.7.2"

	// authSecret is the Secret deploy/base mounts for authentication, and
	// authMountPath is where its keys appear in the container.
	authSecret    = "profgate-auth" //nolint:gosec // the Secret's name, not its contents
	authMountPath = "/etc/profgate/auth"

	// credsSecret is the Secret deploy/base mounts, credsSecretKey the entry in
	// it, and credsFile where the pair appears in the container.
	credsSecret    = "profgate-nats-creds" //nolint:gosec // the Secret's name, not its contents
	credsSecretKey = "nats.creds"
	credsFile      = "/etc/profgate/nats/nats.creds" //nolint:gosec // a path, not a credential

	// tlsSecret is the certificate Secret the tls-gateway overlay mounts, and
	// tlsMountPath is where the pair appears in the container.
	tlsSecret    = "profgate-tls" //nolint:gosec // the Secret's name, not its contents
	tlsMountPath = "/etc/profgate/tls"

	// The three stores of the bucket contract.
	configBucket    = "PROFGATE_CONFIG"
	jobsBucket      = "PROFGATE_JOBS"
	artifactsBucket = "PROFGATE_ARTIFACTS"

	// natsDeadline bounds one harness call against NATS.
	natsDeadline = 30 * time.Second

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
	NATS     *natsServer          // the JetStream server the PGO scenarios run against
	scenario *Scenario            // set by TestScenarios before Run

	stopGateways func() // closes the standing gateway forwards; RefreshGateways replaces them

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
		"port selection":                 scenarioPortSelection,
		"port selection refused":         scenarioPortSelectionRefused,
		"pgo-on-demand":                  scenarioPGOOnDemand,
		"pgo-scheduled-slot":             scenarioPGOScheduledSlot,
		"pgo-cancel":                     scenarioPGOCancel,
		"pgo-version-conflict":           scenarioPGOVersionConflict,
		"pgo-reclaim":                    scenarioPGOReclaim,
		"pgo-realm-flags":                scenarioPGORealmFlags,
		"pgo-disabled":                   scenarioPGODisabled,
		"pgo-clusterrole":                scenarioPGOClusterRole,
		"pgo-preflight-negative":         scenarioPGOPreflightNegative,
		"tls-rotation":                   scenarioTLSRotation,
		"auth-oidc-browser":              scenarioAuthOIDCBrowser,
		"auth-basic":                     scenarioAuthBasic,
		"auth-oidc-keycloak":             scenarioAuthOIDCKeycloak,
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
	if err := h.loadNATSImage(ctx); err != nil {
		fail("load nats image", err)
	}
	if err := h.loadImage(ctx, dexImage); err != nil {
		fail("load dex image", err)
	}
	if err := h.loadImage(ctx, keycloakImage); err != nil {
		fail("load keycloak image", err)
	}

	kubeconfigDir, err := os.MkdirTemp("", "profgate-e2e-")
	if err != nil {
		fail("kubeconfig dir", err)
	}
	if err := h.connect(ctx, filepath.Join(kubeconfigDir, "kubeconfig")); err != nil {
		fail("connect", err)
	}
	// The stores must exist and hold the run's credentials before any gateway
	// starts: NATS preflight is fatal, so a gateway that came up first would
	// exit instead of waiting.
	if err := h.createNamespace(ctx, gatewayNamespace); err != nil {
		fail("gateway namespace", err)
	}
	nsrv, err := h.deployNATS(ctx, gatewayNamespace)
	if err != nil {
		fail("deploy nats", err)
	}
	h.NATS = nsrv
	if err := nsrv.provisionStores(ctx, 0); err != nil {
		fail("provision stores", err)
	}
	pub, sub, err := gatewayPermissions(h.root)
	if err != nil {
		fail("gateway nats permissions", err)
	}
	user, err := nsrv.ID.user("profgate", pub, sub)
	if err != nil {
		fail("gateway nats user", err)
	}
	if err := h.applyCredsSecret(ctx, gatewayNamespace, user.Creds); err != nil {
		fail("gateway credentials", err)
	}
	if err := h.deployGateway(ctx); err != nil {
		h.dumpGateway(ctx)
		fail("deploy gateway", err)
	}
	h.stopGateways, err = h.forwardGateways(ctx)
	if err != nil {
		fail("port-forward gateways", err)
	}

	harness = h
	code := m.Run()

	h.stopGateways()
	nsrv.close()
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

// loadNATSImage pulls the NATS server and loads it onto the node.
func (h *Harness) loadNATSImage(ctx context.Context) error {
	return h.loadImage(ctx, natsImage)
}

// loadImage pulls a published image and loads it onto the node.
// The two images ko builds carry one platform each and go in directly, but
// published images are multi-platform indexes: a plain export of one names
// every platform's manifest while the daemon holds only the blobs of the one
// it pulled, and the node's import rejects the archive for the digests that
// are missing.
// Exporting the node's own platform alone leaves an archive whose every
// reference resolves.
// That export still carries the attestation manifest attached to the platform
// manifest, which older node containerd unpacks as an image and rejects, so
// the archive goes to the node without its OCI index.
func (h *Harness) loadImage(ctx context.Context, image string) error {
	if err := h.run(ctx, nil, "docker", "pull", image); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	dir, err := os.MkdirTemp("", "profgate-e2e-image-")
	if err != nil {
		return fmt.Errorf("image archive directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	archive := filepath.Join(dir, "image.tar")
	if err := h.run(ctx, nil, "docker", "save", "--platform", "linux/"+runtime.GOARCH,
		"--output", archive, image); err != nil {
		return fmt.Errorf("save %s: %w", image, err)
	}
	loadable := filepath.Join(dir, "image-docker.tar")
	if err := dropOCIIndex(archive, loadable); err != nil {
		return fmt.Errorf("rewrite %s archive: %w", image, err)
	}
	if err := h.kind(ctx, "load", "image-archive", "--name", h.Cluster, loadable); err != nil {
		return fmt.Errorf("load %s: %w", image, err)
	}
	return nil
}

// ociIndexMembers are the archive entries that make a Docker export an OCI
// layout. Dropping them leaves manifest.json as the only entry point, which
// names the image and none of the manifests attached to it.
var ociIndexMembers = map[string]bool{"index.json": true, "oci-layout": true}

// dropOCIIndex copies the tar at src to dst without the OCI index members.
func dropOCIIndex(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is the archive the harness just wrote
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) //nolint:gosec // dst is a path in the harness's own temporary directory
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer func() { _ = out.Close() }()

	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if ociIndexMembers[path.Clean(hdr.Name)] {
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write %s: %w", hdr.Name, err)
		}
		if _, err := io.CopyN(tw, tr, hdr.Size); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("copy %s: %w", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}

	return out.Close()
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
	cfg := gatewayConfig(gatewayConfigOptions{NATSURL: natsURL(gatewayNamespace), RealmPGO: true})
	if err := h.apply(ctx, gatewayNamespace, "default", configPatch(gatewayConfigMap, cfg)); err != nil {
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
// One connection the Pod resets ends the whole session: client-go's
// handleConnection closes the stream connection on any message from the
// kubelet's error stream, ForwardPorts returns, and its deferred Close drops
// every local listener, so the next dial is refused.
// The session is reopened on the same local ports when that happens, so the
// address a caller holds stays valid for as long as the Pod lives.
func (h *Harness) forward(ctx context.Context, ns, pod string, ports []string) ([]uint16, func(), error) {
	req := h.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward")
	transport, upgrader, err := spdy.RoundTripperFor(h.restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("spdy transport: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())
	stopCh := make(chan struct{})
	forwarded, done, err := openForward(ctx, dialer, ports, stopCh)
	if err != nil {
		close(stopCh)

		return nil, nil, fmt.Errorf("port-forward %s/%s: %w", ns, pod, err)
	}
	local := make([]uint16, len(forwarded))
	pinned := make([]string, len(forwarded))
	for i, fp := range forwarded {
		local[i] = fp.Local
		pinned[i] = fmt.Sprintf("%d:%d", fp.Local, fp.Remote)
	}
	go func() {
		for {
			select {
			case <-stopCh:
				return
			case err := <-done:
				h.log.Warn("port-forward ended; reopening", "namespace", ns, "pod", pod, "ports", pinned, "err", err)
			}
			for {
				_, next, err := openForward(context.Background(), dialer, pinned, stopCh)
				if err == nil {
					done = next

					break
				}
				h.log.Warn("port-forward reopen failed", "namespace", ns, "pod", pod, "ports", pinned, "err", err)
				select {
				case <-stopCh:
					return
				case <-time.After(pollInterval):
				}
			}
		}
	}()
	var once bool
	stop := func() {
		if !once {
			once = true
			close(stopCh)
		}
	}
	return local, stop, nil
}

// openForward starts one port-forward session over dialer and waits for its
// listeners; done carries ForwardPorts' result once the session ends.
// A session still opening when ctx ends or stopCh closes is left to the
// caller, whose close of stopCh ends it.
func openForward(ctx context.Context, dialer httpstream.Dialer, ports []string, stopCh chan struct{}) ([]portforward.ForwardedPort, <-chan error, error) {
	readyCh := make(chan struct{})
	var errOut bytes.Buffer
	pf, err := portforward.New(dialer, ports, stopCh, readyCh, io.Discard, &errOut)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- pf.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-done:
		return nil, nil, fmt.Errorf("%w (%s)", err, errOut.String())
	case <-stopCh:
		return nil, nil, errors.New("stopped")
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	forwarded, err := pf.GetPorts()
	if err != nil {
		return nil, nil, fmt.Errorf("forwarded ports: %w", err)
	}

	return forwarded, done, nil
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
func (h *Harness) Apply(t *testing.T, ns, overlay string, patches ...patch) {
	t.Helper()
	if err := h.apply(t.Context(), ns, overlay, patches...); err != nil {
		t.Fatal(err)
	}
}

// patch is one kustomize patch applied over an overlay: the file name it is
// written under, the resource it selects, and the manifest fragment it holds.
// The selector is written out rather than left to the fragment's own metadata,
// because a resource the overlay inherits from deploy/base already carries a
// namespace and one written in the overlay does not,
// so a fragment that matched by its metadata would have to know which it is.
type patch struct {
	file string
	kind string
	name string
	body string
}

// apply wraps the overlay in a kustomization that sets the namespace,
// which also rewrites the ServiceAccount namespace in the overlay's ClusterRoleBinding subjects.
// The patches, when any are given, are written beside the wrapper and merged over the overlay.
func (h *Harness) apply(ctx context.Context, ns, overlay string, patches ...patch) error {
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
	if len(patches) > 0 {
		wrapper += "patches:\n"
		for _, p := range patches {
			wrapper += fmt.Sprintf("  - path: %s\n    target:\n      kind: %s\n      name: %s\n", p.file, p.kind, p.name)
			if err := os.WriteFile(filepath.Join(dir, p.file), []byte(p.body), 0o600); err != nil {
				return fmt.Errorf("write patch %s: %w", p.file, err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(wrapper), 0o600); err != nil {
		return fmt.Errorf("write kustomization: %w", err)
	}
	if err := h.kubectl(ctx, "apply", "-k", dir); err != nil {
		return fmt.Errorf("apply overlay %s into %s: %w", overlay, ns, err)
	}
	return nil
}

// gatewayConfigOptions is what varies between the gateways the suite runs.
type gatewayConfigOptions struct {
	// NATSURL turns PGO on: an empty one leaves out both the nats and the pgo
	// block, which is a gateway with PGO disabled.
	NATSURL string
	// RealmPGO writes the realm's three PGO flags as true.
	// A realm without the block has every flag false.
	RealmPGO bool
	// TLSMount, when set, is where the certificate Secret is mounted, and
	// turns the API listener into an HTTPS listener serving the pair under it.
	TLSMount string
	// AuthBlock, when set, is the whole auth block, written in place of the
	// disabled one the other gateways run with.
	AuthBlock string
	// UIEnabled turns the console on: /ui/ serves the shell and / redirects to it.
	UIEnabled bool
}

// gatewayConfig renders the configuration one gateway runs with:
// the shipped base's, with the realm wide open, plus what PGO needs.
// allowedSelections admits the test app's second port by number and by name,
// so the default gateway proves the accepted outcome; the ports-gateway
// overlay keeps an empty list and proves the refused one.
// minEvery is a minute so a scheduled slot fires inside a test rather than a
// quarter of an hour later, leaseTTL the lowest the ceiling admits so a reclaim
// waits thirty seconds rather than a minute, the default jitter is the smallest
// value that is not the zero one the loader reads as unset,
// and the sampling defaults are seconds so a Collection that names none finishes
// while the test watches it.
func gatewayConfig(o gatewayConfigOptions) string {
	var b strings.Builder
	b.WriteString(`server:
  listen: ":8080"
  opsListen: ":9090"
`)
	if o.TLSMount != "" {
		fmt.Fprintf(&b, `  tls:
    certFile: %s/tls.crt
    keyFile: %s/tls.key
`, o.TLSMount, o.TLSMount)
	}
	b.WriteString(`discovery:
  versionLabel: app.kubernetes.io/version
  pprof:
    port: 6060
    allowedSelections:
      - port: 6061
      - portName: pprof-alt
limits:
  cpuSeconds: 60
  traceSeconds: 60
  maxConcurrentProfiles: 16
`)
	if o.AuthBlock != "" {
		b.WriteString(o.AuthBlock)
	} else {
		b.WriteString(`auth:
  mode: disabled
  anonymousRealm: developer
`)
	}
	if o.NATSURL != "" {
		fmt.Fprintf(&b, `nats:
  url: %s
  credsFile: %s
pgo:
  enabled: true
  configAPI: enabled
  leaseTTL: 30s
  limits:
    minEvery: 1m
  defaults:
    schedule:
      every: 1m
      jitter: 1s
    sampling:
      duration: 2s
      rounds: 1
      roundInterval: 1s
`, o.NATSURL, credsFile)
	}
	if o.UIEnabled {
		b.WriteString(`ui:
  enabled: true
`)
	}
	b.WriteString(`realms:
  developer:
    namespaces: ["*"]
    services: ["*"]
    profiles: ["*"]
`)
	if o.RealmPGO {
		b.WriteString(`    pgo:
      read: true
      collect: true
      configure: true
`)
	}
	return b.String()
}

// credsMountPatch adds the credentials mount deploy/base carries to a gateway
// Deployment an overlay wrote without one.
// A gateway configured for PGO refuses to start when the file its configuration
// names is not readable, so an overlay that gains the configuration needs the
// mount with it.
func credsMountPatch(deployment string) patch {
	return patch{file: "deployment.yaml", kind: "Deployment", name: deployment, body: fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  template:
    spec:
      securityContext:
        fsGroup: 65532
      containers:
        - name: profgate
          volumeMounts:
            - name: %s
              mountPath: %s
              readOnly: true
      volumes:
        - name: %s
          secret:
            secretName: %s
            defaultMode: 0440
            optional: true
`, deployment, credsSecret, filepath.Dir(credsFile)+"/", credsSecret, credsSecret)}
}

// configPatch replaces the configuration in the named gateway ConfigMap.
func configPatch(name, cfg string) patch {
	var body strings.Builder
	fmt.Fprintf(&body, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\ndata:\n  config.yaml: |\n", name)
	for _, line := range strings.Split(strings.TrimRight(cfg, "\n"), "\n") {
		body.WriteString("    " + line + "\n")
	}
	return patch{file: "configmap.yaml", kind: "ConfigMap", name: name, body: body.String()}
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

// natsIdentity is the operator, the system account, and the PROFGATE account
// the harness mints for one NATS server.
// Users are signed by the account key, so a scenario can mint one with reduced
// permissions against a server that is already running:
// the memory resolver holds the account, and every user the account signs is
// accepted without touching the server's configuration.
type natsIdentity struct {
	operatorJWT string
	sysPub      string
	sysJWT      string
	accountPub  string
	accountJWT  string
	accountKey  nkeys.KeyPair
}

// newNATSIdentity mints one run's operator, system account, and PROFGATE
// account, with JetStream unlimited on the account that owns the stores.
func newNATSIdentity() (*natsIdentity, error) {
	operatorKey, err := nkeys.CreateOperator()
	if err != nil {
		return nil, fmt.Errorf("operator key: %w", err)
	}
	operatorPub, err := operatorKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("operator key: %w", err)
	}
	sysKey, err := nkeys.CreateAccount()
	if err != nil {
		return nil, fmt.Errorf("system account key: %w", err)
	}
	sysPub, err := sysKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("system account key: %w", err)
	}
	accountKey, err := nkeys.CreateAccount()
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}
	accountPub, err := accountKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}

	sysClaims := jwt.NewAccountClaims(sysPub)
	sysClaims.Name = "SYS"
	sysJWT, err := sysClaims.Encode(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("system account jwt: %w", err)
	}
	accountClaims := jwt.NewAccountClaims(accountPub)
	accountClaims.Name = "PROFGATE"
	// Without an explicit grant JetStream is off for the account and every
	// store operation fails as if the buckets did not exist.
	accountClaims.Limits.DiskStorage = -1
	accountClaims.Limits.MemoryStorage = -1
	accountClaims.Limits.Streams = -1
	accountJWT, err := accountClaims.Encode(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("account jwt: %w", err)
	}
	operatorClaims := jwt.NewOperatorClaims(operatorPub)
	operatorClaims.Name = "profgate-e2e"
	operatorClaims.SystemAccount = sysPub
	operatorJWT, err := operatorClaims.Encode(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("operator jwt: %w", err)
	}

	return &natsIdentity{
		operatorJWT: operatorJWT,
		sysPub:      sysPub,
		sysJWT:      sysJWT,
		accountPub:  accountPub,
		accountJWT:  accountJWT,
		accountKey:  accountKey,
	}, nil
}

// serverConf is the nats-server configuration: who signs users, which account
// is the system account, and the two account JWTs, resolved from memory.
// JetStream, its store directory, and the monitoring listener are arguments in
// nats.yaml, so this file carries authentication and nothing else.
func (id *natsIdentity) serverConf() string {
	return fmt.Sprintf(`operator: %q
system_account: %q
resolver: MEMORY
resolver_preload: {
  %s: %q
  %s: %q
}
`, id.operatorJWT, id.sysPub, id.sysPub, id.sysJWT, id.accountPub, id.accountJWT)
}

// natsUser is one minted user: the credentials file a gateway mounts and the
// JWT and seed the harness's own connections authenticate with directly.
type natsUser struct {
	Creds []byte
	jwt   string
	seed  string
}

// user mints a user of the PROFGATE account with exactly the permissions given.
func (id *natsIdentity) user(name string, pub, sub []string) (natsUser, error) {
	key, err := nkeys.CreateUser()
	if err != nil {
		return natsUser{}, fmt.Errorf("user key %s: %w", name, err)
	}
	userPub, err := key.PublicKey()
	if err != nil {
		return natsUser{}, fmt.Errorf("user key %s: %w", name, err)
	}
	seed, err := key.Seed()
	if err != nil {
		return natsUser{}, fmt.Errorf("user seed %s: %w", name, err)
	}
	claims := jwt.NewUserClaims(userPub)
	claims.Name = name
	claims.Pub.Allow = pub
	claims.Sub.Allow = sub
	userJWT, err := claims.Encode(id.accountKey)
	if err != nil {
		return natsUser{}, fmt.Errorf("user jwt %s: %w", name, err)
	}
	creds, err := jwt.FormatUserConfig(userJWT, seed)
	if err != nil {
		return natsUser{}, fmt.Errorf("user credentials %s: %w", name, err)
	}
	return natsUser{Creds: creds, jwt: userJWT, seed: string(seed)}, nil
}

// gatewayPermissions returns the publish and subscribe subjects
// deploy/nats/account.conf grants.
// The end-to-end gateway user is exactly the set the repository ships, so a
// subject the gateway needs and the fragment omits fails the suite instead of
// passing against a copy of the list that has drifted.
func gatewayPermissions(root string) (pub, sub []string, err error) {
	path := filepath.Join(root, "deploy", "nats", "account.conf")
	b, err := os.ReadFile(path) //nolint:gosec // the path is composed from the module root
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var into *[]string
	for _, line := range strings.Split(string(b), "\n") {
		text := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(text, "publish:"):
			into = &pub
		case strings.HasPrefix(text, "subscribe:"):
			into = &sub
		case strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`) && into != nil:
			*into = append(*into, strings.Trim(text, `"`))
		}
	}
	if len(pub) == 0 || len(sub) == 0 {
		return nil, nil, fmt.Errorf("%s granted %d publish and %d subscribe subjects", path, len(pub), len(sub))
	}
	return pub, sub, nil
}

// without returns subjects with one entry removed, and fails when the entry was
// not there: a reduced user must be reduced by something the full set granted.
func without(subjects []string, drop string) ([]string, error) {
	out := make([]string, 0, len(subjects))
	for _, s := range subjects {
		if s != drop {
			out = append(out, s)
		}
	}
	if len(out) == len(subjects) {
		return nil, fmt.Errorf("deploy/nats/account.conf does not grant %q, so removing it reduces nothing", drop)
	}
	return out, nil
}

// natsServer is a running NATS Deployment: the identity its users are minted
// from, an administrative connection through a standing port-forward, and the
// monitoring endpoint the connection count is read from.
type natsServer struct {
	Namespace string
	ID        *natsIdentity

	conn    *nats.Conn
	js      jetstream.JetStream
	monitor string // http://127.0.0.1:<local port>
	stop    func()
}

// natsURL is the in-cluster address of the server in ns.
func natsURL(ns string) string {
	return "nats://" + natsName + "." + ns + ".svc:" + natsClientPort
}

// deployNATS writes the server configuration, applies nats.yaml into ns, waits
// for the rollout, and opens the administrative connection through a
// port-forward.
// A Deployment that already existed is restarted: every run mints a new
// account, and a Pod still holding the previous one would reject every
// connection this run makes.
func (h *Harness) deployNATS(ctx context.Context, ns string) (*natsServer, error) {
	id, err := newNATSIdentity()
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: natsConfigMap, Namespace: ns},
		Data:       map[string]string{"nats.conf": id.serverConf()},
	}
	if err := h.applyConfigMap(ctx, ns, cm); err != nil {
		return nil, err
	}
	_, err = h.Client.AppsV1().Deployments(ns).Get(ctx, natsName, metav1.GetOptions{})
	existed := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get deployment %s: %w", natsName, err)
	}
	if err := h.kubectl(ctx, "apply", "-n", ns, "-f", natsManifest); err != nil {
		return nil, err
	}
	if existed {
		if err := h.kubectl(ctx, "rollout", "restart", "deployment/"+natsName, "-n", ns); err != nil {
			return nil, err
		}
	}
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+natsName, "-n", ns,
		"--timeout="+rolloutTimeout.String()); err != nil {
		return nil, err
	}
	pod, err := h.waitOnePod(ctx, ns, "app.kubernetes.io/name="+natsName)
	if err != nil {
		return nil, err
	}
	ports, stop, err := h.forward(ctx, ns, pod, []string{"0:" + natsClientPort, "0:" + natsMonitorPort})
	if err != nil {
		return nil, fmt.Errorf("port-forward %s: %w", pod, err)
	}
	admin, err := id.user("harness", []string{">"}, []string{">"})
	if err != nil {
		stop()
		return nil, err
	}
	conn, err := nats.Connect("nats://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0]))),
		nats.UserJWTAndSeed(admin.jwt, admin.seed), nats.Name("profgate-e2e-harness"))
	if err != nil {
		stop()
		return nil, fmt.Errorf("connect to nats in %s: %w", ns, err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		stop()
		return nil, fmt.Errorf("jetstream in %s: %w", ns, err)
	}
	h.log.Info("nats", "namespace", ns, "pod", pod, "client", ports[0], "monitor", ports[1])

	return &natsServer{
		Namespace: ns,
		ID:        id,
		conn:      conn,
		js:        js,
		monitor:   "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[1]))),
		stop:      stop,
	}, nil
}

// close ends the administrative connection and its port-forward.
func (s *natsServer) close() {
	s.conn.Close()
	s.stop()
}

// provisionStores creates the three stores with the configuration of the bucket
// contract: file storage, no TTL, and no size ceiling.
// jobsTTL is zero for every caller but the preflight scenario, which provisions
// a bucket the contract forbids on purpose.
func (s *natsServer) provisionStores(ctx context.Context, jobsTTL time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, natsDeadline)
	defer cancel()
	buckets := []struct {
		name string
		ttl  time.Duration
	}{{configBucket, 0}, {jobsBucket, jobsTTL}}
	for _, b := range buckets {
		_, err := s.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:       b.name,
			Storage:      jetstream.FileStorage,
			History:      1,
			TTL:          b.ttl,
			MaxBytes:     -1,
			MaxValueSize: -1,
		})
		if err != nil {
			return fmt.Errorf("provision %s: %w", b.name, err)
		}
	}
	_, err := s.js.CreateOrUpdateObjectStore(ctx, jetstream.ObjectStoreConfig{
		Bucket:   artifactsBucket,
		Storage:  jetstream.FileStorage,
		TTL:      0,
		MaxBytes: -1,
	})
	if err != nil {
		return fmt.Errorf("provision %s: %w", artifactsBucket, err)
	}
	return nil
}

// purgeStores removes every key and object so the next scenario starts empty.
// The buckets themselves are left alone: the gateways' watches and consumers
// belong to the streams that exist now, and a recreated stream would leave them
// watching nothing.
func (s *natsServer) purgeStores(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, natsDeadline)
	defer cancel()
	for _, bucket := range []string{configBucket, jobsBucket} {
		kv, err := s.js.KeyValue(ctx, bucket)
		if err != nil {
			return fmt.Errorf("open %s: %w", bucket, err)
		}
		keys, err := kv.Keys(ctx)
		if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
			return fmt.Errorf("list %s: %w", bucket, err)
		}
		for _, key := range keys {
			if err := kv.Purge(ctx, key); err != nil {
				return fmt.Errorf("purge %s %s: %w", bucket, key, err)
			}
		}
	}
	obs, err := s.js.ObjectStore(ctx, artifactsBucket)
	if err != nil {
		return fmt.Errorf("open %s: %w", artifactsBucket, err)
	}
	infos, err := obs.List(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoObjectsFound) {
		return fmt.Errorf("list %s: %w", artifactsBucket, err)
	}
	for _, info := range infos {
		if err := obs.Delete(ctx, info.Name); err != nil {
			return fmt.Errorf("delete %s %s: %w", artifactsBucket, info.Name, err)
		}
	}
	return nil
}

// keys lists the keys of one KV bucket, empty when it holds none.
func (s *natsServer) keys(ctx context.Context, bucket string) ([]string, error) {
	kv, err := s.js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", bucket, err)
	}
	keys, err := kv.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list %s: %w", bucket, err)
	}
	return keys, nil
}

// objects lists the names in the artifact store, empty when it holds none.
func (s *natsServer) objects(ctx context.Context) ([]string, error) {
	obs, err := s.js.ObjectStore(ctx, artifactsBucket)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", artifactsBucket, err)
	}
	infos, err := obs.List(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoObjectsFound) {
		return nil, fmt.Errorf("list %s: %w", artifactsBucket, err)
	}
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names, nil
}

// recordsOf returns the Collection records of one Service, read from
// PROFGATE_JOBS directly.
// The API answers from a watched cache; a scenario that counts what the
// schedulers wrote reads the durable keys instead.
func (s *natsServer) recordsOf(ctx context.Context, ns, service string) ([]collectionRecord, error) {
	kv, err := s.js.KeyValue(ctx, jobsBucket)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", jobsBucket, err)
	}
	keys, err := kv.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list %s: %w", jobsBucket, err)
	}
	var out []collectionRecord
	for _, key := range keys {
		if !strings.HasPrefix(key, "job.") {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("read %s %s: %w", jobsBucket, key, err)
		}
		var rec collectionRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("decode %s %s: %w", jobsBucket, key, err)
		}
		if rec.Namespace == ns && rec.Service == service {
			out = append(out, rec)
		}
	}
	return out, nil
}

// connections is the server's current client connection count, from the
// monitoring endpoint.
func (s *natsServer) connections(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.monitor+"/varz", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("varz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var varz struct {
		Connections int `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&varz); err != nil {
		return 0, fmt.Errorf("varz: %w", err)
	}
	return varz.Connections, nil
}

// applyConfigMap creates cm, or replaces the data of one that already exists.
func (h *Harness) applyConfigMap(ctx context.Context, ns string, cm *corev1.ConfigMap) error {
	api := h.Client.CoreV1().ConfigMaps(ns)
	_, err := api.Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = api.Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply configmap %s/%s: %w", ns, cm.Name, err)
	}
	return nil
}

// applyCredsSecret creates or replaces the NATS credentials Secret in ns.
// The harness holds the cluster's administrative kubeconfig; the gateway reads
// the file through a mounted volume and needs no Secrets permission for it.
func (h *Harness) applyCredsSecret(ctx context.Context, ns string, creds []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credsSecret, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{credsSecretKey: creds},
	}
	api := h.Client.CoreV1().Secrets(ns)
	_, err := api.Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = api.Update(ctx, secret, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply secret %s/%s: %w", ns, credsSecret, err)
	}
	return nil
}

// applyTLSSecret creates or replaces the API listener's certificate Secret in ns.
// It is a kubernetes.io/tls Secret with the two standard keys, which is what
// cert-manager writes and what the chart's volume expects.
// The harness holds the cluster's administrative kubeconfig; the gateway reads
// the pair through a mounted volume and needs no Secrets permission for it.
func (h *Harness) applyTLSSecret(ctx context.Context, ns string, cert, key []byte) error {
	return h.applyNamedTLSSecret(ctx, ns, tlsSecret, cert, key)
}

// applyNamedTLSSecret is applyTLSSecret for a Secret of any name, which is how
// the issuer the auth scenarios deploy gets a certificate of its own.
func (h *Harness) applyNamedTLSSecret(ctx context.Context, ns, name string, cert, key []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: key},
	}
	api := h.Client.CoreV1().Secrets(ns)
	_, err := api.Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = api.Update(ctx, secret, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply secret %s/%s: %w", ns, name, err)
	}

	return nil
}

// applyAuthSecret creates or replaces the authentication Secret in ns with
// exactly the files given, keyed by the name each appears under the mount.
// The harness holds the cluster's administrative kubeconfig; the gateway reads
// the files through a mounted volume and needs no Secrets permission for them.
func (h *Harness) applyAuthSecret(ctx context.Context, ns string, files map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: authSecret, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       files,
	}
	api := h.Client.CoreV1().Secrets(ns)
	_, err := api.Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = api.Update(ctx, secret, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply secret %s/%s: %w", ns, authSecret, err)
	}

	return nil
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

// RefreshGateways reopens the standing port-forwards against the gateway Pods
// running now.
// A scenario that deletes a gateway Pod leaves its forward pointing at a Pod
// that no longer exists, and every later scenario reads both replicas.
// The context is the process's, not the test's:
// the scenario that needs this calls it from a cleanup, which runs after the
// test's context is cancelled, and the forwards it opens serve every scenario
// after this one.
func (h *Harness) RefreshGateways(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := poll(ctx, rolloutTimeout, func(ctx context.Context) (bool, error) {
		_, err := h.gatewayPods(ctx)
		return err == nil, nil
	}); err != nil {
		t.Fatalf("gateways never returned to %d ready replicas: %v", gatewayReplicas, err)
	}
	h.stopGateways()
	stop, err := h.forwardGateways(ctx)
	if err != nil {
		t.Fatalf("reopen gateway port-forwards: %v", err)
	}
	h.stopGateways = stop
}
