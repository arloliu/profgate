// Package deploy_test pins the shapes of the kustomize base manifests in deploy/base to what docs/specs/gateway.md requires:
// the golden ClusterRole, the hardened Deployment, and the NetworkPolicy and ConfigMap shapes the spec describes.
package deploy_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nats-io/nats-server/v2/conf"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/arloliu/profgate/internal/config"
)

const baseDir = "base"

// credsSecretName is the Secret the operator creates from their NATS account
// credentials; the Deployment mounts it optionally, so the base stays
// deployable while PGO is off and nothing has created it.
const credsSecretName = "profgate-nats-creds" //nolint:gosec // the Secret's name, not its contents

// credsMountPath is where the credentials file appears in the container.
const credsMountPath = "/etc/profgate/nats/" //nolint:gosec // a mount path, not a credential

// tlsSecretName is the Secret the chart mounts the API listener's certificate
// from; the base serves plain HTTP and ships only a commented example of it.
const tlsSecretName = "profgate-tls" //nolint:gosec // the Secret's name, not its contents

// authSecretName is the Secret an auth.mode's files come from -- the users
// file, the cookie key file, the issuer CA, and a client secret, whichever a
// mode needs.
// The Deployment mounts it optionally, so the base stays deployable while
// authentication is disabled and nothing has created it.
const authSecretName = "profgate-auth" //nolint:gosec // the Secret's name, not its contents

// authMountPath is where the authentication files appear in the container.
const authMountPath = "/etc/profgate/auth/" //nolint:gosec // a mount path, not a credential

// ptr returns a pointer to v, for the pointer-typed fields k8s.io/api uses.
func ptr[T any](v T) *T { return &v }

// decode reads name from deploy/base and unmarshals it into a T through sigs.k8s.io/yaml,
// which converts YAML to JSON before decoding into the typed Kubernetes object
// so struct tags and defaulting behave the same way the API server sees them.
func decode[T any](t *testing.T, name string) T {
	t.Helper()
	return decodePath[T](t, filepath.Join(baseDir, name))
}

// decodePath is decode for a file outside deploy/base, addressed relative to
// this package directory.
func decodePath[T any](t *testing.T, path string) T {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // path is a fixed literal from each test, not external input
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v T
	if err := yaml.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return v
}

func TestClusterRoleTuples(t *testing.T) {
	cr := decode[rbacv1.ClusterRole](t, "clusterrole.yaml")

	type tuple struct{ group, resource, verb string }
	want := map[tuple]bool{
		{"", "services", "list"}:                        true,
		{"", "services", "watch"}:                       true,
		{"", "pods", "get"}:                             true,
		{"", "pods", "list"}:                            true,
		{"", "pods", "watch"}:                           true,
		{"discovery.k8s.io", "endpointslices", "list"}:  true,
		{"discovery.k8s.io", "endpointslices", "watch"}: true,
	}

	got := map[tuple]bool{}
	for _, rule := range cr.Rules {
		if len(rule.ResourceNames) != 0 || len(rule.NonResourceURLs) != 0 {
			t.Fatalf("rule %+v carries resourceNames or nonResourceURLs, which the boundary forbids", rule)
		}
		for _, g := range rule.APIGroups {
			for _, r := range rule.Resources {
				for _, v := range rule.Verbs {
					if g == "*" || r == "*" || v == "*" {
						t.Fatalf("rule %+v contains a wildcard, which the boundary forbids", rule)
					}
					got[tuple{g, r, v}] = true
				}
			}
		}
	}

	if len(got) != len(want) {
		t.Fatalf("ClusterRole has %d distinct tuples, want %d: got %v", len(got), len(want), got)
	}
	for tp := range want {
		if !got[tp] {
			t.Errorf("ClusterRole is missing tuple %+v", tp)
		}
	}
	for tp := range got {
		if !want[tp] {
			t.Errorf("ClusterRole has unexpected tuple %+v", tp)
		}
	}
}

func TestClusterRoleBinding(t *testing.T) {
	crb := decode[rbacv1.ClusterRoleBinding](t, "clusterrolebinding.yaml")

	wantRef := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "profgate"}
	if crb.RoleRef != wantRef {
		t.Errorf("RoleRef = %+v, want %+v", crb.RoleRef, wantRef)
	}

	if len(crb.Subjects) != 1 {
		t.Fatalf("Subjects = %+v, want exactly one", crb.Subjects)
	}
	wantSubject := rbacv1.Subject{Kind: "ServiceAccount", Name: "profgate", Namespace: "profgate"}
	if crb.Subjects[0] != wantSubject {
		t.Errorf("Subjects[0] = %+v, want %+v", crb.Subjects[0], wantSubject)
	}
}

func TestDeployment(t *testing.T) {
	dep := decode[appsv1.Deployment](t, "deployment.yaml")
	podSpec := dep.Spec.Template.Spec

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("Replicas = %v, want 2", dep.Spec.Replicas)
	}
	if len(podSpec.Containers) != 1 {
		t.Fatalf("Containers = %+v, want exactly one", podSpec.Containers)
	}
	c := podSpec.Containers[0]

	if c.Image != "ghcr.io/arloliu/profgate:latest" {
		t.Errorf("Image = %q, want ghcr.io/arloliu/profgate:latest", c.Image)
	}

	wantSecurityContext := &corev1.SecurityContext{
		RunAsNonRoot:             ptr(true),
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	if !reflect.DeepEqual(c.SecurityContext, wantSecurityContext) {
		t.Errorf("SecurityContext = %+v, want exactly the spec's Container block %+v", c.SecurityContext, wantSecurityContext)
	}

	wantPorts := map[string]int32{"api": 8080, "ops": 9090}
	if len(c.Ports) != len(wantPorts) {
		t.Fatalf("Ports = %+v, want exactly %v", c.Ports, wantPorts)
	}
	for _, p := range c.Ports {
		want, ok := wantPorts[p.Name]
		if !ok {
			t.Errorf("unexpected port name %q", p.Name)
			continue
		}
		if p.ContainerPort != want {
			t.Errorf("port %q = %d, want %d", p.Name, p.ContainerPort, want)
		}
	}

	hasArg := func(want string) bool {
		for _, a := range c.Args {
			if a == want {
				return true
			}
		}
		return false
	}
	if !hasArg("--config") {
		t.Errorf("Args = %v, want it to contain --config", c.Args)
	}
	if !hasArg("/etc/profgate/config.yaml") {
		t.Errorf("Args = %v, want it to contain /etc/profgate/config.yaml", c.Args)
	}

	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil {
		t.Fatal("ReadinessProbe.HTTPGet is nil, want /readyz on port ops")
	}
	if c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("ReadinessProbe path = %q, want /readyz", c.ReadinessProbe.HTTPGet.Path)
	}
	if c.ReadinessProbe.HTTPGet.Port != intstr.FromString("ops") {
		t.Errorf("ReadinessProbe port = %+v, want the ops port (9090)", c.ReadinessProbe.HTTPGet.Port)
	}

	// fsGroup is what makes a Secret volume readable by the non-root image:
	// the kubelet only changes a volume's group ownership when it is set.
	if podSpec.SecurityContext == nil || podSpec.SecurityContext.FSGroup == nil || *podSpec.SecurityContext.FSGroup != 65532 {
		t.Errorf("pod securityContext.fsGroup = %+v, want 65532, the uid/gid the image runs as", podSpec.SecurityContext)
	}

	if len(podSpec.Volumes) != 3 {
		t.Fatalf("Volumes = %+v, want exactly three: the config, the NATS credentials, and the authentication files", podSpec.Volumes)
	}
	vols := map[string]corev1.Volume{}
	for _, v := range podSpec.Volumes {
		vols[v.Name] = v
	}

	vol, ok := vols["config"]
	if !ok || vol.ConfigMap == nil {
		t.Fatalf("Volumes = %+v, want a ConfigMap volume named config", podSpec.Volumes)
	}
	cm := decode[corev1.ConfigMap](t, "configmap.yaml")
	if vol.ConfigMap.Name != cm.Name {
		t.Errorf("the config volume names ConfigMap %q, want configmap.yaml's metadata.name %q", vol.ConfigMap.Name, cm.Name)
	}

	creds, ok := vols[credsSecretName]
	if !ok || creds.Secret == nil {
		t.Fatalf("Volumes = %+v, want a Secret volume named %s", podSpec.Volumes, credsSecretName)
	}
	if creds.Secret.SecretName != credsSecretName {
		t.Errorf("the credentials volume names Secret %q, want %q", creds.Secret.SecretName, credsSecretName)
	}
	// 0440 with fsGroup 65532 is the narrowest mode the gateway can read.
	if creds.Secret.DefaultMode == nil || *creds.Secret.DefaultMode != 0o440 {
		t.Errorf("the credentials volume defaultMode = %v, want 0440", creds.Secret.DefaultMode)
	}
	// optional is what keeps the base deployable while PGO is off and the
	// operator has created no Secret.
	if creds.Secret.Optional == nil || !*creds.Secret.Optional {
		t.Errorf("the credentials volume optional = %v, want true", creds.Secret.Optional)
	}

	auth, ok := vols[authSecretName]
	if !ok || auth.Secret == nil {
		t.Fatalf("Volumes = %+v, want a Secret volume named %s", podSpec.Volumes, authSecretName)
	}
	if auth.Secret.SecretName != authSecretName {
		t.Errorf("the authentication volume names Secret %q, want %q", auth.Secret.SecretName, authSecretName)
	}
	if auth.Secret.DefaultMode == nil || *auth.Secret.DefaultMode != 0o440 {
		t.Errorf("the authentication volume defaultMode = %v, want 0440", auth.Secret.DefaultMode)
	}
	// optional is what keeps the base deployable while auth.mode is disabled
	// and the operator has created no Secret.
	if auth.Secret.Optional == nil || !*auth.Secret.Optional {
		t.Errorf("the authentication volume optional = %v, want true", auth.Secret.Optional)
	}

	// fsGroup is asserted once above; it is what makes this Secret volume
	// readable too, the same way it makes the NATS credentials readable.
	if podSpec.SecurityContext == nil || podSpec.SecurityContext.FSGroup == nil || *podSpec.SecurityContext.FSGroup != 65532 {
		t.Errorf("pod securityContext.fsGroup = %+v, want it unchanged at 65532", podSpec.SecurityContext)
	}

	if len(c.VolumeMounts) != 3 {
		t.Fatalf("VolumeMounts = %+v, want exactly three: the config, the NATS credentials, and the authentication files", c.VolumeMounts)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m
	}

	mount, ok := mounts["config"]
	if !ok {
		t.Fatalf("VolumeMounts = %+v, want one named config", c.VolumeMounts)
	}
	if mount.MountPath != "/etc/profgate" {
		t.Errorf("the config mount path = %q, want /etc/profgate", mount.MountPath)
	}
	if !mount.ReadOnly {
		t.Error("the config mount is writable, want readOnly")
	}

	credsMount, ok := mounts[credsSecretName]
	if !ok {
		t.Fatalf("VolumeMounts = %+v, want one named %s", c.VolumeMounts, credsSecretName)
	}
	if credsMount.MountPath != credsMountPath {
		t.Errorf("the credentials mount path = %q, want %q", credsMount.MountPath, credsMountPath)
	}
	if !credsMount.ReadOnly {
		t.Error("the credentials mount is writable, want readOnly")
	}

	authMount, ok := mounts[authSecretName]
	if !ok {
		t.Fatalf("VolumeMounts = %+v, want one named %s", c.VolumeMounts, authSecretName)
	}
	if authMount.MountPath != authMountPath {
		t.Errorf("the authentication mount path = %q, want %q", authMount.MountPath, authMountPath)
	}
	if !authMount.ReadOnly {
		t.Error("the authentication mount is writable, want readOnly")
	}
}

func TestService(t *testing.T) {
	svc := decode[corev1.Service](t, "service.yaml")

	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Type = %q, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("Ports = %+v, want exactly one", svc.Spec.Ports)
	}
	p := svc.Spec.Ports[0]
	if p.Port != 8080 || p.Name != "api" {
		t.Errorf("Ports[0] = %+v, want port 8080 named api", p)
	}
}

func TestGatewayNetworkPolicy(t *testing.T) {
	np := decode[networkingv1.NetworkPolicy](t, "networkpolicy-gateway.yaml")

	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("PolicyTypes = %v, want exactly [Ingress]", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Egress) != 0 {
		t.Errorf("Egress = %+v, want none", np.Spec.Egress)
	}
	if len(np.Spec.Ingress) != 2 {
		t.Fatalf("Ingress = %+v, want exactly two rules", np.Spec.Ingress)
	}

	checkRule := func(t *testing.T, rule networkingv1.NetworkPolicyIngressRule, port int32, namespace string) {
		t.Helper()
		if len(rule.Ports) != 1 || rule.Ports[0].Port == nil || rule.Ports[0].Port.IntVal != port {
			t.Errorf("Ports = %+v, want exactly port %d", rule.Ports, port)
		}
		if len(rule.From) != 1 {
			t.Fatalf("From = %+v, want exactly one peer", rule.From)
		}
		peer := rule.From[0]
		if peer.PodSelector != nil {
			t.Errorf("From[0].PodSelector = %+v, want none", peer.PodSelector)
		}
		if peer.NamespaceSelector == nil {
			t.Fatal("From[0].NamespaceSelector is nil")
		}
		want := map[string]string{"kubernetes.io/metadata.name": namespace}
		if len(peer.NamespaceSelector.MatchLabels) != 1 || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != namespace {
			t.Errorf("From[0].NamespaceSelector.MatchLabels = %v, want %v", peer.NamespaceSelector.MatchLabels, want)
		}
	}
	checkRule(t, np.Spec.Ingress[0], 8080, "ingress-nginx")
	checkRule(t, np.Spec.Ingress[1], 9090, "monitoring")

	// The live policy stays ingress-only; a commented example is the only
	// mention of Egress, so applying the base never isolates a destination
	// the gateway or auth.mode oidc needs.
	b, err := os.ReadFile(filepath.Join(baseDir, "networkpolicy-gateway.yaml"))
	if err != nil {
		t.Fatalf("read networkpolicy-gateway.yaml: %v", err)
	}
	var sawEgress bool
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "Egress") {
			t.Errorf("networkpolicy-gateway.yaml line %q is live YAML naming Egress, want it commented", line)
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "Egress") {
			sawEgress = true
			break
		}
	}
	if !sawEgress {
		t.Error("networkpolicy-gateway.yaml never mentions Egress, want a commented example rule for the issuer")
	}
	if !strings.Contains(string(b), "issuer") {
		t.Error("networkpolicy-gateway.yaml's commented Egress block never names the issuer")
	}
}

// TestEgressCommentNamesRequiredDestinations pins the commented Egress
// example the base NetworkPolicy and the chart's values.yaml both carry:
// once a policy selects the gateway Pods for egress too, every destination
// needs its own rule or the gateway stops working, and the comment has to
// name all of them, not just the issuer that motivated it.
func TestEgressCommentNamesRequiredDestinations(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"base", filepath.Join(baseDir, "networkpolicy-gateway.yaml")},
		{"chart values", filepath.Join(chartDir, "values.yaml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}
			body := string(b)
			for _, want := range []string{"DNS", "Kubernetes API", "pprof port", "NATS", "issuer"} {
				if !strings.Contains(body, want) {
					t.Errorf("%s does not name %q in its egress comment", tc.path, want)
				}
			}
		})
	}
}

// TestAppExampleNetworkPolicy covers the application-side policy template.
// The file is a template for the application's namespace, so it lives outside
// deploy/base and must never be applied by `kubectl apply -k deploy/base`:
// the base would otherwise mutate whatever workload matches the example
// selector in the target namespace.
func TestAppExampleNetworkPolicy(t *testing.T) {
	const file = "networkpolicy-app-example.yaml"

	if _, err := os.Stat(filepath.Join(baseDir, file)); err == nil {
		t.Errorf("%s is inside deploy/base, where the resource list would have to name it", file)
	}
	k := decode[kustomization](t, "kustomization.yaml")
	for _, r := range k.Resources {
		if r == file {
			t.Errorf("kustomization.yaml resources name %s; the example must not be applied with the base", file)
		}
	}

	np := decodePath[networkingv1.NetworkPolicy](t, file)

	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("PolicyTypes = %v, want exactly [Ingress]", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Egress) != 0 {
		t.Errorf("Egress = %+v, want none", np.Spec.Egress)
	}
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("Ingress = %+v, want exactly one rule", np.Spec.Ingress)
	}

	rule := np.Spec.Ingress[0]
	if len(rule.Ports) != 1 {
		t.Fatalf("Ports = %+v, want exactly one", rule.Ports)
	}
	port := rule.Ports[0]
	if port.Port == nil || port.Port.IntVal != 6060 {
		t.Errorf("Port = %+v, want 6060", port.Port)
	}
	if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP {
		t.Errorf("Protocol = %v, want TCP", port.Protocol)
	}

	if len(rule.From) != 1 {
		t.Fatalf("From = %+v, want exactly one peer", rule.From)
	}
	peer := rule.From[0]
	if peer.NamespaceSelector == nil {
		t.Fatal("From[0].NamespaceSelector is nil")
	}
	if len(peer.NamespaceSelector.MatchLabels) != 1 || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "profgate" {
		t.Errorf("From[0].NamespaceSelector.MatchLabels = %v, want kubernetes.io/metadata.name=profgate", peer.NamespaceSelector.MatchLabels)
	}
	if peer.PodSelector == nil {
		t.Fatal("From[0].PodSelector is nil")
	}
	if len(peer.PodSelector.MatchLabels) != 1 || peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "profgate" {
		t.Errorf("From[0].PodSelector.MatchLabels = %v, want app.kubernetes.io/name=profgate", peer.PodSelector.MatchLabels)
	}
}

func TestConfigMap(t *testing.T) {
	cm := decode[corev1.ConfigMap](t, "configmap.yaml")

	body, ok := cm.Data["config.yaml"]
	if !ok {
		t.Fatal(`ConfigMap data has no "config.yaml" key`)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write embedded config.yaml: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(embedded config.yaml) error = %v", err)
	}

	realm, ok := cfg.Realms[cfg.Auth.AnonymousRealm]
	if !ok {
		t.Fatalf("anonymous realm %q is not in Realms %+v", cfg.Auth.AnonymousRealm, cfg.Realms)
	}
	for name, list := range map[string][]string{
		"namespaces": realm.Namespaces,
		"services":   realm.Services,
		"profiles":   realm.Profiles,
	} {
		if len(list) != 1 || list[0] != "*" {
			t.Errorf("anonymous realm %s = %v, want exactly [\"*\"]", name, list)
		}
	}

	// The shipped list is empty, which admits only the configured default.
	if !strings.Contains(body, "allowedSelections: []") {
		t.Errorf("config.yaml does not contain \"allowedSelections: []\":\n%s", body)
	}
	if len(cfg.Discovery.Pprof.AllowedSelections) != 0 {
		t.Errorf("discovery.pprof.allowedSelections = %v, want empty", cfg.Discovery.Pprof.AllowedSelections)
	}

	if cfg.UI.Enabled {
		t.Errorf("UI.Enabled = %v, want false: the console is off by default", cfg.UI.Enabled)
	}
	if !strings.Contains(body, "# ui:") {
		t.Errorf("config.yaml does not contain a commented \"# ui:\" block:\n%s", body)
	}
	if !strings.Contains(body, "#   enabled: true") {
		t.Errorf("config.yaml's commented ui block does not contain \"#   enabled: true\":\n%s", body)
	}
}

// kustomization is the minimal shape deploy_test.go needs from kustomization.yaml:
// the list of files it wires into the base.
type kustomization struct {
	Resources []string `json:"resources"`
}

func TestKustomizationListsEveryFile(t *testing.T) {
	k := decode[kustomization](t, "kustomization.yaml")

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		t.Fatalf("read %s: %v", baseDir, err)
	}
	var wantFiles []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == "kustomization.yaml" {
			continue
		}
		wantFiles = append(wantFiles, e.Name())
	}
	sort.Strings(wantFiles)

	gotFiles := append([]string(nil), k.Resources...)
	sort.Strings(gotFiles)

	if len(gotFiles) != len(wantFiles) {
		t.Fatalf("kustomization.yaml resources = %v, want exactly the directory's files %v", gotFiles, wantFiles)
	}
	for i := range wantFiles {
		if gotFiles[i] != wantFiles[i] {
			t.Errorf("kustomization.yaml resources = %v, want exactly the directory's files %v", gotFiles, wantFiles)
			break
		}
	}
}

// backtickedSubject matches one `subject` span of a Markdown table cell.
var backtickedSubject = regexp.MustCompile("`([^`]+)`")

// specPermissions reads the NATS permission table of docs/specs/pgo.md and
// returns the publish and subscribe subject sets it grants.
// Only table rows count: the prose around the table names subjects the account
// must not hold, and a sweep over the whole section would collect those too.
func specPermissions(t *testing.T) (map[string]bool, map[string]bool) {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("..", "docs", "specs", "pgo.md"))
	if err != nil {
		t.Fatalf("read the PGO spec: %v", err)
	}

	publish, subscribe := map[string]bool{}, map[string]bool{}
	var inSection, inTable bool
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "#") {
			if inSection {
				break
			}
			inSection = strings.Contains(line, "NATS permissions")
			continue
		}
		if !inSection {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		inTable = true

		cols := strings.Split(strings.Trim(line, "|"), "|")
		if len(cols) != 2 {
			continue
		}
		var into map[string]bool
		switch strings.TrimSpace(cols[0]) {
		case "publish":
			into = publish
		case "subscribe":
			into = subscribe
		default:
			continue
		}
		for _, m := range backtickedSubject.FindAllStringSubmatch(cols[1], -1) {
			into[m[1]] = true
		}
	}

	if !publish["$JS.API.INFO"] || len(subscribe) == 0 {
		t.Fatalf("the spec's permission table did not parse: publish = %v, subscribe = %v", publish, subscribe)
	}

	return publish, subscribe
}

// fragmentSubjects returns the allow list of one permission direction of the
// shipped account fragment.
func fragmentSubjects(t *testing.T, perms map[string]any, direction string) map[string]bool {
	t.Helper()

	block, ok := perms[direction].(map[string]any)
	if !ok {
		t.Fatalf("account.conf permissions has no %s block: %v", direction, perms)
	}
	allow, ok := block["allow"].([]any)
	if !ok {
		t.Fatalf("account.conf %s block has no allow list: %v", direction, block)
	}

	got := map[string]bool{}
	for _, v := range allow {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("account.conf %s.allow holds %v, want subject strings", direction, v)
		}
		if got[s] {
			t.Errorf("account.conf %s.allow lists %q twice", direction, s)
		}
		got[s] = true
	}

	return got
}

// TestNATSAccountFragment pins deploy/nats/account.conf to the spec's
// permission table the way TestClusterRoleTuples pins the ClusterRole:
// the fragment is what an operator pastes into their NATS account, so a
// subject the spec does not grant is a widening of the permission boundary and
// a subject it grants but the fragment omits is a gateway that fails preflight.
func TestNATSAccountFragment(t *testing.T) {
	parsed, err := conf.ParseFile(filepath.Join("nats", "account.conf"))
	if err != nil {
		t.Fatalf("parse nats/account.conf: %v", err)
	}
	perms, ok := parsed["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("nats/account.conf has no permissions block: %v", parsed)
	}

	wantPublish, wantSubscribe := specPermissions(t)
	for _, tc := range []struct {
		direction string
		want      map[string]bool
	}{
		{direction: "publish", want: wantPublish},
		{direction: "subscribe", want: wantSubscribe},
	} {
		t.Run(tc.direction, func(t *testing.T) {
			got := fragmentSubjects(t, perms, tc.direction)
			for s := range tc.want {
				if !got[s] {
					t.Errorf("account.conf %s is missing the spec's subject %q", tc.direction, s)
				}
			}
			for s := range got {
				if !tc.want[s] {
					t.Errorf("account.conf %s grants %q, which the spec's table does not list", tc.direction, s)
				}
			}
		})
	}
}

// TestSecretExamplesAreCommented holds both example Secrets to being examples
// and nothing else: each carries a credential -- a NATS account, a private key
// -- so every line is a comment and `kubectl apply -f deploy/` cannot create
// either. They live outside deploy/base because the base's resource list must
// name every file there, and a comment-only file cannot be applied.
func TestSecretExamplesAreCommented(t *testing.T) {
	for _, tc := range []struct {
		file   string
		secret string
		// names holds extra text every line of tc.file must be checked for
		// presence of, beyond the Secret's own name: the data keys an
		// auth.mode's files appear under.
		names []string
	}{
		{file: "secret-nats-example.yaml", secret: credsSecretName},
		{file: "secret-tls-example.yaml", secret: tlsSecretName},
		{
			file:   "secret-auth-example.yaml",
			secret: authSecretName,
			names:  []string{"users.yaml", "cookie.key", "issuer-ca.crt", "client-secret"},
		},
	} {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(tc.file) //nolint:gosec // the file name is a fixed literal from this table
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}

			var mentionsSecret bool
			for i, line := range strings.Split(string(b), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				if !strings.HasPrefix(trimmed, "#") {
					t.Errorf("%s line %d is live YAML, want every line commented: %q", tc.file, i+1, line)
				}
				if strings.Contains(trimmed, tc.secret) {
					mentionsSecret = true
				}
			}
			if !mentionsSecret {
				t.Errorf("%s never names the Secret %q it is an example of", tc.file, tc.secret)
			}
			for _, name := range tc.names {
				if !strings.Contains(string(b), name) {
					t.Errorf("%s never names %q", tc.file, name)
				}
			}

			if _, err := os.Stat(filepath.Join(baseDir, tc.file)); err == nil {
				t.Errorf("%s is inside deploy/base, where the resource list would have to name it", tc.file)
			}
		})
	}
}

// TestDeploymentGracePeriod ties the grace period to the drain the ConfigMap
// this base ships actually asks for, so raising server.drainDelay or a profile
// limit without raising the period fails here instead of as a SIGKILL through
// an in-flight profile.
func TestDeploymentGracePeriod(t *testing.T) {
	cm := decode[corev1.ConfigMap](t, "configmap.yaml")
	body, ok := cm.Data["config.yaml"]
	if !ok {
		t.Fatal(`ConfigMap data has no "config.yaml" key`)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write embedded config.yaml: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(embedded config.yaml) error = %v", err)
	}

	dep := decode[appsv1.Deployment](t, "deployment.yaml")
	grace := dep.Spec.Template.Spec.TerminationGracePeriodSeconds
	want := int64(cfg.RequiredGracePeriod().Seconds())
	if grace == nil || *grace != want {
		t.Errorf("terminationGracePeriodSeconds = %v, want %d from the ConfigMap's own drain delay and limits", grace, want)
	}
}

// TestDeploymentMemoryLimit ties the container's memory limit to the spec's
// sizing formula applied to the configuration this base actually ships,
// so raising a pgo.limits ceiling in the ConfigMap without raising the limit
// fails here instead of as an out-of-memory kill during a merge.
func TestDeploymentMemoryLimit(t *testing.T) {
	cm := decode[corev1.ConfigMap](t, "configmap.yaml")
	body, ok := cm.Data["config.yaml"]
	if !ok {
		t.Fatal(`ConfigMap data has no "config.yaml" key`)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write embedded config.yaml: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(embedded config.yaml) error = %v", err)
	}

	dep := decode[appsv1.Deployment](t, "deployment.yaml")
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("Containers = %+v, want exactly one", dep.Spec.Template.Spec.Containers)
	}
	limits := dep.Spec.Template.Spec.Containers[0].Resources.Limits
	mem, ok := limits[corev1.ResourceMemory]
	if !ok {
		t.Fatalf("resources.limits = %v, want a memory limit", limits)
	}
	if got, want := mem.Value(), cfg.PGOMemoryBytes(); got != want {
		t.Errorf("resources.limits.memory = %d bytes, want %d from the ConfigMap's own pgo.limits", got, want)
	}
}
