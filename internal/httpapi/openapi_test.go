package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/auth"
	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

// documentPath is the file the route serves and this check reads.
const documentPath = "openapi.json"

// operationKeys are the keys of a path item that declare an operation.
// A path item also carries parameters, summaries, and descriptions,
// which name no method, so the check reads these keys and no others.
var operationKeys = []string{"get", "put", "post", "delete", "head", "patch", "options", "trace"}

// detailVocabularies is every details vocabulary of the error envelope, by the code that carries one.
// The invalid_parameter and port_not_allowed values are this package's own constants.
// The limit_exceeded values are the violation vocabulary internal/pgo writes,
// which has no constant this package can name; adding a value to either is a spec change.
var detailVocabularies = map[string][]string{
	CodeInvalidParameter: {
		detailUnknownParameter,
		detailRepeatedParameter,
		detailEmptyParameter,
		detailMalformedParameter,
		detailParameterNotApplicable,
		detailMutuallyExclusive,
		detailHeaderRequired,
		detailHeaderMalformed,
		detailUnknownField,
		detailFieldNotApplicable,
		detailBodyNotAllowed,
		detailBodyMalformed,
	},
	CodeLimitExceeded: {
		"above_maximum",
		"below_minimum",
		"out_of_range",
		"not_permitted",
		"retention_under_interval",
	},
	CodePortNotAllowed: {detailNotAdmitted},
}

// queryParameters is every query parameter a route's handler accepts, by operation.
// A route the map does not name takes none, which the check holds it to,
// so a parameter added to a handler without being described fails here.
var queryParameters = map[string][]string{
	"GET /v1/namespaces/{namespace}/services/{service}/targets": {
		"explain", "pod", "port", "portName", "version",
	},
	"GET /v1/namespaces/{namespace}/services/{service}/profiles/{profile}": {
		"pod", "port", "portName", "seconds", "strategy", "version",
	},
	"GET /v1/namespaces/{namespace}/services/{service}/collections": {
		cursorParam, limitParam, originParam, sinceParam, stateParam,
	},
	"GET /v1/collections/{id}": {waitParam},
	// The three browser-login routes read their own query,
	// which internal/auth parses rather than the parameter step.
	"GET /auth/login":    {"return"},
	"GET /auth/callback": {"code", "error", "state"},
}

// readDocument is the file as it is shipped, and the parsed form of it.
func readDocument(t *testing.T) ([]byte, map[string]any) {
	t.Helper()

	raw, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatalf("read %s: %v", documentPath, err)
	}
	if !bytes.Equal(raw, openAPIDocument) {
		t.Fatalf("%s and the embedded bytes differ", documentPath)
	}

	return raw, parseDocument(t, raw)
}

// parseDocument decodes the document into the maps the comparisons walk.
func parseDocument(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not JSON: %v", documentPath, err)
	}

	return doc
}

// copyDocument is a deep copy, so a fixture mutates its own document and not the shipped one.
func copyDocument(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode the document: %v", err)
	}

	return parseDocument(t, raw)
}

// object reads a nested object, and is nil at the first key that is absent or holds something else.
func object(v any, keys ...string) map[string]any {
	for _, key := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	m, _ := v.(map[string]any)

	return m
}

// stringList reads an array of strings, and is nil for anything else.
func stringList(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}

	return out
}

// paths is the document's path items, by template.
func paths(doc map[string]any) map[string]any {
	return object(doc, "paths")
}

// operations is one path item's operations, by upper-case method.
func operations(item any) map[string]any {
	out := map[string]any{}
	for _, key := range operationKeys {
		if op := object(item, key); op != nil {
			out[strings.ToUpper(key)] = op
		}
	}

	return out
}

// documentPairs is every path-and-method pair the document declares.
func documentPairs(doc map[string]any) []string {
	var pairs []string
	for template, item := range paths(doc) {
		for method := range operations(item) {
			pairs = append(pairs, method+" "+template)
		}
	}
	slices.Sort(pairs)

	return pairs
}

// tablePairs is every path-and-method pair the route table declares.
func tablePairs() []string {
	var pairs []string
	for _, d := range declarations() {
		for _, method := range d.Methods {
			pairs = append(pairs, method+" "+d.Template)
		}
	}
	slices.Sort(pairs)

	return pairs
}

// difference is what is in want and not in got.
func difference(want, got []string) []string {
	var out []string
	for _, v := range want {
		if !slices.Contains(got, v) {
			out = append(out, v)
		}
	}

	return out
}

// compareRoutes is the first comparison:
// every pair of the route table appears in the document,
// and the document declares no pair the table does not hold.
func compareRoutes(doc map[string]any) error {
	table, document := tablePairs(), documentPairs(doc)
	missing := difference(table, document)
	extra := difference(document, table)
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}

	return fmt.Errorf("routes: the document is missing %v and declares %v, which the route table does not", missing, extra)
}

// documentCodes is the envelope codes the document enumerates.
func documentCodes(doc map[string]any) []string {
	return stringList(object(doc, "components", "schemas", "Error", "properties", "code")["enum"])
}

// compareCodes is the second comparison:
// the registry and the codes the document enumerates are the same set.
func compareCodes(doc map[string]any) error {
	registry, document := EnvelopeCodes(), documentCodes(doc)
	missing := difference(registry, document)
	extra := difference(document, registry)
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}

	return fmt.Errorf("codes: the document is missing %v and enumerates %v, which the registry does not hold", missing, extra)
}

// enumerations is every enum the document holds, wherever it sits.
func enumerations(v any) [][]string {
	var out [][]string
	switch value := v.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(value)) {
			if key == "enum" {
				if values := stringList(value[key]); values != nil {
					out = append(out, values)
				}

				continue
			}
			out = append(out, enumerations(value[key])...)
		}
	case []any:
		for _, item := range value {
			out = append(out, enumerations(item)...)
		}
	}

	return out
}

// compareVocabularies is the third comparison:
// every details vocabulary appears as an enumeration in the document.
func compareVocabularies(doc map[string]any) error {
	found := enumerations(doc)
	for _, code := range slices.Sorted(maps.Keys(detailVocabularies)) {
		want := slices.Sorted(slices.Values(detailVocabularies[code]))
		if !slices.ContainsFunc(found, func(got []string) bool {
			return slices.Equal(slices.Sorted(slices.Values(got)), want)
		}) {
			return fmt.Errorf("vocabularies: no enumeration of the document is the %s vocabulary %v", code, want)
		}
	}

	return nil
}

// compareEncoding is the fourth comparison:
// re-encoding the parsed document equals the file byte for byte,
// so a hand edit cannot leave the file formatted one way and read another.
func compareEncoding(raw []byte) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("encoding: the document is not JSON: %w", err)
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding: the document does not re-encode: %w", err)
	}
	encoded = append(encoded, '\n')
	if !bytes.Equal(raw, encoded) {
		return fmt.Errorf("encoding: the file is %d bytes and its re-encoding is %d; "+
			"object keys are in name order, indented by two spaces, with a trailing newline",
			len(raw), len(encoded))
	}

	return nil
}

// references is every reference the document holds, wherever it sits.
func references(v any) []string {
	var out []string
	switch value := v.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(value)) {
			if key == "$ref" {
				if ref, ok := value[key].(string); ok {
					out = append(out, ref)
				}

				continue
			}
			out = append(out, references(value[key])...)
		}
	case []any:
		for _, item := range value {
			out = append(out, references(item)...)
		}
	}

	return out
}

// resolves reports whether a reference names an object this document holds.
// The document is served alone,
// so a pointer into another file names nothing a client can follow and does not resolve here either.
func resolves(doc map[string]any, ref string) bool {
	pointer, found := strings.CutPrefix(ref, "#/")
	if !found {
		return false
	}
	keys := strings.Split(pointer, "/")
	for i, key := range keys {
		keys[i] = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
	}

	return object(doc, keys...) != nil
}

// compareReferences is the fifth comparison:
// every reference of the document resolves within it.
// It walks the whole document rather than the component kinds the other comparisons dereference,
// so a component pointed at by a name it does not carry fails here.
func compareReferences(doc map[string]any) error {
	var dangling []string
	for _, ref := range references(doc) {
		if !resolves(doc, ref) && !slices.Contains(dangling, ref) {
			dangling = append(dangling, ref)
		}
	}
	if len(dangling) == 0 {
		return nil
	}

	return fmt.Errorf("references: the document points at %v, which it does not hold", dangling)
}

func TestOpenAPIDocumentRoutes(t *testing.T) {
	_, doc := readDocument(t)
	if err := compareRoutes(doc); err != nil {
		t.Error(err)
	}
}

func TestOpenAPIDocumentCodes(t *testing.T) {
	_, doc := readDocument(t)
	if err := compareCodes(doc); err != nil {
		t.Error(err)
	}
}

func TestOpenAPIDocumentVocabularies(t *testing.T) {
	_, doc := readDocument(t)
	if err := compareVocabularies(doc); err != nil {
		t.Error(err)
	}
}

func TestOpenAPIDocumentEncoding(t *testing.T) {
	raw, _ := readDocument(t)
	if err := compareEncoding(raw); err != nil {
		t.Error(err)
	}
}

func TestOpenAPIDocumentReferences(t *testing.T) {
	_, doc := readDocument(t)
	if err := compareReferences(doc); err != nil {
		t.Error(err)
	}
}

// TestOpenAPICheckFailsOnDrift drives the comparisons over documents
// that differ from the code in exactly one way.
// A check that passed on all of these would hold the shipped document to nothing.
func TestOpenAPICheckFailsOnDrift(t *testing.T) {
	raw, shipped := readDocument(t)
	cases := []struct {
		name   string
		mutate func(t *testing.T, doc map[string]any) error
	}{
		{"a missing route", func(t *testing.T, doc map[string]any) error {
			t.Helper()
			delete(paths(doc), "/v1/whoami")

			return compareRoutes(doc)
		}},
		{"an extra route", func(t *testing.T, doc map[string]any) error {
			t.Helper()
			paths(doc)["/v1/invented"] = map[string]any{"get": map[string]any{}}

			return compareRoutes(doc)
		}},
		{"a missing method", func(t *testing.T, doc map[string]any) error {
			t.Helper()
			delete(object(paths(doc), "/v1/namespaces/{namespace}/services/{service}/pgo"), "delete")

			return compareRoutes(doc)
		}},
		{"a missing code", func(t *testing.T, doc map[string]any) error {
			t.Helper()
			codes := object(doc, "components", "schemas", "Error", "properties", "code")
			codes["enum"] = codes["enum"].([]any)[1:]

			return compareCodes(doc)
		}},
		{"an extra code", func(t *testing.T, doc map[string]any) error {
			t.Helper()
			codes := object(doc, "components", "schemas", "Error", "properties", "code")
			codes["enum"] = append(codes["enum"].([]any), "invented_code")

			return compareCodes(doc)
		}},
		{"a renamed component", func(t *testing.T, doc map[string]any) error {
			t.Helper()
			headers := object(doc, "components", "headers")
			headers["Renamed"] = headers["RequestId"]
			delete(headers, "RequestId")

			return compareReferences(doc)
		}},
		{"a missing vocabulary value", func(t *testing.T, doc map[string]any) error {
			t.Helper()
			vocabulary := object(doc, "components", "schemas", "InvalidParameterDetailCode")
			vocabulary["enum"] = vocabulary["enum"].([]any)[1:]

			return compareVocabularies(doc)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.mutate(t, copyDocument(t, shipped)); err == nil {
				t.Error("the check passed over a document that differs from the code")
			}
		})
	}

	t.Run("a reindented file", func(t *testing.T) {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		reindented, err := json.MarshalIndent(doc, "", "    ")
		if err != nil {
			t.Fatal(err)
		}
		if err := compareEncoding(append(reindented, '\n')); err == nil {
			t.Error("the check passed over a file indented by hand")
		}
	})
}

// resolveResponse follows a response reference into components,
// and is the response itself when it carries none.
func resolveResponse(t *testing.T, doc map[string]any, response any) map[string]any {
	t.Helper()

	m, ok := response.(map[string]any)
	if !ok {
		t.Fatalf("a response is not an object: %v", response)
	}
	ref, ok := m["$ref"].(string)
	if !ok {
		return m
	}
	name, found := strings.CutPrefix(ref, "#/components/responses/")
	if !found {
		t.Fatalf("response reference %q does not name a component response", ref)
	}
	target := object(doc, "components", "responses", name)
	if target == nil {
		t.Fatalf("response reference %q resolves to nothing", ref)
	}

	return target
}

// resolveParameter follows a parameter reference into components,
// and is the parameter itself when it carries none.
func resolveParameter(t *testing.T, doc map[string]any, parameter any) map[string]any {
	t.Helper()

	m, ok := parameter.(map[string]any)
	if !ok {
		t.Fatalf("a parameter is not an object: %v", parameter)
	}
	ref, ok := m["$ref"].(string)
	if !ok {
		return m
	}
	name, found := strings.CutPrefix(ref, "#/components/parameters/")
	if !found {
		t.Fatalf("parameter reference %q does not name a component parameter", ref)
	}
	target := object(doc, "components", "parameters", name)
	if target == nil {
		t.Fatalf("parameter reference %q resolves to nothing", ref)
	}

	return target
}

// parametersOf is one operation's parameters of the given location, by name.
func parametersOf(t *testing.T, doc map[string]any, op map[string]any, in string) []string {
	t.Helper()

	items, _ := op["parameters"].([]any)
	var names []string
	for _, item := range items {
		p := resolveParameter(t, doc, item)
		if p["in"] == in {
			name, _ := p["name"].(string)
			names = append(names, name)
		}
	}
	slices.Sort(names)

	return names
}

// eachOperation runs fn over every operation of the document, named as the comparisons name it.
func eachOperation(doc map[string]any, fn func(pair string, op map[string]any)) {
	for _, template := range slices.Sorted(maps.Keys(paths(doc))) {
		ops := operations(paths(doc)[template])
		for _, method := range slices.Sorted(maps.Keys(ops)) {
			fn(method+" "+template, ops[method].(map[string]any))
		}
	}
}

// TestOpenAPIDocumentParameters holds the document to the parameters a client would send:
// every query parameter a handler accepts is described on that operation,
// every path parameter the template names is described,
// and an operation whose handler takes no query parameter describes none.
func TestOpenAPIDocumentParameters(t *testing.T) {
	_, doc := readDocument(t)
	eachOperation(doc, func(pair string, op map[string]any) {
		want := slices.Sorted(slices.Values(queryParameters[pair]))
		if got := parametersOf(t, doc, op, "query"); !slices.Equal(got, want) {
			t.Errorf("%s describes query parameters %v, want %v", pair, got, want)
		}
		template := pair[strings.Index(pair, " ")+1:]
		var captured []string
		for _, part := range strings.Split(template, "/") {
			if name, ok := strings.CutPrefix(part, "{"); ok {
				captured = append(captured, strings.TrimSuffix(name, "}"))
			}
		}
		slices.Sort(captured)
		if got := parametersOf(t, doc, op, "path"); !slices.Equal(got, captured) {
			t.Errorf("%s describes path parameters %v, want %v", pair, got, captured)
		}
	})
}

// TestOpenAPIDocumentWriteRoutesRequireJSON reads the media type a write declares:
// the two POST routes the media-type step holds, and the policy write,
// take a JSON request body, and no other operation describes one.
func TestOpenAPIDocumentWriteRoutesRequireJSON(t *testing.T) {
	writes := []string{
		"POST /v1/namespaces/{namespace}/services/{service}/collections",
		"POST /v1/collections/{id}/cancel",
		"PUT /v1/namespaces/{namespace}/services/{service}/pgo",
	}
	_, doc := readDocument(t)
	eachOperation(doc, func(pair string, op map[string]any) {
		content := object(op, "requestBody", "content")
		if !slices.Contains(writes, pair) {
			if content != nil {
				t.Errorf("%s describes a request body, which its handler accepts none of", pair)
			}

			return
		}
		if got := slices.Sorted(maps.Keys(content)); !slices.Equal(got, []string{jsonMediaType}) {
			t.Errorf("%s takes request media types %v, want [%s]", pair, got, jsonMediaType)
		}
	})
}

// TestOpenAPIDocumentHeaders reads the two headers a client acts on:
// X-Request-Id names the request on every answer,
// and Idempotency-Key is what a create is retried with.
func TestOpenAPIDocumentHeaders(t *testing.T) {
	const create = "POST /v1/namespaces/{namespace}/services/{service}/collections"

	_, doc := readDocument(t)
	eachOperation(doc, func(pair string, op map[string]any) {
		responses := object(op, "responses")
		if len(responses) == 0 {
			t.Errorf("%s describes no response", pair)

			return
		}
		for _, status := range slices.Sorted(maps.Keys(responses)) {
			resolved := resolveResponse(t, doc, responses[status])
			if object(resolved, "headers", requestIDHeader) == nil {
				t.Errorf("%s answers %s without describing %s", pair, status, requestIDHeader)
			}
		}
		headers := parametersOf(t, doc, op, "header")
		if pair == create {
			if !slices.Contains(headers, idempotencyKeyHeader) {
				t.Errorf("%s describes headers %v, which do not include %s", pair, headers, idempotencyKeyHeader)
			}
		} else if slices.Contains(headers, idempotencyKeyHeader) {
			t.Errorf("%s describes %s, which only the create takes", pair, idempotencyKeyHeader)
		}
	})
}

// TestOpenAPIDocumentCreateReplay reads the replay:
// a create answers 202 for a Collection it accepted and 200 for one it had accepted before,
// with the same body, and 409 for a key that stands for something else.
func TestOpenAPIDocumentCreateReplay(t *testing.T) {
	_, doc := readDocument(t)
	op := object(paths(doc), "/v1/namespaces/{namespace}/services/{service}/collections", "post")
	responses := object(op, "responses")
	schemas := map[string]any{}
	for _, status := range []string{"200", "202"} {
		response, ok := responses[status]
		if !ok {
			t.Fatalf("the create describes no %s response", status)
		}
		resolved := resolveResponse(t, doc, response)
		schemas[status] = object(resolved, "content", jsonMediaType)["schema"]
		if schemas[status] == nil {
			t.Fatalf("the create's %s response describes no %s body", status, jsonMediaType)
		}
	}
	if fmt.Sprint(schemas["200"]) != fmt.Sprint(schemas["202"]) {
		t.Errorf("the create answers 202 with %v and 200 with %v, which are different bodies",
			schemas["202"], schemas["200"])
	}
	if _, ok := responses["409"]; !ok {
		t.Error("the create describes no 409 response, which is the key that stands for something else")
	}
}

// TestOpenAPIRoute drives the route the document is served on.
func TestOpenAPIRoute(t *testing.T) {
	const path = "/v1/openapi.json"

	t.Run("serves the embedded bytes", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), openAPIDocument) {
			t.Error("the body is not the embedded document")
		}
		if got := rec.Header().Get("Content-Type"); got != jsonMediaType {
			t.Errorf("Content-Type = %q, want %q", got, jsonMediaType)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		h.expectMetric(t, metrics.EndpointOpenAPI, labelNone)
		h.expectMetricCode(t, codeOK)
		h.expectAudit(t, http.StatusOK, codeOK)
	})

	t.Run("not ready", func(t *testing.T) {
		h := newHarness(baseTarget())
		h.ready = func() bool { return false }
		rec := h.do(t, http.MethodGet, path)
		h.expectError(t, rec, http.StatusServiceUnavailable, CodeNotReady)
		h.expectMetric(t, metrics.EndpointOpenAPI, labelNone)
	})

	t.Run("refuses every query parameter", func(t *testing.T) {
		for _, query := range []string{"?access_token=secret", "?pretty=true"} {
			t.Run(query, func(t *testing.T) {
				h := newHarness(baseTarget())
				rec := h.do(t, http.MethodGet, path+query)
				h.expectError(t, rec, http.StatusBadRequest, CodeInvalidParameter)
				items := detailsOf(t, rec, CodeInvalidParameter)
				if len(items) != 1 || items[0].Code != detailUnknownParameter {
					t.Errorf("details = %+v, want one %s item", items, detailUnknownParameter)
				}
				if strings.Contains(rec.Body.String(), "secret") {
					t.Errorf("the refusal echoes the value the client sent: %q", rec.Body.String())
				}
				h.expectMetric(t, metrics.EndpointOpenAPI, labelNone)
			})
		}
	})

	t.Run("refuses every other method", func(t *testing.T) {
		h := newHarness(baseTarget())
		rec := h.do(t, http.MethodPost, path)
		h.expectError(t, rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Errorf("Allow = %q, want %q", got, http.MethodGet)
		}
		h.expectMetric(t, metrics.EndpointOpenAPI, labelNone)
	})

	// The route publishes the grammar 404 route_unknown and the Allow header of a 405 already publish,
	// so no credential is read and no realm is evaluated.
	for _, mode := range []string{config.ModeBasic, config.ModeOIDC} {
		t.Run("answers without a credential under "+mode, func(t *testing.T) {
			h := newHarness(baseTarget())
			h.configure(func(cfg *config.Config) {
				cfg.Auth.Mode = mode
				cfg.Auth.AnonymousRealm = ""
			})
			a := refuse(&auth.Failure{Status: http.StatusUnauthorized, Reason: auth.ReasonMissing})
			h.auth = a
			rec := h.do(t, http.MethodGet, path)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if a.called() != 0 {
				t.Errorf("authenticator calls = %d, want none: the route runs no authentication step", a.called())
			}
		})
	}
}

// TestOpenAPIDocumentDoesNotVaryWithConfiguration reads what the document carries under every configuration.
// The console, the browser-login, and the PGO routes are in it,
// and the ops listener's three paths are not.
func TestOpenAPIDocumentDoesNotVaryWithConfiguration(t *testing.T) {
	_, doc := readDocument(t)
	for _, template := range []string{
		"/ui/",
		"/auth/login",
		"/v1/namespaces/{namespace}/services/{service}/collections",
	} {
		if paths(doc)[template] == nil {
			t.Errorf("the document lacks %s, which it describes whatever the configuration enables", template)
		}
	}
	for _, template := range []string{"/healthz", "/readyz", "/metrics"} {
		if paths(doc)[template] != nil {
			t.Errorf("the document describes %s, which the ops listener serves", template)
		}
	}
}
