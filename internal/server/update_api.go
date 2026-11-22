package server

import (
	"net/http"

	"github.com/nesaon03/fastrouter/internal/update"
)

// handleUpdate reports the npm release channel: the running version, the latest
// published one, and the command the dashboard shows. The checker caches, so a
// dashboard reload does not hit the registry every time.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	update.SetUserAgent(Version)
	adminJSON(w, http.StatusOK, s.updates.Check(r.Context(), Version))
}
