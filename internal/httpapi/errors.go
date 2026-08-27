package httpapi

import (
	"encoding/json"
	"net/http"
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
}

// errorBody is the envelope every gateway-generated error is written as.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
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
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(ErrorEnvelope(code, message)) //nolint:gosec // G705: a JSON-encoded envelope of gateway-chosen strings, never request input
}
