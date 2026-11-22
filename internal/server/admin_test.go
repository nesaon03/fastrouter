package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nesaon03/fastrouter/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// adminSession opens a dashboard session (keyless when password is "") and
// returns the headers a dashboard write needs.
func adminSession(t *testing.T, base, password string) map[string]string {
	t.Helper()
	var resp *http.Response
	var err error
	if password == "" {
		resp, err = http.Get(base + "/api/auth/session")
	} else {
		resp, err = http.Post(base+"/api/auth/login", "application/json", strings.NewReader(`{"password":"`+password+`"}`))
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Authenticated bool
		CSRF          string
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cookie = c.Name + "=" + c.Value
		}
	}
	if out.CSRF == "" || cookie == "" {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	return map[string]string{"Cookie": cookie, "X-CSRF-Token": out.CSRF, "Content-Type": "application/json", "Authorization": ""}
}

func adminState(t *testing.T, base string, hdr map[string]string) (map[string]any, string) {
	t.Helper()
	resp, body := do(t, "GET", base+"/api/admin/state", "", map[string]string{"Cookie": hdr["Cookie"], "Authorization": ""})
	if resp.StatusCode != 200 {
		t.Fatalf("state: %d %s", resp.StatusCode, body)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	return m, body
}

func passwordServer(t *testing.T, pw string) (*Server, *httptest.Server) {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap := &store.Snapshot{Settings: map[string]json.RawMessage{}, DashboardPassword: string(h),
		Connections: []store.Connection{{ID: "d1", Provider: "deepseek", Name: "someone@example.com", AuthType: "apikey", Priority: 1, Active: true, Status: "ok",
			Secrets: map[string]string{"apiKey": "sk-deepseek-SECRET-0001"}, Meta: map[string]any{}}},
		APIKeys:  []store.APIKey{{ID: "k", Name: "k", Key: localKey, Active: true}},
		Warnings: []store.Warning{{Kind: "skipped", Subject: "codex", Message: "account someone@example.com needs a sign-in"}}}
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	s := New(testConfig(), snap, st)
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() { hs.Close(); s.Close(); st.Close() })
	return s, hs
}

func TestAdminGuards(t *testing.T) {
	_, hs := passwordServer(t, "correct horse")
	u := hs.URL + "/api/admin/combos"
	body := `{"name":"c1","models":["ds/deepseek-v4.1-flash"]}`
	if r, _ := do(t, "POST", u, body, map[string]string{"Content-Type": "application/json"}); r.StatusCode != 401 {
		t.Fatalf("no session: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", hs.URL+"/api/admin/state", "", map[string]string{"Authorization": ""}); r.StatusCode != 401 {
		t.Fatalf("state without login: %d", r.StatusCode)
	}
	hdr := adminSession(t, hs.URL, "correct horse")
	noCSRF := map[string]string{"Cookie": hdr["Cookie"], "Content-Type": "application/json"}
	if r, _ := do(t, "POST", u, body, noCSRF); r.StatusCode != 403 {
		t.Fatalf("no csrf: %d", r.StatusCode)
	}
	if r, _ := do(t, "POST", u, body, map[string]string{"Cookie": hdr["Cookie"], "X-CSRF-Token": "nope", "Content-Type": "application/json"}); r.StatusCode != 403 {
		t.Fatalf("wrong csrf: %d", r.StatusCode)
	}
	cross := map[string]string{"Origin": "https://evil.example"}
	for k, v := range hdr {
		cross[k] = v
	}
	if r, _ := do(t, "POST", u, body, cross); r.StatusCode != 403 {
		t.Fatalf("cross-origin: %d", r.StatusCode)
	}
	form := map[string]string{"Content-Type": "text/plain"}
	for k, v := range hdr {
		if k != "Content-Type" {
			form[k] = v
		}
	}
	if r, _ := do(t, "POST", u, body, form); r.StatusCode != 415 {
		t.Fatalf("content type: %d", r.StatusCode)
	}
	if r, b := do(t, "POST", u, `{"name":"c1","models":["x"],"extra":1}`, hdr); r.StatusCode != 400 {
		t.Fatalf("unknown field: %d %s", r.StatusCode, b)
	}
	if r, _ := do(t, "POST", u, body, hdr); r.StatusCode != 200 {
		t.Fatalf("valid write: %d", r.StatusCode)
	}
	// Logout ends the session.
	do(t, "POST", hs.URL+"/api/auth/logout", `{}`, hdr)
	if r, _ := do(t, "POST", u, `{"name":"c2","models":["x"]}`, hdr); r.StatusCode != 401 {
		t.Fatalf("after logout: %d", r.StatusCode)
	}
}

func TestAdminLoginLimiter(t *testing.T) {
	_, hs := passwordServer(t, "correct horse")
	var last int
	for i := 0; i < 6; i++ {
		r, _ := do(t, "POST", hs.URL+"/api/auth/login", `{"password":"wrong"}`, map[string]string{"Content-Type": "application/json"})
		last = r.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("want lockout, got %d", last)
	}
	if r, _ := do(t, "POST", hs.URL+"/api/auth/login", `{"password":"correct horse"}`, map[string]string{"Content-Type": "application/json"}); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("locked out even with the right password: %d", r.StatusCode)
	}
}

func TestAdminStateNeverLeaksSecrets(t *testing.T) {
	_, hs := passwordServer(t, "correct horse")
	hdr := adminSession(t, hs.URL, "correct horse")
	_, body := adminState(t, hs.URL, hdr)
	for _, bad := range []string{"sk-deepseek-SECRET-0001", localKey, "someone@example.com", "$2a$"} {
		if strings.Contains(body, bad) {
			t.Fatalf("state leaks %q", bad)
		}
	}
	mustContain(t, body, `"keyHint":"••••0001"`, `so***@example.com`)
	resp, sbody := do(t, "GET", hs.URL+"/api/status", "", map[string]string{"Cookie": hdr["Cookie"], "Authorization": ""})
	if resp.StatusCode != 200 || strings.Contains(sbody, "someone@example.com") {
		t.Fatalf("status leaks email: %d", resp.StatusCode)
	}
}

func TestAdminCRUDAppliesLive(t *testing.T) {
	s, hs := passwordServer(t, "correct horse")
	hdr := adminSession(t, hs.URL, "correct horse")
	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		r, b := do(t, method, hs.URL+path, body, hdr)
		if r.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, r.StatusCode, b)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(b), &m)
		return m
	}
	// Account: create, edit (empty key keeps the old one), reorder, delete.
	acct := call("POST", "/api/admin/accounts", `{"provider":"deepseek","name":"backup","apiKey":"sk-deepseek-SECRET-0002"}`, 200)["id"].(string)
	call("POST", "/api/admin/accounts", `{"provider":"nope","name":"x","apiKey":"sk-123456789012"}`, 400)
	call("POST", "/api/admin/accounts", `{"provider":"codex","name":"x","apiKey":"sk-123456789012"}`, 400)
	call("PATCH", "/api/admin/accounts/"+acct, `{"name":"renamed","apiKey":""}`, 200)
	call("PATCH", "/api/admin/accounts/missing", `{"name":"x"}`, 404)
	call("POST", "/api/admin/accounts/reorder", `{"provider":"deepseek","ids":["`+acct+`","d1"]}`, 200)
	p, _ := s.state.Load().Provider("deepseek")
	if accts := p.Pool.Accounts(); len(accts) != 2 || accts[0].ID != acct {
		t.Fatalf("reorder not live: %+v", accts)
	}
	for _, c := range s.state.Load().Snap.Connections {
		if c.ID == acct && (c.Secrets["apiKey"] != "sk-deepseek-SECRET-0002" || c.Name != "renamed") {
			t.Fatal("edit lost the key or name")
		}
	}
	// The usage stats survive a rewrite.
	if _, err := s.st.RawDB().Exec(`UPDATE connections SET uses=7, last_status=200, last_used_at=? WHERE id='d1'`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	call("PATCH", "/api/admin/accounts/"+acct, `{"name":"again"}`, 200)
	if st, _ := s.st.ConnectionStats(); st["d1"].Uses != 7 || st["d1"].LastStatus != 200 {
		t.Fatalf("stats lost: %+v", st["d1"])
	}
	call("DELETE", "/api/admin/accounts/"+acct, "", 200)

	// Compatible endpoint plus an account behind it.
	node := call("POST", "/api/admin/nodes", `{"type":"openai-compatible","name":"GW","prefix":"gw","baseUrl":"https://gw.example/v1","apiType":"chat"}`, 200)["id"].(string)
	call("POST", "/api/admin/nodes", `{"type":"openai-compatible","name":"GW2","prefix":"gw","baseUrl":"https://gw.example/v1"}`, 400)
	call("POST", "/api/admin/nodes", `{"type":"openai-compatible","name":"GW3","prefix":"g3","baseUrl":"file:///etc/passwd"}`, 400)
	call("POST", "/api/admin/accounts", `{"provider":"`+node+`","name":"gw key","apiKey":"gw-secret-123456"}`, 200)
	if _, ok := s.state.Load().Provider("gw"); !ok {
		t.Fatal("node not routable")
	}
	call("DELETE", "/api/admin/nodes/"+node, "", 200)
	if _, ok := s.state.Load().Provider("gw"); ok {
		t.Fatal("node still routable")
	}

	// Combo and key.
	combo := call("POST", "/api/admin/combos", `{"name":"fast","strategy":"round-robin","models":["ds/deepseek-v4.1-flash"]}`, 200)["id"].(string)
	call("POST", "/api/admin/combos", `{"name":"bad name!","models":["x"]}`, 400)
	call("PATCH", "/api/admin/combos/"+combo, `{"models":["ds/deepseek-v4-pro","ds/deepseek-v4.1-flash"]}`, 200)
	key := call("POST", "/api/admin/keys", `{"name":"laptop","key":"sk-fr-0123456789abcdef0123456789abcdef","restricted":true,"allow":["fast"]}`, 200)["id"].(string)
	call("POST", "/api/admin/keys", `{"name":"short","key":"abc"}`, 400)
	call("PATCH", "/api/admin/keys/"+key, `{"key":"sk-fr-ffffffffffffffffffffffffffffffff"}`, 400)
	_, body := adminState(t, hs.URL, hdr)
	if strings.Contains(body, "0123456789abcdef0123456789abcdef") {
		t.Fatal("state leaks the new key")
	}
	mustContain(t, body, `"name":"fast"`, `"ds/deepseek-v4-pro"`, `"hint":"••••cdef"`)
	call("DELETE", "/api/admin/combos/"+combo, "", 200)
	call("DELETE", "/api/admin/keys/"+key, "", 200)

	// Settings PATCH merges and applies live.
	call("PATCH", "/api/admin/settings", `{"cavemanEnabled":true,"cavemanLevel":"ultra","providerThinking":{"deepseek":"high"},"providerStrategies":{"deepseek":{"fallbackStrategy":"round-robin"}}}`, 200)
	call("PATCH", "/api/admin/settings", `{"cavemanLevel":"extreme"}`, 400)
	call("PATCH", "/api/admin/settings", `{"providerThinking":{"deepseek":"loud"}}`, 400)
	call("PATCH", "/api/admin/settings", `{"headroomUrl":"javascript:alert(1)"}`, 400)
	st, _ := adminState(t, hs.URL, hdr)
	set := st["settings"].(map[string]any)
	if set["cavemanEnabled"] != true || set["cavemanLevel"] != "ultra" || set["providerThinking"].(map[string]any)["deepseek"] != "high" {
		t.Fatalf("settings: %v", set)
	}
	if string(s.state.Load().Snap.Settings["cavemanLevel"]) != `"ultra"` {
		t.Fatal("settings not live")
	}
	back, _ := s.st.Load()
	if string(back.Settings["cavemanLevel"]) != `"ultra"` {
		t.Fatal("settings not persisted")
	}

	// Disabled models drop out of the provider's model list and the picker.
	call("PATCH", "/api/admin/settings", `{"disabledModels":{"deepseek":["deepseek-chat"]}}`, 200)
	call("PATCH", "/api/admin/settings", `{"disabledModels":{"nope":["x"]}}`, 400)
	pm := call("GET", "/api/admin/providers/deepseek/models", "", 200)
	found := false
	for _, m := range pm["models"].([]any) {
		if mm := m.(map[string]any); mm["id"] == "deepseek-chat" {
			found = mm["disabled"] == true
		}
	}
	if !found || pm["thinking"] != "high" || pm["prefix"] == "" {
		t.Fatalf("provider models: %v", pm)
	}
	call("GET", "/api/admin/providers/nope/models", "", 404)
}

func TestDashboardAssetsServedWithCSP(t *testing.T) {
	_, hs := newTestServer(t, &store.Snapshot{})
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := noRedirect.Get(hs.URL + "/dashboard")
	if err != nil || r.StatusCode != http.StatusMovedPermanently || r.Header.Get("Location") != "/dashboard/" {
		t.Fatalf("redirect: %v %v", err, r)
	}
	for _, p := range []string{"/dashboard/", "/dashboard/app.js", "/dashboard/app.css", "/dashboard/vendor/preact-htm.js", "/dashboard/logos/deepseek.png", "/dashboard/NOTICE.txt"} {
		r, err := http.Get(hs.URL + p)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, err, r)
		}
		r.Body.Close()
		if !strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src 'self'") || r.Header.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s: missing security headers", p)
		}
	}
	if r, _ := http.Get(hs.URL + "/dashboard/../go.mod"); r.StatusCode == 200 {
		t.Fatal("path escape")
	}
}

func TestEnsureDefaultPassword(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	set, err := s.EnsureDefaultPassword()
	if err != nil || !set {
		t.Fatalf("ensure: set=%v err=%v", set, err)
	}
	sn := s.state.Load().Snap
	if bcrypt.CompareHashAndPassword([]byte(sn.DashboardPassword), []byte("1234")) != nil {
		t.Fatalf("hash does not match 1234")
	}
	if !defaultPasswordActive(sn) {
		t.Fatal("default password flag not set")
	}
	if set, _ := s.EnsureDefaultPassword(); set {
		t.Fatal("default password set twice")
	}
	// Clearing the password later is a deliberate choice, not a fresh install.
	if err := s.mutate(func(sn *store.Snapshot) error { sn.DashboardPassword = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if set, _ := s.EnsureDefaultPassword(); set {
		t.Fatal("cleared password was reset to the default")
	}
	if s.state.Load().Snap.DashboardPassword != "" {
		t.Fatal("password changed after clear")
	}
}

func TestEnsureDefaultPasswordKeepsImported(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	h, _ := bcrypt.GenerateFromPassword([]byte("imported-password"), bcrypt.MinCost)
	if err := s.mutate(func(sn *store.Snapshot) error { sn.DashboardPassword = string(h); return nil }); err != nil {
		t.Fatal(err)
	}
	set, err := s.EnsureDefaultPassword()
	if err != nil || set {
		t.Fatalf("ensure: set=%v err=%v", set, err)
	}
	sn := s.state.Load().Snap
	if bcrypt.CompareHashAndPassword([]byte(sn.DashboardPassword), []byte("imported-password")) != nil {
		t.Fatal("imported password was replaced")
	}
	if defaultPasswordActive(sn) {
		t.Fatal("imported password marked as default")
	}
}

func TestDefaultPasswordLoginAndChange(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	if set, err := s.EnsureDefaultPassword(); err != nil || !set {
		t.Fatalf("ensure: set=%v err=%v", set, err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	hdr := adminSession(t, hs.URL, "1234")
	_, body := do(t, "GET", hs.URL+"/api/auth/session", "", hdr)
	var sess map[string]any
	_ = json.Unmarshal([]byte(body), &sess)
	if sess["defaultPassword"] != true {
		t.Fatalf("session default flag = %v", sess["defaultPassword"])
	}
	resp, out := do(t, "POST", hs.URL+"/api/auth/password", `{"current":"1234","new":"abcd"}`, hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("change: %d %s", resp.StatusCode, out)
	}
	if r, _ := do(t, "POST", hs.URL+"/api/auth/login", `{"password":"1234"}`, map[string]string{"Content-Type": "application/json"}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still works: %d", r.StatusCode)
	}
	hdr = adminSession(t, hs.URL, "abcd")
	_, body = do(t, "GET", hs.URL+"/api/auth/session", "", hdr)
	sess = map[string]any{}
	_ = json.Unmarshal([]byte(body), &sess)
	if sess["defaultPassword"] != false {
		t.Fatalf("default flag after change = %v", sess["defaultPassword"])
	}
}
