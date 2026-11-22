package server

// Swarm Agent combos: the tests matter more than the feature here, because the
// engine makes several upstream calls per request and a mistake in the barrier,
// the prompt or the failure handling is invisible until it costs money.
//
// The fake upstream is scripted per account key (see combos_test.go), and each
// request body is recorded, so a round can be inspected after the fact.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

// swarmSnap builds agents a, b, c plus judge r, all on one fake upstream.
func swarmSnap(up, strategy string, models []string, opts string) *store.Snapshot {
	snap := &store.Snapshot{Settings: map[string]json.RawMessage{}}
	for _, p := range []string{"a", "b", "c", "r"} {
		snap.Nodes = append(snap.Nodes, store.Node{ID: "n" + p, Name: p, Prefix: p, Type: "openai-compatible", BaseURL: up + "/v1"})
		snap.Connections = append(snap.Connections, store.Connection{ID: "c" + p, Provider: "n" + p, Name: p, AuthType: "apikey", Priority: 1,
			Active: true, Status: "ok", Secrets: map[string]string{"apiKey": "key-" + p}, Meta: map[string]any{}})
	}
	snap.Combos = []store.Combo{{ID: "c1", Name: "team", Strategy: strategy, Models: models}}
	if opts != "" {
		snap.Settings["comboStrategies"] = json.RawMessage(`{"team":` + opts + `}`)
	}
	return snap
}

// promptOf returns the text of the last message a scripted call received.
func promptOf(body map[string]any) string {
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	content, _ := last["content"].(string)
	return content
}

// userTextOf returns the original user turn (not the swarm instruction).
func userTextOf(body map[string]any) string {
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].(string)
	return content
}

// Three agents answer, then debate, then the judge writes the reply.
func TestSwarmDebatesThenJudges(t *testing.T) {
	sc, up := newScripted(t)
	var mu sync.Mutex
	rounds := map[string][]string{} // model -> prompts it received, in order
	record := func(model, prompt string) {
		mu.Lock()
		rounds[model] = append(rounds[model], prompt)
		mu.Unlock()
	}
	for _, k := range []string{"a", "b", "c"} {
		m := map[string]string{"a": "m1", "b": "m2", "c": "m3"}[k]
		sc.reply["key-"+k] = func(w http.ResponseWriter, r *http.Request) {
			record(m, promptOf(sc.last().Body))
			oaText("answer from "+m+" at step "+fmt.Sprint(len(rounds[m])))(w, r)
		}
	}
	var judgeSaw string
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		judgeSaw = promptOf(sc.last().Body)
		oaText("the debated final answer")(w, r)
	}
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2", "c/m3"},
		`{"judgeModel":"r/judge","rounds":2}`))

	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"explain X"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(out, "the debated final answer") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}

	// Round 1: every agent answered with no swarm instruction.
	// Round 2: every agent got its own previous answer plus both peers.
	mu.Lock()
	defer mu.Unlock()
	for _, m := range []string{"m1", "m2", "m3"} {
		got := rounds[m]
		if len(got) != 2 {
			t.Fatalf("%s was called %d times, want 2", m, len(got))
		}
		if strings.Contains(got[0], "SWARM ROUND") {
			t.Fatalf("%s got a debate prompt in round 1: %s", m, got[0])
		}
		if !strings.Contains(got[1], "SWARM ROUND 2 of 2") {
			t.Fatalf("%s round 2 prompt missing the round header: %s", m, got[1])
		}
		if !strings.Contains(got[1], "Your previous answer") {
			t.Fatalf("%s round 2 must show the agent its own answer: %s", m, got[1])
		}
		for _, peer := range []string{"m1", "m2", "m3"} {
			if peer == m {
				continue
			}
			if !strings.Contains(got[1], "answer from "+peer+" at step 1") {
				t.Fatalf("%s round 2 must carry %s's round-1 answer: %s", m, peer, got[1])
			}
		}
	}
	// The judge saw the final (round 2) answers, not the first ones.
	if !strings.Contains(judgeSaw, "JUDGE") || !strings.Contains(judgeSaw, "multi-agent debate") {
		t.Fatalf("judge prompt: %s", judgeSaw)
	}
	for _, m := range []string{"m1", "m2", "m3"} {
		if !strings.Contains(judgeSaw, "answer from "+m+" at step 2") {
			t.Fatalf("judge should read %s's final answer: %s", m, judgeSaw)
		}
	}
}

// One round means no debate at all: independent answers, straight to the judge.
func TestSwarmSingleRoundSkipsDebate(t *testing.T) {
	sc, up := newScripted(t)
	var calls atomic.Int32
	for _, k := range []string{"a", "b"} {
		k := k
		sc.reply["key-"+k] = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if strings.Contains(promptOf(sc.last().Body), "SWARM ROUND") {
				t.Error("rounds=1 must not produce a debate prompt")
			}
			oaText("independent from "+k)(w, r)
		}
	}
	sc.reply["key-r"] = oaText("judged")
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"}, `{"rounds":1,"judgeModel":"r/judge"}`))
	if resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`); resp.StatusCode != 200 || !strings.Contains(out, "judged") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if calls.Load() != 2 {
		t.Fatalf("agent calls = %d, want 2 (one each)", calls.Load())
	}
}

// An agent that dies mid-debate drops out; the debate continues with the rest.
func TestSwarmAgentFailureMidDebate(t *testing.T) {
	sc, up := newScripted(t)
	var bCalls atomic.Int32
	sc.reply["key-a"] = oaText("steady from a")
	sc.reply["key-b"] = func(w http.ResponseWriter, r *http.Request) {
		if bCalls.Add(1) == 1 {
			oaText("b first answer")(w, r)
			return
		}
		jsonReply(500, `{"error":{"message":"b died"}}`)(w, r)
	}
	sc.reply["key-c"] = oaText("steady from c")
	var judgeSaw string
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		judgeSaw = promptOf(sc.last().Body)
		oaText("judged anyway")(w, r)
	}
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2", "c/m3"}, `{"rounds":2,"judgeModel":"r/judge"}`))
	if resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`); resp.StatusCode != 200 || !strings.Contains(out, "judged anyway") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	// b keeps its round-1 answer in the debate (it did answer once), so the judge
	// still sees three voices, not two.
	if !strings.Contains(judgeSaw, "steady from a") || !strings.Contains(judgeSaw, "steady from c") {
		t.Fatalf("judge lost a surviving agent: %s", judgeSaw)
	}
}

// An agent that never answers is not carried into the judge: three agents, two
// survive, so the judge still runs and must see only those two.
func TestSwarmAgentNeverAnswers(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["key-a"] = oaText("only a")
	sc.reply["key-b"] = jsonReply(500, `{"error":{"message":"b down"}}`)
	sc.reply["key-c"] = oaText("only c")
	var judgeSaw string
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		judgeSaw = promptOf(sc.last().Body)
		oaText("judged")(w, r)
	}
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2", "c/m3"}, `{"rounds":2,"judgeModel":"r/judge"}`))
	post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`)
	if !strings.Contains(judgeSaw, "only a") || !strings.Contains(judgeSaw, "only c") {
		t.Fatalf("judge must see both survivors: %s", judgeSaw)
	}
	if strings.Contains(judgeSaw, "[Agent 3]") {
		t.Fatalf("the dead agent must not occupy a slot: %s", judgeSaw)
	}
}

// One survivor is returned as-is: a debate needs two voices.
func TestSwarmOneSurvivorAnswersDirectly(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["key-a"] = oaText("the only answer")
	sc.reply["key-b"] = jsonReply(500, `{"error":{"message":"down"}}`)
	var judgeCalls atomic.Int32
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		judgeCalls.Add(1)
		oaText("judge should not run")(w, r)
	}
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"}, `{"rounds":2,"judgeModel":"r/judge"}`))
	start := time.Now()
	_, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`)
	if !strings.Contains(out, "the only answer") {
		t.Fatalf("out: %s", out)
	}
	if judgeCalls.Load() != 0 {
		t.Fatal("the judge must not run for a single surviving answer")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a single survivor should be returned immediately")
	}
}

// Every agent down is a 503, not an empty answer.
func TestSwarmAllAgentsFail(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["key-a"] = jsonReply(500, `{"error":{"message":"a down"}}`)
	sc.reply["key-b"] = jsonReply(500, `{"error":{"message":"b down"}}`)
	sc.reply["key-r"] = oaText("never")
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"}, `{"rounds":2,"judgeModel":"r/judge"}`))
	if resp, _ := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`); resp.StatusCode != 503 {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}

// Agents cannot call tools mid-debate, exactly like fusion's panel.
func TestSwarmStripsTools(t *testing.T) {
	sc, up := newScripted(t)
	for _, k := range []string{"a", "b"} {
		sc.reply["key-"+k] = oaText("ok")
	}
	sc.reply["key-r"] = oaText("judged")
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"}, `{"rounds":2,"judgeModel":"r/judge"}`))
	post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}],"tools":[{"type":"function","function":{"name":"x","parameters":{"type":"object"}}}]}`)
	for _, r := range sc.reqs {
		if r.Body["model"] != "judge" && r.Body["tools"] != nil {
			t.Fatalf("agent %v carried tools", r.Body["model"])
		}
	}
}

// Rounds and agents are clamped: a hand-edited setting cannot explode the bill.
func TestSwarmClampsRoundsAndAgents(t *testing.T) {
	if got := swarmRounds(0); got != swarmDefaultRounds {
		t.Fatalf("default rounds = %d", got)
	}
	if got := swarmRounds(99); got != swarmMaxRounds {
		t.Fatalf("rounds not capped: %d", got)
	}
	if got := swarmRounds(-3); got != swarmDefaultRounds {
		t.Fatalf("negative rounds = %d", got)
	}
	agents := swarmAgents([]string{"a/m", "b/m", "a/m", "c/m", "d/m", "e/m"})
	if len(agents) != swarmMaxAgents {
		t.Fatalf("agents not capped: %v", agents)
	}
	seen := map[string]bool{}
	for _, a := range agents {
		if seen[a] {
			t.Fatalf("duplicate agent %s", a)
		}
		seen[a] = true
	}
	if len(swarmAgents([]string{"a/only"})) != 1 {
		t.Fatal("a one-model combo stays one agent")
	}
}

// A combo with a single model is not a debate: it is just that model.
func TestSwarmSingleAgentRoutesDirectly(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["key-a"] = oaText("direct")
	sc.reply["key-r"] = oaText("never")
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1"}, `{"rounds":2,"judgeModel":"r/judge"}`))
	if _, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`); !strings.Contains(out, "direct") {
		t.Fatalf("out: %s", out)
	}
}

// A slow agent does not hold the round open past the barrier timeout.
func TestSwarmRoundTimeout(t *testing.T) {
	old := swarmRoundWait
	swarmRoundWait = 300 * time.Millisecond
	defer func() { swarmRoundWait = old }()

	sc, up := newScripted(t)
	sc.reply["key-a"] = oaText("fast a")
	sc.reply["key-b"] = func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(promptOf(sc.last().Body), "SWARM ROUND") {
			time.Sleep(2 * time.Second) // slower than the barrier
		}
		oaText("late b")(w, r)
	}
	sc.reply["key-r"] = oaText("judged without b's revision")
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"}, `{"rounds":2,"judgeModel":"r/judge"}`))
	start := time.Now()
	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(out, "judged without") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("the barrier did not bound the round: %v", d)
	}
}

// The client's streaming mode is the judge's: SSE when asked, JSON otherwise.
func TestSwarmStreamsFromJudge(t *testing.T) {
	sc, up := newScripted(t)
	sc.reply["key-a"] = oaText("a")
	sc.reply["key-b"] = oaText("b")
	// A streamed judge answer in OpenAI SSE form.
	sc.reply["key-r"] = jsonReply(200, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"streamed final\"}}]}\n\ndata: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"streamed final\"}}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"}, `{"rounds":2,"judgeModel":"r/judge"}`))
	resp, out := post(t, hs.URL, `{"model":"team","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(out, "streamed final") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type: %s", ct)
	}
}

// The judge's brief must not leak the machinery to the end user.
func TestSwarmPromptsStayHidden(t *testing.T) {
	if p := swarmJudgePrompt(2, []string{"a1", "a2"}); !strings.Contains(p, "Do NOT mention") {
		t.Fatalf("judge prompt should forbid mentioning the panel: %s", p)
	}
	p := swarmRoundPrompt(1, 3, "m2", "mine", []string{"theirs"})
	for _, want := range []string{"SWARM ROUND 2 of 3", "Your previous answer", "mine", "[Peer 1]", "theirs", "Never mention the swarm"} {
		if !strings.Contains(p, want) {
			t.Fatalf("round prompt missing %q:\n%s", want, p)
		}
	}
	// A huge answer is clipped so one agent cannot crowd out the others.
	long := strings.Repeat("x", swarmAnswerChars+500)
	if got := clipAnswer(long); len(got) > swarmAnswerChars+len("\n[truncated]") {
		t.Fatalf("clipAnswer did not clip: %d bytes", len(got))
	}
}

// When every agent gives the same answer in round 1, remaining debate rounds
// are skipped so the swarm does not pay 2x-3x tokens on trivial consensus.
func TestSwarmEarlyConsensusSkipsDebate(t *testing.T) {
	sc, up := newScripted(t)
	var calls atomic.Int32
	for _, k := range []string{"a", "b", "c"} {
		sc.reply["key-"+k] = func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			oaText("The capital of France is Paris.")(w, r)
		}
	}
	sc.reply["key-r"] = oaText("Paris")
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2", "c/m3"},
		`{"judgeModel":"r/judge","rounds":3}`))
	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"capital of France?"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(out, "Paris") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 agent calls (1 round x 3 agents), got %d", calls.Load())
	}
}

// With roles enabled, each agent gets a distinct reasoning lens in round 1.
func TestSwarmRolesInjectDistinctLenses(t *testing.T) {
	sc, up := newScripted(t)
	var mu sync.Mutex
	sysSaw := map[string]string{}
	for _, k := range []string{"a", "b"} {
		k := k
		sc.reply["key-"+k] = func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			msgs, _ := sc.last().Body["messages"].([]any)
			if len(msgs) > 0 {
				first, _ := msgs[0].(map[string]any)
				if first["role"] == "system" {
					sysSaw[k], _ = first["content"].(string)
				}
			}
			mu.Unlock()
			oaText("distinct answer from "+k+" with unique details "+strings.Repeat(k, 10))(w, r)
		}
	}
	sc.reply["key-r"] = oaText("judged")
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"},
		`{"judgeModel":"r/judge","rounds":1,"roles":true}`))
	resp, _ := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"design a cache"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if sysSaw["a"] == "" || sysSaw["b"] == "" || sysSaw["a"] == sysSaw["b"] {
		t.Fatalf("expected distinct role system prompts, got a=%q b=%q", sysSaw["a"], sysSaw["b"])
	}
}

// The dashboard can read the live/recent swarm run: per-agent per-round states,
// judge state and the consensus flag.
func TestSwarmRunTelemetry(t *testing.T) {
	sc, up := newScripted(t)
	for _, k := range []string{"a", "b"} {
		k := k
		sc.reply["key-"+k] = func(w http.ResponseWriter, r *http.Request) {
			oaText("answer from "+k+" with enough distinct detail to avoid consensus "+strings.Repeat(k, 20))(w, r)
		}
	}
	sc.reply["key-r"] = oaText("judged answer")
	s, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"},
		`{"judgeModel":"r/judge","rounds":2}`))
	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"explain X"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "judged answer") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}

	rec, got := adminCall(t, s, http.MethodGet, "/api/admin/swarm", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry: %d %s", rec.Code, rec.Body.String())
	}
	runs, _ := got["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", got["runs"])
	}
	run, _ := runs[0].(map[string]any)
	if run["combo"] != "team" || run["status"] != swarmDone || run["judgeState"] != swarmOK {
		t.Fatalf("run %v", run)
	}
	if run["consensus"] == true {
		t.Fatal("distinct answers reported as consensus")
	}
	if n := len(run["agents"].([]any)); n != 2 {
		t.Fatalf("agents = %d", n)
	}
	states, _ := run["states"].([]any)
	if len(states) != 2 {
		t.Fatalf("rounds = %d", len(states))
	}
	for r, row := range states {
		cells, _ := row.([]any)
		if len(cells) != 2 {
			t.Fatalf("round %d cells %v", r+1, cells)
		}
		for i, c := range cells {
			if c != swarmOK {
				t.Fatalf("round %d agent %d = %v, want ok", r+1, i, c)
			}
		}
	}
}

// An early consensus is recorded with the round it stopped at; rounds that never
// started are marked skipped, not timed out.
func TestSwarmRunTelemetryConsensus(t *testing.T) {
	sc, up := newScripted(t)
	for _, k := range []string{"a", "b", "c"} {
		sc.reply["key-"+k] = oaText("The capital of France is Paris.")
	}
	sc.reply["key-r"] = oaText("Paris")
	s, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2", "c/m3"},
		`{"judgeModel":"r/judge","rounds":3}`))
	if resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"capital?"}]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	_, got := adminCall(t, s, http.MethodGet, "/api/admin/swarm", nil)
	runs, _ := got["runs"].([]any)
	run, _ := runs[0].(map[string]any)
	if run["consensus"] != true || run["round"] != float64(1) {
		t.Fatalf("consensus run %v", run)
	}
	states, _ := run["states"].([]any)
	if len(states) != 3 {
		t.Fatalf("states %v", states)
	}
	round2, _ := states[1].([]any)
	for _, c := range round2 {
		if c != swarmSkipped {
			t.Fatalf("unstarted round not skipped: %v", round2)
		}
	}
}

// A failed swarm still leaves a readable run instead of a spinner.
func TestSwarmRunTelemetryFailure(t *testing.T) {
	sc, up := newScripted(t)
	for _, k := range []string{"a", "b"} {
		sc.reply["key-"+k] = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
	s, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"}, `{"rounds":1}`))
	if resp, _ := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, got := adminCall(t, s, http.MethodGet, "/api/admin/swarm", nil)
	runs, _ := got["runs"].([]any)
	run, _ := runs[0].(map[string]any)
	if run["status"] != swarmError || run["error"] == "" {
		t.Fatalf("failed run %v", run)
	}
}

// Once a quorum answered, a straggler gets the short grace, not the full round.
func TestSwarmQuorumGrace(t *testing.T) {
	oldWait, oldGrace := swarmRoundWait, swarmGraceAfter
	swarmRoundWait = 5 * time.Second
	swarmGraceAfter = 150 * time.Millisecond
	defer func() { swarmRoundWait, swarmGraceAfter = oldWait, oldGrace }()

	sc, up := newScripted(t)
	sc.reply["key-a"] = oaText("answer a")
	sc.reply["key-b"] = oaText("answer b")
	sc.reply["key-c"] = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(3 * time.Second):
		}
		oaText("late c")(w, r)
	}
	sc.reply["key-r"] = oaText("judged after quorum")
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2", "c/m3"},
		`{"rounds":1,"judgeModel":"r/judge"}`))
	start := time.Now()
	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "judged after quorum") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("quorum grace did not bound the round: %v", d)
	}
}

// Debate answers use the smaller agent budget; the judge keeps the client's.
func TestSwarmCapsAgentTokens(t *testing.T) {
	sc, up := newScripted(t)
	maxOf := func() int {
		v, _ := sc.last().Body["max_tokens"].(float64)
		return int(v)
	}
	var mu sync.Mutex
	got := map[string]int{}
	sc.reply["key-a"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got["a"] = maxOf()
		mu.Unlock()
		oaText("answer a")(w, r)
	}
	sc.reply["key-b"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got["b"] = maxOf()
		mu.Unlock()
		oaText("answer b")(w, r)
	}
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got["judge"] = maxOf()
		mu.Unlock()
		oaText("judged")(w, r)
	}
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2"},
		`{"rounds":1,"judgeModel":"r/judge"}`))
	post(t, hs.URL, `{"model":"team","max_tokens":4096,"messages":[{"role":"user","content":"q"}]}`)
	mu.Lock()
	defer mu.Unlock()
	if got["a"] != swarmAgentMaxTokens || got["b"] != swarmAgentMaxTokens {
		t.Fatalf("agent budgets = %v, want %d", got, swarmAgentMaxTokens)
	}
	if got["judge"] != 4096 {
		t.Fatalf("judge budget = %d, want 4096", got["judge"])
	}
}

// Round 2 carries a clipped peer answer, not the full 6k one.
func TestSwarmClipsPeerAnswers(t *testing.T) {
	sc, up := newScripted(t)
	var mu sync.Mutex
	round2 := ""
	long := func(prefix string) string { return prefix + strings.Repeat("x", 3000) }
	for _, k := range []string{"a", "b", "c"} {
		k := k
		sc.reply["key-"+k] = func(w http.ResponseWriter, r *http.Request) {
			p := promptOf(sc.last().Body)
			if k == "a" && strings.Contains(p, "SWARM ROUND 2") {
				mu.Lock()
				round2 = p
				mu.Unlock()
			}
			oaText(long("answer "+k+" "))(w, r)
		}
	}
	sc.reply["key-r"] = oaText("judged")
	_, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2", "c/m3"},
		`{"rounds":2,"judgeModel":"r/judge"}`))
	post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`)
	mu.Lock()
	defer mu.Unlock()
	if round2 == "" {
		t.Fatal("round 2 did not run")
	}
	if !strings.Contains(round2, "[truncated]") {
		t.Fatalf("peer answers were not clipped: %d bytes", len(round2))
	}
	if len(round2) > 9000 {
		t.Fatalf("round 2 prompt is %d bytes; clipping did not bound it", len(round2))
	}
}

// Identical answers skip the judge entirely and return the agreed text.
func TestSwarmSkipsJudgeOnExactConsensus(t *testing.T) {
	sc, up := newScripted(t)
	for _, k := range []string{"a", "b", "c"} {
		sc.reply["key-"+k] = oaText("The same final answer.")
	}
	var judgeCalls atomic.Int32
	sc.reply["key-r"] = func(w http.ResponseWriter, r *http.Request) {
		judgeCalls.Add(1)
		oaText("the judge should not run")(w, r)
	}
	s, hs := newTestServer(t, swarmSnap(up.URL, "swarm", []string{"a/m1", "b/m2", "c/m3"},
		`{"rounds":2,"judgeModel":"r/judge"}`))
	resp, out := post(t, hs.URL, `{"model":"team","messages":[{"role":"user","content":"q"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, "The same final answer.") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if strings.Contains(out, "judge should not run") || judgeCalls.Load() != 0 {
		t.Fatalf("judge ran on exact consensus: %d", judgeCalls.Load())
	}
	_, got := adminCall(t, s, http.MethodGet, "/api/admin/swarm", nil)
	runs, _ := got["runs"].([]any)
	run, _ := runs[0].(map[string]any)
	if run["judgeState"] != swarmSkipped {
		t.Fatalf("judge state %v", run["judgeState"])
	}
}
