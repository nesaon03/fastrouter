package translate

import "strings"

// Collector folds stream events into a complete Response, e.g. when a client
// asked for JSON but the upstream only streams.
type Collector struct {
	r     Response
	cur   *Part // open text/thinking part
	tools map[int]int
}

// Add folds events into the response.
func (c *Collector) Add(evs []Event) {
	for _, e := range evs {
		switch e.Kind {
		case EvStart:
			c.r.ID, c.r.Model = firstNonEmpty(c.r.ID, e.ID), firstNonEmpty(c.r.Model, e.Name)
		case EvText, EvThinking:
			t := PartText
			if e.Kind == EvThinking {
				t = PartThinking
			}
			if c.cur == nil || c.cur.Type != t {
				c.r.Parts = append(c.r.Parts, Part{Type: t})
				c.cur = &c.r.Parts[len(c.r.Parts)-1]
			}
			c.cur.Text += e.Text
		case EvSignature:
			if e.Name == "tool" {
				if n := len(c.r.Parts); n > 0 && c.r.Parts[n-1].Type == PartToolCall {
					c.r.Parts[n-1].Signature = e.Text
				}
				continue
			}
			if c.cur == nil || c.cur.Type != PartThinking {
				c.r.Parts = append(c.r.Parts, Part{Type: PartThinking})
				c.cur = &c.r.Parts[len(c.r.Parts)-1]
			}
			c.cur.Signature += e.Text
			c.cur = nil
		case EvRedacted:
			c.r.Parts = append(c.r.Parts, Part{Type: PartRedactedThinking, Data: e.Text})
			c.cur = nil
		case EvToolStart:
			if c.tools == nil {
				c.tools = map[int]int{}
			}
			c.r.Parts = append(c.r.Parts, Part{Type: PartToolCall, ToolCallID: e.ID, ToolName: e.Name})
			c.tools[e.Index] = len(c.r.Parts) - 1
			c.cur = nil
		case EvToolArgs:
			if i, ok := c.tools[e.Index]; ok {
				c.r.Parts[i].Args += e.Text
			}
		case EvUsage:
			c.r.Usage = e.Usage
		case EvFinish:
			c.r.Finish = e.Finish
		}
	}
}

// Response returns the folded response.
func (c *Collector) Response() *Response {
	for i := range c.r.Parts {
		if p := &c.r.Parts[i]; p.Type == PartToolCall && strings.TrimSpace(p.Args) == "" {
			p.Args = "{}"
		}
	}
	return &c.r
}

// ResponseEvents turns a complete response into stream events, e.g. when a
// client asked to stream but the upstream answered with plain JSON.
func ResponseEvents(r *Response) []Event {
	evs := []Event{{Kind: EvStart, ID: r.ID, Name: r.Model}}
	tool := 0
	for _, p := range r.Parts {
		switch p.Type {
		case PartText:
			evs = append(evs, Event{Kind: EvText, Text: p.Text})
		case PartThinking:
			evs = append(evs, Event{Kind: EvThinking, Text: p.Text})
			if p.Signature != "" {
				evs = append(evs, Event{Kind: EvSignature, Text: p.Signature})
			}
		case PartRedactedThinking:
			evs = append(evs, Event{Kind: EvRedacted, Text: p.Data})
		case PartToolCall:
			evs = append(evs, Event{Kind: EvToolStart, Index: tool, ID: p.ToolCallID, Name: p.ToolName},
				Event{Kind: EvToolArgs, Index: tool, Text: p.Args})
			if p.Signature != "" {
				evs = append(evs, Event{Kind: EvSignature, Name: "tool", Text: p.Signature})
			}
			tool++
		}
	}
	return append(evs, Event{Kind: EvUsage, Usage: r.Usage}, Event{Kind: EvFinish, Finish: r.Finish})
}
