package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// recordingTransport answers every request with one body and records what it saw:
// the method, the path, the query, and how many requests arrived.
type recordingTransport struct {
	body     string
	status   int
	requests []*http.Request
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.requests = append(rt.requests, r)
	return jsonResponse(rt.status, rt.body), nil
}

// runRead drives one client verb against a gateway answering body and returns
// the exit code with the transport that saw the requests.
func runRead(t *testing.T, te *testEnv, body string, args ...string) (int, *recordingTransport) {
	t.Helper()
	rt := &recordingTransport{body: body, status: http.StatusOK}
	te.env.transport = rt
	args = append(args, "--server", "https://g.example")
	code := dispatch(context.Background(), te.env, clientVerbs(), args)
	return code, rt
}

const whoamiBody = `{"principal":"alice","realm":{"name":"payments-dev","namespaces":["payments"],"services":["*"],"profiles":["cpu","heap","goroutine"],"pgo":{"read":true,"collect":false,"configure":false}},"auth":{"mode":"oidc"}}` + "\n"

const limitsBody = `{"cpuSeconds":60,"traceSeconds":60,"profiles":["cpu","trace","heap"],"pprof":{"default":{"port":6060},"allowedSelections":[{"port":6061},{"portName":"pprof-alt"}]},"pgo":{"enabled":true}}` + "\n"

// readVerbs is every read verb with its arguments, the route it names, and
// a body the gateway answers with.
var readVerbs = []struct {
	name string
	args []string
	path string
	body string
}{
	{name: "whoami", args: []string{"whoami"}, path: "/v1/whoami", body: whoamiBody},
	{name: "limits", args: []string{"limits"}, path: "/v1/limits", body: limitsBody},
	{name: "namespaces", args: []string{"namespaces"}, path: "/v1/namespaces", body: `{"namespaces":["orders","payments"]}` + "\n"},
	{name: "services", args: []string{"services", "payments"}, path: "/v1/namespaces/payments/services", body: `{"namespace":"payments","services":["checkout","ledger"]}` + "\n"},
	{name: "targets", args: []string{"targets", "payments/checkout"}, path: "/v1/namespaces/payments/services/checkout/targets", body: `{"namespace":"payments","service":"checkout","targets":[{"pod":"checkout-1","node":"worker-07","version":"1.42.3"}]}` + "\n"},
}

func TestReadVerbsOneGET(t *testing.T) {
	for _, tc := range readVerbs {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			code, rt := runRead(t, te, tc.body, tc.args...)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if len(rt.requests) != 1 {
				t.Fatalf("%d requests, want exactly one", len(rt.requests))
			}
			r := rt.requests[0]
			if r.Method != http.MethodGet || r.URL.Path != tc.path {
				t.Fatalf("request = %s %s, want GET %s", r.Method, r.URL.Path, tc.path)
			}
			if r.URL.RawQuery != "" {
				t.Fatalf("query = %q, want none", r.URL.RawQuery)
			}
		})
	}
}

func TestReadVerbsJSON(t *testing.T) {
	for _, tc := range readVerbs {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			args := append([]string{}, tc.args...)
			args = append(args, "--output", "json")
			code, _ := runRead(t, te, tc.body, args...)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if te.stdout.String() != tc.body {
				t.Fatalf("stdout = %q, want the body byte for byte %q", te.stdout.String(), tc.body)
			}
		})
	}
}

func TestReadVerbsTable(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		body     string
		terminal bool
		want     string
	}{
		{
			name: "whoami prints the six rows in order",
			args: []string{"whoami"},
			body: whoamiBody,
			want: "principal\talice\nrealm\tpayments-dev\nnamespaces\tpayments\nservices\t*\nprofiles\tcpu, heap, goroutine\npgo\tread\n",
		},
		{
			name:     "whoami on a terminal pads",
			args:     []string{"whoami"},
			body:     whoamiBody,
			terminal: true,
			want:     "principal   alice\nrealm       payments-dev\nnamespaces  payments\nservices    *\nprofiles    cpu, heap, goroutine\npgo         read\n",
		},
		{
			name: "limits prints one row per limit with the selections listed",
			args: []string{"limits"},
			body: limitsBody,
			want: "cpuSeconds\t60\ntraceSeconds\t60\nprofiles\tcpu, trace, heap\ndefault\tport 6060\nallowedSelections\tport 6061, portName pprof-alt\npgo\tenabled\n",
		},
		{
			name: "namespaces is one column",
			args: []string{"namespaces"},
			body: `{"namespaces":["orders","payments"]}`,
			want: "NAMESPACE\norders\npayments\n",
		},
		{
			name: "services is one column",
			args: []string{"services", "payments"},
			body: `{"namespace":"payments","services":["checkout","ledger"]}`,
			want: "SERVICE\ncheckout\nledger\n",
		},
		{
			name: "targets is pod, node, version",
			args: []string{"targets", "payments/checkout"},
			body: `{"namespace":"payments","service":"checkout","targets":[{"pod":"checkout-1","node":"worker-07","version":"1.42.3"},{"pod":"checkout-2","node":"worker-03","version":""}]}`,
			want: "POD\tNODE\tVERSION\ncheckout-1\tworker-07\t1.42.3\ncheckout-2\tworker-03\t\n",
		},
		{
			name:     "targets on a terminal pads",
			args:     []string{"targets", "payments/checkout"},
			body:     `{"namespace":"payments","service":"checkout","targets":[{"pod":"checkout-1","node":"worker-07","version":"1.42.3"}]}`,
			terminal: true,
			want:     "POD         NODE       VERSION\ncheckout-1  worker-07  1.42.3\n",
		},
		{
			name: "an empty list prints its header alone",
			args: []string{"targets", "payments/checkout"},
			body: `{"namespace":"payments","service":"checkout","targets":[]}`,
			want: "POD\tNODE\tVERSION\n",
		},
		{
			name: "an empty namespace list prints its header alone",
			args: []string{"namespaces"},
			body: `{"namespaces":[]}`,
			want: "NAMESPACE\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			te.env.terminal = tc.terminal
			code, _ := runRead(t, te, tc.body, tc.args...)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if te.stdout.String() != tc.want {
				t.Fatalf("stdout = %q, want %q", te.stdout.String(), tc.want)
			}
		})
	}
}

func TestReadVerbNotTheShape(t *testing.T) {
	te := newTestEnv(t)
	code, _ := runRead(t, te, `{"namespaces":"orders"}`, "namespaces")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
	}
}

func TestVerboseLine(t *testing.T) {
	te := newTestEnv(t)
	te.vars["PROFGATE_TOKEN"] = "secret-token-value"
	code, _ := runRead(t, te, whoamiBody, "whoami", "--verbose")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(te.stderr.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr = %q, want one line", te.stderr.String())
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 4 || fields[0] != "GET" || fields[1] != "https://g.example/v1/whoami" || fields[2] != "200" {
		t.Fatalf("line = %q, want method, the full URL, the status, and the duration", lines[0])
	}
	if strings.Contains(te.stderr.String(), "secret-token-value") || strings.Contains(te.stderr.String(), "Authorization") {
		t.Fatalf("stderr = %q, holds a header value", te.stderr.String())
	}
}

func TestTargetsPortFlags(t *testing.T) {
	t.Run("both is a usage error before any request", func(t *testing.T) {
		te := newTestEnv(t)
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"targets", "payments/checkout", "--port", "6060", "--port-name", "pprof", "--server", "https://g.example"})
		if code != 2 {
			t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
		}
		if !strings.Contains(te.stderr.String(), "--port") || !strings.Contains(te.stderr.String(), "--port-name") {
			t.Fatalf("stderr = %q, want both flags named", te.stderr.String())
		}
	})

	tests := []struct {
		name  string
		flags []string
		query string
	}{
		{name: "port", flags: []string{"--port", "6060"}, query: "port=6060"},
		{name: "port name", flags: []string{"--port-name", "pprof"}, query: "portName=pprof"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			args := append([]string{"targets", "payments/checkout"}, tc.flags...)
			code, rt := runRead(t, te, `{"namespace":"payments","service":"checkout","targets":[]}`, args...)
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if len(rt.requests) != 1 || rt.requests[0].URL.RawQuery != tc.query {
				t.Fatalf("requests = %d, query = %q, want exactly one with %q", len(rt.requests), rt.requests[0].URL.RawQuery, tc.query)
			}
		})
	}
}

func TestTargetsBareServiceUsesTheNamespace(t *testing.T) {
	te := newTestEnv(t)
	code, rt := runRead(t, te, `{"namespace":"payments","service":"checkout","targets":[]}`, "targets", "checkout", "--namespace", "payments")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if rt.requests[0].URL.Path != "/v1/namespaces/payments/services/checkout/targets" {
		t.Fatalf("path = %q", rt.requests[0].URL.Path)
	}
}

// routedTransport answers 200 with the body its map holds for the path,
// 404 route_unknown for any other path, and records the requests.
type routedTransport struct {
	routes   map[string]string
	requests []*http.Request
}

func (rt *routedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.requests = append(rt.requests, r)
	if body, ok := rt.routes[r.URL.Path]; ok {
		return jsonResponse(http.StatusOK, body), nil
	}
	return jsonResponse(http.StatusNotFound, `{"error":"no such route","code":"route_unknown"}`), nil
}

func TestLoginVerb(t *testing.T) {
	t.Run("the timeout range is checked before any request", func(t *testing.T) {
		te := newTestEnv(t)
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"login", "--server", "https://g.example", "--login-timeout", "45m"})
		if code != 2 {
			t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
		}
		if !strings.Contains(te.stderr.String(), "1m0s to 30m0s") {
			t.Fatalf("stderr = %q, want the range named", te.stderr.String())
		}
	})

	t.Run("under disabled it prints that nobody authenticates", func(t *testing.T) {
		te := newTestEnv(t)
		rt := &routedTransport{routes: map[string]string{"/v1/auth": `{"mode":"disabled"}`}}
		te.env.transport = rt
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"login", "--server", "https://g.example"})
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if !strings.Contains(te.stdout.String(), "authenticates nobody") {
			t.Fatalf("stdout = %q", te.stdout.String())
		}
	})

	t.Run("under basic it prompts through the seam and prints what whoami returned", func(t *testing.T) {
		te := newTestEnv(t)
		var prompted string
		te.env.prompt = func(user string) (string, error) {
			prompted = user
			return "s3cret", nil
		}
		rt := &routedTransport{routes: map[string]string{
			"/v1/auth":   `{"mode":"basic"}`,
			"/v1/whoami": whoamiBody,
		}}
		te.env.transport = rt
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"login", "--server", "https://g.example", "-u", "alice"})
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if prompted != "alice" {
			t.Fatalf("prompted for %q, want alice", prompted)
		}
		if !strings.Contains(te.stdout.String(), "principal: alice") || !strings.Contains(te.stdout.String(), "realm: payments-dev") {
			t.Fatalf("stdout = %q, want the principal and realm", te.stdout.String())
		}
		if strings.Contains(te.stdout.String()+te.stderr.String(), "s3cret") {
			t.Fatal("the password reached an output stream")
		}
	})

	t.Run("the flags reach the login", func(t *testing.T) {
		te := newTestEnv(t)
		te.env.transport = &routedTransport{routes: map[string]string{}}
		var discovered string
		te.env.issuerTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			discovered = r.URL.String()
			return nil, errors.New("refused")
		})
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"login", "--server", "https://g.example", "--issuer", "https://issuer.test", "--client-id", "profgate-cli"})
		if code != 1 {
			t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
		}
		if discovered != "https://issuer.test/.well-known/openid-configuration" {
			t.Fatalf("discovery reached %q, want the --issuer flag's issuer", discovered)
		}
	})

	t.Run("pkce and no-pkce contradict", func(t *testing.T) {
		te := newTestEnv(t)
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"login", "--server", "https://g.example", "--pkce", "--no-pkce"})
		if code != 2 {
			t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
		}
	})
}

func TestLogoutVerb(t *testing.T) {
	te := newTestEnv(t)
	code := dispatch(context.Background(), te.env, clientVerbs(), []string{"logout", "--server", "https://g.example"})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	if !strings.Contains(te.stdout.String(), "nothing is cached for https://g.example:443") {
		t.Fatalf("stdout = %q, want the notice that nothing was cached", te.stdout.String())
	}
}

const explainBody = `{"namespace":"payments","service":"checkout","targets":[{"pod":"checkout-1","node":"worker-07","version":"1.42.3"}],"selectorMatched":4,"excluded":[{"reason":"pod_not_ready","count":2},{"reason":"port_name_not_declared","count":1}]}` + "\n"

func TestTargetsExplain(t *testing.T) {
	t.Run("without the flag no explain parameter is sent and one table prints", func(t *testing.T) {
		te := newTestEnv(t)
		code, rt := runRead(t, te, explainBody, "targets", "payments/checkout")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if len(rt.requests) != 1 || rt.requests[0].URL.Query().Has("explain") {
			t.Fatalf("requests = %d, query = %q, want one without explain", len(rt.requests), rt.requests[0].URL.RawQuery)
		}
		if want := "POD\tNODE\tVERSION\ncheckout-1\tworker-07\t1.42.3\n"; te.stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
		}
	})

	t.Run("the flag sends explain=true once", func(t *testing.T) {
		te := newTestEnv(t)
		code, rt := runRead(t, te, explainBody, "targets", "payments/checkout", "--explain")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if len(rt.requests) != 1 || rt.requests[0].URL.RawQuery != "explain=true" {
			t.Fatalf("requests = %d, query = %q, want exactly one with explain=true", len(rt.requests), rt.requests[0].URL.RawQuery)
		}
	})

	tables := []struct {
		name     string
		body     string
		terminal bool
		want     string
	}{
		{
			name: "counted reasons print in the order the body listed them",
			body: explainBody,
			want: "POD\tNODE\tVERSION\ncheckout-1\tworker-07\t1.42.3\n\nselectorMatched\t4\n\nREASON\tCOUNT\npod_not_ready\t2\nport_name_not_declared\t1\n",
		},
		{
			name:     "on a terminal both tables pad",
			body:     explainBody,
			terminal: true,
			want:     "POD         NODE       VERSION\ncheckout-1  worker-07  1.42.3\n\nselectorMatched  4\n\nREASON                  COUNT\npod_not_ready           2\nport_name_not_declared  1\n",
		},
		{
			name: "excluded absent prints the second header alone",
			body: `{"namespace":"payments","service":"checkout","targets":[]}`,
			want: "POD\tNODE\tVERSION\n\nselectorMatched\t0\n\nREASON\tCOUNT\n",
		},
		{
			name: "excluded empty prints the second header alone",
			body: `{"namespace":"payments","service":"checkout","targets":[],"selectorMatched":0,"excluded":[]}`,
			want: "POD\tNODE\tVERSION\n\nselectorMatched\t0\n\nREASON\tCOUNT\n",
		},
		{
			name: "a reason outside the vocabulary prints as it arrived",
			body: `{"namespace":"payments","service":"checkout","targets":[],"selectorMatched":1,"excluded":[{"reason":"quarantined_by_operator","count":1}]}`,
			want: "POD\tNODE\tVERSION\n\nselectorMatched\t1\n\nREASON\tCOUNT\nquarantined_by_operator\t1\n",
		},
	}
	for _, tc := range tables {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			te.env.terminal = tc.terminal
			code, _ := runRead(t, te, tc.body, "targets", "payments/checkout", "--explain")
			if code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
			}
			if te.stdout.String() != tc.want {
				t.Fatalf("stdout = %q, want %q", te.stdout.String(), tc.want)
			}
		})
	}

	t.Run("json copies the body and prints no table", func(t *testing.T) {
		te := newTestEnv(t)
		code, _ := runRead(t, te, explainBody, "targets", "payments/checkout", "--explain", "--output", "json")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		if te.stdout.String() != explainBody {
			t.Fatalf("stdout = %q, want the body byte for byte", te.stdout.String())
		}
	})

	t.Run("beside --port both parameters are sent", func(t *testing.T) {
		te := newTestEnv(t)
		code, rt := runRead(t, te, explainBody, "targets", "payments/checkout", "--explain", "--port", "6061")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		q := rt.requests[0].URL.Query()
		if len(rt.requests) != 1 || q.Get("explain") != "true" || q.Get("port") != "6061" || len(q) != 2 {
			t.Fatalf("requests = %d, query = %q, want one with explain=true and port=6061", len(rt.requests), rt.requests[0].URL.RawQuery)
		}
	})

	t.Run("beside both port flags it is the usage error before any request", func(t *testing.T) {
		te := newTestEnv(t)
		rt := &recordingTransport{body: explainBody, status: http.StatusOK}
		te.env.transport = rt
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"targets", "payments/checkout", "--explain", "--port", "6060", "--port-name", "pprof", "--server", "https://g.example"})
		if code != 2 {
			t.Fatalf("code = %d, want 2 (stderr=%q)", code, te.stderr.String())
		}
		if len(rt.requests) != 0 {
			t.Fatalf("requests = %d, want none", len(rt.requests))
		}
	})

	t.Run("a 400 invalid_parameter is the envelope's message and exit 1 with no retry", func(t *testing.T) {
		te := newTestEnv(t)
		rt := &recordingTransport{body: `{"error":"explain is not a boolean","code":"invalid_parameter"}`, status: http.StatusBadRequest}
		te.env.transport = rt
		code := dispatch(context.Background(), te.env, clientVerbs(), []string{"targets", "payments/checkout", "--explain", "--server", "https://g.example"})
		if code != 1 {
			t.Fatalf("code = %d, want 1 (stderr=%q)", code, te.stderr.String())
		}
		if len(rt.requests) != 1 {
			t.Fatalf("requests = %d, want exactly one", len(rt.requests))
		}
		if !strings.Contains(te.stderr.String(), "invalid_parameter: explain is not a boolean") {
			t.Fatalf("stderr = %q, want the envelope's message", te.stderr.String())
		}
		if te.stdout.String() != "" {
			t.Fatalf("stdout = %q, want nothing", te.stdout.String())
		}
	})
}

// TestTargetsExplainNamesTheSelector records that the count of Pods the
// selector matched prints as its own row between the target list and the
// REASON table, including when it is zero:
// that is the number that tells "the selector selected no Pod"
// apart from "it selected Pods and every one was excluded", which two empty headers cannot.
func TestTargetsExplainNamesTheSelector(t *testing.T) {
	te := newTestEnv(t)
	code, _ := runRead(t, te, explainBody, "targets", "payments/checkout", "--explain")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
	}
	want := "POD\tNODE\tVERSION\ncheckout-1\tworker-07\t1.42.3\n\nselectorMatched\t4\n\nREASON\tCOUNT\npod_not_ready\t2\nport_name_not_declared\t1\n"
	if te.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
	}

	t.Run("a selector that matched nothing still prints its zero", func(t *testing.T) {
		te := newTestEnv(t)
		body := `{"namespace":"payments","service":"checkout","targets":[],"selectorMatched":0,"excluded":[]}`
		code, _ := runRead(t, te, body, "targets", "payments/checkout", "--explain")
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, te.stderr.String())
		}
		want := "POD\tNODE\tVERSION\n\nselectorMatched\t0\n\nREASON\tCOUNT\n"
		if te.stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", te.stdout.String(), want)
		}
	})
}

// TestServicesUnknownNamespace records that a namespace the gateway does not know
// is the empty list it answers and not a not-found this client invents:
// the gateway observes no Namespace objects,
// so an empty namespace and one that does not exist are one fact to it.
func TestServicesUnknownNamespace(t *testing.T) {
	te := newTestEnv(t)
	code, _ := runRead(t, te, `{"namespace":"nope","services":[]}`+"\n", "services", "nope")
	if code != exitOK {
		t.Fatalf("code = %d, want %d (stderr=%q)", code, exitOK, te.stderr.String())
	}
	if te.stdout.String() != "SERVICE\n" {
		t.Fatalf("stdout = %q, want the header alone", te.stdout.String())
	}
	if te.stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want it empty", te.stderr.String())
	}
}
