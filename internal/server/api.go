package server

import (
	"io"
	"net/http"

	tr "github.com/nesaon03/fastrouter/internal/translate"
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
