package tlscert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/profgate/internal/metrics"
)

// countingRecorder remembers every reload result and the last expiry reported.
type countingRecorder struct {
	metrics.Noop
	mu      sync.Mutex
	results []string
	expiry  time.Time
}

func (r *countingRecorder) TLSReload(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
}

func (r *countingRecorder) TLSCertificateExpiry(notAfter time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expiry = notAfter
}

func (r *countingRecorder) snapshot() ([]string, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.results...), r.expiry
}

// pair is one self-signed certificate and its key, in PEM.
type pair struct {
	name     string
	cert     []byte
	key      []byte
	notAfter time.Time
}

// newPair mints a self-signed certificate whose common name is name,
// so a test can say which certificate a handshake would receive.
func newPair(t *testing.T, name string) pair {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	notAfter := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	return pair{
		name:     name,
		cert:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:      pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		notAfter: notAfter,
	}
}

// write puts the pair at certPath and keyPath.
func (p pair) write(t *testing.T, certPath, keyPath string) {
	t.Helper()

	if err := os.WriteFile(certPath, p.cert, 0o600); err != nil {
		t.Fatalf("write %s: %v", certPath, err)
	}
	if err := os.WriteFile(keyPath, p.key, 0o600); err != nil {
		t.Fatalf("write %s: %v", keyPath, err)
	}
}

// fixture is one loader over one temporary directory, built fresh per subtest.
type fixture struct {
	dir      string
	certPath string
	keyPath  string
	loader   *Loader
	rec      *countingRecorder
}

// newFixture writes p into a temporary directory and loads it.
func newFixture(t *testing.T, p pair) fixture {
	t.Helper()

	f := fixture{dir: t.TempDir(), rec: &countingRecorder{}}
	f.certPath = filepath.Join(f.dir, "tls.crt")
	f.keyPath = filepath.Join(f.dir, "tls.key")
	p.write(t, f.certPath, f.keyPath)

	loader, err := New(Options{
		CertFile: f.certPath,
		KeyFile:  f.keyPath,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recorder: f.rec,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.loader = loader

	return f
}

// served returns the common name of the certificate a handshake would receive.
func served(t *testing.T, l *Loader) string {
	t.Helper()

	cert, err := l.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil || cert.Leaf == nil {
		t.Fatalf("GetCertificate returned %+v, want a certificate with a parsed leaf", cert)
	}

	return cert.Leaf.Subject.CommonName
}

// TestNew covers the startup contract: the pair on disk is served, and a pair
// that cannot be read or does not match is a startup failure rather than a
// listener that answers every handshake with an error.
func TestNew(t *testing.T) {
	t.Run("serves the pair on disk", func(t *testing.T) {
		p := newPair(t, "first")
		f := newFixture(t, p)

		if got := served(t, f.loader); got != "first" {
			t.Errorf("served certificate = %q, want %q", got, "first")
		}
		results, expiry := f.rec.snapshot()
		if len(results) != 1 || results[0] != "applied" {
			t.Errorf("reload results = %v, want one applied", results)
		}
		if !expiry.Equal(p.notAfter) {
			t.Errorf("recorded expiry = %v, want the leaf's notAfter %v", expiry, p.notAfter)
		}
	})

	t.Run("a mismatched pair fails", func(t *testing.T) {
		dir := t.TempDir()
		certPath := filepath.Join(dir, "tls.crt")
		keyPath := filepath.Join(dir, "tls.key")
		if err := os.WriteFile(certPath, newPair(t, "first").cert, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, newPair(t, "second").key, 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := New(Options{
			CertFile: certPath,
			KeyFile:  keyPath,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Recorder: metrics.Noop{},
		})
		if err == nil {
			t.Fatal("New accepted a certificate that does not match its key")
		}
	})

	t.Run("a missing file fails", func(t *testing.T) {
		dir := t.TempDir()
		_, err := New(Options{
			CertFile: filepath.Join(dir, "absent.crt"),
			KeyFile:  filepath.Join(dir, "absent.key"),
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Recorder: metrics.Noop{},
		})
		if err == nil {
			t.Fatal("New accepted a certificate path that does not exist")
		}
	})
}

// TestGetCertificateReadsNoFile is why the loader exists at all: the handshake
// path takes the parsed pair from memory, so a disk that is momentarily
// unreadable cannot fail a connection.
func TestGetCertificateReadsNoFile(t *testing.T) {
	f := newFixture(t, newPair(t, "first"))
	if err := os.Remove(f.certPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.keyPath); err != nil {
		t.Fatal(err)
	}

	if got := served(t, f.loader); got != "first" {
		t.Errorf("served certificate = %q, want %q with both files deleted", got, "first")
	}
}

// TestRefresh covers what happens between handshakes: a rotated pair is picked
// up, an unreadable or mismatched one is ignored in favor of the pair already
// being served, and files nobody touched are not parsed again.
func TestRefresh(t *testing.T) {
	t.Run("a rotated pair is served", func(t *testing.T) {
		f := newFixture(t, newPair(t, "first"))
		second := newPair(t, "second")
		second.write(t, f.certPath, f.keyPath)

		f.loader.refresh()

		if got := served(t, f.loader); got != "second" {
			t.Errorf("served certificate = %q, want the rotated %q", got, "second")
		}
		results, expiry := f.rec.snapshot()
		if len(results) != 2 || results[1] != "applied" {
			t.Errorf("reload results = %v, want a second applied", results)
		}
		if !expiry.Equal(second.notAfter) {
			t.Errorf("recorded expiry = %v, want the rotated leaf's notAfter %v", expiry, second.notAfter)
		}
	})

	t.Run("unchanged files are not parsed again", func(t *testing.T) {
		f := newFixture(t, newPair(t, "first"))
		before, err := f.loader.GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			t.Fatal(err)
		}

		f.loader.refresh()

		after, err := f.loader.GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			t.Fatal(err)
		}
		// Pointer identity, not the common name: a loader that re-parsed and
		// swapped on every tick would serve an equal certificate and pass any
		// comparison by value.
		if before != after {
			t.Error("an unchanged refresh replaced the certificate, so every tick re-parses the files")
		}
		results, _ := f.rec.snapshot()
		if len(results) != 2 || results[1] != "unchanged" {
			t.Errorf("reload results = %v, want an unchanged second result", results)
		}
	})

	t.Run("a broken pair keeps the last good one", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			cert []byte
			key  []byte
		}{
			{name: "mismatched key", cert: newPair(t, "second").cert, key: newPair(t, "third").key},
			{name: "truncated certificate", cert: []byte("-----BEGIN CERTIFICATE-----\n"), key: newPair(t, "third").key},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newFixture(t, newPair(t, "first"))
				if err := os.WriteFile(f.certPath, tc.cert, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.keyPath, tc.key, 0o600); err != nil {
					t.Fatal(err)
				}

				f.loader.refresh()

				if got := served(t, f.loader); got != "first" {
					t.Errorf("served certificate = %q, want the last good %q", got, "first")
				}
				results, _ := f.rec.snapshot()
				if len(results) != 2 || results[1] != "failed" {
					t.Errorf("reload results = %v, want a failed second result", results)
				}
			})
		}
	})

	t.Run("an unreadable file keeps the last good pair", func(t *testing.T) {
		f := newFixture(t, newPair(t, "first"))
		if err := os.Remove(f.certPath); err != nil {
			t.Fatal(err)
		}

		f.loader.refresh()

		if got := served(t, f.loader); got != "first" {
			t.Errorf("served certificate = %q, want the last good %q", got, "first")
		}
		results, _ := f.rec.snapshot()
		if len(results) != 2 || results[1] != "failed" {
			t.Errorf("reload results = %v, want a failed second result", results)
		}
	})
}

// TestRefreshFollowsASecretVolumeSwap reproduces how the kubelet updates a
// mounted Secret: it writes the new contents into a fresh directory and
// renames a symlink over the old one, which replaces the inode behind the path
// the configuration names.
// A loader that held the file open, or watched it by inode,
// would serve the old certificate forever after the first rotation.
func TestRefreshFollowsASecretVolumeSwap(t *testing.T) {
	dir := t.TempDir()
	first, second := newPair(t, "first"), newPair(t, "second")

	// The layout a Secret volume has: ..data points at a timestamped
	// directory, and the user-facing names point through it.
	dataA := filepath.Join(dir, "..data_a")
	dataB := filepath.Join(dir, "..data_b")
	for _, d := range []string{dataA, dataB} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	first.write(t, filepath.Join(dataA, "tls.crt"), filepath.Join(dataA, "tls.key"))
	second.write(t, filepath.Join(dataB, "tls.crt"), filepath.Join(dataB, "tls.key"))

	data := filepath.Join(dir, "..data")
	if err := os.Symlink(dataA, data); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.Symlink(filepath.Join(data, "tls.crt"), certPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(data, "tls.key"), keyPath); err != nil {
		t.Fatal(err)
	}

	rec := &countingRecorder{}
	loader, err := New(Options{
		CertFile: certPath,
		KeyFile:  keyPath,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recorder: rec,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := served(t, loader); got != "first" {
		t.Fatalf("served certificate = %q, want %q before the swap", got, "first")
	}

	// The swap itself: a new symlink renamed over ..data, which is atomic.
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(dataB, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, data); err != nil {
		t.Fatal(err)
	}

	loader.refresh()

	if got := served(t, loader); got != "second" {
		t.Errorf("served certificate = %q, want %q after the ..data swap", got, "second")
	}
}

// TestRun proves the ticker is wired to the same refresh the tests above call,
// and that the goroutine ends with its context so a drain is never held up by it.
func TestRun(t *testing.T) {
	f := newFixture(t, newPair(t, "first"))
	f.loader.interval = 5 * time.Millisecond
	newPair(t, "second").write(t, f.certPath, f.keyPath)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.loader.Run(ctx)
	}()

	deadline := time.After(2 * time.Second)
	for served(t, f.loader) != "second" {
		select {
		case <-deadline:
			t.Fatal("Run never picked up the rotated certificate")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
}

// TestTLSConfig pins what the listener is built with: the loader's own
// GetCertificate, and the floor the configuration asked for.
func TestTLSConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		min  string
		want uint16
	}{
		{name: "default", min: "", want: tls.VersionTLS12},
		{name: "1.2", min: "1.2", want: tls.VersionTLS12},
		{name: "1.3", min: "1.3", want: tls.VersionTLS13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, newPair(t, "first"))
			f.loader.minVersion = parseMinVersion(tc.min)

			cfg := f.loader.TLSConfig()

			if cfg.MinVersion != tc.want {
				t.Errorf("MinVersion = %#x, want %#x", cfg.MinVersion, tc.want)
			}
			if cfg.GetCertificate == nil {
				t.Fatal("TLSConfig has no GetCertificate, so the listener would serve no certificate")
			}
			if len(cfg.Certificates) != 0 {
				t.Errorf("Certificates = %+v, want none: a fixed certificate would never rotate", cfg.Certificates)
			}
			cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
			if err != nil {
				t.Fatalf("GetCertificate: %v", err)
			}
			if cert.Leaf.Subject.CommonName != "first" {
				t.Errorf("GetCertificate served %q, want the loader's own certificate", cert.Leaf.Subject.CommonName)
			}
		})
	}
}
