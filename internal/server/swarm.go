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

	"github.com/nesaon03/fastrouter/internal/router"
	tr "github.com/nesaon03/fastrouter/internal/translate"
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
	// swarmAnswerChars caps each answer carried into the judge, so a verbose
	// agent cannot crowd out the others.
	swarmAnswerChars = 6000
	// swarmPeerChars caps an answer carried into the next debate round. Round 2
	// multiplies this by the number of agents, so a smaller cap is the main
	// lever on debate input cost.
	swarmPeerChars = 2500
	// swarmAgentMaxTokens caps a debate answer. Only the judge writes the final
	// answer, so agents do not need the client's full budget.
	swarmAgentMaxTokens = 1200
)

// swarmGraceAfter is how long a round waits for stragglers once a quorum has
// answered. A variable so tests can shorten it.
var swarmGraceAfter = 8 * time.Second

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

// clipAnswer keeps one answer from dominating the judge prompt.
func clipAnswer(s string) string { return clipAnswerN(s, swarmAnswerChars) }

// clipAnswerN trims s to n bytes with a marker. Debate rounds use the smaller
// swarmPeerChars cap.
func clipAnswerN(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[truncated]"
}

// swarmSameText reports whether every answer normalizes to the same text.
// Used to skip the judge when the team already produced one answer.
func swarmSameText(answers []string) bool {
	first := ""
	seen := false
	for _, a := range answers {
		if strings.TrimSpace(a) == "" {
			continue
		}
		norm, _ := normalizeSwarmText(a)
		if norm == "" {
			continue
		}
		if !seen {
			first, seen = norm, true
			continue
		}
		if norm != first {
			return false
		}
	}
	return seen
}

// swarmRoundPrompt is the message each agent gets at the start of a debate
// round: its own previous answer plus every peer's, anonymized.
func swarmRoundPrompt(round, rounds int, self string, own string, peers []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== SWARM ROUND %d of %d ===\n", round+1, rounds)
	b.WriteString("You are one of several agents answering the user's request. Below are the latest answers.\n\n")
	if own = strings.TrimSpace(own); own != "" {
		b.WriteString("[Your previous answer]\n" + clipAnswerN(own, swarmPeerChars) + "\n\n")
	}
	for i, p := range peers {
		fmt.Fprintf(&b, "[Peer %d]\n%s\n\n", i+1, clipAnswerN(p, swarmPeerChars))
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

	runID := s.swarms.start(c.Name, agents, judge, rounds)
	consensus := false
	// Safety net: a path that returns without an explicit finish still ends the
	// run instead of leaving a spinner in the dashboard forever.
	defer func() { s.swarms.finish(runID, swarmError, consensus, "interrupted") }()

	// answers[i] is agent i's latest answer; "" means it has not produced one.
	answers := make([]string, len(agents))
	usage := make([]tr.Usage, len(agents))
	live := make([]bool, len(agents))
	for i := range live {
		live[i] = true
	}

	for round := 0; round < rounds && ctx.Err() == nil; round++ {
		s.swarms.round(runID, round+1)
		next := make([]string, len(agents))
		done := make(chan struct {
			i     int
			text  string
			usage tr.Usage
			err   error
		}, len(agents))
		waiting := 0
		for i, model := range agents {
			if !live[i] {
				s.swarms.agent(runID, round+1, i, swarmSkipped, 0)
				continue
			}
			q := base
			// Debate answers only need to be good enough to argue with; the judge
			// writes the final reply with the client's full budget.
			if q.MaxTokens <= 0 || q.MaxTokens > swarmAgentMaxTokens {
				q.MaxTokens = swarmAgentMaxTokens
			}
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
			s.swarms.agent(runID, round+1, i, swarmThinking, 0)
			go func(i int, model string) {
				started := time.Now()
				resp, err := s.internalCall(ctx, p.r, &q, model)
				text := ""
				var used tr.Usage
				if err == nil {
					if text = strings.TrimSpace(partsText(resp.Parts)); text == "" {
						err = fmt.Errorf("%s: empty answer", model)
					} else {
						used = resp.Usage
					}
				}
				ms := time.Since(started).Milliseconds()
				switch {
				case err != nil:
					s.swarms.agent(runID, round+1, i, swarmError, ms)
				case text == "":
					s.swarms.agent(runID, round+1, i, swarmEmpty, ms)
				default:
					s.swarms.agent(runID, round+1, i, swarmOK, ms)
				}
				if text != "" {
					s.swarms.text(runID, round+1, i, text)
				}
				done <- struct {
					i     int
					text  string
					usage tr.Usage
					err   error
				}{i, text, used, err}
			}(i, model)
		}

		// The barrier: nobody starts the next round until this one resolves.
		// Once a quorum has answered, stragglers get swarmGraceAfter instead of
		// holding the whole swarm for the full round deadline.
		deadline := time.NewTimer(swarmRoundWait)
		quorum := min(2, waiting)
		answered := 0
		var grace <-chan time.Time
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
				usage[r.i] = r.usage
				answered++
				if answered == quorum && got < waiting && grace == nil {
					grace = time.After(swarmGraceAfter)
				}
			case <-grace:
				log.Printf("SWARM %s round %d: quorum answered; not waiting for the rest", c.Name, round+1)
				got = waiting
			case <-deadline.C:
				log.Printf("SWARM %s round %d: %d of %d agents answered in time", c.Name, round+1, got, waiting)
				got = waiting
			case <-ctx.Done():
				got = waiting
			}
		}
		deadline.Stop()
		s.swarms.settleRound(runID, round+1)
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
			consensus = true
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
		s.swarms.finish(runID, swarmError, consensus, "every swarm agent failed")
		writeError(w, p.cf, http.StatusServiceUnavailable, "api_error", "every swarm agent failed")
		return
	case 1:
		// A debate needs at least two voices; one answer is just the answer.
		log.Printf("SWARM %s: only %s answered; replying with it directly", c.Name, from[0])
		sw := &swarmStatusWriter{ResponseWriter: w}
		s.routeModel(sw, p, st, from[0])
		s.swarms.judgeText(runID, sw.judgeText())
		if sw.status >= 400 {
			s.swarms.judge(runID, swarmError)
		} else {
			s.swarms.judge(runID, swarmOK)
		}
		s.swarms.finish(runID, swarmDone, consensus, "")
		return
	}

	// The team already produced one answer: the judge would only rephrase it.
	// Return the longest copy so the most complete wording survives.
	if swarmSameText(texts) {
		best := texts[0]
		for _, t := range texts[1:] {
			if len(t) > len(best) {
				best = t
			}
		}
		var sum tr.Usage
		for i, a := range answers {
			if a != "" {
				sum.Input += usage[i].Input
				sum.Output += usage[i].Output
				sum.CacheRead += usage[i].CacheRead
				sum.CacheWrite += usage[i].CacheWrite
				sum.Reasoning += usage[i].Reasoning
			}
		}
		log.Printf("SWARM %s: %d agents agree exactly; skipping the judge", c.Name, len(texts))
		s.swarms.judge(runID, swarmSkipped)
		s.swarms.judgeText(runID, best)
		s.swarms.finish(runID, swarmDone, true, "")
		writeBufferedAnswer(w, p, from[0], &tr.Response{Model: from[0],
			Parts: []tr.Part{{Type: tr.PartText, Text: best}}, Finish: tr.FinishStop, Usage: sum})
		return
	}

	j := *ir
	j.Messages = append(append([]tr.Message{}, ir.Messages...),
		tr.Message{Role: "user", Parts: []tr.Part{{Type: tr.PartText, Text: swarmJudgePrompt(rounds, texts)}}})
	body, err := tr.EmitRequest(p.cf, &j, tr.EmitOptions{Model: judge})
	if err != nil {
		s.swarms.finish(runID, swarmError, consensus, "could not build the judge request")
		writeError(w, p.cf, http.StatusInternalServerError, "api_error", "could not build the judge request: "+err.Error())
		return
	}
	p2 := &proxyReq{r: p.r, cf: p.cf, body: body, model: judge, stream: p.stream}
	if err := scanHead(body, &p2.head); err != nil {
		s.swarms.finish(runID, swarmError, consensus, err.Error())
		writeError(w, p.cf, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	log.Printf("SWARM %s: judging %d answers after %d rounds with %s", c.Name, len(texts), rounds, judge)
	s.swarms.judge(runID, swarmThinking)
	sw := &swarmStatusWriter{ResponseWriter: w}
	s.routeModel(sw, p2, st, judge)
	s.swarms.judgeText(runID, sw.judgeText())
	if sw.status == 0 || sw.status >= 400 {
		s.swarms.judge(runID, swarmError)
	} else {
		s.swarms.judge(runID, swarmOK)
	}
	s.swarms.finish(runID, swarmDone, consensus, "")
}
