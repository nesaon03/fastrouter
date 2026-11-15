// Package dashboard serves the embedded read-only status page.
package dashboard

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var page []byte

// Handler serves the dashboard page; it reads its data from /api/status.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		_, _ = w.Write(page)
	})
}
