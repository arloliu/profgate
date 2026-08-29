package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/arloliu/profgate/internal/httpapi"
)

//go:embed static
var static embed.FS

const (
	// Prefix is where the console lives; the shell is served at exactly this path,
	// and every other file of the tree at Prefix + its path under static/.
	Prefix = "/ui/"

	// indexName is the shell; it is served at Prefix and has no asset route of its own,
	// because one file at two URLs is two answers to cache.
	indexName = "index.html"

	// contentSecurityPolicy lists by name every source the page needs, over a default of none.
	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src data:; " +
		"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'"

	cacheNoStore = "no-store"
	cacheNoCache = "no-cache"
	allowMethods = "GET, HEAD"
)

// asset is one regular file of the embedded tree, answered without reading or hashing anything per request.
type asset struct {
	data   []byte
	ctype  string
	length string
	etag   string
}

// Handler serves the console: the shell at /ui/, every other file of the tree at its own path under /ui/,
// and the 302 from /.
type Handler struct {
	// assets holds every regular file of the tree, index.html included, keyed by its path under static/.
	// It is built once and never written afterwards, so ServeHTTP reads it without a lock.
	assets map[string]asset
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
	assets := make(map[string]asset)
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		sum := sha256.Sum256(b)
		assets[p] = asset{
			data:   b,
			ctype:  contentType(p),
			length: strconv.Itoa(len(b)),
			etag:   `"` + hex.EncodeToString(sum[:]) + `"`,
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk static tree: %w", err)
	}
	if _, ok := assets[indexName]; !ok {
		return nil, fmt.Errorf("static tree holds no %s", indexName)
	}

	return &Handler{assets: assets}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	head := r.Method == http.MethodHead
	if r.Method != http.MethodGet && !head {
		w.Header().Set("Allow", allowMethods)
		h.writeError(w, head, http.StatusMethodNotAllowed, httpapi.CodeMethodNotAllowed,
			"method "+r.Method+" not allowed")

		return
	}

	switch p := r.URL.Path; p {
	case "/":
		w.Header().Set("Location", Prefix)
		w.Header().Set("Cache-Control", cacheNoStore)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusFound)
	case Prefix:
		h.writeShell(w, head)
	default:
		name, ok := h.assetName(p)
		if !ok {
			h.writeError(w, head, http.StatusNotFound, httpapi.CodeRouteUnknown, "no such route")

			return
		}
		h.writeAsset(w, head, r.Header.Get("If-None-Match"), h.assets[name])
	}
}

// assetName maps a request path under /ui/ to a regular file of the tree.
// It refuses an empty rest, a leading slash, a backslash, any "." or ".." segment,
// the shell, and anything the table does not hold.
// A directory name and a traversal are refused by these rules alone,
// since neither is a key of a table built from regular files.
func (h *Handler) assetName(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, Prefix)
	if !ok || rest == "" || strings.HasPrefix(rest, "/") || strings.ContainsRune(rest, '\\') {
		return "", false
	}
	if !fs.ValidPath(rest) || rest == indexName {
		return "", false
	}
	if _, ok := h.assets[rest]; !ok {
		return "", false
	}

	return rest, true
}

// writeShell answers /ui/ with index.html as it was written:
// never stored, and with no entity tag, because the shell is the one file
// whose content decides what the browser fetches next.
func (h *Handler) writeShell(w http.ResponseWriter, head bool) {
	a := h.assets[indexName]
	w.Header().Set("Content-Type", a.ctype)
	w.Header().Set("Cache-Control", cacheNoStore)
	w.Header().Set("Content-Length", a.length)
	w.WriteHeader(http.StatusOK)
	if !head {
		_, _ = w.Write(a.data) //nolint:gosec // G705: an embedded file, never request input
	}
}

// writeAsset answers one file of the tree, or the 304 an If-None-Match holding its tag earns.
// A 304 carries the tag and the cache policy, no body, and no Content-Length.
func (h *Handler) writeAsset(w http.ResponseWriter, head bool, condition string, a asset) {
	w.Header().Set("Content-Type", a.ctype)
	w.Header().Set("Cache-Control", cacheNoCache)
	w.Header().Set("ETag", a.etag)
	if tagMatches(condition, a.etag) {
		w.WriteHeader(http.StatusNotModified)

		return
	}
	w.Header().Set("Content-Length", a.length)
	w.WriteHeader(http.StatusOK)
	if !head {
		_, _ = w.Write(a.data) //nolint:gosec // G705: an embedded file, never request input
	}
}

// tagMatches reports whether an If-None-Match header value holds tag.
// The value is a comma-separated list, and "*" matches any entity.
// A leading "W/" is trimmed from each member so a proxy that weakened the tag still revalidates;
// no response of this handler sets one.
func tagMatches(condition, tag string) bool {
	for member := range strings.SplitSeq(condition, ",") {
		member = strings.TrimSpace(member)
		if member == "*" {
			return true
		}
		if strings.TrimPrefix(member, "W/") == tag {
			return true
		}
	}

	return false
}

// writeError answers the gateway's error envelope with its Content-Length,
// or on HEAD the same headers and status with no body.
// The code is one of the gateway's registry constants, so the console and the
// rest of the gateway cannot spell the same refusal two ways.
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
