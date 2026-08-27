package httpapi

import (
	"log/slog"
	"time"
)

// auditRecord is the one log record every request emits on completion.
// It names the principal, the route, and the selected Pod or Collection, never the Pod's address.
// An interactive request, a PGO one, and an /auth/ one carry different keys, because they answer different questions:
// which Pod and profile the caller reached, which Collection and method it acted on,
// or which login step a browser took.
// An authentication failure adds the reason and never the credential.
type auditRecord struct {
	pgo        bool
	route      string // auth_login, auth_callback, or auth_logout; set only for the /auth/ routes
	reason     string // auth_reason; empty on a successful authentication
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
	var attrs []any
	switch {
	case rec.route != "":
		attrs = []any{
			"principal", rec.principal,
			"route", rec.route,
			"method", rec.method,
		}
	case rec.pgo:
		attrs = []any{
			"principal", rec.principal,
			"namespace", rec.namespace,
			"service", rec.service,
			"collection", rec.collection,
			"method", rec.method,
		}
	default:
		attrs = []any{
			"principal", rec.principal,
			"namespace", rec.namespace,
			"service", rec.service,
			"pod", rec.pod,
			"profile", rec.profile,
			"seconds", rec.seconds,
			"port", rec.port,
		}
	}
	attrs = append(attrs,
		"status", rec.status,
		"code", rec.code,
		"duration_ms", rec.duration.Milliseconds(),
	)
	if rec.reason != "" {
		attrs = append(attrs, "auth_reason", rec.reason)
	}
	log.Info("request", attrs...)
}
