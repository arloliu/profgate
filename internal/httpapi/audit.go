package httpapi

import (
	"log/slog"
	"time"
)

// auditRecord is the one log record every /v1 request emits on completion.
// It names the principal, the route, and the selected Pod, never the Pod's address.
type auditRecord struct {
	principal string
	namespace string
	service   string
	pod       string
	profile   string
	seconds   int
	status    int
	code      string
	duration  time.Duration
}

// writeAudit emits rec as the "request" record at info level.
func writeAudit(log *slog.Logger, rec auditRecord) {
	log.Info("request",
		"principal", rec.principal,
		"namespace", rec.namespace,
		"service", rec.service,
		"pod", rec.pod,
		"profile", rec.profile,
		"seconds", rec.seconds,
		"status", rec.status,
		"code", rec.code,
		"duration_ms", rec.duration.Milliseconds(),
	)
}
