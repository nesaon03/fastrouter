package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNodeCheckAndPrefix(t *testing.T) {
	const key = "sk-node-SECRET-998877"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := r.Header.Get("Authorization") == "Bearer "+key
		switch r.URL.Path {
		case "/v1/models":
			if !bearer {
				w.WriteHeader(401)
				w.Write([]byte(`{"error":{"message":"bad key ` + r.Header.Get("Authorization") + `"}}`))
				return
			}
			w.Write([]byte(`{"data":[{"id":"m-one"},{"id":"m-two"}]}`))
		case "/nolist/chat/completions":
			if bearer {
				w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
				return
			}
			w.WriteHeader(401)
		case "/anth/messages":
			if r.Header.Get("x-api-key") == key && r.Header.Get("anthropic-version") != "" {
				w.Write([]byte(`{"content":[{"type":"text","text":"OK"}]}`))
				return
			}
			w.WriteHeader(401)
		default:
			w.WriteHeader(404)
		}
	}))
	defer up.Close()
	s, hs := passwordServer(t, "correct horse")
	hdr := adminSession(t, hs.URL, "correct horse")
	call := func(method, path, body string) (map[string]any, string) {
		t.Helper()
		r, b := do(t, method, hs.URL+path, body, hdr)
		if r.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", method, path, r.StatusCode, b)
		}
		if strings.Contains(b, key) {
			t.Fatalf("response leaks the key: %s", b)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(b), &m)
		return m, b
	}
	check := func(body string) map[string]any { m, _ := call("POST", "/api/admin/nodes/check", body); return m }

	m := check(`{"type":"openai-compatible","baseUrl":"` + up.URL + `/v1","apiKey":"` + key + `","modelId":"m-two","prefix":"oc-prod"}`)
	if m["ok"] != true || m["method"] != "models" || m["models"] != 2.0 || m["firstModel"] != "m-one" || m["modelFound"] != true || m["prefixOk"] != true {
		t.Fatalf("models check: %v", m)
	}
	if m = check(`{"type":"openai-compatible","baseUrl":"` + up.URL + `/v1","apiKey":"` + key + `x"}`); m["ok"] != false || m["class"] != "auth" {
		t.Fatalf("bad key: %v", m)
	}
	if m = check(`{"type":"openai-compatible","baseUrl":"` + up.URL + `/nolist","apiKey":"` + key + `"}`); m["ok"] != false || m["class"] != "model" {
		t.Fatalf("no list, no model: %v", m)
	}
	if m = check(`{"type":"openai-compatible","baseUrl":"` + up.URL + `/nolist","apiKey":"` + key + `","modelId":"x1"}`); m["ok"] != true || m["method"] != "request" {
		t.Fatalf("request fallback: %v", m)
	}
	if m = check(`{"type":"anthropic-compatible","baseUrl":"` + up.URL + `/anth/messages","apiKey":"` + key + `","modelId":"c1"}`); m["ok"] != true {
		t.Fatalf("anthropic: %v", m)
	}
	if m = check(`{"type":"openai-compatible","baseUrl":"http://127.0.0.1:1/v1","apiKey":"` + key + `"}`); m["ok"] != false || m["class"] != "network" {
		t.Fatalf("network: %v", m)
	}

	// Saved node + saved key: check without retyping the key; prefix rules; default model routes.
	node := mustID(t, call, `{"type":"openai-compatible","name":"GW","prefix":"gw","baseUrl":"`+up.URL+`/v1","modelId":"m-two"}`)
	call("POST", "/api/admin/accounts", `{"provider":"`+node+`","name":"k","apiKey":"`+key+`"}`)
	if m = check(`{"type":"openai-compatible","baseUrl":"` + up.URL + `/v1","nodeId":"` + node + `","prefix":"gw"}`); m["ok"] != true || m["keySource"] != "saved" || m["prefixOk"] != true {
		t.Fatalf("saved key: %v", m)
	}
	for p, ok := range map[string]bool{"gw": false, "a/b": false, "a b": false, "fresh-1": true, "deepseek": false} {
		if m, _ = call("GET", "/api/admin/nodes/prefix?prefix="+urlq(p), ""); m["ok"] != ok {
			t.Fatalf("prefix %q: %v", p, m)
		}
	}
	if m, _ = call("GET", "/api/admin/nodes/prefix?prefix=gw&except="+node, ""); m["ok"] != true {
		t.Fatalf("own prefix: %v", m)
	}
	ts, err := s.state.Load().Resolve("gw/default")
	if err != nil || len(ts) != 1 || ts[0].Model != "m-two" {
		t.Fatalf("default model route: %v %v", ts, err)
	}
	_, st := adminState(t, hs.URL, hdr)
	mustContain(t, st, `"defaultModel":"m-two"`)
}

func mustID(t *testing.T, call func(string, string, string) (map[string]any, string), body string) string {
	t.Helper()
	m, _ := call("POST", "/api/admin/nodes", body)
	return m["id"].(string)
}

func urlq(s string) string { return strings.NewReplacer("/", "%2F", " ", "%20").Replace(s) }
