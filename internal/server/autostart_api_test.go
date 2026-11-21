package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The tag the savers return is the only honest source for "Running": a switch
// that is on but has seen no traffic must not read as running.
func TestSaverRunsFromTag(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	s.noteSaverTag("RTK:2,HEADROOM,CAVEMAN:ultra,PONYTAIL:ultra")
	s.noteSaverTag("RTK:1")

	v := s.saverRunsView()
	for _, name := range []string{"rtk", "headroom", "caveman", "ponytail"} {
		entry, ok := v[name].(map[string]any)
		if !ok {
			t.Fatalf("%s missing from view", name)
		}
		if running, _ := entry["running"].(bool); !running {
			t.Fatalf("%s should read as running right after it ran", name)
		}
	}
	if got := v["rtk"].(map[string]any)["count"]; got != int64(2) {
		t.Fatalf("rtk count = %v, want 2", got)
	}
	if got := v["customprompt"].(map[string]any)["running"]; got != false {
		t.Fatal("a saver that never ran must not read as running")
	}
	if _, ok := v["customprompt"].(map[string]any)["last"]; ok {
		t.Fatal("never-run saver should not report a last time")
	}
}

// An unknown tag component is ignored rather than recorded under a wrong name.
func TestSaverTagUnknownName(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	s.noteSaverTag("NOPE:1,RTK:1")
	if got := s.saverRunsView()["rtk"].(map[string]any)["count"]; got != int64(1) {
		t.Fatalf("rtk count = %v", got)
	}
	if got := s.saverRunsView()["headroom"].(map[string]any)["count"]; got != int64(0) {
		t.Fatalf("headroom should be untouched, got %v", got)
	}
}

// A saver that ran hours ago is idle, not running.
func TestSaverRunsGoIdle(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	s.saverRuns.rtk.note(time.Now().Add(-10 * time.Minute))
	entry := s.saverRunsView()["rtk"].(map[string]any)
	if running, _ := entry["running"].(bool); running {
		t.Fatal("ten minutes ago is not running")
	}
	if entry["count"] != int64(1) {
		t.Fatalf("count %v", entry["count"])
	}
}

// The login entry must run the same config and port this process is using,
// otherwise a reboot silently comes back on defaults.
func TestAutostartServeArgs(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	s.cfgPath = `C:\Users\x\.fastrouter\fastrouter.json`
	s.port = 20129
	s.host = "127.0.0.1"
	args := strings.Join(s.serveArgs("ignored"), " ")
	if !strings.Contains(args, "serve") || !strings.Contains(args, s.cfgPath) {
		t.Fatalf("args: %s", args)
	}
	if !strings.Contains(args, "--port 20129") {
		t.Fatalf("non-default port missing: %s", args)
	}
	if strings.Contains(args, "--host") {
		t.Fatalf("loopback host should not be written: %s", args)
	}
	// The default port is the built-in one: no flag needed.
	s.port = 20128
	if strings.Contains(strings.Join(s.serveArgs(""), " "), "--port") {
		t.Fatal("default port should not be written")
	}
}

// The Settings page reads this shape, so it must always answer with the keys it
// renders, on every OS.
func TestAutostartStateShape(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	st := s.autostartState()
	for _, k := range []string{"supported", "installed", "path"} {
		if _, ok := st[k]; !ok {
			t.Fatalf("missing %q in %v", k, st)
		}
	}
	if _, ok := st["installed"].(bool); !ok {
		t.Fatal("installed must be a bool")
	}
}

// /api/admin/state carries both additions, so the dashboard needs one fetch.
func TestAdminStateHasSaversAndAutostart(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	rec, got := adminCall(t, s, http.MethodGet, "/api/admin/state", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("state: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := got["savers"].(map[string]any); !ok {
		t.Fatal("state has no savers")
	}
	if _, ok := got["autostart"].(map[string]any); !ok {
		t.Fatal("state has no autostart")
	}
}
