// Package bench measures how fast FastRouter answers IDE and CLI clients. It
// calls FastRouter's own /v1 endpoints over HTTP exactly as a client would and
// reads FastRouter's X-FastRouter-Timing trailer for its internal overhead.
package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zenpo/fastrouter/internal/hrtime"
)

// Endpoints a run can call.
const (
	Chat      = "chat"      // POST /v1/chat/completions (Cursor, Cline, Continue, most CLIs)
	Messages  = "messages"  // POST /v1/messages (Claude Code)
	Responses = "responses" // POST /v1/responses (Codex)
)

// Options configures one benchmark.
type Options struct {
	BaseURL   string   `json:"-"` // e.g. http://127.0.0.1:20128
	APIKey    string   `json:"-"` // FastRouter key; never stored or returned
	Models    []string `json:"models"`
	Endpoints []string `json:"endpoints"`
	Stream    bool     `json:"stream"`
	Runs      int      `json:"runs"`   // per model and endpoint
	Prompt    string   `json:"prompt"` // tiny | medium
	IDE       bool     `json:"ide"`    // Simulate IDE: system prompt + tools, per-client shape
	MaxTokens int      `json:"maxTokens"`
}

// Normalize fills defaults and checks limits.
func (o *Options) Normalize() error {
	if len(o.Models) == 0 {
		return errors.New("pick at least one model")
	}
	if len(o.Models) > 40 {
		return errors.New("at most 40 models per run")
	}
	if len(o.Endpoints) == 0 {
		o.Endpoints = []string{Chat}
	}
	seen := map[string]bool{}
	var eps []string
	for _, e := range o.Endpoints {
		switch e {
		case Chat, Messages, Responses:
			if !seen[e] {
				seen[e] = true
				eps = append(eps, e)
			}
		default:
			return fmt.Errorf("unknown endpoint %q (chat, messages, responses)", e)
		}
	}
	o.Endpoints = eps
	if o.Runs <= 0 {
		o.Runs = 3
	}
	if o.Runs > 20 {
		return errors.New("at most 20 runs")
	}
	switch o.Prompt {
	case "":
		o.Prompt = "tiny"
	case "tiny", "medium":
	default:
		return fmt.Errorf("prompt must be tiny or medium")
	}
	if o.MaxTokens <= 0 { // room for reasoning models to think and still answer
		o.MaxTokens = 512
		if o.Prompt == "medium" {
			o.MaxTokens = 1024
		}
	}
	return nil
}

// Total is the number of requests the options produce.
func (o *Options) Total() int { return len(o.Models) * len(o.Endpoints) * o.Runs }

// Result is one request.
type Result struct {
	Seq       int       `json:"seq"`
	At        time.Time `json:"at"`
	Model     string    `json:"model"`
	Endpoint  string    `json:"endpoint"`
	Stream    bool      `json:"stream"`
	OK        bool      `json:"ok"`
	Status    int       `json:"status"`
	Class     string    `json:"class,omitempty"`
	Message   string    `json:"message,omitempty"`
	TTFBMs    float64   `json:"ttfbMs"`  // response headers
	TTFTMs    float64   `json:"ttftMs"`  // first content token (= total when not streaming)
	TotalMs   float64   `json:"totalMs"` // whole answer read
	InTokens  int       `json:"inTokens,omitempty"`
	OutTokens int       `json:"outTokens"`
	Estimated bool      `json:"estimated,omitempty"` // OutTokens counted from characters (no usage sent)
	NoText    bool      `json:"noText,omitempty"`    // only hidden (reasoning) tokens came back: TTFT = total
	TokPerSec float64   `json:"tokPerSec"`

	// From FastRouter's timing trailer.
	PreMs      float64 `json:"preMs"`      // request in -> sent upstream
	PostMs     float64 `json:"postMs"`     // upstream ready -> client first byte
	HoldMs     float64 `json:"holdMs"`     // waiting for first real content (empty-answer guard)
	OverheadMs float64 `json:"overheadMs"` // pre + post
	UpTTFBMs   float64 `json:"upTtfbMs"`   // upstream headers
	Attempts   int     `json:"attempts,omitempty"`
	FailoverMs float64 `json:"failoverMs,omitempty"`
	Timed      bool    `json:"timed"` // the trailer arrived

	AccountID string `json:"-"`
	Account   string `json:"account,omitempty"` // masked display name
	Upstream  string `json:"upstream,omitempty"`
}

// Run executes the benchmark one request at a time (so runs do not slow each
// other down) and calls each(r) after every request. A cancelled ctx stops it.
func Run(ctx context.Context, c *http.Client, o Options, each func(Result)) []Result {
	if c == nil {
		c = Client()
	}
	var out []Result
	seq := 0
	for run := 0; run < o.Runs; run++ {
		for _, m := range o.Models {
			for _, ep := range o.Endpoints {
				if ctx.Err() != nil {
					return out
				}
				seq++
				r := One(ctx, c, o, m, ep)
				r.Seq = seq
				out = append(out, r)
				if each != nil {
					each(r)
				}
			}
		}
	}
	return out
}

// Client is an HTTP client like an IDE's: keep-alive, no compression.
func Client() *http.Client {
	return &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, MaxIdleConnsPerHost: 8,
		DisableCompression: true, ResponseHeaderTimeout: 2 * time.Minute}}
}

// One sends a single request.
func One(ctx context.Context, c *http.Client, o Options, model, ep string) Result {
	r := Result{At: time.Now(), Model: model, Endpoint: ep, Stream: o.Stream}
	body, path, hdr := Build(o, model, ep)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		r.Class, r.Message = "bad_request", err.Error()
		return r
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-FastRouter-Timing", "1")
	if o.APIKey != "" {
		if ep == Messages {
			req.Header.Set("x-api-key", o.APIKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+o.APIKey)
		}
	}
	start := hrtime.Now()
	resp, err := c.Do(req)
	if err != nil {
		r.TotalMs = ms(hrtime.Since(start))
		r.Class, r.Message = "network", scrub(err.Error(), o.APIKey)
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Timeout") {
			r.Class = "timeout"
		}
		return r
	}
	defer resp.Body.Close()
	r.TTFBMs = ms(hrtime.Since(start))
	r.Status = resp.StatusCode
	r.AccountID = resp.Header.Get("X-FastRouter-Account")
	r.Upstream = resp.Header.Get("X-FastRouter-Model")

	var text strings.Builder
	var firstTok time.Duration
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		r.TotalMs = ms(hrtime.Since(start))
		r.Class, r.Message = classify(resp.StatusCode, errMessage(b)), scrub(clip(errMessage(b), 300), o.APIKey)
		readTiming(&r, resp.Trailer)
		return r
	}
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
	var streamErr string
	if isSSE {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 8<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			d := bytes.TrimSpace(line[5:])
			if len(d) == 0 || d[0] != '{' {
				continue
			}
			t, in, out, e := parseEvent(d)
			if t != "" {
				if firstTok == 0 {
					firstTok = hrtime.Since(start)
				}
				text.WriteString(t)
			}
			if in > 0 {
				r.InTokens = in
			}
			if out > 0 {
				r.OutTokens = out
			}
			if e != "" {
				streamErr = e
			}
		}
		if err := sc.Err(); err != nil && streamErr == "" {
			streamErr = err.Error()
		}
	} else {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			streamErr = err.Error()
		}
		t, in, out := parseJSON(b)
		text.WriteString(t)
		r.InTokens, r.OutTokens = in, out
	}
	total := hrtime.Since(start)
	r.TotalMs = ms(total)
	if firstTok == 0 {
		firstTok = total
	}
	r.TTFTMs = ms(firstTok)
	readTiming(&r, resp.Trailer)
	if r.OutTokens == 0 && text.Len() > 0 {
		r.OutTokens, r.Estimated = max(1, (len([]rune(text.String()))+3)/4), true
	}
	gen := total - firstTok
	if !isSSE || gen < 50*time.Millisecond {
		gen = total
	}
	if r.OutTokens > 0 && gen > 0 {
		r.TokPerSec = math.Round(float64(r.OutTokens)/gen.Seconds()*10) / 10
	}
	switch {
	case streamErr != "":
		r.Class, r.Message = "upstream", scrub(clip(streamErr, 300), o.APIKey)
	case text.Len() == 0 && r.OutTokens == 0:
		r.Class, r.Message = "empty", "the answer had no text"
	default:
		r.OK, r.NoText = true, text.Len() == 0
	}
	return r
}

// ParseTimingHeader reads an X-FastRouter-Timing value into a Result.
func ParseTimingHeader(h http.Header) Result {
	var r Result
	readTiming(&r, h)
	return r
}

func readTiming(r *Result, h http.Header) {
	v := h.Get("X-FastRouter-Timing")
	if v == "" {
		return
	}
	r.Timed = true
	for _, kv := range strings.Split(v, ";") {
		k, val, _ := strings.Cut(kv, "=")
		f, _ := strconv.ParseFloat(val, 64)
		switch k {
		case "pre":
			r.PreMs = f
		case "post":
			r.PostMs = f
		case "hold":
			r.HoldMs = f
		case "upttfb":
			r.UpTTFBMs = f
		case "attempts":
			r.Attempts = int(f)
		case "failover":
			r.FailoverMs = f
		}
	}
	r.OverheadMs = math.Round((r.PreMs+r.PostMs)*100) / 100
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/10) / 100 }

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func scrub(s, key string) string {
	if len(key) >= 6 {
		s = strings.ReplaceAll(s, key, "[redacted]")
	}
	return s
}

// parseEvent pulls text and usage out of one SSE data payload in any of the
// three client formats.
func parseEvent(d []byte) (text string, in, out int, errMsg string) {
	var e struct {
		Type    string `json:"type"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Delta    json.RawMessage         `json:"delta"` // object (Anthropic) or string (Responses)
		Usage    *usage                  `json:"usage"`
		Message  *struct{ Usage *usage } `json:"message"`
		Response *struct{ Usage *usage } `json:"response"`
		Error    json.RawMessage         `json:"error"`
	}
	if json.Unmarshal(d, &e) != nil {
		return
	}
	for _, c := range e.Choices {
		text += c.Delta.Content + c.Delta.ReasoningContent + c.Delta.Reasoning
		for _, tc := range c.Delta.ToolCalls {
			text += tc.Function.Arguments
		}
	}
	switch e.Type {
	case "content_block_delta":
		var cd struct {
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		}
		_ = json.Unmarshal(e.Delta, &cd)
		text += cd.Text + cd.Thinking + cd.PartialJSON
	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
		var sd string
		_ = json.Unmarshal(e.Delta, &sd)
		text += sd
	case "error", "response.failed":
		errMsg = errMessage(d)
	}
	for _, u := range []*usage{e.Usage, msgUsage(e.Message), msgUsage(e.Response)} {
		if u != nil {
			in, out = max(in, u.in()), max(out, u.out())
		}
	}
	if len(e.Error) > 0 && string(e.Error) != "null" && errMsg == "" {
		errMsg = errMessage(d)
	}
	return
}

func msgUsage(m *struct{ Usage *usage }) *usage {
	if m == nil {
		return nil
	}
	return m.Usage
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

func (u *usage) in() int  { return max(u.PromptTokens, u.InputTokens) }
func (u *usage) out() int { return max(u.CompletionTokens, u.OutputTokens) }

// parseJSON reads a whole (non-streamed) answer in any client format.
func parseJSON(b []byte) (text string, in, out int) {
	var v struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Content []struct {
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		Output []struct {
			Arguments string `json:"arguments"`
			Content   []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *usage `json:"usage"`
	}
	if json.Unmarshal(b, &v) != nil {
		return
	}
	for _, c := range v.Choices {
		text += c.Message.Content + c.Message.ReasoningContent
		for _, tc := range c.Message.ToolCalls {
			text += tc.Function.Arguments
		}
	}
	for _, c := range v.Content {
		text += c.Text + c.Thinking
		if len(c.Input) > 2 {
			text += string(c.Input)
		}
	}
	for _, o := range v.Output {
		text += o.Arguments
		for _, c := range o.Content {
			text += c.Text
		}
	}
	if v.Usage != nil {
		in, out = v.Usage.in(), v.Usage.out()
	}
	return
}

func errMessage(b []byte) string {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(b, &v) == nil {
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(v.Error, &inner) == nil && inner.Message != "" {
			return inner.Message
		}
		var s string
		if json.Unmarshal(v.Error, &s) == nil && s != "" {
			return s
		}
		if v.Message != "" {
			return v.Message
		}
	}
	return strings.TrimSpace(string(b))
}

// classify matches FastRouter's error classes (see server/livetest.go).
func classify(status int, msg string) string {
	l := strings.ToLower(msg)
	switch {
	case status == 401 || status == 403 && (strings.Contains(l, "key") || strings.Contains(l, "auth")):
		return "auth"
	case status == 403 && (strings.Contains(l, "terms of service") || strings.Contains(l, "disabled")):
		return "banned"
	case status == 402 || strings.Contains(l, "quota") || strings.Contains(l, "balance") || strings.Contains(l, "credit"):
		return "quota"
	case status == 429:
		return "rate_limit"
	case status == 404 || strings.Contains(l, "model"):
		return "model"
	case status == 400 || status == 413 || status == 422:
		return "bad_request"
	case status == 408 || status == 504:
		return "timeout"
	case status == 403:
		return "auth"
	}
	return "upstream"
}

// Stat summarises the passing runs of one model and endpoint.
type Stat struct {
	Model    string  `json:"model"`
	Endpoint string  `json:"endpoint"`
	Runs     int     `json:"runs"`
	OK       int     `json:"ok"`
	TTFT     Dist    `json:"ttft"`
	Total    Dist    `json:"total"`
	Overhead Dist    `json:"overhead"`
	TokPS    float64 `json:"tokPerSec"`
	Accounts string  `json:"accounts,omitempty"`
	Errors   string  `json:"errors,omitempty"`
}

// Dist is min / avg / p50 / p95 in milliseconds.
type Dist struct {
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// NewDist summarises xs (nearest-rank percentiles).
func NewDist(xs []float64) Dist {
	if len(xs) == 0 {
		return Dist{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	sum := 0.0
	for _, x := range s {
		sum += x
	}
	pct := func(p float64) float64 { return s[int(math.Ceil(p*float64(len(s))))-1] }
	r := func(v float64) float64 { return math.Round(v*100) / 100 }
	return Dist{Min: r(s[0]), Avg: r(sum / float64(len(s))), P50: r(pct(.5)), P95: r(pct(.95)), Max: r(s[len(s)-1])}
}

// Summarize groups results by model and endpoint, in first-seen order.
func Summarize(rs []Result) []Stat {
	var keys []string
	by := map[string][]Result{}
	for _, r := range rs {
		k := r.Model + "\x00" + r.Endpoint
		if _, ok := by[k]; !ok {
			keys = append(keys, k)
		}
		by[k] = append(by[k], r)
	}
	out := make([]Stat, 0, len(keys))
	for _, k := range keys {
		g := by[k]
		st := Stat{Model: g[0].Model, Endpoint: g[0].Endpoint, Runs: len(g)}
		var ttft, total, over, tps []float64
		accts, errs := map[string]int{}, map[string]int{}
		for _, r := range g {
			if !r.OK {
				errs[r.Class]++
				continue
			}
			st.OK++
			total = append(total, r.TotalMs)
			if !r.NoText {
				ttft = append(ttft, r.TTFTMs)
			}
			if r.Timed {
				over = append(over, r.OverheadMs)
			}
			if r.TokPerSec > 0 {
				tps = append(tps, r.TokPerSec)
			}
			if r.Account != "" {
				accts[r.Account]++
			}
		}
		st.TTFT, st.Total, st.Overhead = NewDist(ttft), NewDist(total), NewDist(over)
		if len(tps) > 0 {
			st.TokPS = NewDist(tps).Avg
		}
		st.Accounts, st.Errors = joinCounts(accts), joinCounts(errs)
		out = append(out, st)
	}
	return out
}

func joinCounts(m map[string]int) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for i, k := range ks {
		if m[k] > 1 {
			ks[i] = k + " ×" + strconv.Itoa(m[k])
		}
	}
	return strings.Join(ks, ", ")
}
