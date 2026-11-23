package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nesaon03/fastrouter/internal/config"
	"github.com/nesaon03/fastrouter/internal/store"
)

func TestSwarmJudgeTextExtraction(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"openai json", `{"choices":[{"message":{"content":"Paris"}}]}`, "Paris"},
		{"openai parts", `{"choices":[{"message":{"content":[{"type":"text","text":"Hi"}]}}]}`, "Hi"},
		{"openai sse", "data: {\"choices\":[{\"delta\":{\"content\":\"Pa\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ris\"}}]}\n\ndata: [DONE]\n\n", "Paris"},
		{"anthropic", `{"content":[{"type":"text","text":"Hello"}]}`, "Hello"},
		{"anthropic delta", `{"type":"content_block_delta","delta":{"text":"there"}}`, "there"},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"World"}]}}]}`, "World"},
		{"json containing data", `{"choices":[{"message":{"content":"the data: protocol"}}]}`, "the data: protocol"},
		{"malformed", "not json", ""},
		{"empty sse delta", "data: {\"choices\":[{\"delta\":{}}]}\n\n", ""},
	}
	for _, c := range cases {
		w := &swarmStatusWriter{body: []byte(c.body)}
		if got := w.judgeText(); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestSwarmFinishPersistsOnce(t *testing.T) {
	var tr swarmTracker
	var n int
	tr.onFinish = func(r *swarmRun) {
		n++
		if r.JudgeText != "final" || len(r.FullTexts) != 1 {
			t.Errorf("persisted run = %+v", r)
		}
	}
	id := tr.start("team", []string{"a"}, "r", 1, "local", "general", 0, true)
	tr.text(id, 1, 0, "full")
	tr.judgeText(id, "final")
	tr.finish(id, swarmDone, false, "")
	tr.finish(id, swarmError, false, "") // deferred safety net must not persist again
	if n != 1 {
		t.Fatalf("onFinish called %d times, want 1", n)
	}
}

func TestSwarmSnapshotStripsFullTranscript(t *testing.T) {
	var tr swarmTracker
	tr.onFinish = func(r *swarmRun) { /* pretend persist */ }
	id := tr.start("team", []string{"a", "b"}, "r", 2, "local", "general", 0, true)
	tr.text(id, 1, 0, strings.Repeat("full answer ", 100))
	tr.judgeText(id, "the final answer")
	tr.judge(id, swarmOK)
	tr.finish(id, swarmDone, false, "")
	snap := tr.snapshot()
	if len(snap) != 1 {
		t.Fatalf("snap = %v", snap)
	}
	if snap[0].FullTexts != nil || snap[0].JudgeText != "" {
		t.Fatalf("snapshot leaked full transcript: %+v", snap[0])
	}
	if len(snap[0].Texts[0][0]) == 0 || snap[0].JudgeState != swarmOK {
		t.Fatalf("snapshot missing live data: %+v", snap[0])
	}
}

func TestSwarmPersistAndHistory(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["key-a"] = oaText("alpha answer " + strings.Repeat("x", 80))
	sc.reply["key-b"] = oaText("beta answer " + strings.Repeat("y", 80))
	sc.reply["key-r"] = oaText("judged final answer")

	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap := swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"}, `{"judgeModel":"r/judge","rounds":2}`)
	snap.APIKeys = append(snap.APIKeys, store.APIKey{ID: "k", Name: "k", Key: localKey, Active: true})
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, loaded, st)
	defer s.Close()
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"explain X"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "judged final answer") {
		t.Fatalf("swarm: %d %s", resp.StatusCode, out)
	}

	rec, got := adminCall(t, s, http.MethodGet, "/api/admin/swarm", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("swarm api: %d %s", rec.Code, rec.Body.String())
	}
	runs, _ := got["runs"].([]any)
	hist, _ := got["history"].([]any)
	if len(runs) != 1 || len(hist) != 1 {
		t.Fatalf("runs=%v history=%v", runs, hist)
	}
	row, _ := hist[0].(map[string]any)
	id, _ := row["id"].(string)
	if id == "" || row["status"] != swarmDone {
		t.Fatalf("history row = %+v", row)
	}

	rec, doc := adminCall(t, s, http.MethodGet, "/api/admin/swarm/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("transcript: %d %s", rec.Code, rec.Body.String())
	}
	if doc["judgeText"] != "judged final answer" {
		t.Fatalf("judgeText = %v", doc["judgeText"])
	}
	full, ok := doc["fullTexts"].([]any)
	if !ok || len(full) != 2 {
		t.Fatalf("fullTexts = %v", doc["fullTexts"])
	}
	r1, _ := full[0].([]any)
	if len(r1) != 2 || !strings.Contains(r1[0].(string), "alpha answer") {
		t.Fatalf("round 1 full texts = %v", r1)
	}
	if _, ok := doc["texts"].([]any); !ok {
		t.Fatal("transcript lost the snippet texts")
	}
}
