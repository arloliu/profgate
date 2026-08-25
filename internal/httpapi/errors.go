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

// writeError writes the gateway's JSON error envelope.
// It sets only gateway-owned headers and never a target header:
// target headers belong to forwarded upstream responses alone.
func writeError(w http.ResponseWriter, status int, code, message string) {
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	// json.Marshal of a struct of two strings cannot fail,
	// so there is no error path to handle.
	body, _ := json.Marshal(errorBody{Error: message, Code: code})
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
