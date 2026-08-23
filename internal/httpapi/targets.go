package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/arloliu/profgate/internal/k8s"
)

// targetView is what the targets endpoint says about one backend: never its address.
type targetView struct {
	Pod     string `json:"pod"`
	Node    string `json:"node"`
	Version string `json:"version"`
}

// targetsBody is the targets endpoint's response.
type targetsBody struct {
	Namespace string       `json:"namespace"`
	Service   string       `json:"service"`
	Targets   []targetView `json:"targets"`
}

// writeTargets writes the targets response, sorted by Pod name, with an empty array rather than null
// when the Service has no eligible backend.
func writeTargets(w http.ResponseWriter, namespace, service string, targets []k8s.Target) {
	views := make([]targetView, 0, len(targets))
	for _, t := range targets {
		views = append(views, targetView{Pod: t.Pod, Node: t.Node, Version: t.Version})
	}
	slices.SortFunc(views, func(a, b targetView) int { return strings.Compare(a.Pod, b.Pod) })

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Encode adds the trailing newline; a failure here is the client's connection going away.
	_ = json.NewEncoder(w).Encode(targetsBody{Namespace: namespace, Service: service, Targets: views})
}
