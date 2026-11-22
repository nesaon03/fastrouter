package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestSaverPreviewEndpoint(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	in := "On branch main\nChanges not staged for commit:\n"
	for i := 0; i < 30; i++ {
		in += "\tmodified:   src/file" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".go\n"
	}
	in += "Untracked files:\n\tfoo.txt\n"
	rec, got := adminCall(t, s, http.MethodPost, "/api/admin/savers/preview", map[string]any{"text": in})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	if got["changed"] != true || got["filter"] == "" {
		t.Fatalf("preview result = %+v", got)
	}
	out, _ := got["compressed"].(string)
	if out == "" || len(out) >= len(in) || !strings.Contains(out, "Modified") {
		t.Fatalf("compressed = %q", out)
	}
}

func TestSaverSettingsAndSavingsView(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	rec, _ := adminCall(t, s, http.MethodPatch, "/api/admin/settings", map[string]any{
		"compactEnabled": true, "compactKeepToolResults": 4, "dedupeEnabled": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	snap := s.state.Load().Snap
	if v := b2(snap.Settings["compactEnabled"]); !v {
		t.Fatal("compactEnabled not saved")
	}
	if v := b2(snap.Settings["dedupeEnabled"]); !v {
		t.Fatal("dedupeEnabled not saved")
	}
	_, got := adminCall(t, s, http.MethodGet, "/api/admin/state", nil)
	sv, ok := got["savings"].(map[string]any)
	if !ok {
		t.Fatal("state has no savings")
	}
	if sv["window"] != "30d" {
		t.Fatalf("savings = %+v", sv)
	}
	settings, _ := got["settings"].(map[string]any)
	if settings["compactKeepToolResults"] != float64(4) {
		t.Fatalf("keep = %v", settings["compactKeepToolResults"])
	}
}

func b2(raw []byte) bool {
	return strings.TrimSpace(string(raw)) == "true"
}
