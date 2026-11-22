package server

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nesaon03/fastrouter/internal/logring"
	"github.com/nesaon03/fastrouter/internal/store"
)

func TestAdminComboStrategies(t *testing.T) {
	_, hs := passwordServer(t, "pw-123456")
	hdr := adminSession(t, hs.URL, "pw-123456")
	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		resp, out := do(t, method, hs.URL+path, body, hdr)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, out)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(out), &m)
		return m
	}
	call("POST", "/api/admin/combos", `{"name":"x","strategy":"weighted"}`, 400)
	call("POST", "/api/admin/combos", `{"name":"x","strategy":"smart","smartPolicy":"sometimes"}`, 400)
	call("POST", "/api/admin/combos", `{"name":"x","strategy":"smart","confidenceThreshold":7}`, 400)
	id := call("POST", "/api/admin/combos", `{"name":"brain","strategy":"smart","models":["ds/deepseek-chat"],"smartPolicy":"per-turn","routerModel":"ds/deepseek-chat","confidenceThreshold":0.7}`, 200)["id"].(string)
	call("POST", "/api/admin/combos", `{"name":"panel","strategy":"fusion","models":["ds/a","ds/b"],"judgeModel":"ds/deepseek-chat"}`, 200)
	st, _ := adminState(t, hs.URL, hdr)
	var brain map[string]any
	for _, c := range st["combos"].([]any) {
		if c.(map[string]any)["name"] == "brain" {
			brain = c.(map[string]any)
		}
	}
	if brain["strategy"] != "smart" || brain["smartPolicy"] != "per-turn" || brain["routerModel"] != "ds/deepseek-chat" || brain["confidenceThreshold"] != 0.7 {
		t.Fatalf("%v", brain)
	}
	// Rename moves the options; delete prunes them.
	call("PATCH", "/api/admin/combos/"+id, `{"name":"brain2"}`, 200)
	_, body := adminState(t, hs.URL, hdr)
	if !strings.Contains(body, `"name":"brain2","kind":"","strategy":"smart","models":["ds/deepseek-chat"],"smartPolicy":"per-turn"`) {
		t.Fatalf("rename lost options: %s", body)
	}
	call("DELETE", "/api/admin/combos/"+id, "", 200)
	_, body = adminState(t, hs.URL, hdr)
	if strings.Contains(body, "per-turn") {
		t.Fatal("options not pruned")
	}
}

func TestUsageEndpoints(t *testing.T) {
	s, hs := passwordServer(t, "pw-123456")
	hdr := adminSession(t, hs.URL, "pw-123456")
	now := time.Now()
	_ = s.st.WriteUsage([]store.UsageEvent{
		{At: now.Add(-2 * time.Hour), ConnectionID: "d1", Provider: "deepseek", Model: "deepseek-chat", Status: 200, Input: 100, Output: 20, Cost: 0.01},
		{At: now.Add(-3 * 24 * time.Hour), ConnectionID: "d1", Provider: "deepseek", Model: "deepseek-chat", Status: 500, Input: 10, Output: 0},
	})
	get := func(path string) map[string]any {
		resp, out := do(t, "GET", hs.URL+path, "", map[string]string{"Cookie": hdr["Cookie"], "Authorization": ""})
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		if strings.Contains(out, "someone@example.com") {
			t.Fatal("unmasked account email")
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(out), &m)
		return m
	}
	d := get("/api/usage?period=24h")
	if len(d["series"].([]any)) < 24 || len(d["recent"].([]any)) != 2 || len(d["providers"].([]any)) != 1 {
		t.Fatalf("24h: %v", d)
	}
	if d := get("/api/usage?period=7d"); d["providers"].([]any)[0].(map[string]any)["requests"] != 2.0 || len(d["series"].([]any)) < 7 {
		t.Fatalf("7d: %v", d)
	}
	r := get("/api/usage/requests?period=all&limit=1&offset=1&provider=deepseek")
	if r["total"] != 2.0 || len(r["requests"].([]any)) != 1 || r["requests"].([]any)[0].(map[string]any)["status"] != 500.0 {
		t.Fatalf("requests: %v", r)
	}
}

func TestLogStream(t *testing.T) {
	_, hs := passwordServer(t, "pw-123456")
	if resp, _ := do(t, "GET", hs.URL+"/api/admin/logs/stream", "", map[string]string{"Authorization": ""}); resp.StatusCode != 401 {
		t.Fatalf("anonymous stream: %d", resp.StatusCode)
	}
	hdr := adminSession(t, hs.URL, "pw-123456")
	prev := log.Writer()
	log.SetOutput(logring.Default)
	defer log.SetOutput(prev)
	req, _ := http.NewRequest("GET", hs.URL+"/api/admin/logs/stream", nil)
	req.Header.Set("Cookie", hdr["Cookie"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	next := func() string {
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: ") {
				ev := strings.TrimPrefix(sc.Text(), "event: ")
				sc.Scan()
				return ev + " " + sc.Text()
			}
		}
		return ""
	}
	if ev := next(); !strings.HasPrefix(ev, "init ") {
		t.Fatal(ev)
	}
	log.Printf("upstream failed with api_key=sk-test-123456789012345")
	if ev := next(); !strings.HasPrefix(ev, "line ") || strings.Contains(ev, "sk-test") || !strings.Contains(ev, `"level":"error"`) {
		t.Fatal(ev)
	}
	if resp, _ := do(t, "DELETE", hs.URL+"/api/admin/logs", "", map[string]string{"Cookie": hdr["Cookie"], "Authorization": ""}); resp.StatusCode != 403 {
		t.Fatalf("clear without CSRF: %d", resp.StatusCode)
	}
	do(t, "DELETE", hs.URL+"/api/admin/logs", "", hdr)
	if ev := next(); !strings.HasPrefix(ev, "clear ") {
		t.Fatal(ev)
	}
}
