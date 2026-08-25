package deploy_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/arloliu/profgate/internal/config"
)

// chartDir is the Helm chart, the second shipped deployment surface next to
// the kustomize base these tests also cover.
const chartDir = "chart/profgate"

// helmBin returns the helm executable, or skips.
// mise.toml pins helm, so `mise run test` always has it;
// a bare `go test` on a machine without it skips these tests rather than
// reporting a failure the developer cannot act on.
func helmBin(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on PATH; run the suite through `mise run test`, which uses the pinned helm")
	}

	return path
}

// runHelm runs helm with args and returns its stdout.
func runHelm(t *testing.T, args ...string) []byte {
	t.Helper()

	//nolint:gosec // args come from the test's own literals, not external input
	cmd := exec.CommandContext(t.Context(), helmBin(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}

	return stdout.Bytes()
}

// render templates one file of the chart and decodes the single object it
// produces into a T. One template at a time keeps the caller free of
// multi-document splitting, and every assertion here is about one object.
func render[T any](t *testing.T, template string, values ...string) T {
	t.Helper()

	args := []string{
		"template", "profgate", chartDir,
		"--namespace", "profgate",
		"-s", "templates/" + template,
	}
	args = append(args, values...)

	var v T
	if err := yaml.Unmarshal(runHelm(t, args...), &v); err != nil {
		t.Fatalf("unmarshal rendered %s: %v", template, err)
	}

	return v
}

// pgoValues turns PGO collection on with a credentials file that exists,
// because config.Load opens the file nats.credsFile names.
func pgoValues(t *testing.T) []string {
	t.Helper()

	creds := filepath.Join(t.TempDir(), "nats.creds")
	if err := os.WriteFile(creds, []byte("-----BEGIN NATS USER JWT-----\n"), 0o600); err != nil {
		t.Fatalf("write a stand-in credentials file: %v", err)
	}

	return []string{
		"--set", "pgo.enabled=true",
		"--set", "nats.url=nats://nats.profgate.svc:4222",
		"--set", "nats.credsFile=" + creds,
	}
}

// tlsValues turns HTTPS on with a mount path that exists on this machine,
// because config.Load opens the two files server.tls names.
// The mount path is the only lever: the certificate paths are derived from it
// and the key names, so there is no configuration key to point elsewhere.
func tlsValues(t *testing.T) []string {
	t.Helper()

	dir := t.TempDir()
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
			t.Fatalf("write a stand-in %s: %v", name, err)
		}
	}

	return []string{"--set", "tls.enabled=true", "--set", "tls.mountPath=" + dir}
}

// volumeNamed returns the pod's volume of that name, or nil.
func volumeNamed(spec corev1.PodSpec, name string) *corev1.Volume {
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == name {
			return &spec.Volumes[i]
		}
	}

	return nil
}

// mountNamed returns the one container's volume mount of that name, or nil.
func mountNamed(spec corev1.PodSpec, name string) *corev1.VolumeMount {
	mounts := spec.Containers[0].VolumeMounts
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}

	return nil
}

// loadRenderedConfig loads the chart's ConfigMap through internal/config,
// which is both how the memory arithmetic is checked against the gateway's own
// formula and the proof that the rendered file parses at all.
func loadRenderedConfig(t *testing.T, values ...string) *config.Config {
	t.Helper()

	cm := render[corev1.ConfigMap](t, "configmap.yaml", values...)
	body, ok := cm.Data["config.yaml"]
	if !ok {
		t.Fatalf(`the rendered ConfigMap has no "config.yaml" key: %v`, cm.Data)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the rendered config.yaml: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(rendered config.yaml) error = %v\n%s", err, body)
	}

	return cfg
}

// containerMemoryLimit returns the one container's memory limit.
func containerMemoryLimit(t *testing.T, dep appsv1.Deployment) resource.Quantity {
	t.Helper()

	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("Containers = %+v, want exactly one", dep.Spec.Template.Spec.Containers)
	}
	limits := dep.Spec.Template.Spec.Containers[0].Resources.Limits
	mem, ok := limits[corev1.ResourceMemory]
	if !ok {
		t.Fatalf("resources.limits = %v, want a memory limit", limits)
	}

	return mem
}

// TestChartLint holds the chart to what `helm lint` accepts, on both paths:
// PGO off, which is the default an operator installs first, and PGO on, where
// the NATS keys and the derived memory limit come into play.
func TestChartLint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{name: "defaults"},
		{name: "pgo enabled", values: []string{"--set", "pgo.enabled=true", "--set", "nats.url=nats://nats.profgate.svc:4222"}},
		{name: "tls enabled", values: []string{"--set", "tls.enabled=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"lint", chartDir}, tc.values...)
			out := runHelm(t, args...)
			if !bytes.Contains(out, []byte("0 chart(s) failed")) {
				t.Errorf("helm lint did not pass:\n%s", out)
			}
		})
	}
}

// TestChartClusterRoleMatchesBase pins the chart's ClusterRole to the kustomize
// base's, rule for rule. Both are the permission boundary, and a chart that
// grants one verb the base does not is a second, quieter place to widen it.
func TestChartClusterRoleMatchesBase(t *testing.T) {
	want := decode[rbacv1.ClusterRole](t, "clusterrole.yaml")
	got := render[rbacv1.ClusterRole](t, "clusterrole.yaml")

	if !reflect.DeepEqual(got.Rules, want.Rules) {
		t.Errorf("the chart's ClusterRole rules = %+v, want deploy/base/clusterrole.yaml's %+v", got.Rules, want.Rules)
	}
}

// TestChartClusterResourcesAreReleaseScoped proves two releases can share a
// cluster: the ClusterRole and the ClusterRoleBinding are cluster-scoped, so a
// fixed name would make the second install silently take over the first's RBAC.
func TestChartClusterResourcesAreReleaseScoped(t *testing.T) {
	renderAs := func(t *testing.T, release string) (string, rbacv1.ClusterRoleBinding) {
		t.Helper()
		args := []string{"template", release, chartDir, "--namespace", release + "-ns"}
		crb := rbacv1.ClusterRoleBinding{}
		if err := yaml.Unmarshal(runHelm(t, append(args, "-s", "templates/clusterrolebinding.yaml")...), &crb); err != nil {
			t.Fatalf("unmarshal the rendered ClusterRoleBinding: %v", err)
		}
		cr := rbacv1.ClusterRole{}
		if err := yaml.Unmarshal(runHelm(t, append(args, "-s", "templates/clusterrole.yaml")...), &cr); err != nil {
			t.Fatalf("unmarshal the rendered ClusterRole: %v", err)
		}

		return cr.Name, crb
	}

	firstRole, firstBinding := renderAs(t, "alpha")
	secondRole, secondBinding := renderAs(t, "beta")

	if firstRole == secondRole {
		t.Errorf("both releases render the ClusterRole %q; two releases in one cluster would collide", firstRole)
	}
	if firstBinding.Name == secondBinding.Name {
		t.Errorf("both releases render the ClusterRoleBinding %q; two releases in one cluster would collide", firstBinding.Name)
	}

	for _, tc := range []struct {
		release string
		role    string
		binding rbacv1.ClusterRoleBinding
	}{
		{release: "alpha", role: firstRole, binding: firstBinding},
		{release: "beta", role: secondRole, binding: secondBinding},
	} {
		t.Run(tc.release, func(t *testing.T) {
			if tc.binding.RoleRef.Name != tc.role {
				t.Errorf("RoleRef.Name = %q, want the release's own ClusterRole %q", tc.binding.RoleRef.Name, tc.role)
			}
			if len(tc.binding.Subjects) != 1 {
				t.Fatalf("Subjects = %+v, want exactly one", tc.binding.Subjects)
			}
			if ns := tc.binding.Subjects[0].Namespace; ns != tc.release+"-ns" {
				t.Errorf("the subject's namespace = %q, want the release namespace %q", ns, tc.release+"-ns")
			}
		})
	}
}

// TestChartMemoryLimitIsDerived is the reason the chart renders the limit
// rather than carrying one: the number the templates compute is compared
// against config.PGOMemoryBytes applied to the configuration the same render
// produced, so an operator who raises a pgo.limits ceiling raises the limit
// with it and cannot end up with a container the merge will not fit in.
// The custom case moves all four inputs, which is what separates a real
// derivation from a hard-coded 4Gi that happens to match the defaults.
func TestChartMemoryLimitIsDerived(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{name: "shipped ceilings"},
		{
			name: "raised ceilings",
			values: []string{
				"--set", "pgo.limits.maxParallel=5",
				"--set", "pgo.limits.maxActiveCollections=3",
				"--set", "pgo.limits.maxSampleBytes=67108864",
				"--set", "pgo.limits.maxMergedBytes=134217728",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := append(pgoValues(t), tc.values...)

			cfg := loadRenderedConfig(t, values...)
			mem := containerMemoryLimit(t, render[appsv1.Deployment](t, "deployment.yaml", values...))

			if got, want := mem.Value(), cfg.PGOMemoryBytes(); got != want {
				t.Errorf("resources.limits.memory = %d bytes, want %d from the rendered ConfigMap's own pgo.limits", got, want)
			}
		})
	}
}

// TestChartMemoryLimitWithoutPGO covers the other path. The sizing formula
// reads pgo.limits and never pgo.enabled, so applying it with collection off
// would ask for the 4Gi a merge needs on a gateway that never merges;
// the chart uses a static limit there instead, and holds to limits.memory
// alone, the way the base does.
func TestChartMemoryLimitWithoutPGO(t *testing.T) {
	dep := render[appsv1.Deployment](t, "deployment.yaml")
	res := dep.Spec.Template.Spec.Containers[0].Resources

	if len(res.Requests) != 0 {
		t.Errorf("resources.requests = %v, want none, as in deploy/base", res.Requests)
	}
	if len(res.Limits) != 1 {
		t.Errorf("resources.limits = %v, want the memory limit alone", res.Limits)
	}
	want := resource.MustParse("512Mi")
	if got := containerMemoryLimit(t, dep); got.Value() != want.Value() {
		t.Errorf("resources.limits.memory = %s, want the chart's memoryLimitWithoutPGO %s", got.String(), want.String())
	}
}

// TestChartResourcesOverride covers the opt-out: a cluster with a LimitRange
// or a quota needs requests the derived path does not render, and an explicit
// block replaces it wholesale even where the derivation would apply.
func TestChartResourcesOverride(t *testing.T) {
	values := append(pgoValues(t),
		"--set", "resources.limits.memory=8Gi",
		"--set", "resources.requests.memory=1Gi",
		"--set", "resources.requests.cpu=500m",
	)
	res := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec.Containers[0].Resources

	if got := res.Limits[corev1.ResourceMemory]; got.String() != "8Gi" {
		t.Errorf("resources.limits.memory = %s, want the override 8Gi rather than the derived figure", got.String())
	}
	if got := res.Requests[corev1.ResourceMemory]; got.String() != "1Gi" {
		t.Errorf("resources.requests.memory = %s, want the override 1Gi", got.String())
	}
	if got := res.Requests[corev1.ResourceCPU]; got.String() != "500m" {
		t.Errorf("resources.requests.cpu = %s, want the override 500m", got.String())
	}
}

// TestChartGracePeriod ties the chart's default grace period to the drain its
// own default values ask for: server.drainDelay and the profile limits.
func TestChartGracePeriod(t *testing.T) {
	cfg := loadRenderedConfig(t)
	grace := render[appsv1.Deployment](t, "deployment.yaml").Spec.Template.Spec.TerminationGracePeriodSeconds
	want := int64(cfg.RequiredGracePeriod().Seconds())
	if grace == nil || *grace != want {
		t.Errorf("terminationGracePeriodSeconds = %v, want %d from the rendered configuration", grace, want)
	}
}

// TestChartConfigVolumeNamesTheConfigMap pins the reference between the two
// templates. The Deployment reaches the ConfigMap through two name helpers and
// the checksum reaches it through a template path, so a rename that misses one
// of them renders cleanly and produces a Pod that never starts.
func TestChartConfigVolumeNamesTheConfigMap(t *testing.T) {
	cm := render[corev1.ConfigMap](t, "configmap.yaml")
	podSpec := render[appsv1.Deployment](t, "deployment.yaml").Spec.Template.Spec

	var vol *corev1.Volume
	for i, v := range podSpec.Volumes {
		if v.Name == "config" {
			vol = &podSpec.Volumes[i]
			break
		}
	}
	if vol == nil || vol.ConfigMap == nil {
		t.Fatalf("Volumes = %+v, want a ConfigMap volume named config", podSpec.Volumes)
	}
	if vol.ConfigMap.Name != cm.Name {
		t.Errorf("the config volume names ConfigMap %q, want the rendered ConfigMap's %q", vol.ConfigMap.Name, cm.Name)
	}
}

// checksumAnnotation returns the pod template's checksum/config annotation.
func checksumAnnotation(t *testing.T, values ...string) string {
	t.Helper()

	return render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Annotations["checksum/config"]
}

// TestChartConfigChecksum covers the annotation the gateway's lack of a
// configuration reload makes necessary: the binary reads the file once at
// startup, so a `helm upgrade` that changes only the ConfigMap has to change
// the pod template too or it rolls nothing out. The negative half matters as
// much as the positive one -- a checksum over the whole pod template would
// pass the first assertion and restart every Pod for every unrelated edit.
func TestChartConfigChecksum(t *testing.T) {
	base := checksumAnnotation(t)
	if base == "" {
		t.Fatal("the pod template has no checksum/config annotation, so a configuration change would roll out nothing")
	}

	t.Run("a config change moves it", func(t *testing.T) {
		got := checksumAnnotation(t, "--set", "server.logLevel=debug")
		if got == base {
			t.Errorf("checksum/config is %q with and without a logLevel change; the upgrade would not restart any Pod", got)
		}
	})

	t.Run("an unrelated change leaves it alone", func(t *testing.T) {
		got := checksumAnnotation(t, "--set", "replicaCount=5")
		if got != base {
			t.Errorf("checksum/config = %q, want the unchanged %q: replicaCount is not part of the configuration", got, base)
		}
	})

	t.Run("turning TLS on moves it", func(t *testing.T) {
		// tls.enabled adds server.tls to the ConfigMap, and both paths are
		// restart-class, so the upgrade that turns HTTPS on has to roll the
		// Pods. Only the certificate's contents are re-read while running.
		if got := checksumAnnotation(t, tlsValues(t)...); got == base {
			t.Errorf("checksum/config is %q with and without tls.enabled; the upgrade would not restart any Pod", got)
		}
	})

	t.Run("it can be turned off", func(t *testing.T) {
		if got := checksumAnnotation(t, "--set", "configChecksumAnnotation=false"); got != "" {
			t.Errorf("checksum/config = %q, want it absent when configChecksumAnnotation is false", got)
		}
	})
}

// TestChartSecurityContexts covers the two halves of the container boundary.
// The pod half is a compatibility knob: fsGroup is what makes the credentials
// Secret readable by the non-root image, and a cluster whose SCC assigns its
// own ranges rejects a Pod that asks for one, so a null value has to render no
// key at all rather than an empty block.
// The container half is not a knob, and is pinned to the base's.
func TestChartSecurityContexts(t *testing.T) {
	t.Run("fsGroup by default", func(t *testing.T) {
		sc := render[appsv1.Deployment](t, "deployment.yaml").Spec.Template.Spec.SecurityContext
		if sc == nil || sc.FSGroup == nil || *sc.FSGroup != 65532 {
			t.Errorf("pod securityContext = %+v, want fsGroup 65532, the uid/gid the image runs as", sc)
		}
	})

	t.Run("fsGroup omitted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(path, []byte("podSecurityContext:\n  fsGroup: null\n"), 0o600); err != nil {
			t.Fatalf("write the values file: %v", err)
		}
		sc := render[appsv1.Deployment](t, "deployment.yaml", "-f", path).Spec.Template.Spec.SecurityContext
		if sc != nil {
			t.Errorf("pod securityContext = %+v, want none: a null fsGroup must render no key", sc)
		}
	})

	t.Run("container context matches the base", func(t *testing.T) {
		want := decode[appsv1.Deployment](t, "deployment.yaml").Spec.Template.Spec.Containers[0].SecurityContext
		got := render[appsv1.Deployment](t, "deployment.yaml").Spec.Template.Spec.Containers[0].SecurityContext
		if !reflect.DeepEqual(got, want) {
			t.Errorf("container securityContext = %+v, want deploy/base/deployment.yaml's %+v", got, want)
		}
	})
}

// TestChartConfigIsMergedAndParses covers the configuration design: structured
// values for the keys the chart models, and a raw config block merged over
// them for everything else. Both halves have to reach the gateway as a file
// config.Load accepts, unknown keys and all.
func TestChartConfigIsMergedAndParses(t *testing.T) {
	t.Run("structured values", func(t *testing.T) {
		cfg := loadRenderedConfig(t, "--set", "server.logLevel=warn", "--set", "auth.anonymousRealm=ops",
			"--set", "realms.ops.namespaces[0]=*", "--set", "realms.ops.services[0]=*", "--set", "realms.ops.profiles[0]=cpu")

		if cfg.Server.LogLevel != "warn" {
			t.Errorf("server.logLevel = %q, want warn", cfg.Server.LogLevel)
		}
		realm, ok := cfg.Realms[cfg.Auth.AnonymousRealm]
		if !ok {
			t.Fatalf("anonymous realm %q is not in Realms %+v", cfg.Auth.AnonymousRealm, cfg.Realms)
		}
		if len(realm.Profiles) != 1 || realm.Profiles[0] != "cpu" {
			t.Errorf("the anonymous realm's profiles = %v, want [cpu]", realm.Profiles)
		}
	})

	t.Run("raw config block", func(t *testing.T) {
		cfg := loadRenderedConfig(t, "--set", "config.limits.cpuSeconds=30", "--set", "config.discovery.pprof.port=7070")

		if cfg.Limits.CPUSeconds != 30 {
			t.Errorf("limits.cpuSeconds = %d, want the raw block's 30", cfg.Limits.CPUSeconds)
		}
		if cfg.Discovery.Pprof.Port != 7070 {
			t.Errorf("discovery.pprof.port = %d, want the raw block's 7070", cfg.Discovery.Pprof.Port)
		}
	})

	t.Run("the raw block wins", func(t *testing.T) {
		cfg := loadRenderedConfig(t, "--set", "server.logLevel=warn", "--set", "config.server.logLevel=error")

		if cfg.Server.LogLevel != "error" {
			t.Errorf("server.logLevel = %q, want the raw block's error over the structured warn", cfg.Server.LogLevel)
		}
	})
}

// TestChartTLS covers the volume the certificate arrives through and the
// configuration that points at it. Both halves have to agree, because the
// gateway opens the files the ConfigMap names and exits when it cannot.
func TestChartTLS(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		podSpec := render[appsv1.Deployment](t, "deployment.yaml").Spec.Template.Spec

		if vol := volumeNamed(podSpec, "tls"); vol != nil {
			t.Errorf("Volumes carries %+v with tls.enabled false, want none", vol)
		}
		if cfg := loadRenderedConfig(t); cfg.Server.TLS.Enabled() {
			t.Errorf("server.tls = %+v, want the plaintext default", cfg.Server.TLS)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		values := tlsValues(t)
		podSpec := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec
		cfg := loadRenderedConfig(t, values...)

		vol := volumeNamed(podSpec, "tls")
		if vol == nil || vol.Secret == nil {
			t.Fatalf("Volumes = %+v, want a Secret volume named tls", podSpec.Volumes)
		}
		if vol.Secret.SecretName != "profgate-tls" {
			t.Errorf("the tls volume names Secret %q, want the default profgate-tls", vol.Secret.SecretName)
		}
		if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o440 {
			t.Errorf("defaultMode = %v, want 0440, the narrowest mode the non-root image can read", vol.Secret.DefaultMode)
		}
		// Not optional, unlike the NATS volume: enabling TLS asserts the
		// certificate exists, so a missing Secret has to stop the Pod at mount
		// time rather than let it start and exit over a file it cannot open.
		if vol.Secret.Optional == nil || *vol.Secret.Optional {
			t.Errorf("optional = %v, want an explicit false", vol.Secret.Optional)
		}

		mount := mountNamed(podSpec, "tls")
		if mount == nil {
			t.Fatalf("volumeMounts = %+v, want one named tls", podSpec.Containers[0].VolumeMounts)
		}
		if !mount.ReadOnly {
			t.Error("the tls mount is writable, want read-only")
		}
		if !cfg.Server.TLS.Enabled() {
			t.Fatalf("server.tls = %+v, want both files named", cfg.Server.TLS)
		}
		if got := filepath.Dir(cfg.Server.TLS.CertFile); got != mount.MountPath {
			t.Errorf("server.tls.certFile is under %q, want the mount path %q", got, mount.MountPath)
		}
		if got := filepath.Base(cfg.Server.TLS.KeyFile); got != "tls.key" {
			t.Errorf("server.tls.keyFile names %q, want the tls.keyKey default tls.key", got)
		}
		if cfg.Server.TLS.MinVersion != "1.2" {
			t.Errorf("server.tls.minVersion = %q, want the 1.2 default", cfg.Server.TLS.MinVersion)
		}
	})

	t.Run("the Secret is not hashed into the pod template", func(t *testing.T) {
		// The gateway re-reads the files while it runs, so a renewal must not
		// roll the Deployment. An annotation added here for symmetry with
		// checksum/config would undo that.
		annotations := render[appsv1.Deployment](t, "deployment.yaml", tlsValues(t)...).Spec.Template.Annotations
		for key := range annotations {
			if strings.Contains(key, "tls") {
				t.Errorf("the pod template carries %q; a renewal would then roll the Deployment", key)
			}
		}
	})
}
