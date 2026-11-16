package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIToolsApplyResolvesKeyServerSide(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	s, hs := passwordServer(t, "pw")
	s.dataDir, s.port = t.TempDir(), 20129
	hdr := adminSession(t, hs.URL, "pw")
	read := map[string]string{"Cookie": hdr["Cookie"], "Authorization": ""}

	r, body := do(t, "GET", hs.URL+"/api/admin/cli-tools", "", read)
	if r.StatusCode != 200 || strings.Contains(body, localKey) || !strings.Contains(body, `"id":"hermes"`) {
		t.Fatalf("list: %d", r.StatusCode)
	}
	u := hs.URL + "/api/admin/cli-tools/claude/apply"
	for _, bad := range []string{
		`{"endpoint":"http://evil.example","keyId":"k","model":"m"}`,
		`{"endpoint":"http://127.0.0.1:20129","keyId":"nope","model":"m"}`,
		`{"endpoint":"http://127.0.0.1:20129","model":"m"}`, // keys exist: one must be chosen
	} {
		if r, _ := do(t, "POST", u, bad, hdr); r.StatusCode != 400 {
			t.Fatalf("accepted %s: %d", bad, r.StatusCode)
		}
	}
	if r, _ := do(t, "POST", u, `{"endpoint":"http://127.0.0.1:20129","keyId":"k","model":"m"}`, read); r.StatusCode != 403 {
		t.Fatalf("no csrf: %d", r.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Fatal("rejected requests wrote files")
	}
	r, body = do(t, "POST", u, `{"endpoint":"http://127.0.0.1:20129","keyId":"k","model":"cc/claude-sonnet-5"}`, hdr)
	if r.StatusCode != 200 || strings.Contains(body, localKey) {
		t.Fatalf("apply: %d %s", r.StatusCode, body)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(string(b), localKey) || !strings.Contains(string(b), `"ANTHROPIC_BASE_URL": "http://127.0.0.1:20129"`) {
		t.Fatal("settings.json not written as expected")
	}
	if r, body := do(t, "POST", hs.URL+"/api/admin/cli-tools/claude/reset", `{}`, hdr); r.StatusCode != 200 || !strings.Contains(body, "cli-backups") {
		t.Fatalf("reset: %d", r.StatusCode)
	}
	if r, _ := do(t, "POST", hs.URL+"/api/admin/cli-tools/cursor/apply", `{"endpoint":"http://127.0.0.1:20129","keyId":"k","model":"m"}`, hdr); r.StatusCode != 400 {
		t.Fatalf("guide applied: %d", r.StatusCode)
	}
}
