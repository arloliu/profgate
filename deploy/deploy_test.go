// Package deploy_test pins the shapes of the kustomize base manifests in deploy/base to what docs/specs/gateway.md requires:
// the golden ClusterRole, the hardened Deployment, and the NetworkPolicy and ConfigMap shapes the spec describes.
package deploy_test

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/arloliu/profgate/internal/config"
)

const baseDir = "base"

// ptr returns a pointer to v, for the pointer-typed fields k8s.io/api uses.
func ptr[T any](v T) *T { return &v }

// decode reads name from deploy/base and unmarshals it into a T through sigs.k8s.io/yaml,
// which converts YAML to JSON before decoding into the typed Kubernetes object
// so struct tags and defaulting behave the same way the API server sees them.
func decode[T any](t *testing.T, name string) T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(baseDir, name)) //nolint:gosec // name is a fixed literal from each test, not external input
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var v T
	if err := yaml.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
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
	if podSpec.TerminationGracePeriodSeconds == nil || *podSpec.TerminationGracePeriodSeconds != 120 {
		t.Errorf("TerminationGracePeriodSeconds = %v, want 120", podSpec.TerminationGracePeriodSeconds)
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

	if len(podSpec.Volumes) != 1 {
		t.Fatalf("Volumes = %+v, want exactly one", podSpec.Volumes)
	}
	vol := podSpec.Volumes[0]
	if vol.ConfigMap == nil {
		t.Fatalf("Volumes[0] = %+v, want a ConfigMap volume", vol)
	}
	cm := decode[corev1.ConfigMap](t, "configmap.yaml")
	if vol.ConfigMap.Name != cm.Name {
		t.Errorf("Volumes[0].ConfigMap.Name = %q, want it to match configmap.yaml's metadata.name %q", vol.ConfigMap.Name, cm.Name)
	}

	if len(c.VolumeMounts) != 1 {
		t.Fatalf("VolumeMounts = %+v, want exactly one", c.VolumeMounts)
	}
	mount := c.VolumeMounts[0]
	if mount.MountPath != "/etc/profgate" {
		t.Errorf("VolumeMounts[0].MountPath = %q, want /etc/profgate", mount.MountPath)
	}
	if !mount.ReadOnly {
		t.Error("VolumeMounts[0].ReadOnly = false, want true")
	}
	if vol.ConfigMap != nil && mount.Name != vol.Name {
		t.Errorf("VolumeMounts[0].Name = %q, want it to match the ConfigMap volume %q", mount.Name, vol.Name)
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
}

func TestAppExampleNetworkPolicy(t *testing.T) {
	np := decode[networkingv1.NetworkPolicy](t, "networkpolicy-app-example.yaml")

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
