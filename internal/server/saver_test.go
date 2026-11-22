package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nesaon03/fastrouter/internal/config"
	"github.com/nesaon03/fastrouter/internal/saver"
	"github.com/nesaon03/fastrouter/internal/store"
)

func TestTokenSaversOnProxyPathAndOptOut(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k1"] = jsonReply(200, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	set := map[string]json.RawMessage{}
	for k, v := range map[string]any{"cavemanEnabled": true, "cavemanLevel": "ultra", "ponytailEnabled": true, "ponytailLevel": "ultra"} {
		set[k], _ = json.Marshal(v)
	}
	_, hs := newTestServer(t, &store.Snapshot{Settings: set,
		Nodes:       []store.Node{node("n1", "zz", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "k1", 1)}})
	body := `{"model":"zz/m","messages":[{"role":"user","content":"hi"}]}`

	resp, _ := do(t, "POST", hs.URL+"/v1/chat/completions", body, nil)
	if got := resp.Header.Get(saver.TagHeader); got != "CAVEMAN:ultra,PONYTAIL:ultra" {
		t.Fatalf("tag %q", got)
	}
	msgs := sc.last().Body["messages"].([]any)
	if sys := msgs[0].(map[string]any); sys["role"] != "system" || !strings.Contains(sys["content"].(string), "Telegraphic") {
		t.Fatalf("prompt not injected: %v", msgs)
	}

	resp, _ = do(t, "POST", hs.URL+"/v1/chat/completions", body, map[string]string{"X-9Router-Token-Saver": "off"})
	if resp.Header.Get(saver.TagHeader) != "" || len(sc.last().Body["messages"].([]any)) != 1 {
		t.Fatalf("opt-out header ignored: %v", sc.last().Body)
	}
}

// The proxy path records what the savers changed, and the usage API can show
// it later: this is the end-to-end test for the savings columns.
func TestProxyRecordsSaverSavings(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["k1"] = jsonReply(200, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	set := map[string]json.RawMessage{}
	set["compactEnabled"], _ = json.Marshal(true)
	set["compactKeepToolResults"], _ = json.Marshal(1)
	snap := &store.Snapshot{Settings: set,
		Nodes:       []store.Node{node("n1", "zz", up.URL+"/v1")},
		Connections: []store.Connection{conn("a1", "n1", "k1", 1)},
		APIKeys:     []store.APIKey{{ID: "lk", Name: "lk", Key: localKey, Active: true}}}
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, loaded, st)
	defer s.Close()
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "\tmodified:   src/file" + strconv.Itoa(i) + ".go"
	}
	status := "On branch main\nChanges not staged for commit:\n" + strings.Join(lines, "\n") + "\n"
	body := `{"model":"zz/m","messages":[{"role":"tool","content":` + strconv.Quote(status) + `},{"role":"tool","content":` + strconv.Quote(status) + `}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+localKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body.String())
	}
	s.rec.close()
	entries, total, err := st.UsageEntries(time.Time{}, "", 10, 0)
	if err != nil || total != 1 || len(entries) != 1 {
		t.Fatalf("entries: %v total=%d", err, total)
	}
	if entries[0].SavedInput <= 0 || !strings.Contains(entries[0].SaverTag, "COMPACT") {
		t.Fatalf("savings not recorded: %+v", entries[0])
	}
	tot, err := st.SaverTotals(time.Now().Add(-time.Hour))
	if err != nil || tot.SavedInput != entries[0].SavedInput {
		t.Fatalf("totals = %+v err=%v", tot, err)
	}
}
