//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// pgoGatewayMemoryLimit is what `profgate config validate` prints for the configuration gatewayConfig writes with a NATS URL:
// collection on with every sizing ceiling at its shipped default,
// a 1440Mi working set over the gateway's own 512Mi.
// The base ships collection off at 512Mi.
const pgoGatewayMemoryLimit = "1952Mi"

// memoryLimitPatch raises the profgate container's memory limit on the named
// Deployment to limit, for a gateway whose configuration turns collection on
// over a base sized for collection off.
func memoryLimitPatch(deployment, limit string) patch {
	return patch{file: "memory-limit.yaml", kind: "Deployment", name: deployment, body: fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  template:
    spec:
      containers:
        - name: profgate
          resources:
            limits:
              memory: %s
`, deployment, limit)}
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
