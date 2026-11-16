package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zenpo/fastrouter/internal/saver"
	"github.com/zenpo/fastrouter/internal/store"
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
