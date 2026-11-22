package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nesaon03/fastrouter/internal/store"
)

func TestTOSBannedIsStrict(t *testing.T) {
	tos := "This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product."
	for _, c := range []struct {
		status int
		msg    string
		want   bool
	}{
		{403, tos, true},
		{401, tos, false},
		{403, "The caller does not have permission", false},
		{403, "Gemini Code Assist API has been disabled in project x", false},
		{403, "Your account has been suspended", false},
		{429, tos, false},
	} {
		if got := tosBanned(c.status, c.msg); got != c.want {
			t.Errorf("tosBanned(%d, %q) = %v", c.status, c.msg, got)
		}
	}
}

func agBanServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap := &store.Snapshot{Settings: map[string]json.RawMessage{},
		Connections: []store.Connection{
			{ID: "ag1", Provider: "antigravity", Name: "one@x.io", Active: true, Status: "ok", Priority: 1,
				Secrets: map[string]string{"accessToken": "ya29.SECRET-one"}, Meta: map[string]any{"projectId": "p1", "email": "one@x.io"}},
		},
		APIKeys: []store.APIKey{{ID: "k", Name: "k", Key: localKey, Active: true}}}
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	s := New(testConfig(), snap, st)
	t.Cleanup(func() { s.Close(); st.Close() })
	return s, st
}

const tosBody = `{"error":{"code":403,"message":"This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.","status":"PERMISSION_DENIED"}}`

func status(t *testing.T, st *store.Store, id string) (string, string) {
	t.Helper()
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range snap.Connections {
		if c.ID == id {
			e, _ := c.Meta["lastError"].(string)
			return c.Status, e
		}
	}
	return "", ""
}

func TestAntigravityBanNeedsEveryHost(t *testing.T) {
	s, st := agBanServer(t)
	// The daily host refuses, but the production host answers: not an account ban.
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("X-Orig-Host") == "cloudcode-pa.googleapis.com" {
			_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}]}}`)
			return
		}
		w.WriteHeader(403)
		_, _ = io.WriteString(w, tosBody)
	})
	res := s.LiveTest(t.Context(), LiveOptions{Provider: "antigravity"})
	if len(res) != 1 || res[0].OK || res[0].Class == "banned" || !strings.Contains(res[0].Message, "Terms of Service") {
		t.Fatalf("%+v", res)
	}
	if got, _ := status(t, st, "ag1"); got != "ok" {
		t.Fatalf("status %q", got)
	}
}

func TestAntigravityBanMarksAndClears(t *testing.T) {
	s, st := agBanServer(t)
	var ok atomic.Bool
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		if ok.Load() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}]}}`+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, tosBody)
	})
	res := s.LiveTest(t.Context(), LiveOptions{Provider: "antigravity"})
	if len(res) != 1 || res[0].Class != "banned" {
		t.Fatalf("%+v", res)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "SECRET") {
		t.Fatalf("leak: %s", b)
	}
	if got, e := status(t, st, "ag1"); got != "banned" || !strings.Contains(e, "Terms of Service") {
		t.Fatalf("status %q %q", got, e)
	}
	// Banned accounts are out of routing (no cooldown games).
	if p, _ := s.state.Load().Provider("antigravity"); p != nil && len(p.Pool.Accounts()) != 0 {
		t.Fatal("banned account still routable")
	}
	// A manual retest that passes clears the ban.
	ok.Store(true)
	res = s.LiveTest(t.Context(), LiveOptions{Provider: "antigravity", Account: "ag1"})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("%+v", res)
	}
	if got, _ := status(t, st, "ag1"); got != "ok" {
		t.Fatalf("status %q", got)
	}
}

func TestProxyNotesBanInBackground(t *testing.T) {
	s, st := agBanServer(t)
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, tosBody)
	})
	p, _ := s.state.Load().Provider("antigravity")
	s.noteBan("antigravity", p.Pool.Accounts()[0], "This service has been disabled in this account for violation of Terms of Service.")
	for i := 0; i < 100; i++ {
		if got, _ := status(t, st, "ag1"); got == "banned" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("not marked banned")
}
