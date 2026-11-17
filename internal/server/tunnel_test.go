package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zenpo/fastrouter/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func get(t *testing.T, url string, hdr map[string]string, pass string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if pass != "" {
		req.SetBasicAuth("u", pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestTunnelOpenModeNeedsKey(t *testing.T) {
	s := New(testConfig(), &store.Snapshot{Settings: map[string]json.RawMessage{}}, nil)
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() { hs.Close(); s.Close() })
	if c := get(t, hs.URL+"/v1/models", nil, ""); c != 200 {
		t.Fatalf("local open mode: %d", c)
	}
	for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "X-Forwarded-For", "Tailscale-User-Login", "Tailscale-Funnel-Request"} {
		if c := get(t, hs.URL+"/v1/models", map[string]string{h: "1.2.3.4"}, ""); c != 403 {
			t.Errorf("%s without key: %d, want 403", h, c)
		}
	}
	if err := s.exposeReady(); err == nil {
		t.Fatal("exposing with no key and no password must be refused")
	}
}

func TestTunnelKeepsDashboardLocal(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	s, hs := newTestServer(t, &store.Snapshot{DashboardPassword: string(hash)})
	cf := map[string]string{"Cf-Connecting-Ip": "1.2.3.4"}
	if c := get(t, hs.URL+"/api/status", nil, "pw"); c != 200 {
		t.Fatalf("local dashboard: %d", c)
	}
	for _, p := range []string{"/api/status", "/dashboard/", "/api/auth/session", "/"} {
		if c := get(t, hs.URL+p, cf, "pw"); c != 403 {
			t.Errorf("%s over tunnel: %d, want 403", p, c)
		}
	}
	if c := get(t, hs.URL+"/api/health", cf, ""); c != 200 {
		t.Errorf("health over tunnel: %d", c)
	}
	req, _ := http.NewRequest("GET", hs.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+localKey)
	req.Header.Set("Cf-Connecting-Ip", "1.2.3.4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("API with key over tunnel: %d", resp.StatusCode)
	}
	if err := s.exposeReady(); err != nil {
		t.Fatalf("key + password should allow exposing: %v", err)
	}
	if err := s.setSettingBool("remoteDashboard", true); err != nil {
		t.Fatal(err)
	}
	if c := get(t, hs.URL+"/api/status", cf, "pw"); c != 200 {
		t.Errorf("remote dashboard on, with password: %d", c)
	}
	if c := get(t, hs.URL+"/api/status", cf, ""); c != 401 {
		t.Errorf("remote dashboard on, no password: %d, want 401", c)
	}
}
