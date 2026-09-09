package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// softLimitNumerator and softLimitDenominator are the share of a figure the soft memory limit takes.
	// The tenth left over is what the process costs outside the Go heap:
	// the runtime's own bookkeeping, goroutine stacks,
	// and whatever else the kernel accounts to the container.
	// The arithmetic divides before it multiplies,
	// so no product of two large numbers is formed.
	softLimitNumerator   = 9
	softLimitDenominator = 10
	// cgroupV1Unlimited is how cgroup version 1 spells no limit:
	// a page-aligned value near the largest int64, which the kernel reports rather than a word.
	// Version 2 spells it "max",
	// so this is a version 1 rule and a version 2 reading of the same number is a real limit.
	cgroupV1Unlimited = int64(1) << 62
	// procCgroup and procMountinfo are the two files the resolution reads, under the root it is given.
	procCgroup    = "proc/self/cgroup"
	procMountinfo = "proc/self/mountinfo"
	// cgroupV2LimitFile and cgroupV1LimitFile are what each version holds its memory limit in.
	cgroupV2LimitFile = "memory.max"
	cgroupV1LimitFile = "memory.limit_in_bytes"
)

// SoftMemoryLimit is the GOMEMLIMIT a collecting process sets:
// 90% of the smaller of the container figure this configuration derives and the memory limit of its own cgroup.
// Reading the cgroup is what makes an explicitly lowered container limit count.
// A process that collects nothing sets no limit:
// the base term is asserted rather than measured,
// and a soft limit over an unmeasured figure would change how an installation behaves that decodes nothing.
func (c *Config) SoftMemoryLimit() (int64, bool) {
	return c.softMemoryLimit("/")
}

// softMemoryLimit is SoftMemoryLimit against a filesystem root,
// so a test can point the cgroup read at a fixture tree instead of at the cgroup the test binary is running in.
func (c *Config) softMemoryLimit(root string) (int64, bool) {
	if !c.PGO.Enabled {
		return 0, false
	}
	figure := c.GatewayMemoryBytes()
	if limit, ok := cgroupMemoryLimit(root); ok && limit < figure {
		figure = limit
	}

	return figure / softLimitDenominator * softLimitNumerator, true
}

// cgroupMemoryLimit is the memory limit of the cgroup this process belongs to, read under root.
// It resolves the limit rather than reading a conventional path,
// because a visible mount can expose a parent cgroup whose limit is not this process's own,
// and a number read there binds nothing.
// The membership comes first,
// then a mount that exposes the hierarchy it names,
// then the mount's own root subtracted from the membership path.
// Every failure means there is no cgroup limit to read.
// An unreadable file is a fact about the sandbox rather than about the ceiling,
// so nothing here is an error the caller has to handle.
func cgroupMemoryLimit(root string) (int64, bool) {
	membership, v2, ok := cgroupMembership(root)
	if !ok {
		return 0, false
	}
	want := cgroupV1LimitFile
	if v2 {
		want = cgroupV2LimitFile
	}
	for _, m := range cgroupMounts(root, v2) {
		dir, ok := cgroupPath(m, membership)
		if !ok {
			// This mount does not expose the process's own cgroup,
			// either because the membership is not under its root,
			// or because what is left would walk out of the hierarchy.
			// Another mount may still expose it.
			continue
		}

		return readCgroupLimit(filepath.Join(root, dir, want), v2)
	}

	return 0, false
}

// cgroupMembership is the cgroup path this process belongs to,
// and whether it came from the unified hierarchy.
// Each line of /proc/self/cgroup is `hierarchy-ID:controller-list:cgroup-path`.
// A version 1 memory membership is a line whose controller list holds `memory`;
// a version 2 membership is the line whose hierarchy id is 0 and whose controller list is empty.
// The version 1 memory membership wins where both exist.
// A kernel may put the memory controller on version 1 while a unified hierarchy also exists,
// so a bare `0::` line alone does not establish that memory is controlled there.
func cgroupMembership(root string) (string, bool, bool) {
	body, err := readUnder(root, procCgroup)
	if err != nil {
		return "", false, false
	}
	var (
		unified     string
		haveUnified bool
	)
	for _, line := range strings.Split(string(body), "\n") {
		// The cgroup path may itself hold a colon, so only the first two are separators.
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		id, controllers, path := parts[0], parts[1], parts[2]
		if controllers != "" {
			if hasOption(controllers, "memory") {
				return path, false, true
			}

			continue
		}
		if id == "0" && !haveUnified {
			unified, haveUnified = path, true
		}
	}

	return unified, haveUnified, haveUnified
}

// cgroupMount is what one mountinfo line contributes to the resolution:
// the subtree of the hierarchy the mount exposes, and where it exposes it.
type cgroupMount struct {
	root  string
	point string
}

// cgroupMounts is every mount that exposes the hierarchy the membership names,
// in the order /proc/self/mountinfo lists them.
// A mountinfo line is `id parent major:minor root mountpoint options… - fstype source superopts`,
// where the optional fields between the options and the lone separator vary in number,
// so the separator is what the tail is found from.
// Version 2 is a `cgroup2` mount;
// version 1 is a `cgroup` mount whose super options name the memory controller.
func cgroupMounts(root string, v2 bool) []cgroupMount {
	body, err := readUnder(root, procMountinfo)
	if err != nil {
		return nil
	}
	var out []cgroupMount
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i

				break
			}
		}
		if sep < 6 || sep+3 >= len(fields) {
			continue
		}
		fstype, superopts := fields[sep+1], fields[sep+3]
		if v2 {
			if fstype != "cgroup2" {
				continue
			}
		} else if fstype != "cgroup" || !hasOption(superopts, "memory") {
			continue
		}
		out = append(out, cgroupMount{root: unescapeMountField(fields[3]), point: unescapeMountField(fields[4])})
	}

	return out
}

// cgroupPath is the directory that holds this membership's limit under one mount,
// and whether the mount exposes it at all.
// The mount's own root is a prefix of the membership path,
// matched on whole path components,
// so that a mount rooted at /tenant does not admit a membership of /tenantry;
// what is left of the membership hangs off the mount point.
// A remainder holding a `..` component can name an unrelated part of the filesystem,
// so it is refused rather than cleaned,
// and refusing it rejects this mount and not the search:
// a membership of /../../init leaves an escaping remainder under a mount rooted at /,
// and a usable one under a mount rooted at /../.., which is the limit that binds.
func cgroupPath(m cgroupMount, membership string) (string, bool) {
	mountRoot := pathComponents(m.root)
	want := pathComponents(membership)
	if len(want) < len(mountRoot) {
		return "", false
	}
	for i, component := range mountRoot {
		if want[i] != component {
			return "", false
		}
	}
	suffix := want[len(mountRoot):]
	for _, component := range suffix {
		if component == ".." {
			return "", false
		}
	}

	return filepath.Join(append([]string{m.point}, suffix...)...), true
}

// readCgroupLimit reads one limit file and says whether it names a limit.
// Version 2 spells unlimited "max",
// and version 1 spells it as a page-aligned value near the largest int64,
// so the sentinel is applied to version 1 only.
// A negative or zero reading is refused rather than treated as small:
// it would derive a soft limit the runtime reads as a query and silently declines to set,
// leaving the process logging a limit it does not hold.
func readCgroupLimit(path string, v2 bool) (int64, bool) {
	body, err := os.ReadFile(path) //nolint:gosec // path is resolved from this process's own cgroup membership
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(body))
	if v2 && text == "max" {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	if !v2 && value >= cgroupV1Unlimited {
		return 0, false
	}

	return value, true
}

// readUnder reads one of the two /proc files under the given filesystem root.
func readUnder(root, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, name)) //nolint:gosec // root is "/" in production and a fixture tree in tests
}

// pathComponents splits a cgroup path into its non-empty components,
// so a prefix test compares components rather than characters.
// It deliberately does not clean the path:
// a `..` component is what the caller has to see in order to refuse it.
func pathComponents(p string) []string {
	var out []string
	for _, component := range strings.Split(p, "/") {
		if component != "" && component != "." {
			out = append(out, component)
		}
	}

	return out
}

// hasOption reports whether a comma-separated option list holds name.
func hasOption(options, name string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == name {
			return true
		}
	}

	return false
}

// unescapeMountField decodes mountinfo's octal escapes:
// space, tab, newline, and the backslash itself, which would otherwise break its field separation.
// The scan is a single left-to-right pass,
// so a decoded backslash is never re-read as the start of another escape.
func unescapeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var b strings.Builder
	b.Grow(len(field))
	for i := 0; i < len(field); {
		if field[i] == '\\' && i+3 < len(field) {
			if value, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(value))
				i += 4

				continue
			}
		}
		b.WriteByte(field[i])
		i++
	}

	return b.String()
}
