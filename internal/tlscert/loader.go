// Package tlscert holds the certificate the API listener serves.
// It parses the pair once at startup, hands it to every handshake from memory,
// and re-reads the two files while the process runs,
// so a certificate rotated in place is served without a restart.
package tlscert

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/arloliu/profgate/internal/metrics"
)

// refreshInterval is how often the two files are read again.
// The end-to-end delay after a Secret changes is dominated by the kubelet's
// own sync period, so a shorter interval would buy nothing measurable.
const refreshInterval = 30 * time.Second

// Options is what a Loader needs: the pair's two paths, the floor handshakes
// are held to, and where it logs and records.
type Options struct {
	CertFile string
	KeyFile  string
	// MinVersion is "1.2" or "1.3"; config's oneof tag pins the set,
	// and anything else is read as 1.2.
	MinVersion string
	// Interval overrides how often the files are read again.
	// Zero is refreshInterval.
	Interval time.Duration
	Logger   *slog.Logger
	Recorder metrics.Recorder
}

// A Loader serves one certificate and keeps it current with the files it came from.
type Loader struct {
	certFile   string
	keyFile    string
	minVersion uint16
	interval   time.Duration
	// cur is the parsed pair every handshake reads, and the only state shared
	// between the refresh goroutine and the handshakes.
	cur atomic.Pointer[tls.Certificate]
	// sum fingerprints the bytes cur was parsed from.
	// Only New and the refresh goroutine touch it.
	sum fingerprint
	log *slog.Logger
	rec metrics.Recorder
}

// fingerprint is one digest per file rather than one over both,
// so no pair of files can hash equal to a differently split pair.
type fingerprint struct {
	cert [sha256.Size]byte
	key  [sha256.Size]byte
}

// New reads and parses the pair, and fails when it cannot.
// A gateway configured for TLS that cannot serve a certificate has nothing to
// offer a client, so this is fatal at startup rather than a listener that
// answers every handshake with an error.
func New(opts Options) (*Loader, error) {
	l := &Loader{
		certFile:   opts.CertFile,
		keyFile:    opts.KeyFile,
		minVersion: parseMinVersion(opts.MinVersion),
		interval:   opts.Interval,
		log:        opts.Logger,
		rec:        opts.Recorder,
	}
	if l.interval == 0 {
		l.interval = refreshInterval
	}
	cert, sum, err := l.read()
	if err != nil {
		return nil, err
	}
	l.apply(cert, sum)

	return l, nil
}

// TLSConfig is the configuration the API listener is built with.
// It carries no Certificates: a fixed certificate is one that never rotates,
// and GetCertificate is what makes the rotation reach a handshake.
func (l *Loader) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: l.GetCertificate,
		MinVersion:     l.minVersion,
	}
}

// GetCertificate serves the certificate currently loaded.
// It reads no file, so a disk that is momentarily unreadable, or a Secret
// mid-rotation, cannot fail a handshake.
func (l *Loader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return l.cur.Load(), nil
}

// Run re-reads the pair every interval until ctx ends.
// It holds nothing a shutdown waits for: the certificate already loaded keeps
// serving connections through the drain.
func (l *Loader) Run(ctx context.Context) {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.refresh()
		case <-ctx.Done():
			return
		}
	}
}

// refresh reads both files and swaps the certificate only when their contents
// have changed.
// A read or parse that fails leaves the pair already loaded in place: half a
// written file, or a certificate without its matching key, is a state the disk
// passes through, not a reason to stop serving.
func (l *Loader) refresh() {
	cert, sum, err := l.read()
	if err != nil {
		l.rec.TLSReload("failed")
		l.log.Warn("certificate reload failed; keeping the one already loaded",
			"certFile", l.certFile, "error", err)

		return
	}
	if sum == l.sum {
		l.rec.TLSReload("unchanged")

		return
	}
	l.apply(cert, sum)
	l.log.Info("certificate reloaded", "certFile", l.certFile, "notAfter", cert.Leaf.NotAfter)
}

// apply publishes the pair to the handshake path and reports it.
func (l *Loader) apply(cert *tls.Certificate, sum fingerprint) {
	l.cur.Store(cert)
	l.sum = sum
	l.rec.TLSReload("applied")
	l.rec.TLSCertificateExpiry(cert.Leaf.NotAfter)
}

// read reads both files and parses them as a key pair.
// The files are read rather than watched because the kubelet replaces a Secret
// volume by renaming a directory symlink over the old one, which changes the
// inode behind each path; reading through the path follows the swap.
// The contents are hashed rather than their timestamps compared, because a
// hash has no filesystem-granularity or clock-skew failure modes and the files
// are a few kilobytes.
func (l *Loader) read() (*tls.Certificate, fingerprint, error) {
	certPEM, err := os.ReadFile(l.certFile) //nolint:gosec // the operator names the file; reading it is the purpose
	if err != nil {
		return nil, fingerprint{}, fmt.Errorf("tlscert: read certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(l.keyFile) //nolint:gosec // the operator names the file; reading it is the purpose
	if err != nil {
		return nil, fingerprint{}, fmt.Errorf("tlscert: read key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fingerprint{}, fmt.Errorf("tlscert: %s and %s: %w", l.certFile, l.keyFile, err)
	}

	return &cert, fingerprint{cert: sha256.Sum256(certPEM), key: sha256.Sum256(keyPEM)}, nil
}

// parseMinVersion maps the configured name to the version handshakes are held to.
// The oneof tag on server.tls.minVersion pins the set to two names,
// so an unknown one never reaches here and 1.2 is the unreachable fallback.
func parseMinVersion(name string) uint16 {
	if name == "1.3" {
		return tls.VersionTLS13
	}

	return tls.VersionTLS12
}
