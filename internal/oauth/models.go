package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// fetchAvailableModels is the Code Assist call that answers "what may this
// account use". It is the only model list Antigravity and Gemini CLI publish;
// both speak it, on their own host.
const fetchAvailableModels = ":fetchAvailableModels"

// ListedModel is one model id a provider reported, with whatever it said about
// capabilities. Providers that publish nothing leave the flags nil.
type ListedModel struct {
	ID     string
	Vision bool // accepts images
	Think  bool // supports reasoning / thinking
}

// CodeAssistModels lists the models a Code Assist account may call.
//
// The answer is the served side, the same ids the catalog maps a public name
// onto, so callers must reverse-map before storing. The payload is only the
// project: the endpoint rejects anything else ("Unknown name metadata").
func CodeAssistModels(ctx context.Context, c *http.Client, provider, token string, a Account) ([]ListedModel, error) {
	base, ua := codeAssistEndpoint(provider)
	if base == "" {
		return nil, errors.New("not a Code Assist provider")
	}
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	in := map[string]any{}
	if project := a.Extra["projectId"]; project != "" {
		in["project"] = project
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+fetchAvailableModels, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
		req.Header.Set("x-request-source", "local")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &httpError{Status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{Status: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseAvailableModels(raw), nil
}

// codeAssistEndpoint returns the host to call and the user agent it expects.
func codeAssistEndpoint(provider string) (base, ua string) {
	switch provider {
	case "antigravity":
		// AntigravityBase has no /v1internal; the adapter appends it per call.
		return AntigravityBase + "/v1internal", AntigravityUserAgent
	case "gemini-cli":
		// GeminiCLIBase already ends in /v1internal.
		return GeminiCLIBase, "GeminiCLI/" + geminiCLIVersion
	}
	return "", ""
}

// parseAvailableModels reads both shapes the API has returned: a map keyed by
// model id (with capability flags), and a list of objects carrying one.
func parseAvailableModels(raw []byte) []ListedModel {
	var asMap struct {
		Models map[string]struct {
			SupportsImages   *bool `json:"supportsImages"`
			SupportsThinking *bool `json:"supportsThinking"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &asMap) == nil && len(asMap.Models) > 0 {
		out := make([]ListedModel, 0, len(asMap.Models))
		for id, m := range asMap.Models {
			id = strings.TrimPrefix(strings.TrimSpace(id), "models/")
			if id == "" {
				continue
			}
			lm := ListedModel{ID: id}
			if m.SupportsImages != nil {
				lm.Vision = *m.SupportsImages
			}
			if m.SupportsThinking != nil {
				lm.Think = *m.SupportsThinking
			}
			out = append(out, lm)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	var asList struct {
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &asList) == nil {
		out := make([]ListedModel, 0, len(asList.Models))
		for _, m := range asList.Models {
			id := firstNonEmpty(strings.TrimSpace(m.ID), strings.TrimPrefix(strings.TrimSpace(m.Name), "models/"))
			if id != "" {
				out = append(out, ListedModel{ID: id})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	return nil
}
