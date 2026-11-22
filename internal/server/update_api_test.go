package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The update endpoint reports the latest published version and the command.
func TestUpdateEndpoint(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"version":"9.9.9"}`))
	}))
	defer srv.Close()

	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	s.updates.URL = srv.URL
	rec, got := adminCall(t, s, http.MethodGet, "/api/update", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got["latest"] != "9.9.9" || got["updateAvailable"] != true || got["command"] != "npm i -g @zenpo/fastrouter@latest" {
		t.Fatalf("%v", got)
	}
	// A second read is served from the checker's cache.
	adminCall(t, s, http.MethodGet, "/api/update", nil)
	if calls != 1 {
		t.Fatalf("registry calls = %d, want 1", calls)
	}
}

// package.json and internal/server.Version must be released together: an npm
// package whose version differs would download a binary that does not exist.
func TestPackageVersionMatchesServer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "package.json"))
	if err != nil {
		t.Skip("no package.json in this checkout")
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Version != Version {
		t.Fatalf("package.json version %q != server.Version %q; bump both", pkg.Version, Version)
	}
}
