package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The two mountinfo lines every fixture below is built from.
// The fields are id, parent, major:minor, root, mount point, mount options,
// optional fields, a lone separator, fstype, source, and super options.
const (
	v2Mount = "36 35 0:31 %s %s rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw"
	v1Mount = "31 30 0:27 %s %s rw,nosuid,nodev,noexec,relatime shared:5 - cgroup cgroup rw,memory"
)

// cgroupTree is a filesystem root a resolution runs against.
// cgroup and mountinfo are the bodies of the two /proc files;
// files are paths under the root and what each holds;
// dirs are paths under the root that exist as directories,
// which is how a fixture makes a file unreadable without depending on the user the test runs as.
type cgroupTree struct {
	cgroup    string
	mountinfo string
	files     map[string]string
	dirs      []string
}

// write materializes the tree under a fresh temporary directory and returns its path.
func (tr cgroupTree) write(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeUnder := func(rel, body string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if tr.cgroup != "" {
		writeUnder("proc/self/cgroup", tr.cgroup)
	}
	if tr.mountinfo != "" {
		writeUnder("proc/self/mountinfo", tr.mountinfo)
	}
	for rel, body := range tr.files {
		writeUnder(rel, body)
	}
	for _, rel := range tr.dirs {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}

	return root
}

// mount formats one mountinfo line from a template, a mount root, and a mount point.
func mount(template, root, point string) string {
	return fmt.Sprintf(template, root, point)
}

// TestCgroupMemoryLimitResolution drives the resolution itself:
// which membership line is taken, which mount admits it,
// what the mount root leaves of the membership path, and what each reading means.
// Every row runs against a fixture tree, so no row reads the cgroup this test is running in.
func TestCgroupMemoryLimitResolution(t *testing.T) {
	const v2Only = "0::/\n"

	for _, tc := range []struct {
		name string
		tree cgroupTree
		want int64
		ok   bool
	}{
		{
			name: "the mount root is subtracted from the membership path",
			tree: cgroupTree{
				cgroup:    "0::/tenant/container\n",
				mountinfo: mount(v2Mount, "/tenant", "/sys/fs/cgroup") + "\n",
				files: map[string]string{
					"sys/fs/cgroup/container/memory.max":        "12345",
					"sys/fs/cgroup/tenant/container/memory.max": "999",
				},
			},
			want: 12345,
			ok:   true,
		},
		{
			name: "a mount root matches on components, not on characters",
			tree: cgroupTree{
				cgroup:    "0::/tenantry\n",
				mountinfo: mount(v2Mount, "/tenant", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "999"},
			},
		},
		{
			name: "a membership equal to the mount root leaves the mount point itself",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "4194304"},
			},
			want: 4194304,
			ok:   true,
		},
		{
			name: "a membership of / under a mount rooted at /child does not match",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: mount(v2Mount, "/child", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "999"},
			},
		},
		{
			name: "an escaping suffix rejects that mount and continues the search",
			tree: cgroupTree{
				cgroup: "0::/../../init\n",
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n" +
					mount(v2Mount, "/../..", "/hostcg") + "\n",
				files: map[string]string{
					"sys/fs/cgroup/memory.max": "111",
					"hostcg/init/memory.max":   "777",
				},
			},
			want: 777,
			ok:   true,
		},
		{
			name: "a parent visible at the mount point is not read when the membership names a child",
			tree: cgroupTree{
				cgroup:    "0::/child\n",
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files: map[string]string{
					"sys/fs/cgroup/memory.max":       "8589934592",
					"sys/fs/cgroup/child/memory.max": "1073741824",
				},
			},
			want: 1073741824,
			ok:   true,
		},
		{
			name: "a hybrid tree takes the version 1 memory membership over the bare 0:: line",
			tree: cgroupTree{
				cgroup: "7:memory:/v1grp\n0::/unified\n",
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup/unified") + "\n" +
					mount(v1Mount, "/", "/sys/fs/cgroup/memory") + "\n",
				files: map[string]string{
					"sys/fs/cgroup/unified/unified/memory.max":         "222",
					"sys/fs/cgroup/memory/v1grp/memory.limit_in_bytes": "333",
				},
			},
			want: 333,
			ok:   true,
		},
		{
			name: "a mount point carrying an octal escape is decoded",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: mount(v2Mount, "/", `/sys/fs/cgroup\040odd`) + "\n",
				files:     map[string]string{"sys/fs/cgroup odd/memory.max": "555"},
			},
			want: 555,
			ok:   true,
		},
		{
			name: "a membership no candidate mount admits yields no limit",
			tree: cgroupTree{
				cgroup:    "0::/a/b\n",
				mountinfo: mount(v2Mount, "/c", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "999"},
			},
		},
		{
			name: "no memory membership yields no limit",
			tree: cgroupTree{
				cgroup:    "3:cpu,cpuacct:/somewhere\n",
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "999"},
			},
		},
		{
			name: "a membership no mount exposes yields no limit",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: "22 21 0:5 / /proc rw,nosuid - proc proc rw\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "999"},
			},
		},
		{
			name: "every candidate mount leaving an escaping suffix yields no limit",
			tree: cgroupTree{
				cgroup:    "0::/../evil\n",
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "999"},
			},
		},
		{
			name: "an absent file yields no limit, and nothing falls back to the mount point",
			tree: cgroupTree{
				cgroup:    "0::/child\n",
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "999"},
			},
		},
		{
			name: "a file that cannot be read yields no limit",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				dirs:      []string{"sys/fs/cgroup/memory.max"},
			},
		},
		{
			name: "a value that does not parse yields no limit",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "not a number\n"},
			},
		},
		{
			name: "max under version 2 yields no limit",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "max\n"},
			},
		},
		{
			name: "a negative value is refused rather than treated as small",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "-1\n"},
			},
		},
		{
			name: "a zero value is refused rather than treated as small",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "0\n"},
			},
		},
		{
			name: "the version 1 sentinel means unlimited",
			tree: cgroupTree{
				cgroup:    "7:memory:/\n",
				mountinfo: mount(v1Mount, "/", "/sys/fs/cgroup/memory") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n"},
			},
		},
		{
			name: "the same reading under version 2 is a real limit and binds",
			tree: cgroupTree{
				cgroup:    v2Only,
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				files:     map[string]string{"sys/fs/cgroup/memory.max": "4611686018427387904\n"},
			},
			want: 4611686018427387904,
			ok:   true,
		},
		{
			name: "a version 1 reading below the sentinel binds",
			tree: cgroupTree{
				cgroup:    "7:memory:/kubepods/pod1\n",
				mountinfo: mount(v1Mount, "/", "/sys/fs/cgroup/memory") + "\n",
				files: map[string]string{
					"sys/fs/cgroup/memory/kubepods/pod1/memory.limit_in_bytes": "1610612736\n",
				},
			},
			want: 1610612736,
			ok:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cgroupMemoryLimit(tc.tree.write(t))
			if ok != tc.ok {
				t.Fatalf("cgroupMemoryLimit() ok = %v, want %v (limit %d)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Fatalf("cgroupMemoryLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSoftMemoryLimit drives the figure the process holds itself to:
// nine tenths of the smaller of the container figure the configuration derives
// and the memory limit of the cgroup the fixture tree describes.
// The derived figure at the shipped ceilings is 2046820352,
// so the answer where no cgroup limit binds is exactly 1842138315,
// which is what makes the integer division a decision rather than an accident.
func TestSoftMemoryLimit(t *testing.T) {
	const (
		derived  = int64(1952 << 20)
		derived9 = int64(1842138315)
	)

	cfg, err := Load("testdata/pgo-full.yaml")
	if err != nil {
		t.Fatalf("Load(pgo-full.yaml) error = %v", err)
	}
	if got := cfg.GatewayMemoryBytes(); got != derived {
		t.Fatalf("GatewayMemoryBytes() = %d, want %d: every row below is banded against it", got, derived)
	}

	v2 := func(body string) cgroupTree {
		return cgroupTree{
			cgroup:    "0::/\n",
			mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
			files:     map[string]string{"sys/fs/cgroup/memory.max": body},
		}
	}
	v1 := func(body string) cgroupTree {
		return cgroupTree{
			cgroup:    "7:memory:/\n",
			mountinfo: mount(v1Mount, "/", "/sys/fs/cgroup/memory") + "\n",
			files:     map[string]string{"sys/fs/cgroup/memory/memory.limit_in_bytes": body},
		}
	}

	for _, tc := range []struct {
		name string
		tree cgroupTree
		want int64
	}{
		{name: "a version 2 limit below the derived figure binds", tree: v2("1073741824\n"), want: 966367638},
		{name: "max leaves the derived figure alone", tree: v2("max\n"), want: derived9},
		{name: "a version 2 limit above the derived figure does not bind", tree: v2("8589934592\n"), want: derived9},
		{name: "a tree with no cgroup file leaves the derived figure alone", tree: cgroupTree{}, want: derived9},
		{
			name: "a file that cannot be read leaves the derived figure alone",
			tree: cgroupTree{
				cgroup:    "0::/\n",
				mountinfo: mount(v2Mount, "/", "/sys/fs/cgroup") + "\n",
				dirs:      []string{"sys/fs/cgroup/memory.max"},
			},
			want: derived9,
		},
		{name: "a value that does not parse leaves the derived figure alone", tree: v2("not a number\n"), want: derived9},
		{name: "a version 1 limit below the derived figure binds", tree: v1("1610612736\n"), want: 1449551457},
		{name: "the version 1 sentinel leaves the derived figure alone", tree: v1("9223372036854771712\n"), want: derived9},
		{
			name: "the same reading under version 2 is a real limit, and is the larger of the two",
			tree: v2("4611686018427387904\n"),
			want: derived9,
		},
		{name: "a negative reading leaves the derived figure alone", tree: v2("-1\n"), want: derived9},
		{name: "a zero reading leaves the derived figure alone", tree: v2("0\n"), want: derived9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cfg.softMemoryLimit(tc.tree.write(t))
			if !ok {
				t.Fatalf("softMemoryLimit() ok = false, want a limit for a configuration that collects")
			}
			if got != tc.want {
				t.Fatalf("softMemoryLimit() = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("a process that collects nothing sets no limit", func(t *testing.T) {
		off, err := Load("testdata/pgo-disabled.yaml")
		if err != nil {
			t.Fatalf("Load(pgo-disabled.yaml) error = %v", err)
		}
		root := v2("1073741824\n").write(t)
		if got, ok := off.softMemoryLimit(root); ok {
			t.Fatalf("softMemoryLimit() with pgo.enabled false = %d, true; want no limit whatever the tree holds", got)
		}
	})
}
