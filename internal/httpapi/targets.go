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

// exclusionView is one entry of the excluded array:
// a reason from the gateway's own vocabulary and a count, and nothing about any Pod.
type exclusionView struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// explainBody is the targets response with the counts:
// the same targets the plain body carries, plus the two fields explain=true adds.
type explainBody struct {
	Namespace       string          `json:"namespace"`
	Service         string          `json:"service"`
	Targets         []targetView    `json:"targets"`
	SelectorMatched int             `json:"selectorMatched"`
	Excluded        []exclusionView `json:"excluded"`
}

// targetViews converts targets to their views and sorts them by Pod name.
// Both bodies build their targets through it, so the two can never disagree on the view or its order.
func targetViews(targets []k8s.Target) []targetView {
	views := make([]targetView, 0, len(targets))
	for _, t := range targets {
		views = append(views, targetView{Pod: t.Pod, Node: t.Node, Version: t.Version})
	}
	slices.SortFunc(views, func(a, b targetView) int { return strings.Compare(a.Pod, b.Pod) })

	return views
}

// writeTargets writes the targets response, sorted by Pod name, with an empty array rather than null
// when the Service has no eligible backend.
func writeTargets(w http.ResponseWriter, namespace, service string, targets []k8s.Target) {
	writeJSONOK(w, targetsBody{Namespace: namespace, Service: service, Targets: targetViews(targets)})
}

// writeExplain writes the targets response with the exclusion counts:
// targets sorted by Pod name, and excluded as an empty array rather than null when every selected Pod is a target.
func writeExplain(w http.ResponseWriter, namespace, service string, targets []k8s.Target, selectorMatched int, excluded []exclusionView) {
	if excluded == nil {
		excluded = []exclusionView{}
	}
	writeJSONOK(w, explainBody{
		Namespace: namespace, Service: service, Targets: targetViews(targets),
		SelectorMatched: selectorMatched, Excluded: excluded,
	})
}

// writeJSONOK writes body as a 200 application/json response.
func writeJSONOK(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Encode adds the trailing newline; a failure here is the client's connection going away.
	_ = json.NewEncoder(w).Encode(body)
}
