package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// adminCall drives one authenticated dashboard write against the server's mux,
// using the same cookie + CSRF path the browser takes.
func adminCall(t *testing.T, s *Server, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	h := s.Handler()
	// Session: with no dashboard password, loopback is trusted, so only the CSRF
	// token has to come from the session endpoint.
	rec := httptest.NewRecorder()
	sessReq := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	sessReq.Host = "127.0.0.1:20128"      // keyless loopback trust, as the browser gets
	sessReq.RemoteAddr = "127.0.0.1:5555" // loopback client
	h.ServeHTTP(rec, sessReq)
	var sess struct {
		CSRF          string `json:"csrf"`
		Authenticated bool   `json:"authenticated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	cookie := rec.Header().Get("Set-Cookie")

	var rdr *strings.Reader
	if body == nil {
		rdr = strings.NewReader("")
	} else {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Host = "127.0.0.1:20128"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "http://127.0.0.1:20128")
	req.Header.Set("X-CSRF-Token", sess.CSRF)
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	var got map[string]any
	_ = json.Unmarshal(out.Body.Bytes(), &got)
	return out, got
}

// A model added by hand is listed, routable and carries its caps.
func TestAddCustomModel(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	rec, _ := adminCall(t, s, http.MethodPost, "/api/admin/models", map[string]any{
		"provider": "fake", "id": "my-preview-1", "name": "My Preview", "type": "llm",
		"vision": true, "reasoning": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	st := s.state.Load()
	if !hasAnyModel(st.Models, "fk/my-preview-1") {
		t.Fatalf("added model not listed: %v", st.Models)
	}
	snap := st.Snap
	var caps string
	for _, m := range snap.CustomModels {
		if m.ID == "my-preview-1" {
			caps = string(m.Caps)
		}
	}
	if !strings.Contains(caps, `"vision":true`) || !strings.Contains(caps, `"reasoning":true`) {
		t.Fatalf("caps not stored: %q", caps)
	}
}

func hasAnyModel(list []string, want string) bool {
	for _, m := range list {
		if m == want {
			return true
		}
	}
	return false
}

// The same id twice is refused, and so is an id that cannot be a model name.
func TestAddCustomModelValidation(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	body := map[string]any{"provider": "fake", "id": "dup-model"}
	if rec, _ := adminCall(t, s, http.MethodPost, "/api/admin/models", body); rec.Code != http.StatusOK {
		t.Fatalf("first add: %d", rec.Code)
	}
	if rec, _ := adminCall(t, s, http.MethodPost, "/api/admin/models", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate add: %d, want 400", rec.Code)
	}
	if rec, _ := adminCall(t, s, http.MethodPost, "/api/admin/models", map[string]any{"provider": "fake", "id": "bad id!"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid id: %d, want 400", rec.Code)
	}
	if rec, _ := adminCall(t, s, http.MethodPost, "/api/admin/models", map[string]any{"provider": "nope", "id": "x"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider: %d, want 400", rec.Code)
	}
}

func TestDeleteCustomModel(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	adminCall(t, s, http.MethodPost, "/api/admin/models", map[string]any{"provider": "fake", "id": "temp-model"})
	if !hasAnyModel(s.state.Load().Models, "fk/temp-model") {
		t.Fatal("setup failed")
	}
	rec, _ := adminCall(t, s, http.MethodDelete, "/api/admin/models?provider=fake&id=temp-model", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if hasAnyModel(s.state.Load().Models, "fk/temp-model") {
		t.Fatalf("model still listed: %v", s.state.Load().Models)
	}
	if rec, _ := adminCall(t, s, http.MethodDelete, "/api/admin/models?provider=fake&id=temp-model", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d, want 404", rec.Code)
	}
}

// A discovery refresh is one call from the dashboard, and reports what is new.
func TestRefreshModelsEndpoint(t *testing.T) {
	feed, up := newFeed(t, "brand-new-1", "brand-new-2")
	s := newDiscoveryServer(t, up.URL)
	rec, got := adminCall(t, s, http.MethodPost, "/api/admin/providers/fake/refresh-models", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	if n, _ := got["new"].(float64); n != 2 {
		t.Fatalf("new = %v, want 2", got["new"])
	}
	if len(feed.asked()) == 0 {
		t.Fatal("no list request")
	}
}
