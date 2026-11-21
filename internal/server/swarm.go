package server

// Swarm Agent combos: several models work as a team for one answer.
//
// Round 1 every agent answers the user on its own, like fusion's panel. Then
// each agent is shown the other agents' latest answers and its own, and asked to
// revise: keep what is right, drop what it cannot defend, adopt a peer's point
// only if it can verify it. The rounds are synchronized — every agent finishes a
// round before the next one starts, so all of them argue from the same state.
// The judge then writes the single answer the client receives.
//
// The difference from fusion matters: fusion samples the panel once and averages
// it, so a wrong answer from three models outvotes a right one from a fourth.
// Swarm lets the right one be argued for across rounds before the judge reads.
//
// Everything here follows fusion's constraints: tools are stripped (an agent
// cannot run a tool mid-debate), the client's streaming mode is honoured by the
// judge only, and every call goes through the normal proxy path so failover,
// cooldowns, usage accounting and the token savers still apply.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/zenpo/fastrouter/internal/router"
	tr "github.com/zenpo/fastrouter/internal/translate"
)

const (
	// swarmMaxAgents caps the panel: cost is agents x rounds + judge calls.
	swarmMaxAgents = 4
	// swarmDefaultRounds is one independent answer plus one debate round.
	swarmDefaultRounds = 2
	// swarmMaxRounds caps the debate. Two is usually enough; more mostly
	// re-litigates the same points and doubles the bill.
	swarmMaxRounds = 3
	// swarmHardLimit bounds the whole swarm, judge included.
	swarmHardLimit = 150 * time.Second
	// swarmAnswerChars caps each answer carried into the next round and into the
	// judge, so a verbose agent cannot crowd out the others.
	swarmAnswerChars = 6000
)

// swarmRoundWait bounds one round. It is a variable so tests can shorten it.
var swarmRoundWait = 45 * time.Second

// swarmAgents returns the panel for a combo: the first models up to the cap,
// with duplicates dropped so one model cannot argue with itself.
func swarmAgents(models []string) []string {
	out := make([]string, 0, swarmMaxAgents)
	seen := map[string]bool{}
	for _, m := range models {
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
		if len(out) == swarmMaxAgents {
			break
		}
	}
	return out
}

// swarmRounds clamps the configured round count.
func swarmRounds(n int) int {
	switch {
	case n <= 0:
		return swarmDefaultRounds
	case n > swarmMaxRounds:
		return swarmMaxRounds
	}
	return n
}

// swarmRoles are optional reasoning lenses applied in round 1 so the agents do
// not all write the same generic answer before debating.
var swarmRoles = []string{
	"Focus on architectural soundness, complete design, and clear structure while answering the user's request.",
	"Focus on edge cases, failure modes, correctness bugs, and hidden assumptions while answering the user's request.",
	"Focus on minimal, idiomatic, high-performance implementation with zero filler while answering the user's request.",
	"Focus on security, input validation, concurrency safety, and operational robustness while answering the user's request.",
}

// swarmRoleLens returns the system lens for agent index i.
func swarmRoleLens(i int) string {
	if i < 0 || len(swarmRoles) == 0 {
		return ""
	}
	return swarmRoles[i%len(swarmRoles)]
}

// swarmConsensus reports whether every non-empty answer in the round already
// agrees closely enough that another debate round would only burn tokens.
// Exact normalized equality (for short/deterministic answers) or >=0.85 Jaccard
// token similarity across all pairs counts as consensus.
func swarmConsensus(answers []string) bool {
	var live []string
	for _, a := range answers {
		if s := strings.TrimSpace(a); s != "" {
			live = append(live, s)
		}
	}
	if len(live) < 2 {
		return false
	}
	norms := make([]string, len(live))
	sets := make([]map[string]bool, len(live))
	for i, s := range live {
		norms[i], sets[i] = normalizeSwarmText(s)
	}
	for i := 1; i < len(live); i++ {
		if norms[i] == norms[0] && norms[0] != "" {
			continue
		}
		if jaccardTokens(sets[0], sets[i]) < 0.85 {
			return false
		}
	}
	return true
}

func normalizeSwarmText(s string) (string, map[string]bool) {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	words := strings.Fields(b.String())
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return strings.Join(words, " "), set
}

func jaccardTokens(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// clipAnswer keeps one answer from dominating the next round's prompt.
func clipAnswer(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= swarmAnswerChars {
		return s
	}
	return s[:swarmAnswerChars] + "\n[truncated]"
}

// swarmRoundPrompt is the message each agent gets at the start of a debate
// round: its own previous answer plus every peer's, anonymized.
func swarmRoundPrompt(round, rounds int, self string, own string, peers []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== SWARM ROUND %d of %d ===\n", round+1, rounds)
	b.WriteString("You are one of several agents answering the user's request. Below are the latest answers.\n\n")
	if own = strings.TrimSpace(own); own != "" {
		b.WriteString("[Your previous answer]\n" + clipAnswer(own) + "\n\n")
	}
	for i, p := range peers {
		fmt.Fprintf(&b, "[Peer %d]\n%s\n\n", i+1, clipAnswer(p))
	}
	b.WriteString("Revise your answer now. Keep what is correct, fix what is wrong, and adopt a peer's point only if you can verify it — agreement is not evidence. Drop anything you cannot support.\n")
	b.WriteString("Reply with your complete final answer to the user's original request, in your own words. Never mention the swarm, the peers, or this message.")
	return b.String()
}

// swarmJudgePrompt is the judge's brief: a debate happened, write the answer.
func swarmJudgePrompt(rounds int, answers []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the JUDGE of a multi-agent debate. %d agents answered the user's request independently, then debated it over %d synchronized rounds.\n\n", len(answers), rounds)
	b.WriteString("Do NOT mention that multiple models were used, the debate, or the sources. Produce ONE authoritative final answer addressed directly to the user.\n\n")
	b.WriteString("Weigh the answers: where they converged they are more likely right, where they disagreed decide on the merits, and keep insights only one of them surfaced. Prefer the corrected answer over the original one. Write the best possible final answer to the user's original request — more complete and correct than any single answer, with no filler.\n\n")
	b.WriteString("=== FINAL AGENT ANSWERS ===\n")
	for i, a := range answers {
		fmt.Fprintf(&b, "[Agent %d]\n%s\n\n", i+1, clipAnswer(a))
	}
	b.WriteString("=== END FINAL AGENT ANSWERS ===\n\nNow write the final answer to the user's original request.")
	return b.String()
}

// swarm runs one request through the agent team.
func (s *Server) swarm(w http.ResponseWriter, p *proxyReq, st *router.State, c *router.Combo) {
	agents := swarmAgents(c.Models)
	judge := firstNonEmpty(c.JudgeModel, agents[0])
	rounds := swarmRounds(c.Rounds)
	if len(agents) == 1 {
		s.routeModel(w, p, st, agents[0])
		return
	}
	ir, err := p.parse()
	if err != nil {
		writeError(w, p.cf, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	base := *ir
	base.Tools, base.ToolChoice, base.ParallelToolCalls = nil, nil, nil
	base.Messages = flattenTools(ir.Messages)
	log.Printf("SWARM %s: %d agents, %d rounds, judge %s", c.Name, len(agents), rounds, judge)

	ctx, cancel := context.WithTimeout(p.r.Context(), swarmHardLimit)
	defer cancel()

	// answers[i] is agent i's latest answer; "" means it has not produced one.
	answers := make([]string, len(agents))
	live := make([]bool, len(agents))
	for i := range live {
		live[i] = true
	}

	for round := 0; round < rounds && ctx.Err() == nil; round++ {
		next := make([]string, len(agents))
		done := make(chan struct {
			i    int
			text string
			err  error
		}, len(agents))
		waiting := 0
		for i, model := range agents {
			if !live[i] {
				continue
			}
			q := base
			if round == 0 && c.Roles {
				if lens := swarmRoleLens(i); lens != "" {
					q.System = append(append([]tr.Part{}, base.System...), tr.Part{Type: tr.PartText, Text: lens})
				}
			} else if round > 0 {
				var peers []string
				for j, a := range answers {
					if j != i && live[j] && a != "" {
						peers = append(peers, a)
					}
				}
				q.Messages = append(append([]tr.Message{}, base.Messages...),
					tr.Message{Role: "user", Parts: []tr.Part{{Type: tr.PartText, Text: swarmRoundPrompt(round, rounds, model, answers[i], peers)}}})
			}
			waiting++
			go func(i int, model string) {
				resp, err := s.internalCall(ctx, p.r, &q, model)
				text := ""
				if err == nil {
					if text = strings.TrimSpace(partsText(resp.Parts)); text == "" {
						err = fmt.Errorf("%s: empty answer", model)
					}
				}
				done <- struct {
					i    int
					text string
					err  error
				}{i, text, err}
			}(i, model)
		}

		// The barrier: nobody starts the next round until this one resolves.
		deadline := time.NewTimer(swarmRoundWait)
		got := 0
		for got < waiting {
			select {
			case r := <-done:
				got++
				if r.err != nil {
					log.Printf("SWARM %s round %d: %v", c.Name, round+1, r.err)
					continue
				}
				next[r.i] = r.text
			case <-deadline.C:
				log.Printf("SWARM %s round %d: %d of %d agents answered in time", c.Name, round+1, got, waiting)
				got = waiting
			case <-ctx.Done():
				got = waiting
			}
		}
		deadline.Stop()
		// An agent that missed a round keeps its previous answer and stays in the
		// debate; one that never answered at all is out.
		for i := range agents {
			if next[i] != "" {
				answers[i] = next[i]
			}
			live[i] = answers[i] != ""
		}
		if round+1 < rounds && swarmConsensus(answers) {
			log.Printf("SWARM %s: consensus reached after round %d; skipping remaining debate rounds", c.Name, round+1)
			rounds = round + 1
			break
		}
	}

	var texts, from []string
	for i, a := range answers {
		if a != "" {
			texts, from = append(texts, a), append(from, agents[i])
		}
	}
	switch len(texts) {
	case 0:
		writeError(w, p.cf, http.StatusServiceUnavailable, "api_error", "every swarm agent failed")
		return
	case 1:
		// A debate needs at least two voices; one answer is just the answer.
		log.Printf("SWARM %s: only %s answered; replying with it directly", c.Name, from[0])
		s.routeModel(w, p, st, from[0])
		return
	}

	j := *ir
	j.Messages = append(append([]tr.Message{}, ir.Messages...),
		tr.Message{Role: "user", Parts: []tr.Part{{Type: tr.PartText, Text: swarmJudgePrompt(rounds, texts)}}})
	body, err := tr.EmitRequest(p.cf, &j, tr.EmitOptions{Model: judge})
	if err != nil {
		writeError(w, p.cf, http.StatusInternalServerError, "api_error", "could not build the judge request: "+err.Error())
		return
	}
	p2 := &proxyReq{r: p.r, cf: p.cf, body: body, model: judge, stream: p.stream}
	if err := scanHead(body, &p2.head); err != nil {
		writeError(w, p.cf, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	log.Printf("SWARM %s: judging %d answers after %d rounds with %s", c.Name, len(texts), rounds, judge)
	s.routeModel(w, p2, st, judge)
}
