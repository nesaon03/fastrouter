package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zenpo/fastrouter/internal/store"
)

// redirect sends every outbound request to a local test server.
type redirect struct{ to *url.URL }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestFetchQuotaCodexAndErrors(t *testing.T) {
	var gotAuth, gotAcct string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAcct = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID")
		if r.URL.Path == "/user/balance" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"secret":"sk-should-not-leak"}`))
			return
		}
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":62,"reset_after_seconds":3600},"secondary_window":{"used_percent":"31","reset_at":1900000000}}}`))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	s, _ := newTestServer(t, &store.Snapshot{})
	s.client = &http.Client{Transport: redirect{u}}

	res := s.fetchQuota(context.Background(), quotaCred{provider: "codex", key: "tok", accountID: "acc"})
	if res.Error != "" || res.Plan != "plus" || len(res.Quotas) != 2 {
		t.Fatalf("codex: %+v", res)
	}
	if gotAuth != "Bearer tok" || gotAcct != "acc" {
		t.Fatalf("headers: %q %q", gotAuth, gotAcct)
	}
	if q := res.Quotas[0]; q.Used != 62 || q.RemainingPct != 38 || q.ResetAt == "" {
		t.Fatalf("session row: %+v", q)
	}
	if q := res.Quotas[1]; q.Used != 31 || q.ResetAt != "2030-03-17T17:46:40Z" {
		t.Fatalf("weekly row: %+v", q)
	}

	res = s.fetchQuota(context.Background(), quotaCred{provider: "deepseek", key: "k"})
	if !strings.Contains(res.Error, "401") || strings.Contains(res.Error, "sk-") {
		t.Fatalf("deepseek 401: %q", res.Error)
	}
	if res = s.fetchQuota(context.Background(), quotaCred{provider: "codex"}); res.Error == "" {
		t.Fatal("missing credential should error")
	}
}

func TestSkillServedLocally(t *testing.T) {
	_, hs := newTestServer(t, &store.Snapshot{})
	resp, body := do(t, "GET", hs.URL+"/skills/fastrouter/SKILL.md", "", map[string]string{"Authorization": ""})
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("entry skill: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	host := strings.TrimPrefix(hs.URL, "http://")
	if !strings.Contains(body, `"http://`+host+`"`) || strings.Contains(body, "localhost:20128") ||
		!strings.Contains(body, "http://"+host+"/skills/fastrouter-chat/SKILL.md") || strings.Contains(body, "raw.githubusercontent.com") {
		t.Fatalf("base URL not rewritten to %s", host)
	}
	for _, p := range []string{"/skills/nope/SKILL.md", "/skills/..%2Fgo.mod/SKILL.md", "/skills/fastrouter/other.md"} {
		if resp, _ := do(t, "GET", hs.URL+p, "", map[string]string{"Authorization": ""}); resp.StatusCode != 404 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestRound3AdminEndpointsNeedSession(t *testing.T) {
	_, hs := passwordServer(t, "pw-123456")
	for _, c := range [][2]string{{"POST", "/api/admin/providers/test"}, {"GET", "/api/admin/quota"}, {"GET", "/api/admin/headroom"}, {"POST", "/api/admin/headroom/install"}} {
		if resp, _ := do(t, c[0], hs.URL+c[1], `{"category":"all"}`, map[string]string{"Authorization": "", "Content-Type": "application/json"}); resp.StatusCode != 401 {
			t.Fatalf("%s %s without session: %d", c[0], c[1], resp.StatusCode)
		}
	}
	hdr := adminSession(t, hs.URL, "pw-123456")
	if resp, body := do(t, "POST", hs.URL+"/api/admin/providers/test", `{"category":"custom"}`, hdr); resp.StatusCode != 200 {
		t.Fatalf("provider test with session: %d %s", resp.StatusCode, body)
	}
}
