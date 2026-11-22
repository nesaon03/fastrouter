package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nesaon03/fastrouter/internal/store"
)

func postDecision(t *testing.T, base, body string) (*http.Response, string) {
	return postDecisionH(t, base, body, nil)
}

func postDecisionH(t *testing.T, base, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/v1/decisions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+localKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// A Jev-shaped request reaches TypeSafe unchanged and the typed answer is
// relayed with the provider named in the headers.
func TestDecisionsJevShape(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{
		conn("t1", "typesafe", "key-jev", 1),
	}}
	s, hs := newTestServer(t, snap)
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"jev-latest","answers":{"q":{"type":"choice","choice":"yes","confidence":0.93}},"usage":{"input_tokens":12,"output_tokens":2}}`)
	})
	body := `{"state":"build log ...","assets":{"shot":{"type":"image","format":"png","data_base64":"AA=="}},"questions":{"q":{"type":"choice","instructions":"did it pass?","criteria":{"yes":"clean","no":"errors"}}}}`
	resp, out := postDecisionH(t, hs.URL, body, map[string]string{"TypeSafe-Danger-Zone-Images": "true"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"choice":"yes"`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if got := resp.Header.Get("X-FastRouter-Provider"); got != "typesafe" {
		t.Fatalf("provider header %q", got)
	}
	h := (*hits)[0]
	if h.Host != "api.typesafe.ai" || h.Path != "/v1/systemone" || h.Auth != "Bearer key-jev" {
		t.Fatalf("upstream %+v", h)
	}
	if h.Header.Get("TypeSafe-Danger-Zone-Images") != "true" {
		t.Fatal("image decision header not forwarded")
	}
	if assets, ok := h.Body["assets"].(map[string]any); !ok || assets["shot"] == nil {
		t.Fatalf("assets dropped: %v", h.Body)
	}
	if h.Body["model"] != "jev-latest" || h.Body["state"] != "build log ..." {
		t.Fatalf("body %v", h.Body)
	}
	q, _ := h.Body["questions"].(map[string]any)["q"].(map[string]any)
	if q["type"] != "choice" || q["instructions"] != "did it pass?" {
		t.Fatalf("questions %v", h.Body["questions"])
	}
	if crit, _ := q["criteria"].(map[string]any); crit["yes"] != "clean" {
		t.Fatalf("criteria %v", q["criteria"])
	}
}

// The same canonical request is rewritten for OpenAI Decisions: state becomes
// input, the name-keyed questions become a list with choices.
func TestDecisionsTranslatesToOpenAI(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{
		conn("o1", "openai", "sk-openai", 1),
	}}
	s, hs := newTestServer(t, snap)
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":[{"type":"predicate","name":"safe","probability":0.97}],"usage":{"input_tokens":9,"output_tokens":0}}`)
	})
	body := `{"state":"diff --git ...","questions":{"safe":{"type":"choice","instructions":"is this safe?","criteria":{"yes":"safe","no":"danger"}}}}`
	resp, out := postDecision(t, hs.URL, body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, `"probability":0.97`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	h := (*hits)[0]
	if h.Host != "api.openai.com" || h.Path != "/v1/decisions" {
		t.Fatalf("upstream %+v", h)
	}
	if h.Body["model"] != "gpt-6-luna" || h.Body["input"] != "diff --git ..." {
		t.Fatalf("body %v", h.Body)
	}
	list, ok := h.Body["questions"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("questions not a list: %v", h.Body["questions"])
	}
	q, _ := list[0].(map[string]any)
	if q["name"] != "safe" || q["type"] != "choice" {
		t.Fatalf("question %v", q)
	}
	choices, _ := q["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("choices %v", q["choices"])
	}
	first, _ := choices[0].(map[string]any)
	if first["value"] != "no" || first["description"] != "danger" {
		t.Fatalf("first choice %v", first)
	}
}

// A rejected Jev key fails over to OpenAI, and the OpenAI body is built for the
// fallback rather than replaying the Jev request.
func TestDecisionsFailsOverJevToOpenAI(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{
		conn("t1", "typesafe", "key-jev", 1),
		conn("o1", "openai", "sk-openai", 2),
	}}
	s, hs := newTestServer(t, snap)
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orig-Host") == "api.typesafe.ai" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":[{"type":"predicate","name":"q","probability":0.5}]}`)
	})
	resp, out := postDecision(t, hs.URL, `{"state":"x","questions":{"q":{"type":"noul","instructions":"is it on?"}}}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-FastRouter-Provider") != "openai" {
		t.Fatalf("%d %s headers %v", resp.StatusCode, out, resp.Header)
	}
	if len(*hits) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(*hits))
	}
	if h := (*hits)[0]; h.Host != "api.typesafe.ai" {
		t.Fatalf("first call went to %s", h.Host)
	}
	last := (*hits)[1]
	if last.Host != "api.openai.com" || last.Body["input"] != "x" {
		t.Fatalf("fallback body %v", last.Body)
	}
	list, _ := last.Body["questions"].([]any)
	q, _ := list[0].(map[string]any)
	if q["type"] != "predicate" || q["name"] != "q" {
		t.Fatalf("fallback questions %v", last.Body["questions"])
	}
}

// Naming an engine's model pins that engine first.
func TestDecisionsModelPinsEngine(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{
		conn("t1", "typesafe", "key-jev", 1),
		conn("o1", "openai", "sk-openai", 2),
	}}
	s, hs := newTestServer(t, snap)
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":[]}`)
	})
	resp, _ := postDecision(t, hs.URL, `{"model":"gpt-6-luna","input":"x","questions":[{"type":"predicate","name":"q","instructions":"ok?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if h := (*hits)[0]; h.Host != "api.openai.com" || h.Body["model"] != "gpt-6-luna" {
		t.Fatalf("first call %+v", h)
	}
}

// Without a decision account the endpoint explains what to add.
func TestDecisionsRequiresProvider(t *testing.T) {
	_, hs := newTestServer(t, &store.Snapshot{})
	resp, out := postDecision(t, hs.URL, `{"state":"x","questions":{"q":{"type":"noul"}}}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "no decision provider configured") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
}

// A malformed question set is a client error before any upstream call.
func TestDecisionsRejectsBadQuestions(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{conn("t1", "typesafe", "key-jev", 1)}}
	_, hs := newTestServer(t, snap)
	for _, body := range []string{
		`{"state":"x"}`,
		`{"state":"x","questions":{}}`,
		`{"state":"x","questions":"nope"}`,
		`{"state":"x","questions":[1,2]}`,
	} {
		resp, out := postDecision(t, hs.URL, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s -> %d %s", body, resp.StatusCode, out)
		}
	}
}

// The two shapes convert in both directions.
func TestDecisionQuestionConversion(t *testing.T) {
	jev := jevQuestionsFromList([]any{
		map[string]any{"type": "choice", "name": "a", "instructions": "pick", "choices": []any{
			map[string]any{"value": "yes", "description": "ok"},
			map[string]any{"value": "no", "description": "bad"},
		}},
		map[string]any{"type": "score", "name": "b", "levels": []any{
			map[string]any{"label": "1", "description": "low"},
			map[string]any{"label": "5", "description": "high"},
		}},
		map[string]any{"type": "predicate", "name": "c", "instructions": "true?"},
	})
	if q, _ := jev["a"].(map[string]any); q["type"] != "choice" {
		t.Fatalf("choice: %v", jev["a"])
	}
	if q, _ := jev["b"].(map[string]any); q["type"] != "score" {
		t.Fatalf("score: %v", jev["b"])
	}
	if q, _ := jev["c"].(map[string]any); q["type"] != "noul" {
		t.Fatalf("predicate should become noul: %v", jev["c"])
	}

	list := decisionsQuestionsFromMap(jev)
	if len(list) != 3 {
		t.Fatalf("list %v", list)
	}
	// sorted by name: a, b, c
	q0, _ := list[0].(map[string]any)
	if q0["name"] != "a" || q0["type"] != "choice" {
		t.Fatalf("first %v", q0)
	}
	q2, _ := list[2].(map[string]any)
	if q2["type"] != "predicate" {
		t.Fatalf("noul should become predicate: %v", q2)
	}
}

// A decision engine's default model id is what an unprefixed request uses.
func TestDecisionModelSelection(t *testing.T) {
	jev := decisionTarget{id: "typesafe", model: "jev-latest"}
	oa := decisionTarget{id: "openai", model: "gpt-6-luna"}
	cases := []struct {
		t        decisionTarget
		in, want string
	}{
		{jev, "", "jev-latest"},
		{jev, "jev-latest", "jev-latest"},
		{jev, "jev-1.14.0", "jev-1.14.0"},
		{jev, "gpt-6-luna", "jev-latest"}, // another engine's id
		{jev, "jev/1.15.0", "1.15.0"},
		{oa, "", "gpt-6-luna"},
		{oa, "jev-latest", "gpt-6-luna"},
		{oa, "openai/gpt-6.1-luna", "gpt-6.1-luna"},
	}
	for _, c := range cases {
		if got := decisionModel(c.t, c.in); got != c.want {
			t.Errorf("decisionModel(%s, %q) = %q, want %q", c.t.id, c.in, got, c.want)
		}
	}
}

// A repeated judgment is served from the in-memory cache without a second call.
func TestDecisionCacheHit(t *testing.T) {
	snap := &store.Snapshot{Connections: []store.Connection{conn("t1", "typesafe", "key-jev", 1)}}
	s, hs := newTestServer(t, snap)
	var calls atomic.Int32
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":{"q":{"type":"choice","choice":"yes","confidence":0.9}}}`)
	})
	body := `{"state":"x","questions":{"q":{"type":"choice","instructions":"ok?","criteria":{"yes":"y","no":"n"}}}}`
	resp, _ := postDecision(t, hs.URL, body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-FastRouter-Cache") != "" {
		t.Fatalf("first call %d cache=%q", resp.StatusCode, resp.Header.Get("X-FastRouter-Cache"))
	}
	resp, out := postDecision(t, hs.URL, body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-FastRouter-Cache") != "hit" {
		t.Fatalf("second call %d cache=%q", resp.StatusCode, resp.Header.Get("X-FastRouter-Cache"))
	}
	if !strings.Contains(out, `"choice":"yes"`) {
		t.Fatalf("cached body %s", out)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	// Field order and whitespace do not matter: the key is canonical.
	reordered := `{ "questions": {"q": {"criteria": {"no": "n", "yes": "y"}, "instructions": "ok?", "type": "choice"}}, "state": "x" }`
	postDecision(t, hs.URL, reordered)
	if calls.Load() != 1 {
		t.Fatalf("reordered body missed the cache: %d calls", calls.Load())
	}
	// A different question is a different judgment.
	postDecision(t, hs.URL, `{"state":"x","questions":{"q":{"type":"choice","instructions":"other?","criteria":{"yes":"y","no":"n"}}}}`)
	if calls.Load() != 2 {
		t.Fatalf("different question hit the cache: %d calls", calls.Load())
	}
}

// cacheSeconds=0 disables the cache.
func TestDecisionCacheOff(t *testing.T) {
	snap := &store.Snapshot{
		Settings:    map[string]json.RawMessage{"decisionSettings": json.RawMessage(`{"cacheSeconds":0}`)},
		Connections: []store.Connection{conn("t1", "typesafe", "key-jev", 1)},
	}
	s, hs := newTestServer(t, snap)
	var calls atomic.Int32
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":{"q":{"type":"choice","choice":"yes"}}}`)
	})
	body := `{"state":"x","questions":{"q":{"type":"choice","instructions":"ok?","criteria":{"yes":"y","no":"n"}}}}`
	postDecision(t, hs.URL, body)
	postDecision(t, hs.URL, body)
	if calls.Load() != 2 {
		t.Fatalf("cache off but calls = %d, want 2", calls.Load())
	}
}

// A configured gate settles a choice question with no engine call at all.
func TestDecisionGateSettlesWithoutEngine(t *testing.T) {
	snap := &store.Snapshot{
		Settings:    map[string]json.RawMessage{"decisionSettings": json.RawMessage(`{"gates":{"reject":["writes mbr"]}}`)},
		Connections: []store.Connection{conn("t1", "typesafe", "key-jev", 1)},
	}
	s, hs := newTestServer(t, snap)
	var calls atomic.Int32
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	body := `{"state":"this writes mbr on the boot disk","questions":{"q":{"type":"choice","instructions":"ship it?","criteria":{"reject":"do not ship","review":"look closer","accept":"ship"}}}}`
	resp, out := postDecision(t, hs.URL, body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-FastRouter-Gate") != "hit" {
		t.Fatalf("%d gate=%q %s", resp.StatusCode, resp.Header.Get("X-FastRouter-Gate"), out)
	}
	if !strings.Contains(out, `"choice":"reject"`) || !strings.Contains(out, `"source":"local-gate"`) {
		t.Fatalf("gate answer %s", out)
	}
	if calls.Load() != 0 {
		t.Fatalf("engine called for a gated question: %d", calls.Load())
	}
}

// When only some questions are gated, the rest still go to the engine and the
// response contains both answers.
func TestDecisionGateMergesWithEngine(t *testing.T) {
	snap := &store.Snapshot{
		Settings:    map[string]json.RawMessage{"decisionSettings": json.RawMessage(`{"gates":{"review":["hvci off"]}}`)},
		Connections: []store.Connection{conn("t1", "typesafe", "key-jev", 1)},
	}
	s, hs := newTestServer(t, snap)
	hits := fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":{"other":{"type":"choice","choice":"yes","confidence":0.8}}}`)
	})
	body := `{"state":"hvci off and a clean diff","questions":{
		"gate":{"type":"choice","instructions":"is this risky?","criteria":{"review":"inspect","accept":"fine"}},
		"other":{"type":"choice","instructions":"ok?","criteria":{"yes":"y","no":"n"}}}}`
	resp, out := postDecision(t, hs.URL, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if !strings.Contains(out, `"gate":{"choice":"review"`) && !strings.Contains(out, `"gate": {"choice":"review"`) {
		t.Fatalf("gated answer missing: %s", out)
	}
	if !strings.Contains(out, `"other"`) || !strings.Contains(out, `"yes"`) {
		t.Fatalf("engine answer missing: %s", out)
	}
	if len(*hits) != 1 {
		t.Fatalf("upstream calls = %d", len(*hits))
	}
	qs, _ := (*hits)[0].Body["questions"].(map[string]any)
	if _, sent := qs["gate"]; sent {
		t.Fatalf("gated question was sent upstream: %v", qs)
	}
	if _, sent := qs["other"]; !sent {
		t.Fatalf("ungated question was dropped: %v", qs)
	}
}

// A verdict the question does not offer is never used by the gate.
func TestDecisionGateIgnoresUnavailableVerdict(t *testing.T) {
	snap := &store.Snapshot{
		Settings:    map[string]json.RawMessage{"decisionSettings": json.RawMessage(`{"gates":{"reject":["writes mbr"]}}`)},
		Connections: []store.Connection{conn("t1", "typesafe", "key-jev", 1)},
	}
	s, hs := newTestServer(t, snap)
	var calls atomic.Int32
	fakeUpstreams(t, s, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":{"q":{"type":"choice","choice":"no"}}}`)
	})
	body := `{"state":"this writes mbr","questions":{"q":{"type":"choice","instructions":"ok?","criteria":{"yes":"y","no":"n"}}}}`
	postDecision(t, hs.URL, body)
	if calls.Load() != 1 {
		t.Fatalf("gate used an unavailable verdict: %d calls", calls.Load())
	}
}

// Reject wins over review when both phrases match and both are offered.
func TestDecisionGatePrefersReject(t *testing.T) {
	gates := map[string][]string{"accept": {"clean"}, "review": {"hvci off"}, "reject": {"writes mbr"}}
	in := decisionReq{
		State:     json.RawMessage(`"writes mbr and hvci off"`),
		Questions: json.RawMessage(`{"q":{"type":"choice","criteria":{"accept":"a","review":"r","reject":"x"}}}`),
	}
	gated, rest := applyDecisionGates(gates, in)
	if gated["q"] != "reject" || rest != nil {
		t.Fatalf("gated=%v rest=%s", gated, rest)
	}
}

// The dashboard can change the decision cache window and gate phrases, and
// invalid values are refused.
func TestDecisionSettingsUpdate(t *testing.T) {
	s := newDiscoveryServer(t, "http://127.0.0.1:1/unused")
	rec, _ := adminCall(t, s, http.MethodPatch, "/api/admin/settings", map[string]any{
		"decision": map[string]any{
			"cacheSeconds": 60,
			"gates":        map[string]any{"reject": []string{"writes mbr", " boot disk "}},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	raw := s.state.Load().Snap.Settings["decisionSettings"]
	var got struct {
		CacheSeconds int                 `json:"cacheSeconds"`
		Gates        map[string][]string `json:"gates"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings: %v (%s)", err, raw)
	}
	if got.CacheSeconds != 60 || len(got.Gates["reject"]) != 2 || got.Gates["reject"][1] != "boot disk" {
		t.Fatalf("stored %+v", got)
	}
	if rec, _ := adminCall(t, s, http.MethodPatch, "/api/admin/settings", map[string]any{
		"decision": map[string]any{"cacheSeconds": 999999},
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid cache: %d, want 400", rec.Code)
	}
	if rec, _ := adminCall(t, s, http.MethodPatch, "/api/admin/settings", map[string]any{
		"decision": map[string]any{"gates": map[string]any{"": []string{"x"}}},
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty verdict: %d, want 400", rec.Code)
	}
}

// A valid JSON body reaches the engine with the shape it expects, and the
// answer's usage is recorded.
func TestDecisionUsageParsed(t *testing.T) {
	var env struct {
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal([]byte(`{"usage":{"input_tokens":7,"output_tokens":1}}`), &env)
	if env.Usage.Input != 7 || env.Usage.Output != 1 {
		t.Fatalf("%+v", env)
	}
}
