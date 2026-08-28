package client

import "fmt"

// The response shapes the table renderer decodes, one per listing route.
// Each holds what a table prints and nothing else; --output json never
// passes through them.

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

// TargetsResponse is GET .../targets.
type TargetsResponse struct {
	Targets []Target `json:"targets"`
}

// Target is one Pod the gateway would profile right now.
type Target struct {
	Pod     string `json:"pod"`
	Node    string `json:"node"`
	Version string `json:"version"`
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
