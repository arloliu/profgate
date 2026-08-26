package deploy_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
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
		// credsFile must equal mountPath joined with secretKey; the file is
		// named after the secretKey default, so only the mount path moves.
		"--set", "nats.mountPath=" + filepath.Dir(creds),
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

// TestChartPGOValuesAreValidated holds pgo.enabled and the four sizing
// ceilings to the types and ranges the gateway reads.
// pgo.enabled must be an actual boolean, because any non-empty string is
// true to a template conditional, so a quoted "false" would render PGO
// enabled while the values file reads as disabled.
// Each ceiling must be a whole number in the range the gateway's startup
// validation accepts: a coerced check would derive a memory limit from a
// number the rendered configuration does not carry -- a boolean coerces to
// 1, a fraction truncates, a quoted number renders a scalar the gateway's
// integer field rejects, and a negative value renders a negative memory
// limit.
// The one case that must keep rendering is an integral number, whatever
// numeric type Helm delivers it as: a values-file or --set-json number
// arrives as float64.
func TestChartPGOValuesAreValidated(t *testing.T) {
	pgoOn := []string{
		"--set", "pgo.enabled=true",
		"--set", "nats.url=nats://nats.profgate.svc:4222",
		"--set", "nats.credsFile=",
	}

	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{
			name:   "boolean maxParallel",
			values: append(slices.Clone(pgoOn), "--set", "pgo.limits.maxParallel=true"),
			want:   "pgo.limits.maxParallel true has type bool, not a number",
		},
		{
			name:   "string maxMergedBytes",
			values: append(slices.Clone(pgoOn), "--set-string", "pgo.limits.maxMergedBytes=5"),
			want:   "pgo.limits.maxMergedBytes 5 has type string, not a number",
		},
		{
			name:   "fractional maxSampleBytes",
			values: append(slices.Clone(pgoOn), "--set-json", "pgo.limits.maxSampleBytes=1.5"),
			want:   "pgo.limits.maxSampleBytes 1.5 is not a whole number",
		},
		{
			name:   "negative maxActiveCollections",
			values: append(slices.Clone(pgoOn), "--set", "pgo.limits.maxActiveCollections=-1"),
			want:   "pgo.limits.maxActiveCollections -1 must be at least 1",
		},
		{
			name:   "maxParallel above the gateway's ceiling",
			values: append(slices.Clone(pgoOn), "--set", "pgo.limits.maxParallel=65"),
			want:   "pgo.limits.maxParallel 65 must be between 1 and 64",
		},
		{
			// A ceiling that passes its own range can still multiply out
			// past what a 64-bit byte count holds; the product is checked
			// rather than rendered as a wrapped-around number.
			name:   "a product overflowing a 64-bit byte count",
			values: append(slices.Clone(pgoOn), "--set", "pgo.limits.maxActiveCollections=9007199254740992"),
			want:   "overflows, so lower the ceilings",
		},
		{
			// 2^53+1 arrives as int64 through --set; converting it to
			// float64 before the cap check would round it to 2^53 and
			// render the rounded ceiling as if the operator had set it.
			// resources is set so the derived-limit product guard cannot
			// mask the cap check.
			name: "an int64 ceiling above what a number carries exactly",
			values: append(slices.Clone(pgoOn),
				"--set", "resources.limits.memory=1Gi",
				"--set", "pgo.limits.maxActiveCollections=9007199254740993"),
			want: "pgo.limits.maxActiveCollections 9007199254740993 is larger than the count a number value carries exactly",
		},
		{
			name:   "string pgo.enabled",
			values: []string{"--set-string", "pgo.enabled=false"},
			want:   "pgo.enabled false has type string, not bool",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderFailure(t, tc.values...)
			if !strings.Contains(out, tc.want) {
				t.Errorf("helm's error does not say %q:\n%s", tc.want, out)
			}
		})
	}

	// The refusals must not reach the numeric type Helm actually delivers:
	// a values-file or --set-json number arrives as float64, so an integral
	// float64 has to render, carry its integer into the configuration, and
	// take part in the memory arithmetic.
	t.Run("an integral --set-json number renders", func(t *testing.T) {
		values := append(slices.Clone(pgoOn), "--set-json", "pgo.limits.maxParallel=5")

		cfg := loadRenderedConfig(t, values...)
		if got, want := cfg.PGO.Limits.MaxParallel, 5; got != want {
			t.Errorf("pgo.limits.maxParallel = %d, want %d", got, want)
		}
		mem := containerMemoryLimit(t, render[appsv1.Deployment](t, "deployment.yaml", values...))
		if got, want := mem.Value(), cfg.PGOMemoryBytes(); got != want {
			t.Errorf("resources.limits.memory = %d bytes, want %d from the rendered ConfigMap's own pgo.limits", got, want)
		}
	})
}

// TestChartBooleanTogglesAreValidated holds every boolean toggle a template
// conditional reads to an actual boolean, the way pgo.enabled already is
// (covered in TestChartPGOValuesAreValidated).
// A template conditional treats any non-empty string as true, so a quoted
// "false" -- --set-string, or a quoted values-file scalar -- would render
// what the toggle gates as enabled while the operator reads it as disabled:
// a plaintext listener behind a tls.enabled of "false" is the sharpest case.
func TestChartBooleanTogglesAreValidated(t *testing.T) {
	for _, key := range []string{
		"tls.enabled",
		"podDisruptionBudget.enabled",
		"networkPolicy.enabled",
		"rbac.create",
		"serviceAccount.create",
		"configChecksumAnnotation",
	} {
		t.Run(key, func(t *testing.T) {
			for name, tc := range map[string]struct {
				values []string
				want   string
			}{
				"quoted false": {
					values: []string{"--set-string", key + "=false"},
					want:   key + " false has type string, not bool",
				},
				"number": {
					values: []string{"--set", key + "=1"},
					want:   key + " 1 has type int64, not bool",
				},
			} {
				t.Run(name, func(t *testing.T) {
					out := renderFailure(t, tc.values...)
					if !strings.Contains(out, tc.want) {
						t.Errorf("helm's error does not say %q:\n%s", tc.want, out)
					}
				})
			}
		})
	}

	// The refusals must not reach the shipped defaults, which are real
	// booleans on both sides of each toggle.
	t.Run("the defaults render", func(t *testing.T) {
		runHelm(t, "template", "profgate", chartDir, "--namespace", "profgate")
	})
}

// TestChartMemoryLimitWithoutPGO covers the other path.
// The sizing formula reads pgo.limits and never pgo.enabled,
// so applying it with collection off would ask for the 4Gi a merge needs on a gateway that never merges;
// the chart uses a static limit there instead, and holds to limits.memory alone, the way the base does.
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

// renderFailure runs `helm template` over the whole chart expecting it to
// fail, and returns helm's error output.
func renderFailure(t *testing.T, values ...string) string {
	t.Helper()

	args := []string{"template", "profgate", chartDir, "--namespace", "profgate"}
	args = append(args, values...)

	//nolint:gosec // args come from the test's own literals, not external input
	cmd := exec.CommandContext(t.Context(), helmBin(t), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("helm %s rendered cleanly, want a failure", strings.Join(args, " "))
	}

	return stderr.String()
}

// TestChartRejectsDerivedKeyOverrides holds the escape hatches away from the
// keys the Deployment couples to something else it renders:
// the derived memory limit and the Secret mounts.
// The raw config block is merged after profgate.resources has read
// .Values.pgo, and extraEnv overrides the file at runtime,
// so pgo.enabled or a sizing ceiling smuggled through either hatch would run
// PGO under a limit sized for different ceilings;
// the chart refuses to render that instead.
// The file-path keys are the second coupling:
// the credentials mount follows nats.credsFile and the certificate mount
// follows tls.enabled, so nats.credsFile or a server.tls certificate path
// smuggled through either hatch can name a file nothing mounts,
// and startup validation would end the Pod over it.
// A null or scalar config.pgo, config.pgo.limits, config.server,
// config.server.tls, or config.nats is the same override in bulk
// -- the merge copies it over the structured mapping and the binary
// reads the rendered null as absent, restoring its own defaults -- so those
// shapes are refused too, while an empty mapping merges to nothing and
// stays allowed.
func TestChartRejectsDerivedKeyOverrides(t *testing.T) {
	extraEnv := func(name string) []string {
		return []string{"--set", "extraEnv[0].name=" + name, "--set", "extraEnv[0].value=1"}
	}

	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{
			name:   "config.pgo.enabled",
			values: []string{"--set", "config.pgo.enabled=true"},
			want:   "set pgo.enabled instead",
		},
		{
			name:   "config.pgo.limits.maxParallel",
			values: []string{"--set", "config.pgo.limits.maxParallel=8"},
			want:   "set pgo.limits.maxParallel instead",
		},
		{
			name:   "config.pgo.limits.maxSampleBytes",
			values: []string{"--set", "config.pgo.limits.maxSampleBytes=67108864"},
			want:   "set pgo.limits.maxSampleBytes instead",
		},
		{
			name:   "config.pgo.limits.maxMergedBytes",
			values: []string{"--set", "config.pgo.limits.maxMergedBytes=134217728"},
			want:   "set pgo.limits.maxMergedBytes instead",
		},
		{
			name:   "config.pgo.limits.maxActiveCollections",
			values: []string{"--set", "config.pgo.limits.maxActiveCollections=3"},
			want:   "set pgo.limits.maxActiveCollections instead",
		},
		{
			name:   "config.pgo null",
			values: []string{"--set-json", `config={"pgo":null}`},
			want:   "set pgo.enabled, pgo.configAPI, and the keys under pgo.limits instead",
		},
		{
			name:   "config.pgo scalar",
			values: []string{"--set-json", `config={"pgo":5}`},
			want:   "set pgo.enabled, pgo.configAPI, and the keys under pgo.limits instead",
		},
		{
			name:   "config.pgo.limits null",
			values: []string{"--set-json", `config={"pgo":{"limits":null}}`},
			want:   "set pgo.limits.maxParallel, pgo.limits.maxSampleBytes, pgo.limits.maxMergedBytes, and pgo.limits.maxActiveCollections instead",
		},
		{
			name:   "config.pgo.limits scalar",
			values: []string{"--set-json", `config={"pgo":{"limits":5}}`},
			want:   "set pgo.limits.maxParallel, pgo.limits.maxSampleBytes, pgo.limits.maxMergedBytes, and pgo.limits.maxActiveCollections instead",
		},
		{
			name:   "config.nats.credsFile",
			values: []string{"--set", "config.nats.credsFile=/elsewhere/nats.creds"},
			want:   "set nats.credsFile and nats.existingSecret instead",
		},
		{
			name:   "config.server.tls.certFile",
			values: []string{"--set", "config.server.tls.certFile=/elsewhere/tls.crt"},
			want:   "set tls.enabled and tls.existingSecret instead",
		},
		{
			name:   "config.server.tls.keyFile",
			values: []string{"--set", "config.server.tls.keyFile=/elsewhere/tls.key"},
			want:   "set tls.enabled and tls.existingSecret instead",
		},
		{
			name:   "config.server null",
			values: []string{"--set-json", `config={"server":null}`},
			want:   "set the individual keys under config.server instead",
		},
		{
			name:   "config.server scalar",
			values: []string{"--set-json", `config={"server":5}`},
			want:   "set the individual keys under config.server instead",
		},
		{
			name:   "config.server.tls null",
			values: []string{"--set-json", `config={"server":{"tls":null}}`},
			want:   "set tls.enabled, tls.existingSecret, and tls.minVersion instead",
		},
		{
			name:   "config.server.tls scalar",
			values: []string{"--set-json", `config={"server":{"tls":5}}`},
			want:   "set tls.enabled, tls.existingSecret, and tls.minVersion instead",
		},
		{
			name:   "config.nats null",
			values: []string{"--set-json", `config={"nats":null}`},
			want:   "set nats.url, nats.credsFile, and nats.existingSecret instead",
		},
		{
			name:   "config.nats scalar",
			values: []string{"--set-json", `config={"nats":5}`},
			want:   "set nats.url, nats.credsFile, and nats.existingSecret instead",
		},
		{
			name:   "extraEnv PROFGATE_PGO_ENABLED",
			values: extraEnv("PROFGATE_PGO_ENABLED"),
			want:   "extraEnv must not set PROFGATE_PGO_ENABLED",
		},
		{
			name:   "extraEnv PROFGATE_PGO_LIMIT_MAX_PARALLEL",
			values: extraEnv("PROFGATE_PGO_LIMIT_MAX_PARALLEL"),
			want:   "set pgo.limits.maxParallel instead",
		},
		{
			name:   "extraEnv PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES",
			values: extraEnv("PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES"),
			want:   "set pgo.limits.maxSampleBytes instead",
		},
		{
			name:   "extraEnv PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES",
			values: extraEnv("PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES"),
			want:   "set pgo.limits.maxMergedBytes instead",
		},
		{
			name:   "extraEnv PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS",
			values: extraEnv("PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS"),
			want:   "set pgo.limits.maxActiveCollections instead",
		},
		{
			name:   "extraEnv PROFGATE_NATS_CREDS_FILE",
			values: extraEnv("PROFGATE_NATS_CREDS_FILE"),
			want:   "set nats.credsFile and nats.existingSecret instead",
		},
		{
			name:   "extraEnv PROFGATE_TLS_CERT_FILE",
			values: extraEnv("PROFGATE_TLS_CERT_FILE"),
			want:   "set tls.enabled and tls.existingSecret instead",
		},
		{
			name:   "extraEnv PROFGATE_TLS_KEY_FILE",
			values: extraEnv("PROFGATE_TLS_KEY_FILE"),
			want:   "set tls.enabled and tls.existingSecret instead",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderFailure(t, tc.values...)
			if !strings.Contains(out, tc.want) {
				t.Errorf("helm's error does not name the supported knob %q:\n%s", tc.want, out)
			}
		})
	}

	// The guard is these keys and nothing more: any other key keeps both
	// escape hatches, pgo.configAPI and server.tls.minVersion included --
	// minVersion names no file, so no mount can drift from it.
	t.Run("everything else stays overridable", func(t *testing.T) {
		if cfg := loadRenderedConfig(t, "--set", "config.server.logLevel=debug"); cfg.Server.LogLevel != "debug" {
			t.Errorf("server.logLevel = %q, want the raw block's debug", cfg.Server.LogLevel)
		}
		if cfg := loadRenderedConfig(t, "--set", "config.pgo.configAPI=disabled"); cfg.PGO.ConfigAPI != "disabled" {
			t.Errorf("pgo.configAPI = %q, want the raw block's disabled", cfg.PGO.ConfigAPI)
		}
		if cfg := loadRenderedConfig(t, append(tlsValues(t), "--set", "config.server.tls.minVersion=1.3")...); cfg.Server.TLS.MinVersion != "1.3" {
			t.Errorf("server.tls.minVersion = %q, want the raw block's 1.3", cfg.Server.TLS.MinVersion)
		}

		env := render[appsv1.Deployment](t, "deployment.yaml",
			"--set", "extraEnv[0].name=PROFGATE_LOG_LEVEL",
			"--set", "extraEnv[0].value=debug",
		).Spec.Template.Spec.Containers[0].Env
		if len(env) != 1 || env[0].Name != "PROFGATE_LOG_LEVEL" {
			t.Errorf("container env = %+v, want the benign PROFGATE_LOG_LEVEL override rendered", env)
		}
	})

	// The mapping guard stops at nulls and scalars: an empty mapping merges
	// to nothing, so the structured pgo values survive it untouched.
	t.Run("empty mappings stay allowed", func(t *testing.T) {
		cfg := loadRenderedConfig(t, append(pgoValues(t), "--set-json", `config={"pgo":{}}`)...)
		if !cfg.PGO.Enabled {
			t.Error("pgo.enabled = false, want the structured true to survive an empty config.pgo mapping")
		}

		cfg = loadRenderedConfig(t, append(pgoValues(t), "--set-json", `config={"pgo":{"limits":{}}}`)...)
		if got, want := cfg.PGO.Limits.MaxParallel, 4; got != want {
			t.Errorf("pgo.limits.maxParallel = %d, want the shipped %d to survive an empty config.pgo.limits mapping", got, want)
		}
	})
}

// TestChartGuardedEnvNamesMatchTheBinary pins the guard's env names to the
// binary's own: config.Load applies PROFGATE_-prefixed overrides from the
// env struct tags, so a renamed tag would otherwise leave the guard watching
// a name the binary no longer reads.
func TestChartGuardedEnvNamesMatchTheBinary(t *testing.T) {
	envTag := func(structType reflect.Type, field string) string {
		f, ok := structType.FieldByName(field)
		if !ok {
			t.Fatalf("config.%s has no field %s", structType.Name(), field)
		}

		return "PROFGATE_" + f.Tag.Get("env")
	}

	pgo := reflect.TypeOf(config.PGOConfig{})
	if got, want := envTag(pgo, "Enabled"), "PROFGATE_PGO_ENABLED"; got != want {
		t.Errorf("PGOConfig.Enabled env override is %s, but the chart guard watches %s", got, want)
	}

	limits := reflect.TypeOf(config.PGOLimits{})
	for field, want := range map[string]string{
		"MaxParallel":          "PROFGATE_PGO_LIMIT_MAX_PARALLEL",
		"MaxSampleBytes":       "PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES",
		"MaxMergedBytes":       "PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES",
		"MaxActiveCollections": "PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS",
	} {
		if got := envTag(limits, field); got != want {
			t.Errorf("PGOLimits.%s env override is %s, but the chart guard watches %s", field, got, want)
		}
	}

	nats := reflect.TypeOf(config.NATSConfig{})
	if got, want := envTag(nats, "CredsFile"), "PROFGATE_NATS_CREDS_FILE"; got != want {
		t.Errorf("NATSConfig.CredsFile env override is %s, but the chart guard watches %s", got, want)
	}

	tlsConf := reflect.TypeOf(config.TLSConfig{})
	for field, want := range map[string]string{
		"CertFile": "PROFGATE_TLS_CERT_FILE",
		"KeyFile":  "PROFGATE_TLS_KEY_FILE",
	} {
		if got := envTag(tlsConf, field); got != want {
			t.Errorf("TLSConfig.%s env override is %s, but the chart guard watches %s", field, got, want)
		}
	}
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

	// The raw block satisfies nats.url's requiredness too: it merges over
	// the structured nats block, so config.nats.url alone is the URL the
	// gateway reads and rendering must not demand the structured value.
	t.Run("config.nats.url satisfies the requirement", func(t *testing.T) {
		cfg := loadRenderedConfig(t,
			"--set", "pgo.enabled=true",
			"--set", "nats.credsFile=",
			"--set", "config.nats.url=nats://raw.profgate.svc:4222",
		)

		if got, want := cfg.NATS.URL, "nats://raw.profgate.svc:4222"; got != want {
			t.Errorf("nats.url = %q, want the raw block's %q", got, want)
		}
	})
}

// TestChartUnauthenticatedNATS covers nats.credsFile set to "": no JWT
// credentials file, so the chart mounts nothing and the rendered
// configuration carries no credsFile key; authentication, if any, rides in
// the URL.
func TestChartUnauthenticatedNATS(t *testing.T) {
	values := []string{
		"--set", "pgo.enabled=true",
		"--set", "nats.url=nats://nats.profgate.svc:4222",
		"--set", "nats.credsFile=",
	}

	podSpec := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec
	if vol := volumeNamed(podSpec, "nats-creds"); vol != nil {
		t.Errorf("Volumes carries %+v with nats.credsFile empty, want none", vol)
	}
	if m := mountNamed(podSpec, "nats-creds"); m != nil {
		t.Errorf("VolumeMounts carries %+v with nats.credsFile empty, want none", m)
	}

	cm := render[corev1.ConfigMap](t, "configmap.yaml", values...)
	if strings.Contains(cm.Data["config.yaml"], "credsFile") {
		t.Errorf("the rendered config.yaml carries a credsFile key with nats.credsFile empty:\n%s", cm.Data["config.yaml"])
	}

	cfg := loadRenderedConfig(t, values...)
	if cfg.NATS.CredsFile != "" {
		t.Errorf("nats.credsFile = %q, want empty with no credentials file configured", cfg.NATS.CredsFile)
	}
}

// TestChartNATSCredsFileMatchesTheMount covers the coupling between the
// configuration and the credentials volume: the Deployment mounts the Secret
// key nats.secretKey at nats.mountPath while the rendered configuration
// carries nats.credsFile verbatim, so a credsFile pointing anywhere else --
// or a mountPath or secretKey moved without it -- renders cleanly and then
// exits at startup over a file nothing mounts.
// The chart refuses to render the disagreement instead, and credsFile stays
// the explicit value rather than being derived, so the rendered
// configuration never carries a path the values file does not show.
func TestChartNATSCredsFileMatchesTheMount(t *testing.T) {
	t.Run("a credsFile outside the mount fails", func(t *testing.T) {
		stderr := renderFailure(t,
			"--set", "pgo.enabled=true",
			"--set", "nats.url=nats://nats.profgate.svc:4222",
			"--set", "nats.credsFile=/elsewhere/custom.creds",
		)
		for _, want := range []string{`"/elsewhere/custom.creds"`, `"/etc/profgate/nats"`, `"nats.creds"`} {
			if !strings.Contains(stderr, want) {
				t.Errorf("helm's error does not name %s:\n%s", want, stderr)
			}
		}
	})

	t.Run("a moved mountPath alone fails the same way", func(t *testing.T) {
		stderr := renderFailure(t,
			"--set", "pgo.enabled=true",
			"--set", "nats.url=nats://nats.profgate.svc:4222",
			"--set", "nats.mountPath=/etc/elsewhere",
		)
		for _, want := range []string{`"/etc/profgate/nats/nats.creds"`, `"/etc/elsewhere"`} {
			if !strings.Contains(stderr, want) {
				t.Errorf("helm's error does not name %s:\n%s", want, stderr)
			}
		}
	})

	t.Run("a mismatch with PGO disabled renders", func(t *testing.T) {
		// With PGO disabled nothing mounts and the rendered configuration
		// carries no nats block, so the disagreement is between values
		// nothing reads; the guard follows the mount's own gate instead of
		// blocking the render over inactive values.
		podSpec := render[appsv1.Deployment](t, "deployment.yaml",
			"--set", "nats.mountPath=/etc/elsewhere").Spec.Template.Spec
		if m := mountNamed(podSpec, "nats-creds"); m != nil {
			t.Errorf("VolumeMounts carries %+v with PGO disabled, want none", m)
		}
	})

	t.Run("a matching custom triple renders", func(t *testing.T) {
		dir := t.TempDir()
		creds := filepath.Join(dir, "custom.creds")
		if err := os.WriteFile(creds, []byte("-----BEGIN NATS USER JWT-----\n"), 0o600); err != nil {
			t.Fatalf("write a stand-in credentials file: %v", err)
		}
		values := []string{
			"--set", "pgo.enabled=true",
			"--set", "nats.url=nats://nats.profgate.svc:4222",
			"--set", "nats.mountPath=" + dir,
			"--set", "nats.secretKey=custom.creds",
			"--set", "nats.credsFile=" + creds,
		}

		podSpec := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec
		mount := mountNamed(podSpec, "nats-creds")
		if mount == nil || mount.MountPath != dir {
			t.Fatalf("the nats-creds mount = %+v, want it at the custom mountPath %q", mount, dir)
		}
		if cfg := loadRenderedConfig(t, values...); cfg.NATS.CredsFile != creds {
			t.Errorf("nats.credsFile = %q, want the explicit %q", cfg.NATS.CredsFile, creds)
		}
	})

	t.Run("a noncanonical mountPath spelling of the same path renders", func(t *testing.T) {
		dir := t.TempDir()
		creds := filepath.Join(dir, "custom.creds")
		if err := os.WriteFile(creds, []byte("-----BEGIN NATS USER JWT-----\n"), 0o600); err != nil {
			t.Fatalf("write a stand-in credentials file: %v", err)
		}
		values := []string{
			"--set", "pgo.enabled=true",
			"--set", "nats.url=nats://nats.profgate.svc:4222",
			// The same directory spelled with a trailing double slash: the
			// guard compares cleaned paths, so this agrees with the
			// canonical credsFile below instead of failing the render.
			"--set", "nats.mountPath=" + dir + "//",
			"--set", "nats.secretKey=custom.creds",
			"--set", "nats.credsFile=" + creds,
		}

		// The Deployment is where the guard runs, so render it and hold the
		// mount to the same file the configuration names: the mount directory
		// joined with the Secret key must be the credsFile, spelling aside.
		podSpec := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec
		mount := mountNamed(podSpec, "nats-creds")
		if mount == nil {
			t.Fatalf("VolumeMounts = %+v, want one named nats-creds", podSpec.Containers[0].VolumeMounts)
		}
		if got := filepath.Join(mount.MountPath, "custom.creds"); got != creds {
			t.Errorf("the nats-creds mount serves %q, want the rendered credsFile %q", got, creds)
		}
		if cfg := loadRenderedConfig(t, values...); cfg.NATS.CredsFile != creds {
			t.Errorf("nats.credsFile = %q, want the explicit %q", cfg.NATS.CredsFile, creds)
		}
	})
}

// TestChartMountPartsAreValidated holds every part of an active Secret mount
// to what Kubernetes accepts.
// An empty Secret name or mount path renders a Deployment the API server rejects,
// a relative mount path is nowhere a volume can mount,
// and an empty or malformed Secret data key joins into a path
// naming the mount directory instead of a file --
// an empty nats.secretKey even passes the credsFile path comparison that way,
// pointing the gateway at the directory itself.
// Every part must also already be a string: rendering formats a number or a
// boolean differently from the string a coerced check would read, and an
// unquoted secretName that YAML re-types as a boolean cannot decode into the
// Deployment's string field.
// Secret names are further held to the DNS-1123 subdomain shape
// Kubernetes accepts.
func TestChartMountPartsAreValidated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{
			name: "empty nats.secretKey",
			// The join of nats.mountPath and an empty key cleans to the
			// mount directory, so this credsFile agrees with the path
			// comparison; the key check is what refuses it.
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.secretKey=",
				"--set", "nats.credsFile=/etc/profgate/nats",
			},
			want: `nats.secretKey "" is not a Secret data key`,
		},
		{
			name: "nats.secretKey with a path separator",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.secretKey=nats/creds",
			},
			want: `nats.secretKey "nats/creds" is not a Secret data key`,
		},
		{
			name: "relative nats.mountPath",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.mountPath=etc/profgate/nats",
			},
			want: `nats.mountPath "etc/profgate/nats" is not an absolute path`,
		},
		{
			name: "empty nats.existingSecret",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.existingSecret=",
			},
			want: "nats.existingSecret is empty",
		},
		{
			name:   "empty tls.existingSecret",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.existingSecret="},
			want:   "tls.existingSecret is empty",
		},
		{
			name:   "relative tls.mountPath",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.mountPath=etc/profgate/tls"},
			want:   `tls.mountPath "etc/profgate/tls" is not an absolute path`,
		},
		{
			name:   "empty tls.certKey",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.certKey="},
			want:   `tls.certKey "" is not a Secret data key`,
		},
		{
			name:   "tls.keyKey with a space",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.keyKey=tls key"},
			want:   `tls.keyKey "tls key" is not a Secret data key`,
		},
		{
			// An unquoted --set types the value as an integer; a coerced
			// check would pass "123" while rendering formats the integer
			// as %!s(int64=123).
			name:   "numeric tls.certKey",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.certKey=123"},
			want:   "tls.certKey 123 has type int64, not string",
		},
		{
			// An unquoted --set types the value as a boolean, which would
			// render an unquoted secretName no string field can decode.
			name:   "boolean tls.existingSecret",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.existingSecret=true"},
			want:   "tls.existingSecret true has type bool, not string",
		},
		{
			name:   "tls.existingSecret that is not a DNS-1123 subdomain",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.existingSecret=BAD_NAME"},
			want:   `tls.existingSecret "BAD_NAME" is not a name a Secret can carry`,
		},
		{
			name: "numeric nats.secretKey",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.secretKey=123",
			},
			want: "nats.secretKey 123 has type int64, not string",
		},
		{
			// false and 0 are falsy, so a `default ""` applied before the
			// type check would fold them to "" and report an empty value
			// the values file does not carry; the check reads the raw
			// value and names it with its type.
			name:   "boolean tls.mountPath",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.mountPath=false"},
			want:   "tls.mountPath false has type bool, not string",
		},
		{
			name:   "zero tls.certKey",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.certKey=0"},
			want:   "tls.certKey 0 has type int64, not string",
		},
		{
			name: "boolean nats.mountPath",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.mountPath=false",
			},
			want: "nats.mountPath false has type bool, not string",
		},
		{
			name: "zero nats.secretKey",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.secretKey=0",
			},
			want: "nats.secretKey 0 has type int64, not string",
		},
		{
			// "." passes the character class but is a name the kubelet
			// cannot project into a file, and cleaned against
			// nats.mountPath it joins to the mount directory itself, so a
			// credsFile naming the directory would agree with the join
			// comparison; the key check refuses it before that comparison
			// runs.
			name: "dot nats.secretKey",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set-string", "nats.secretKey=.",
				"--set", "nats.credsFile=/etc/profgate/nats",
			},
			want: `nats.secretKey "." is not a Secret data key`,
		},
		{
			name:   "dot tls.certKey",
			values: []string{"--set", "tls.enabled=true", "--set-string", "tls.certKey=."},
			want:   `tls.certKey "." is not a Secret data key`,
		},
		{
			name:   "tls.keyKey starting with dot-dot",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.keyKey=..foo"},
			want:   `tls.keyKey "..foo" is not a Secret data key`,
		},
		{
			// Kubernetes holds a data key to 253 characters, the same
			// ceiling as a DNS subdomain; a longer key renders a
			// Deployment the API server rejects.
			name: "overlong nats.secretKey",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.secretKey=" + strings.Repeat("a", 254),
			},
			want: `nats.secretKey "` + strings.Repeat("a", 254) + `" is not a Secret data key`,
		},
		{
			// false and 0 are falsy, so an emptiness check ahead of the
			// string-kind check would report an empty value the values
			// file does not carry; the kind check runs first and names
			// the value and its type.
			name: "boolean nats.existingSecret",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.existingSecret=false",
			},
			want: "nats.existingSecret false has type bool, not string",
		},
		{
			name: "zero nats.existingSecret",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.existingSecret=0",
			},
			want: "nats.existingSecret 0 has type int64, not string",
		},
		{
			name:   "false tls.existingSecret",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.existingSecret=false"},
			want:   "tls.existingSecret false has type bool, not string",
		},
		{
			name:   "zero tls.existingSecret",
			values: []string{"--set", "tls.enabled=true", "--set", "tls.existingSecret=0"},
			want:   "tls.existingSecret 0 has type int64, not string",
		},
		{
			// A falsy credsFile would otherwise skip every credentials
			// check and silently render the no-credentials path; only a
			// true empty string selects that path.
			name: "boolean nats.credsFile",
			values: []string{
				"--set", "pgo.enabled=true",
				"--set", "nats.url=nats://nats.profgate.svc:4222",
				"--set", "nats.credsFile=false",
			},
			want: "nats.credsFile false has type bool, not string",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderFailure(t, tc.values...)
			if !strings.Contains(out, tc.want) {
				t.Errorf("helm's error does not say %q:\n%s", tc.want, out)
			}
		})
	}

	// The refusals above must not reach values that merely look re-typed:
	// a value --set-string keeps a string is exactly what renders.
	t.Run("a numeric-looking string key renders the literal key", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"123", "tls.key"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
				t.Fatalf("write a stand-in %s: %v", name, err)
			}
		}
		values := []string{
			"--set", "tls.enabled=true",
			"--set", "tls.mountPath=" + dir,
			"--set-string", "tls.certKey=123",
		}

		cfg := loadRenderedConfig(t, values...)
		if want := filepath.Join(dir, "123"); cfg.Server.TLS.CertFile != want {
			t.Errorf("server.tls.certFile = %q, want %q", cfg.Server.TLS.CertFile, want)
		}

		podSpec := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec
		mount := mountNamed(podSpec, "tls")
		if mount == nil {
			t.Fatalf("volumeMounts = %+v, want one named tls", podSpec.Containers[0].VolumeMounts)
		}
		if got := filepath.Join(mount.MountPath, "123"); got != cfg.Server.TLS.CertFile {
			t.Errorf("the tls mount serves %q, want the rendered certFile %q", got, cfg.Server.TLS.CertFile)
		}
	})

	t.Run("a YAML-keyword Secret name stays a string", func(t *testing.T) {
		// "true" is a valid DNS-1123 subdomain, so a Secret really can be
		// named that; only a quoted secretName keeps the rendered value
		// from decoding as a YAML boolean.
		values := append(tlsValues(t), "--set-string", "tls.existingSecret=true")
		podSpec := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec

		vol := volumeNamed(podSpec, "tls")
		if vol == nil || vol.Secret == nil {
			t.Fatalf("Volumes = %+v, want a Secret volume named tls", podSpec.Volumes)
		}
		if vol.Secret.SecretName != "true" {
			t.Errorf(`secretName = %q, want the string "true"`, vol.Secret.SecretName)
		}
	})
}

// TestChartMountPathsRenderQuoted pins each Deployment mountPath to the
// exact path the rendered configuration carries.
// The configuration renders its paths quoted, but a mountPath rendered bare
// is re-read by the YAML decoder, so a path holding a " #" renders cleanly,
// decodes with the tail dropped as a comment, and mounts the Secret
// somewhere the configuration does not name; the gateway then exits over
// files outside the mount. The decoded Deployment is what the API server
// would apply, so the assertion reads it, not the template text.
func TestChartMountPathsRenderQuoted(t *testing.T) {
	t.Run("tls", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tls #dir")
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatalf("make the mount directory: %v", err)
		}
		for _, name := range []string{"tls.crt", "tls.key"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
				t.Fatalf("write a stand-in %s: %v", name, err)
			}
		}
		values := []string{"--set", "tls.enabled=true", "--set", "tls.mountPath=" + dir}

		podSpec := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec
		mount := mountNamed(podSpec, "tls")
		if mount == nil {
			t.Fatalf("volumeMounts = %+v, want one named tls", podSpec.Containers[0].VolumeMounts)
		}
		if mount.MountPath != dir {
			t.Errorf("the decoded tls mountPath = %q, want tls.mountPath %q exactly", mount.MountPath, dir)
		}
		cfg := loadRenderedConfig(t, values...)
		if got := filepath.Dir(cfg.Server.TLS.CertFile); got != mount.MountPath {
			t.Errorf("server.tls.certFile is under %q, but the Secret is mounted at %q", got, mount.MountPath)
		}
	})

	t.Run("nats", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nats #dir")
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatalf("make the mount directory: %v", err)
		}
		creds := filepath.Join(dir, "nats.creds")
		if err := os.WriteFile(creds, []byte("-----BEGIN NATS USER JWT-----\n"), 0o600); err != nil {
			t.Fatalf("write a stand-in credentials file: %v", err)
		}
		values := []string{
			"--set", "pgo.enabled=true",
			"--set", "nats.url=nats://nats.profgate.svc:4222",
			"--set", "nats.credsFile=" + creds,
			"--set", "nats.mountPath=" + dir,
		}

		podSpec := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec
		mount := mountNamed(podSpec, "nats-creds")
		if mount == nil {
			t.Fatalf("volumeMounts = %+v, want one named nats-creds", podSpec.Containers[0].VolumeMounts)
		}
		if mount.MountPath != dir {
			t.Errorf("the decoded nats-creds mountPath = %q, want nats.mountPath %q exactly", mount.MountPath, dir)
		}
		cfg := loadRenderedConfig(t, values...)
		if got := filepath.Dir(cfg.NATS.CredsFile); got != mount.MountPath {
			t.Errorf("nats.credsFile is under %q, but the Secret is mounted at %q", got, mount.MountPath)
		}
	})
}

// TestChartNATSCredsMount covers when the credentials volume exists at all.
// The volume and its mount follow pgo.enabled and nats.credsFile together,
// so PGO off has to mount nothing even though the default credsFile is non-empty,
// and PGO on with credentials has to render the volume and the mount as a pair.
func TestChartNATSCredsMount(t *testing.T) {
	t.Run("pgo disabled mounts nothing", func(t *testing.T) {
		podSpec := render[appsv1.Deployment](t, "deployment.yaml").Spec.Template.Spec
		if vol := volumeNamed(podSpec, "nats-creds"); vol != nil {
			t.Errorf("Volumes carries %+v with pgo.enabled false, want none", vol)
		}
		if m := mountNamed(podSpec, "nats-creds"); m != nil {
			t.Errorf("VolumeMounts carries %+v with pgo.enabled false, want none", m)
		}
	})

	t.Run("pgo enabled with credentials mounts the Secret", func(t *testing.T) {
		dir := t.TempDir()
		creds := filepath.Join(dir, "nats.creds")
		if err := os.WriteFile(creds, []byte("-----BEGIN NATS USER JWT-----\n"), 0o600); err != nil {
			t.Fatalf("write a stand-in credentials file: %v", err)
		}
		values := []string{
			"--set", "pgo.enabled=true",
			"--set", "nats.url=nats://nats.profgate.svc:4222",
			"--set", "nats.credsFile=" + creds,
			"--set", "nats.mountPath=" + dir,
		}
		podSpec := render[appsv1.Deployment](t, "deployment.yaml", values...).Spec.Template.Spec

		vol := volumeNamed(podSpec, "nats-creds")
		if vol == nil || vol.Secret == nil {
			t.Fatalf("Volumes = %+v, want a Secret volume named nats-creds", podSpec.Volumes)
		}
		if vol.Secret.SecretName != "profgate-nats-creds" {
			t.Errorf("the nats-creds volume names Secret %q, want the default profgate-nats-creds", vol.Secret.SecretName)
		}
		if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o440 {
			t.Errorf("defaultMode = %v, want 0440, the narrowest mode the non-root image can read", vol.Secret.DefaultMode)
		}
		// Optional, unlike the TLS volume: a Pod may start before the Secret
		// exists, and startup keeps failing until the file appears.
		if vol.Secret.Optional == nil || !*vol.Secret.Optional {
			t.Errorf("optional = %v, want an explicit true", vol.Secret.Optional)
		}

		mount := mountNamed(podSpec, "nats-creds")
		if mount == nil {
			t.Fatalf("volumeMounts = %+v, want one named nats-creds", podSpec.Containers[0].VolumeMounts)
		}
		if mount.MountPath != dir {
			t.Errorf("the nats-creds mount is at %q, want nats.mountPath %q", mount.MountPath, dir)
		}
		if !mount.ReadOnly {
			t.Error("the nats-creds mount is writable, want read-only")
		}
	})
}

// renderNotes renders templates/NOTES.txt with the given values.
// `helm template` never shows NOTES.txt and parses every other template as a
// YAML manifest, so this copies the chart into a temporary directory and
// wraps the NOTES source in a block scalar under a probe template, where the
// prose renders as one YAML string value.
func renderNotes(t *testing.T, values ...string) string {
	t.Helper()

	return renderNotesForAppVersion(t, "", values...)
}

// renderNotesForAppVersion is renderNotes with the chart copy's appVersion replaced.
// `helm template` has no flag to override appVersion,
// and the provisioning-guide URL in NOTES follows it,
// so the tests that pin that URL rewrite Chart.yaml in the copy.
// An empty appVersion keeps the shipped one.
func renderNotesForAppVersion(t *testing.T, appVersion string, values ...string) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "profgate")
	if err := os.CopyFS(dir, os.DirFS(chartDir)); err != nil {
		t.Fatalf("copy the chart: %v", err)
	}
	if appVersion != "" {
		chartYAML := filepath.Join(dir, "Chart.yaml")
		//nolint:gosec // the path is the test's own temporary chart copy
		raw, err := os.ReadFile(chartYAML)
		if err != nil {
			t.Fatalf("read Chart.yaml: %v", err)
		}
		raw = regexp.MustCompile(`(?m)^appVersion: .*$`).ReplaceAll(raw, []byte("appVersion: "+strconv.Quote(appVersion)))
		//nolint:gosec // the path is the test's own temporary chart copy
		if err := os.WriteFile(chartYAML, raw, 0o600); err != nil {
			t.Fatalf("write Chart.yaml: %v", err)
		}
	}
	//nolint:gosec // the path is the test's own temporary chart copy
	notes, err := os.ReadFile(filepath.Join(dir, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatalf("read NOTES.txt: %v", err)
	}
	var probe strings.Builder
	probe.WriteString("notes: |\n")
	for line := range strings.Lines(string(notes)) {
		probe.WriteString("  " + line)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "notes-probe.yaml"), []byte(probe.String()), 0o600); err != nil {
		t.Fatalf("write the NOTES probe template: %v", err)
	}

	args := []string{"template", "profgate", dir, "--namespace", "profgate", "-s", "templates/notes-probe.yaml"}
	args = append(args, values...)

	var doc struct {
		Notes string `json:"notes"`
	}
	if err := yaml.Unmarshal(runHelm(t, args...), &doc); err != nil {
		t.Fatalf("unmarshal the rendered NOTES probe: %v", err)
	}

	return doc.Notes
}

// TestChartNATSURL covers how the effective NATS URL is resolved.
// A url key in the raw config block wins over nats.url by presence, not by
// value, because the merge copies the raw block over the structured keys;
// the requiredness check and the NOTES.txt print both follow that same rule,
// so a URL the render accepts is the URL the gateway reads and the URL the
// operator is told about.
func TestChartNATSURL(t *testing.T) {
	t.Run("no url anywhere fails at render", func(t *testing.T) {
		// nats.url ships empty, so pgo.enabled alone leaves the gateway
		// without a NATS URL in either the structured values or the raw
		// config block, and rendering has to refuse that.
		stderr := renderFailure(t, "--set", "pgo.enabled=true")
		if want := "nats.url is required when pgo.enabled is true"; !strings.Contains(stderr, want) {
			t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
		}
	})

	t.Run("an empty raw url fails at render", func(t *testing.T) {
		// Without the presence rule this combination renders: the structured
		// URL satisfies the requiredness check, and the merge then replaces
		// it with the raw block's empty string, which startup rejects.
		stderr := renderFailure(t,
			"--set", "pgo.enabled=true",
			"--set", "nats.credsFile=",
			"--set", "nats.url=nats://nats.profgate.svc:4222",
			"--set-json", `config={"nats":{"url":""}}`,
		)
		if want := "config.nats.url is empty"; !strings.Contains(stderr, want) {
			t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
		}
	})

	// `required` treats any non-nil value as present, so without a type
	// check a boolean or numeric url would render into the configuration
	// and fail only at startup, in a field the gateway reads as a string.
	t.Run("a boolean raw url fails at render", func(t *testing.T) {
		stderr := renderFailure(t,
			"--set", "pgo.enabled=true",
			"--set", "nats.credsFile=",
			"--set-json", `config={"nats":{"url":true}}`,
		)
		if want := "config.nats.url true has type bool, not string"; !strings.Contains(stderr, want) {
			t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
		}
	})

	t.Run("a numeric raw url fails at render", func(t *testing.T) {
		stderr := renderFailure(t,
			"--set", "pgo.enabled=true",
			"--set", "nats.credsFile=",
			"--set-json", `config={"nats":{"url":123}}`,
		)
		if want := "config.nats.url 123 has type float64, not string"; !strings.Contains(stderr, want) {
			t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
		}
	})

	t.Run("a numeric structured url fails at render", func(t *testing.T) {
		stderr := renderFailure(t,
			"--set", "pgo.enabled=true",
			"--set", "nats.credsFile=",
			"--set", "nats.url=4222",
		)
		if want := "nats.url 4222 has type int64, not string"; !strings.Contains(stderr, want) {
			t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
		}
	})

	// The scheme check mirrors the gateway's own startup validation:
	// comma-separated URLs, each beginning with nats:// or tls://.
	t.Run("a url with another scheme fails at render", func(t *testing.T) {
		stderr := renderFailure(t,
			"--set", "pgo.enabled=true",
			"--set", "nats.credsFile=",
			"--set", "nats.url=http://nats.profgate.svc:4222",
		)
		if want := `nats.url "http://nats.profgate.svc:4222": every comma-separated URL must begin with nats:// or tls://`; !strings.Contains(stderr, want) {
			t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
		}
	})

	t.Run("a comma-separated pair renders", func(t *testing.T) {
		cfg := loadRenderedConfig(t,
			"--set", "pgo.enabled=true",
			"--set", "nats.credsFile=",
			"--set-string", `nats.url=nats://one.profgate.svc:4222\,tls://two.profgate.svc:4222`,
		)
		if got, want := cfg.NATS.URL, "nats://one.profgate.svc:4222,tls://two.profgate.svc:4222"; got != want {
			t.Errorf("nats.url = %q, want %q", got, want)
		}
	})

	t.Run("a raw url wins over the structured one", func(t *testing.T) {
		values := []string{
			"--set", "pgo.enabled=true",
			"--set", "nats.credsFile=",
			"--set", "nats.url=nats://structured.profgate.svc:4222",
			"--set-json", `config={"nats":{"url":"nats://raw.profgate.svc:4222"}}`,
		}
		if got, want := loadRenderedConfig(t, values...).NATS.URL, "nats://raw.profgate.svc:4222"; got != want {
			t.Errorf("nats.url = %q, want the raw block's %q", got, want)
		}
		notes := renderNotes(t, values...)
		if !strings.Contains(notes, "nats://raw.profgate.svc:4222") {
			t.Errorf("NOTES does not print the effective URL nats://raw.profgate.svc:4222:\n%s", notes)
		}
		if strings.Contains(notes, "nats://structured.profgate.svc:4222") {
			t.Errorf("NOTES prints the structured URL the merge discards:\n%s", notes)
		}
	})

	t.Run("a raw-only url renders and reaches NOTES", func(t *testing.T) {
		values := []string{
			"--set", "pgo.enabled=true",
			"--set", "nats.credsFile=",
			"--set-json", `config={"nats":{"url":"nats://raw.profgate.svc:4222"}}`,
		}
		if got, want := loadRenderedConfig(t, values...).NATS.URL, "nats://raw.profgate.svc:4222"; got != want {
			t.Errorf("nats.url = %q, want the raw block's %q", got, want)
		}
		if notes := renderNotes(t, values...); !strings.Contains(notes, "nats://raw.profgate.svc:4222") {
			t.Errorf("NOTES does not print the effective URL nats://raw.profgate.svc:4222:\n%s", notes)
		}
	})

	t.Run("the structured url reaches NOTES", func(t *testing.T) {
		notes := renderNotes(t, append(pgoValues(t), "--set", "nats.url=nats://structured.profgate.svc:4222")...)
		if !strings.Contains(notes, "nats://structured.profgate.svc:4222") {
			t.Errorf("NOTES does not print the structured URL:\n%s", notes)
		}
	})
}

// TestChartNotesProvisioningGuideURL pins the provisioning-guide URL NOTES
// prints to the release's appVersion:
// a checkout tracks latest and links the main branch,
// and a released chart links the tag its appVersion names.
func TestChartNotesProvisioningGuideURL(t *testing.T) {
	t.Run("latest links main", func(t *testing.T) {
		notes := renderNotes(t, pgoValues(t)...)
		want := "https://github.com/arloliu/profgate/blob/main/deploy/nats/README.md"
		if !strings.Contains(notes, want) {
			t.Errorf("NOTES does not print %s:\n%s", want, notes)
		}
	})

	t.Run("a release tag links itself", func(t *testing.T) {
		notes := renderNotesForAppVersion(t, "v0.9.9", pgoValues(t)...)
		want := "https://github.com/arloliu/profgate/blob/v0.9.9/deploy/nats/README.md"
		if !strings.Contains(notes, want) {
			t.Errorf("NOTES does not print %s:\n%s", want, notes)
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

// TestChartPodDisruptionBudget holds the budget to exactly one of its two
// expressions. minAvailable and maxUnavailable state the same budget from
// opposite ends, so a values file carrying both is a contradiction waiting
// to be half-ignored; the chart refuses to render it instead.
func TestChartPodDisruptionBudget(t *testing.T) {
	t.Run("default renders minAvailable", func(t *testing.T) {
		spec := render[policyv1.PodDisruptionBudget](t, "poddisruptionbudget.yaml").Spec
		if spec.MinAvailable == nil || spec.MinAvailable.String() != "1" {
			t.Errorf("minAvailable = %v, want the default 1", spec.MinAvailable)
		}
		if spec.MaxUnavailable != nil {
			t.Errorf("maxUnavailable = %v, want none", spec.MaxUnavailable)
		}
	})

	t.Run("cleared minAvailable renders maxUnavailable", func(t *testing.T) {
		spec := render[policyv1.PodDisruptionBudget](t, "poddisruptionbudget.yaml",
			"--set", "podDisruptionBudget.minAvailable=",
			"--set", "podDisruptionBudget.maxUnavailable=1").Spec
		if spec.MaxUnavailable == nil || spec.MaxUnavailable.String() != "1" {
			t.Errorf("maxUnavailable = %v, want 1", spec.MaxUnavailable)
		}
		if spec.MinAvailable != nil {
			t.Errorf("minAvailable = %v, want none", spec.MinAvailable)
		}
	})

	t.Run("both set fails", func(t *testing.T) {
		stderr := renderFailure(t, "--set", "podDisruptionBudget.maxUnavailable=1")
		want := `podDisruptionBudget.minAvailable and podDisruptionBudget.maxUnavailable express the same budget, so set exactly one and clear the other (minAvailable: "")`
		if !strings.Contains(stderr, want) {
			t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
		}
	})

	// Zero is a real bound, not an unset one: maxUnavailable: 0 forbids
	// voluntary disruption, so it must render, and it must trip the
	// both-set guard like any other value.
	t.Run("zero maxUnavailable renders", func(t *testing.T) {
		spec := render[policyv1.PodDisruptionBudget](t, "poddisruptionbudget.yaml",
			"--set", "podDisruptionBudget.minAvailable=",
			"--set", "podDisruptionBudget.maxUnavailable=0").Spec
		if spec.MaxUnavailable == nil || spec.MaxUnavailable.String() != "0" {
			t.Errorf("maxUnavailable = %v, want 0", spec.MaxUnavailable)
		}
		if spec.MinAvailable != nil {
			t.Errorf("minAvailable = %v, want none", spec.MinAvailable)
		}
	})

	t.Run("zero minAvailable renders", func(t *testing.T) {
		spec := render[policyv1.PodDisruptionBudget](t, "poddisruptionbudget.yaml",
			"--set", "podDisruptionBudget.minAvailable=0",
			"--set", "podDisruptionBudget.maxUnavailable=null").Spec
		if spec.MinAvailable == nil || spec.MinAvailable.String() != "0" {
			t.Errorf("minAvailable = %v, want 0", spec.MinAvailable)
		}
		if spec.MaxUnavailable != nil {
			t.Errorf("maxUnavailable = %v, want none", spec.MaxUnavailable)
		}
	})

	t.Run("both set fails even when one is zero", func(t *testing.T) {
		want := `podDisruptionBudget.minAvailable and podDisruptionBudget.maxUnavailable express the same budget`
		for name, values := range map[string][]string{
			"zero maxUnavailable beside the default minAvailable": {
				"--set", "podDisruptionBudget.maxUnavailable=0",
			},
			"zero minAvailable beside maxUnavailable": {
				"--set", "podDisruptionBudget.minAvailable=0",
				"--set", "podDisruptionBudget.maxUnavailable=1",
			},
		} {
			t.Run(name, func(t *testing.T) {
				stderr := renderFailure(t, values...)
				if !strings.Contains(stderr, want) {
					t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
				}
			})
		}
	})

	t.Run("neither set fails", func(t *testing.T) {
		stderr := renderFailure(t, "--set", "podDisruptionBudget.minAvailable=")
		want := `podDisruptionBudget is enabled with neither minAvailable nor maxUnavailable`
		if !strings.Contains(stderr, want) {
			t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
		}
	})

	t.Run("a percentage renders", func(t *testing.T) {
		spec := render[policyv1.PodDisruptionBudget](t, "poddisruptionbudget.yaml",
			"--set", "podDisruptionBudget.minAvailable=50%").Spec
		if spec.MinAvailable == nil || spec.MinAvailable.String() != "50%" {
			t.Errorf("minAvailable = %v, want the percentage 50%%", spec.MinAvailable)
		}
		if spec.MaxUnavailable != nil {
			t.Errorf("maxUnavailable = %v, want none", spec.MaxUnavailable)
		}
	})

	// A bound outside what a PodDisruptionBudget can carry has to fail the
	// render rather than reach the API server as a value it cannot decode,
	// such as false or map[].
	t.Run("a junk bound fails", func(t *testing.T) {
		for name, tc := range map[string]struct {
			values []string
			want   string
		}{
			"boolean": {
				values: []string{"--set", "podDisruptionBudget.minAvailable=false"},
				want:   "podDisruptionBudget.minAvailable is a bool",
			},
			"mapping": {
				values: []string{"--set", "podDisruptionBudget.minAvailable.a=1"},
				want:   "podDisruptionBudget.minAvailable is a map",
			},
			"whitespace string": {
				values: []string{"--set", "podDisruptionBudget.minAvailable= "},
				want:   `podDisruptionBudget.minAvailable " " is neither a non-negative integer nor a percentage`,
			},
		} {
			t.Run(name, func(t *testing.T) {
				stderr := renderFailure(t, tc.values...)
				if !strings.Contains(stderr, tc.want) {
					t.Errorf("helm failed with %q, want it to contain %q", stderr, tc.want)
				}
			})
		}
	})

	// A numeric bound decodes into IntOrString's int32, so the ceiling is
	// 2147483647: that value renders, and one past it fails arithmetically.
	// A check on the number's printed form would pass 2147483648 and hand
	// the API server a value the field cannot decode.
	t.Run("the int32 ceiling renders", func(t *testing.T) {
		spec := render[policyv1.PodDisruptionBudget](t, "poddisruptionbudget.yaml",
			"--set", "podDisruptionBudget.minAvailable=2147483647").Spec
		if spec.MinAvailable == nil || spec.MinAvailable.String() != "2147483647" {
			t.Errorf("minAvailable = %v, want 2147483647, the largest count the field decodes", spec.MinAvailable)
		}
	})

	t.Run("a bound past the int32 ceiling fails", func(t *testing.T) {
		stderr := renderFailure(t, "--set", "podDisruptionBudget.minAvailable=2147483648")
		want := "podDisruptionBudget.minAvailable 2147483648 is neither a non-negative integer at most 2147483647"
		if !strings.Contains(stderr, want) {
			t.Errorf("helm failed with %q, want it to contain %q", stderr, want)
		}
	})

	// A values-file or --set-json number arrives as float64, and an
	// integral float64 of this size prints in exponent notation, which no
	// digit-string check accepts and no IntOrString decodes; the bound is
	// checked arithmetically and rendered as the plain integer it holds.
	t.Run("an integral --set-json bound renders the integer", func(t *testing.T) {
		spec := render[policyv1.PodDisruptionBudget](t, "poddisruptionbudget.yaml",
			"--set-json", "podDisruptionBudget.minAvailable=2147483647.0").Spec
		if spec.MinAvailable == nil || spec.MinAvailable.String() != "2147483647" {
			t.Errorf("minAvailable = %v, want the plain integer 2147483647", spec.MinAvailable)
		}
	})
}
