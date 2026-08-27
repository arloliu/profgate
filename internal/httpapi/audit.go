package httpapi

import (
	"log/slog"
	"time"
)

// auditRecord is the one log record every /v1 request emits on completion.
// It names the principal, the route, and the selected Pod or Collection, never the Pod's address.
// An interactive request and a PGO one carry different keys, because they answer different questions:
// which Pod and profile the caller reached, or which Collection and method it acted on.
type auditRecord struct {
	pgo        bool
	principal  string
	namespace  string
	service    string
	pod        string
	profile    string
	collection string
	method     string
	seconds    int
	port       string // the port selection as sent; empty when absent or malformed
	status     int
	code       string
	duration   time.Duration
}

// writeAudit emits rec as the "request" record at info level.
func writeAudit(log *slog.Logger, rec auditRecord) {
	if rec.pgo {
		log.Info("request",
			"principal", rec.principal,
			"namespace", rec.namespace,
			"service", rec.service,
			"collection", rec.collection,
			"method", rec.method,
			"status", rec.status,
			"code", rec.code,
			"duration_ms", rec.duration.Milliseconds(),
		)

		return
	}

	log.Info("request",
		"principal", rec.principal,
		"namespace", rec.namespace,
		"service", rec.service,
		"pod", rec.pod,
		"profile", rec.profile,
		"seconds", rec.seconds,
		"port", rec.port,
		"status", rec.status,
		"code", rec.code,
		"duration_ms", rec.duration.Milliseconds(),
	)
}
