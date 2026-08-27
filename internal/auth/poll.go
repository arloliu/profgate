package auth

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"time"

	"github.com/arloliu/profgate/internal/metrics"
)

// pollInterval is how often a re-read file is read again.
// The end-to-end delay after a Secret changes is dominated by the kubelet's
// own sync period, so a shorter interval would buy nothing measurable.
const pollInterval = 30 * time.Second

// fileReader reads a file by path; production is os.ReadFile and a test
// serves bytes without touching disk.
type fileReader func(path string) ([]byte, error)

// filePoller re-reads one file and hands its bytes to apply when they change.
// The users file and the cookie key file share it.
// The file is read rather than watched because the kubelet replaces a Secret
// volume by renaming a directory symlink over the old one, which changes the
// inode behind the path; reading through the path follows the swap.
type filePoller struct {
	path     string
	file     string // the metrics label: "users" or "cookie_key"
	apply    func([]byte) error
	read     fileReader
	interval time.Duration
	// sum fingerprints the bytes apply last accepted; loaded says whether
	// it has accepted any. Only the caller of Poll touches them.
	sum    [sha256.Size]byte
	loaded bool
	rec    metrics.Recorder
	log    *slog.Logger
}

// newFilePoller builds a poller over path whose accepted bytes go to apply,
// re-reading every interval under Run; zero means pollInterval.
// Nothing is read until Poll.
func newFilePoller(
	path string, apply func([]byte) error, file string, interval time.Duration, rec metrics.Recorder, log *slog.Logger,
) *filePoller {
	if interval <= 0 {
		interval = pollInterval
	}

	return &filePoller{
		path:     path,
		file:     file,
		apply:    apply,
		read:     os.ReadFile,
		interval: interval,
		rec:      rec,
		log:      log,
	}
}

// Poll reads the file once and applies it only when its bytes differ from the
// last accepted read.
// A read or apply that fails leaves the previous state in place and counts a
// failed reload: half a written file is a state the disk passes through, not
// a reason to drop the set already serving.
// The failed bytes are not remembered, so the same bytes are judged again on
// the next poll rather than skipped as unchanged.
func (p *filePoller) Poll() {
	b, err := p.read(p.path)
	if err != nil {
		p.rec.AuthFileReload(p.file, "failed")
		p.log.Warn("file reload failed; keeping what is already loaded", "file", p.file, "path", p.path, "error", err)

		return
	}
	sum := sha256.Sum256(b)
	if p.loaded && sum == p.sum {
		p.rec.AuthFileReload(p.file, "ok")

		return
	}
	if err := p.apply(b); err != nil {
		p.rec.AuthFileReload(p.file, "failed")
		p.log.Warn("file reload failed; keeping what is already loaded", "file", p.file, "path", p.path, "error", err)

		return
	}
	p.sum, p.loaded = sum, true
	p.rec.AuthFileReload(p.file, "ok")
	p.log.Info("file reloaded", "file", p.file, "path", p.path)
}

// load reads and applies the file once at startup and returns what failed,
// so the caller can refuse to start; Poll then re-reads only when the bytes
// change.
func (p *filePoller) load() error {
	b, err := p.read(p.path)
	if err != nil {
		return err
	}
	if err := p.apply(b); err != nil {
		return err
	}
	p.sum, p.loaded = sha256.Sum256(b), true

	return nil
}

// Run polls every interval until ctx ends.
func (p *filePoller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.Poll()
		case <-ctx.Done():
			return
		}
	}
}
