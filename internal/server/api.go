package server

import (
	"io"
	"net/http"
	"time"

	tr "github.com/zenpo/fastrouter/internal/translate"
)

// handleCountTokens estimates prompt tokens locally (no upstream call).
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeError(w, tr.Anthropic, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		return
	}
	req, err := tr.ParseRequest(tr.Anthropic, body, "", false)
	if err != nil {
		writeError(w, tr.Anthropic, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, map[string]int{"input_tokens": tr.EstimateTokens(req)})
}

func (s *Server) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	models := make([]map[string]any, 0, len(st.Models))
	for _, m := range st.Models {
		models = append(models, map[string]any{"name": "models/" + m, "displayName": m,
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"}})
	}
	writeJSON(w, map[string]any{"models": models})
}

// handleUsage reports token usage and cost per account, model and API key.
// ?window=24h|7d|30d|all (default 24h).
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		writeJSON(w, map[string]any{"error": "no database"})
		return
	}
	window := r.URL.Query().Get("window")
	since := time.Now().Add(-24 * time.Hour)
	switch window {
	case "7d":
		since = time.Now().Add(-7 * 24 * time.Hour)
	case "30d":
		since = time.Now().Add(-30 * 24 * time.Hour)
	case "all":
		since = time.Time{}
	default:
		window = "24h"
	}
	out := map[string]any{"window": window, "since": since.UTC().Format(time.RFC3339), "note": "cost is an estimate from 9Router's price table"}
	for _, by := range []string{"account", "model", "key"} {
		rows, err := s.st.UsageReport(since, by)
		if err != nil {
			http.Error(w, "usage query failed", http.StatusInternalServerError)
			return
		}
		if by == "account" {
			for i := range rows {
				rows[i].Name = maskEmail(rows[i].Name)
			}
		}
		out[by+"s"] = rows
	}
	writeJSON(w, out)
}
