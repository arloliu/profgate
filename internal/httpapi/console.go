package httpapi

import (
	"net/http"
	"strings"
)

// uiPrefix is where the console's shell and assets live; every path under it and "/" itself go to Console.
const uiPrefix = "/ui/"

// isConsolePath reports whether path is one the console answers: "/" or anything under /ui/.
func isConsolePath(path string) bool {
	return path == "/" || strings.HasPrefix(path, uiPrefix)
}

// statusWriter captures the status the console wrote so the metrics row can be derived from it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write marks 200 when nothing was written yet, as net/http does for a body without a WriteHeader.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	return w.ResponseWriter.Write(b)
}

// consoleCode maps the status the console wrote to the closed set of ui codes:
// 2xx and 3xx are "ok", 404 is "route_unknown", 405 is "method_not_allowed",
// and every other status is "internal_error", so a console that fails is counted as a failure
// and never as a route miss. No status reaches the label as a number.
func consoleCode(status int) string {
	switch {
	case status >= 200 && status < 400:
		return codeOK
	case status == http.StatusNotFound:
		return "route_unknown"
	case status == http.StatusMethodNotAllowed:
		return "method_not_allowed"
	default:
		return "internal_error"
	}
}

// serveConsole dispatches a path under /ui/ or exactly / to Console, or answers 404 route_unknown when it is nil.
// The console owns the method check, the 302, and its headers; this only counts what it wrote.
func (s *server) serveConsole(w http.ResponseWriter, r *http.Request, q *request) {
	q.console = true
	if s.deps.Console == nil {
		q.fail(w, &requestError{status: http.StatusNotFound, code: "route_unknown", message: "no such route"})

		return
	}
	sw := &statusWriter{ResponseWriter: w}
	s.deps.Console.ServeHTTP(sw, r)
	status := sw.status
	if status == 0 {
		// Nothing was written: net/http answers 200 with an empty body.
		status = http.StatusOK
	}
	q.audit.status = status
	q.audit.code = consoleCode(status)
}
