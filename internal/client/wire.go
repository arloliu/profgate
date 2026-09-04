package client

import "fmt"

// The response shapes the table renderer decodes, one per listing route.
// Each holds what a table prints and nothing else; --output json never passes through them.

// WhoamiResponse is GET /v1/whoami: the principal and its realm.
type WhoamiResponse struct {
	Principal string `json:"principal"`
	Realm     struct {
		Name       string   `json:"name"`
		Namespaces []string `json:"namespaces"`
		Services   []string `json:"services"`
		Profiles   []string `json:"profiles"`
		PGO        struct {
			Read      bool `json:"read"`
			Collect   bool `json:"collect"`
			Configure bool `json:"configure"`
		} `json:"pgo"`
	} `json:"realm"`
}

// LimitsResponse is GET /v1/limits.
type LimitsResponse struct {
	CPUSeconds   int      `json:"cpuSeconds"`
	TraceSeconds int      `json:"traceSeconds"`
	Profiles     []string `json:"profiles"`
	Pprof        struct {
		Default           PortSelection   `json:"default"`
		AllowedSelections []PortSelection `json:"allowedSelections"`
	} `json:"pprof"`
	PGO struct {
		Enabled bool `json:"enabled"`
	} `json:"pgo"`
}

// PortSelection is one {"port": N}, {"port": "*"}, {"portName": "name"}, or
// {"portName": "*"} object; Port is a number or the wildcard string.
type PortSelection struct {
	Port     any    `json:"port,omitempty"`
	PortName string `json:"portName,omitempty"`
}

// String prints the selection as "port 6060", "port *", or "portName pprof".
func (p PortSelection) String() string {
	if p.Port != nil {
		return fmt.Sprintf("port %v", p.Port)
	}
	return "portName " + p.PortName
}

// NamespacesResponse is GET /v1/namespaces.
type NamespacesResponse struct {
	Namespaces []string `json:"namespaces"`
}

// ServicesResponse is GET /v1/namespaces/{ns}/services.
type ServicesResponse struct {
	Namespace string   `json:"namespace"`
	Services  []string `json:"services"`
}

// TargetsResponse is GET .../targets;
// the last two fields are present only for a request that sent explain=true.
type TargetsResponse struct {
	Targets         []Target    `json:"targets"`
	SelectorMatched int         `json:"selectorMatched"`
	Excluded        []Exclusion `json:"excluded"`
}

// Exclusion is one reason the gateway counted, and how many Pods it kept out.
// The reason is kept as the text the gateway sent, so a reason this client does not know prints unchanged.
type Exclusion struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// Target is one Pod the gateway would profile right now.
type Target struct {
	Pod     string `json:"pod"`
	Node    string `json:"node"`
	Version string `json:"version"`
}

// CollectionsResponse is GET .../collections: the Service's records, newest first.
type CollectionsResponse struct {
	Collections []CollectionSummary `json:"collections"`
}

// CollectionSummary is one listing entry; createdAt is kept as the string
// the gateway sent, so the table prints it unchanged.
type CollectionSummary struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Origin    string `json:"origin"`
	CreatedAt string `json:"createdAt"`
}

// CollectionRecord is GET /v1/collections/{id}: the fields the table prints of the full stored record.
// The gateway sends finishedAt and expiresAt as a timestamp or as null,
// and each is kept as the string it sent, so one shape decodes a record that has ended and one that has not.
type CollectionRecord struct {
	ID              string              `json:"id"`
	State           string              `json:"state"`
	Origin          string              `json:"origin"`
	Reason          string              `json:"reason"`
	Progress        CollectionProgress  `json:"progress"`
	ResolvedVersion string              `json:"resolvedVersion"`
	FinishedAt      string              `json:"finishedAt"`
	ExpiresAt       string              `json:"expiresAt"`
	Artifact        *CollectionArtifact `json:"artifact"`
}

// CollectionArtifact is the merged profile a completed record names.
type CollectionArtifact struct {
	Object string `json:"object"`
	Bytes  int64  `json:"bytes"`
}

// CollectionProgress is the owner's last checkpoint; all zero until the
// first round has been claimed.
type CollectionProgress struct {
	Round         int `json:"round"`
	Rounds        int `json:"rounds"`
	SamplesOK     int `json:"samplesOK"`
	SamplesFailed int `json:"samplesFailed"`
}

// PolicyResponse is the body of the policy route, on a read and on a write:
// the source, the effective policy, the violations, and the two update fields a stored override carries.
// Durations stay the strings the gateway sent and replicas is "all" or a
// count, so the table prints each unchanged.
type PolicyResponse struct {
	Source    string `json:"source"`
	Effective struct {
		Enabled  bool `json:"enabled"`
		Schedule struct {
			Every  string `json:"every"`
			Jitter string `json:"jitter"`
		} `json:"schedule"`
		Sampling struct {
			Duration      string `json:"duration"`
			Rounds        int    `json:"rounds"`
			RoundInterval string `json:"roundInterval"`
			Replicas      any    `json:"replicas"`
			MaxParallel   int    `json:"maxParallel"`
		} `json:"sampling"`
		Target struct {
			Version string `json:"version"`
		} `json:"target"`
		Artifact struct {
			Retention string `json:"retention"`
		} `json:"artifact"`
	} `json:"effective"`
	Violations []PolicyViolation `json:"violations"`
	UpdatedBy  string            `json:"updatedBy"`
	UpdatedAt  string            `json:"updatedAt"`
}

// PolicyViolation is one effective field a current ceiling would refuse.
type PolicyViolation struct {
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

// Decode decodes one response body into T: exactly one JSON document, and
// a mismatch between the document and the shape is an error.
func Decode[T any](body []byte) (T, error) {
	var v T
	if err := decodeOne(body, &v); err != nil {
		return v, err
	}
	return v, nil
}
