package httpapi

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/k8s"
)

const (
	// maxSeconds is the top of the seconds grammar.
	maxSeconds = 86400
	// maxPort is the top of the port grammar.
	maxPort = 65535
	// strategyRandom is the only selection strategy, and the default.
	strategyRandom = "random"
)

// profileSpec maps one profile name to its upstream path and duration rule.
type profileSpec struct {
	name           string
	path           string
	takesSeconds   bool
	defaultSeconds int
}

// profileSpecs is the spec's profile table, in its order.
var profileSpecs = [...]profileSpec{
	{name: "cpu", path: "/debug/pprof/profile", takesSeconds: true, defaultSeconds: 30},
	{name: "trace", path: "/debug/pprof/trace", takesSeconds: true, defaultSeconds: 1},
	{name: "heap", path: "/debug/pprof/heap"},
	{name: "allocs", path: "/debug/pprof/allocs"},
	{name: "goroutine", path: "/debug/pprof/goroutine"},
	{name: "mutex", path: "/debug/pprof/mutex"},
	{name: "block", path: "/debug/pprof/block"},
	{name: "threadcreate", path: "/debug/pprof/threadcreate"},
}

// lookupProfile finds the spec for a profile name.
func lookupProfile(name string) (profileSpec, bool) {
	for _, spec := range profileSpecs {
		if spec.name == name {
			return spec, true
		}
	}

	return profileSpec{}, false
}

// portParams is the client's port selection as parsed:
// the selection discovery receives and the value as the client sent it, for the audit line.
type portParams struct {
	sel  k8s.PortSelection
	sent string // "6061" or "pprof-alt"; empty when neither parameter is present
}

// parsePortParams reads port and portName out of values and removes them,
// so the caller's own loop sees only its own parameters.
// It applies the spec's grammar: each at most once with a value,
// port a decimal integer 1–65535, portName a container-port name, never both.
func parsePortParams(values url.Values) (portParams, *requestError) {
	var port, name string
	for _, key := range []struct {
		name string
		dst  *string
	}{{"port", &port}, {"portName", &name}} {
		vs, ok := values[key.name]
		if !ok {
			continue
		}
		if len(vs) != 1 || vs[0] == "" {
			return portParams{}, invalidParameter(fmt.Sprintf("parameter %q must appear once with a value", key.name))
		}
		*key.dst = vs[0]
		delete(values, key.name)
	}
	switch {
	case port != "" && name != "":
		return portParams{}, invalidParameter("port and portName exclude each other")
	case port != "":
		n, ok := parsePort(port)
		if !ok {
			return portParams{}, invalidParameter("port must be a decimal integer between 1 and 65535")
		}

		return portParams{sel: k8s.PortSelection{Port: n}, sent: port}, nil
	case name != "":
		if len(validation.IsValidPortName(name)) > 0 {
			return portParams{}, invalidParameter("portName must be a container port name")
		}

		return portParams{sel: k8s.PortSelection{PortName: name}, sent: name}, nil
	default:
		return portParams{}, nil
	}
}

// targetsParams is a validated targets query:
// the port selection, the two filters, and whether the caller asked for the exclusion counts.
type targetsParams struct {
	port    portParams
	version string
	pod     string
	explain bool
}

// parseTargetsParams validates the targets endpoint's query against the spec's parameter table,
// in name order, so a query with several faults reports the same one every time.
// port and portName together is a fault of the pair and is checked after the loop.
// The port selection is returned beside any error whenever the selection alone is well-formed,
// so the audit line records it as sent;
// a selection that is malformed, repeated, or doubled leaves it empty.
// values is left as it was.
func parseTargetsParams(values url.Values) (targetsParams, *requestError) {
	var (
		params     targetsParams
		port, name string
		perr       *requestError
	)
	for _, key := range slices.Sorted(maps.Keys(values)) {
		vs := values[key]
		if len(vs) != 1 || vs[0] == "" {
			perr = invalidParameter(fmt.Sprintf("parameter %q must appear once with a value", key))

			break
		}
		value := vs[0]
		switch key {
		case "explain":
			switch value {
			case "true":
				params.explain = true
			case "false":
			default:
				perr = invalidParameter("explain must be true or false")
			}
		case "pod":
			if len(validation.IsDNS1123Subdomain(value)) > 0 {
				perr = invalidParameter("pod must be a DNS-1123 subdomain")
			}
			params.pod = value
		case "port":
			if _, ok := parsePort(value); !ok {
				perr = invalidParameter("port must be a decimal integer between 1 and 65535")
			}
			port = value
		case "portName":
			if len(validation.IsValidPortName(value)) > 0 {
				perr = invalidParameter("portName must be a container port name")
			}
			name = value
		case "version":
			params.version = value
		default:
			perr = invalidParameter(fmt.Sprintf("unknown parameter %q", key))
		}
		if perr != nil {
			break
		}
	}
	// The selection is filled only when it is well-formed on its own,
	// whatever else in the query failed.
	params.port = targetsPortSelection(values)
	if perr == nil && port != "" && name != "" {
		perr = invalidParameter("port and portName exclude each other")
	}

	return params, perr
}

// targetsPortSelection reads the port selection out of a targets query without touching it,
// and is empty unless that selection alone is well-formed.
func targetsPortSelection(values url.Values) portParams {
	copied := url.Values{}
	for _, key := range []string{"port", "portName"} {
		if vs, ok := values[key]; ok {
			copied[key] = vs
		}
	}
	port, perr := parsePortParams(copied)
	if perr != nil {
		return portParams{}
	}

	return port
}

// profileParams is a validated profile request:
// the effective duration (0 for profiles without one), the optional Pod and version filters,
// and the port selection.
// Strategy has one value and no field.
type profileParams struct {
	seconds int
	pod     string
	version string
	port    portParams
}

// parseProfileParams validates the decoded query against the spec's parameter table
// and resolves the effective duration against the configured limit.
// The port selection is parsed first and is returned beside any later error, so the audit line records it as sent;
// the remaining parameters are checked in name order so a query with several faults reports the same one every time,
// and the duration limit is checked only once every parameter is well-formed.
func parseProfileParams(values url.Values, spec profileSpec, limits config.LimitsConfig) (profileParams, *requestError) {
	var (
		params  profileParams
		seconds int
		perr    *requestError
	)
	if params.port, perr = parsePortParams(values); perr != nil {
		return profileParams{}, perr
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		vs := values[name]
		if len(vs) != 1 || vs[0] == "" {
			return params, invalidParameter(fmt.Sprintf("parameter %q must appear once with a value", name))
		}
		value := vs[0]
		switch name {
		case "seconds":
			if !spec.takesSeconds {
				return params, invalidParameter(fmt.Sprintf("profile %s takes no seconds parameter", spec.name))
			}
			n, ok := parseSeconds(value)
			if !ok {
				return params, invalidParameter("seconds must be a decimal integer between 1 and 86400")
			}
			seconds = n
		case "pod":
			if len(validation.IsDNS1123Subdomain(value)) > 0 {
				return params, invalidParameter("pod must be a DNS-1123 subdomain")
			}
			params.pod = value
		case "version":
			params.version = value
		case "strategy":
			if value != strategyRandom {
				return params, invalidParameter("strategy must be random")
			}
		default:
			return params, invalidParameter(fmt.Sprintf("unknown parameter %q", name))
		}
	}

	if spec.takesSeconds {
		params.seconds = spec.defaultSeconds
		if seconds > 0 {
			params.seconds = seconds
		}
		limit := limits.CPUSeconds
		if spec.name == "trace" {
			limit = limits.TraceSeconds
		}
		if params.seconds > limit {
			return params, &requestError{
				status:  http.StatusBadRequest,
				code:    "seconds_exceeds_limit",
				message: fmt.Sprintf("effective duration %ds exceeds the %s limit of %ds", params.seconds, spec.name, limit),
			}
		}
	}

	return params, nil
}

// parseSeconds accepts an unsigned decimal integer in 1..86400 and nothing else:
// no sign, no whitespace, no other base.
func parseSeconds(s string) (int, bool) {
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxSeconds {
		return 0, false
	}

	return n, true
}

// parsePort accepts an unsigned decimal integer in 1..65535 and nothing else:
// no sign, no whitespace, no other base.
func parsePort(s string) (int32, bool) {
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxPort {
		return 0, false
	}

	return int32(n), true //nolint:gosec // bounded by maxPort above
}

// portNotAllowed builds the 400 port_not_allowed error for the parameter that fired,
// port or portName; its message and its one details item name only the value the client sent.
func portNotAllowed(field, sent string) *requestError {
	return &requestError{
		status:  http.StatusBadRequest,
		code:    "port_not_allowed",
		message: fmt.Sprintf("port %q is not allowed by this gateway", sent),
		details: []errorDetail{{Field: field, Code: "not_admitted", Message: sent + " is not an admitted selection"}},
	}
}

// invalidParameter builds the 400 invalid_parameter error.
func invalidParameter(message string) *requestError {
	return &requestError{status: http.StatusBadRequest, code: "invalid_parameter", message: message}
}

// selectTarget applies the version filter, then the Pod name, then the strategy:
// choose picks an index among the targets that remain.
func selectTarget(targets []k8s.Target, params profileParams, choose func(n int) int) (k8s.Target, *requestError) {
	remaining := targets
	if params.version != "" {
		remaining = nil
		for _, t := range targets {
			if t.Version == params.version {
				remaining = append(remaining, t)
			}
		}
	}

	if params.pod != "" {
		for _, t := range remaining {
			if t.Pod == params.pod {
				return t, nil
			}
		}

		return k8s.Target{}, &requestError{
			status:  http.StatusNotFound,
			code:    "pod_not_found",
			message: fmt.Sprintf("pod %s is not an eligible target", params.pod),
		}
	}
	if len(remaining) == 0 {
		return k8s.Target{}, &requestError{
			status:  http.StatusServiceUnavailable,
			code:    "no_targets",
			message: "service has no eligible targets",
		}
	}

	return remaining[choose(len(remaining))], nil
}
