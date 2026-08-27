package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/arloliu/profgate/internal/httpapi"
)

//go:embed static
var static embed.FS

const (
	// Prefix is where the console lives; the shell is served at exactly this path.
	Prefix = "/ui/"
	// assetPrefix is where the hashed tree is served: assetPrefix + hash + "/" + path.
	assetPrefix = "/ui/static/"
	// The two placeholders index.html carries, replaced by the hashed asset paths when the shell is rendered.
	stylesheetPlaceholder = "__STYLESHEET__"
	scriptPlaceholder     = "__SCRIPT__"

	// indexName is the shell's template; it is hashed with the tree and has no asset route.
	indexName = "index.html"

	// contentSecurityPolicy lists by name every source the page needs, over a default of none.
	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src data:; " +
		"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'"

	cacheNoStore   = "no-store"
	cacheImmutable = "public, max-age=31536000, immutable"
	allowMethods   = "GET, HEAD"
)

// Handler serves the console: the rendered shell at /ui/, the hashed tree under /ui/static/<hash>/, and the 302 from /.
type Handler struct {
	hash  string
	shell []byte
	files fs.FS // rooted at the static directory
}

// New builds the handler over the embedded tree.
func New() (*Handler, error) {
	fsys, err := fs.Sub(static, "static")
	if err != nil {
		return nil, fmt.Errorf("open embedded static tree: %w", err)
	}

	return newFromFS(fsys)
}

// newFromFS builds the handler over any tree rooted like the static directory; tests hand it a modified copy.
func newFromFS(fsys fs.FS) (*Handler, error) {
	hash, err := treeHash(fsys)
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(fsys, indexName)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", indexName, err)
	}
	shell, err := renderShell(index, hash)
	if err != nil {
		return nil, err
	}

	return &Handler{hash: hash, shell: shell, files: fsys}, nil
}

// Hash is the tree hash every asset is served under.
func (h *Handler) Hash() string {
	return h.hash
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	head := r.Method == http.MethodHead
	if r.Method != http.MethodGet && !head {
		w.Header().Set("Allow", allowMethods)
		h.writeError(w, head, http.StatusMethodNotAllowed, "method_not_allowed", "method "+r.Method+" not allowed")

		return
	}

	switch p := r.URL.Path; p {
	case "/":
		w.Header().Set("Location", Prefix)
		w.Header().Set("Cache-Control", cacheNoStore)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusFound)
	case Prefix:
		h.writeBytes(w, head, h.shell, contentType(indexName), cacheNoStore)
	default:
		name, ok := h.assetName(p)
		if !ok {
			h.writeError(w, head, http.StatusNotFound, "route_unknown", "no such route")

			return
		}
		b, err := fs.ReadFile(h.files, name)
		if err != nil {
			h.writeError(w, head, http.StatusNotFound, "route_unknown", "no such route")

			return
		}
		h.writeBytes(w, head, b, contentType(name), cacheImmutable)
	}
}

// assetName maps a request path under the hashed prefix to a regular file of the tree.
// It refuses an empty rest, a leading slash, a backslash, any "." or ".." segment,
// the shell's template, and anything that is not a regular file.
func (h *Handler) assetName(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, assetPrefix+h.hash+"/")
	if !ok || rest == "" || strings.HasPrefix(rest, "/") || strings.ContainsRune(rest, '\\') {
		return "", false
	}
	if !fs.ValidPath(rest) || rest == indexName {
		return "", false
	}
	info, err := fs.Stat(h.files, rest)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	return rest, true
}

// writeBytes answers a 200 with b, or its headers alone on HEAD.
func (h *Handler) writeBytes(w http.ResponseWriter, head bool, b []byte, ctype, cache string) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if !head {
		_, _ = w.Write(b) //nolint:gosec // G705: b is an embedded file or the rendered shell, never request input
	}
}

// writeError answers the gateway's error envelope with its Content-Length,
// or on HEAD the same headers and status with no body.
func (h *Handler) writeError(w http.ResponseWriter, head bool, status int, code, message string) {
	body := httpapi.ErrorEnvelope(code, message)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", cacheNoStore)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if !head {
		_, _ = w.Write(body) //nolint:gosec // G705: a JSON-encoded envelope of gateway-chosen strings, never request input
	}
}

// treeHash is SHA-256 over every regular file of the tree in path order, index.html and vendor/MANIFEST included,
// truncated to 16 hex digits (Layout and embedding).
// Each file is framed as: the path length as an 8-byte big-endian integer, the path bytes,
// the content length as an 8-byte big-endian integer, the content bytes;
// the framing keeps two trees whose path/content boundaries differ from concatenating to the same input.
func treeHash(fsys fs.FS) (string, error) {
	var paths []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			paths = append(paths, p)
		}

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk static tree: %w", err)
	}
	sort.Strings(paths)

	sum := sha256.New()
	var length [8]byte
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", p, err)
		}
		binary.BigEndian.PutUint64(length[:], uint64(len(p)))
		sum.Write(length[:])
		sum.Write([]byte(p))
		binary.BigEndian.PutUint64(length[:], uint64(len(b)))
		sum.Write(length[:])
		sum.Write(b)
	}

	return hex.EncodeToString(sum.Sum(nil))[:16], nil
}

// renderShell replaces the two placeholders; a placeholder that is absent or repeated is an error.
func renderShell(index []byte, hash string) ([]byte, error) {
	out := index
	for placeholder, file := range map[string]string{stylesheetPlaceholder: "app.css", scriptPlaceholder: "app.js"} {
		if n := bytes.Count(out, []byte(placeholder)); n != 1 {
			return nil, fmt.Errorf("%s: placeholder %s appears %d times, want exactly once", indexName, placeholder, n)
		}
		out = bytes.ReplaceAll(out, []byte(placeholder), []byte(assetPrefix+hash+"/"+file))
	}

	return out, nil
}

// contentType is the Content-Type of Headers: text/javascript, text/css, text/html, else mime.TypeByExtension, else application/octet-stream.
func contentType(name string) string {
	ext := path.Ext(name)
	switch ext {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	}
	if ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
	}

	return "application/octet-stream"
}

// setSecurityHeaders writes every header of Response headers and CSP except Cache-Control and Content-Type.
func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}
