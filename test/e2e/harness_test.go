//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
	// keycloakImage is the second issuer, the one docs/keycloak-realm.json was verified against;
	// the Keycloak scenario imports that realm.
	keycloakImage = "quay.io/keycloak/keycloak:26.7.2"

	// authSecret is the Secret deploy/base mounts for authentication, and
	// authMountPath is where its keys appear in the container.
	authSecret    = "profgate-auth" //nolint:gosec // the Secret's name, not its contents
	authMountPath = "/etc/profgate/auth"

	// credsSecret is the Secret deploy/base mounts, credsSecretKey the entry in
	// it, and credsFile where the pair appears in the container.
	credsSecret    = "profgate-nats-creds"           //nolint:gosec // the Secret's name, not its contents
	credsSecretKey = "nats.creds"                    //nolint:gosec // the key's name inside the Secret, not its contents
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
	Browser  browser              // the Chromium the console scenarios drive, discovered once
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
		"console-oidc":                   scenarioConsoleOIDC,
		"console-basic":                  scenarioConsoleBasic,
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
	h.Browser = discoverBrowser(ctx, logger)

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
	if err := h.apply(ctx, gatewayNamespace, "default",
		configPatch(gatewayConfigMap, cfg), memoryLimitPatch(gatewayDeployment, pgoGatewayMemoryLimit)); err != nil {
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
