package httpapi

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
)

// requestError is a gateway-generated failure: the status to write, the stable code,
// and a human-readable message that names at most a namespace, a Service, or a Pod.
type requestError struct {
	status  int
	code    string
	message string
	// auditCode replaces code in the audit record and the metrics row.
	// It carries the outcomes that have a code of their own but no status of
	// their own — cas_contended, artifact_stream_failed, client_gone — so the
	// operator sees what happened while the client sees a status it can act on.
	auditCode string
	// details names the inputs the caller has to change.
	// A code with no vocabulary carries none, and the encoded body then has no
	// details key at all.
	details []errorDetail
}

// errorDetail is one input the caller has to change.
type errorDetail struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The details vocabulary of invalid_parameter, one value per refusal.
// A value names which kind of input the item's field is,
// so a client never has to tell a parameter name from a header name from a body pointer by its shape.
const (
	detailUnknownParameter       = "unknown_parameter"
	detailRepeatedParameter      = "repeated_parameter"
	detailEmptyParameter         = "empty_parameter"
	detailMalformedParameter     = "malformed_parameter"
	detailParameterNotApplicable = "parameter_not_applicable"
	detailMutuallyExclusive      = "mutually_exclusive"
	detailHeaderRequired         = "header_required"
	detailHeaderMalformed        = "header_malformed"
	detailUnknownField           = "unknown_field"
	detailFieldNotApplicable     = "field_not_applicable"
	detailBodyNotAllowed         = "body_not_allowed"
	detailBodyMalformed          = "body_malformed"
	// detailNotAdmitted is the one value of port_not_allowed.
	detailNotAdmitted = "not_admitted"
)

// invalidParameter is 400 invalid_parameter with the items the refusal earns,
// in the order the parameters were validated, which is name order.
// A caller that has no item to give passes none, and the body then carries no
// details key at all.
func invalidParameter(message string, items ...errorDetail) *requestError {
	return &requestError{
		status:  http.StatusBadRequest,
		code:    "invalid_parameter",
		message: message,
		details: items,
	}
}

// paramFault is one item about a query parameter, named as the client sent it.
func paramFault(code, name, message string) errorDetail {
	return errorDetail{Field: name, Code: code, Message: message}
}

// headerFault is one item about a request header, named as the route spells it.
func headerFault(code, name, message string) errorDetail {
	return errorDetail{Field: name, Code: code, Message: message}
}

// noParameters refuses a route that takes no query parameter,
// naming the first one the request carried in name order.
// A raw query string that does not parse leaves no name at fault,
// which is the one place malformed_parameter names none.
func noParameters(rawQuery string) *requestError {
	const message = "this route takes no query parameter"

	values, err := url.ParseQuery(rawQuery)
	if err != nil || len(values) == 0 {
		return invalidParameter(message,
			paramFault(detailMalformedParameter, "", "the query string does not parse"))
	}

	return invalidParameter(message,
		paramFault(detailUnknownParameter, slices.Min(slices.Collect(maps.Keys(values))),
			"this route takes no parameter of that name"))
}

// bodyFault is one item about the request body:
// a JSON pointer to the field at fault, or empty where no single field is.
func bodyFault(code, pointer, message string) errorDetail {
	return errorDetail{Field: pointer, Code: code, Message: message}
}

// errorBody is the envelope every gateway-generated error is written as.
// Details is omitted when nil: an error without details carries no key, never null or [].
type errorBody struct {
	Error   string        `json:"error"`
	Code    string        `json:"code"`
	Details []errorDetail `json:"details,omitempty"`
}

// ErrorEnvelope is the serialized JSON error envelope, newline-terminated;
// the console uses it to answer a HEAD with the Content-Length its GET would carry.
func ErrorEnvelope(code, message string) []byte {
	// json.Marshal of a struct of two strings cannot fail,
	// so there is no error path to handle.
	body, _ := json.Marshal(errorBody{Error: message, Code: code})

	return append(body, '\n')
}

// WriteError writes the gateway's JSON error envelope; the console calls it for its own 404 and 405.
// It sets only gateway-owned headers and never a target header:
// target headers belong to forwarded upstream responses alone.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	writeErrorBody(w, status, ErrorEnvelope(code, message))
}

// writeError writes a requestError as the envelope, with the details it carries.
func writeError(w http.ResponseWriter, e *requestError) {
	// json.Marshal of strings and a slice of string structs cannot fail.
	body, _ := json.Marshal(errorBody{Error: e.message, Code: e.code, Details: e.details})
	writeErrorBody(w, e.status, append(body, '\n'))
}

// writeErrorBody sets the gateway-owned headers and writes an encoded envelope.
func writeErrorBody(w http.ResponseWriter, status int, body []byte) {
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body) //nolint:gosec // G705: a JSON-encoded envelope; the one client value it can carry, a port selection, was bounded to a port number or an IANA name before it got here, and the encoder escapes it
}
