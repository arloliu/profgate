// Package e2e holds the end-to-end suite for the gateway: the lane matrix and
// scenario registry compile without a build tag so unit tests cover them, while
// the harness and the scenarios that drive a kind cluster sit behind the e2e tag.
package e2e

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Lane is one row of versions.yaml: a Kubernetes version the suite runs against,
// the kind binary that can create it, and what the lane is able to prove.
type Lane struct {
	Name          string `yaml:"name"`
	Frozen        bool   `yaml:"frozen"`
	Degraded      bool   `yaml:"degraded"`
	NetworkPolicy bool   `yaml:"networkPolicy"`
	Kind          string `yaml:"kind"`
	Image         string `yaml:"image"`
}

// knownKindVersions lists the kind binaries mise.toml pins; a lane may only name one of them.
var knownKindVersions = [...]string{"0.22.0", "0.32.0"}

// digestRe matches the "@sha256:<64 hex>" suffix a lane image must carry.
var digestRe = regexp.MustCompile(`^[^@]+@sha256:[0-9a-f]{64}$`)

// KnownKindVersions returns a copy of the kind versions a lane may name.
func KnownKindVersions() []string { return slices.Clone(knownKindVersions[:]) }

// LoadLanes reads and validates the lane matrix at path.
// A lane image is a registry-free repository path pinned by a 64-hex sha256 digest,
// the kind version is one mise pins, degraded is only allowed on a frozen lane,
// and at least one lane must enforce NetworkPolicy so the scenarios that need it run somewhere.
func LoadLanes(path string) ([]Lane, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is the lane matrix the caller names, reading it is the purpose
	if err != nil {
		return nil, fmt.Errorf("read lanes: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	var lanes []Lane
	if err := dec.Decode(&lanes); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validateLanes(lanes); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return lanes, nil
}

// validateLanes checks every lane and the matrix as a whole.
func validateLanes(lanes []Lane) error {
	if len(lanes) == 0 {
		return errors.New("no lanes defined")
	}
	seen := make(map[string]bool, len(lanes))
	enforcing := false
	for _, l := range lanes {
		if l.Name == "" {
			return errors.New("lane without a name")
		}
		if seen[l.Name] {
			return fmt.Errorf("lane %q defined twice", l.Name)
		}
		seen[l.Name] = true
		if err := validateImage(l.Image); err != nil {
			return fmt.Errorf("lane %q: %w", l.Name, err)
		}
		if !slices.Contains(knownKindVersions[:], l.Kind) {
			return fmt.Errorf("lane %q: kind %q is not one of %v", l.Name, l.Kind, knownKindVersions)
		}
		if l.Degraded && !l.Frozen {
			return fmt.Errorf("lane %q: degraded is only allowed on a frozen lane", l.Name)
		}
		enforcing = enforcing || l.NetworkPolicy
	}
	if !enforcing {
		return errors.New("no lane has networkPolicy: true")
	}
	return nil
}

// validateImage requires a repository path without a registry host and with a sha256 digest.
// The registry comes from PROFGATE_E2E_REGISTRY at run time, so a host in the file would be
// silently doubled when the harness prepends it.
func validateImage(image string) error {
	if image == "" {
		return errors.New("image is empty")
	}
	first, _, _ := strings.Cut(image, "/")
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return fmt.Errorf("image %q names a registry host; the registry comes from PROFGATE_E2E_REGISTRY", image)
	}
	if !digestRe.MatchString(image) {
		return fmt.Errorf("image %q is not pinned by a sha256 digest of 64 hex characters", image)
	}
	return nil
}

// LaneNames returns the lane names in file order; unfrozenOnly keeps only lanes with Frozen == false.
func LaneNames(lanes []Lane, unfrozenOnly bool) []string {
	names := make([]string, 0, len(lanes))
	for _, l := range lanes {
		if unfrozenOnly && l.Frozen {
			continue
		}
		names = append(names, l.Name)
	}
	return names
}
