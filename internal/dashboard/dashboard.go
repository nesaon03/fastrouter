// Package dashboard serves the embedded single-page dashboard. It is plain
// static files (Preact + htm, no build step, no Node at run time); every bit of
// data comes from the authenticated /api/admin endpoints.
//
// Layout, palette and provider logos follow 9Router's dashboard (MIT,
// decolua/9router); see static/NOTICE.txt.
package dashboard

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

//go:embed static
var files embed.FS

var static, _ = fs.Sub(files, "static")

const csp = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Handler serves /dashboard (the app shell) and /dashboard/<asset>.
func Handler() http.Handler {
	fsrv := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/dashboard" { // relative asset URLs need the trailing slash
			http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/dashboard")
		p = strings.TrimPrefix(path.Clean("/"+p), "/")
		if p == "" || p == "index.html" {
			b, _ := fs.ReadFile(static, "index.html")
			h.Set("Content-Type", "text/html; charset=utf-8")
			h.Set("Cache-Control", "no-cache")
			_, _ = w.Write(b)
			return
		}
		if _, err := fs.Stat(static, p); err != nil {
			http.NotFound(w, r)
			return
		}
		// Embedded files have no mod time, so revalidate every asset by content hash.
		// A long max-age kept old icon-font subsets alive after upgrades.
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", assetETag(static, p))
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + p
		fsrv.ServeHTTP(w, r2)
	})
}

var etags sync.Map

func assetETag(fsys fs.FS, name string) string {
	if v, ok := etags.Load(name); ok {
		return v.(string)
	}
	b, _ := fs.ReadFile(fsys, name)
	sum := sha256.Sum256(b)
	tag := `"` + hex.EncodeToString(sum[:8]) + `"`
	etags.Store(name, tag)
	return tag
}
