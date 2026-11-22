// Package saver implements 9Router's token savers: RTK tool-output
// compression, the Headroom proxy, and the Caveman and Ponytail prompts.
package saver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nesaon03/fastrouter/internal/config"
	tr "github.com/nesaon03/fastrouter/internal/translate"
)

// OptOutHeaders turn every saver off for one request when set to "off".
var OptOutHeaders = []string{"X-9router-Token-Saver", "X-Fastrouter-Token-Saver"}

// TagHeader reports on the response which savers ran.
const TagHeader = "X-FastRouter-Token-Saver"

// Saver applies the enabled savers to upstream bodies.
type Saver struct {
	RTK                   bool
	CavemanLevel          string
	PonytailLevel         string
	CompactKeep           int  // keep the last N tool results; 0 = off
	Dedupe                bool // replace repeated identical tool output
	caveman, ponytail     string
	custom                string    // user's own system prompt (9Router's customPrompt)
	Headroom              *Headroom // nil: off
	HeadroomURL           string
	HeadroomCodeAware     bool
	HeadroomKompress      bool
	HeadroomAutostartWant bool
}

// Stats describes what one Apply call changed, in bytes.
type Stats struct {
	Tag                     string
	RTKIn, RTKOut           int
	HeadroomIn, HeadroomOut int
	CompactIn, CompactOut   int
	PromptAdded             int
}

// SavedBytes is the gross input reduction from content compression.
func (st Stats) SavedBytes() int {
	return (st.RTKIn - st.RTKOut) + (st.HeadroomIn - st.HeadroomOut) + (st.CompactIn - st.CompactOut)
}

// NetBytes subtracts the prompt text injected by the prompt savers.
func (st Stats) NetBytes() int { return st.SavedBytes() - st.PromptAdded }

// PromptCost is the estimated input-token cost of each prompt saver.
type PromptCost struct{ Caveman, Ponytail, Custom int64 }

// PromptTokens estimates the input tokens the prompt savers add per request.
func (p PromptCost) Total() int64 { return p.Caveman + p.Ponytail + p.Custom }

// New resolves settings: config overrides first, then imported 9Router
// settings, then 9Router's defaults.
func New(settings map[string]json.RawMessage, o config.TokenSavers) *Saver {
	b := func(key string, def bool, ov *bool) bool {
		if ov != nil {
			return *ov
		}
		v := def
		if raw, ok := settings[key]; ok {
			_ = json.Unmarshal(raw, &v)
		}
		return v
	}
	str := func(key, def string, ov *string) string {
		if ov != nil {
			return *ov
		}
		v := ""
		if raw, ok := settings[key]; ok {
			_ = json.Unmarshal(raw, &v)
		}
		if v == "" {
			v = def
		}
		return v
	}
	level := func(enabledKey, levelKey string, ov *string) string {
		if ov != nil {
			if *ov == "off" {
				return ""
			}
			return *ov
		}
		if !b(enabledKey, false, nil) {
			return ""
		}
		return str(levelKey, "full", nil)
	}
	s := &Saver{
		RTK:                   b("rtkEnabled", true, o.RTK),
		CavemanLevel:          level("cavemanEnabled", "cavemanLevel", o.Caveman),
		PonytailLevel:         level("ponytailEnabled", "ponytailLevel", o.Ponytail),
		HeadroomURL:           str("headroomUrl", "http://localhost:8787", o.HeadroomURL),
		HeadroomCodeAware:     b("headroomCodeAware", false, o.HeadroomCodeAware),
		HeadroomKompress:      b("headroomKompress", true, o.HeadroomKompress),
		HeadroomAutostartWant: o.HeadroomAutostart,
	}
	s.caveman = CavemanPrompts[s.CavemanLevel]
	s.ponytail = PonytailPrompts[s.PonytailLevel]
	if s.caveman == "" {
		s.CavemanLevel = ""
	}
	if s.ponytail == "" {
		s.PonytailLevel = ""
	}
	if b("customPromptEnabled", false, nil) {
		s.custom = strings.TrimSpace(str("customPrompt", "", nil))
	}
	if b("compactEnabled", false, nil) {
		s.CompactKeep = 6
		if raw, ok := settings["compactKeepToolResults"]; ok {
			var n int
			if json.Unmarshal(raw, &n) == nil && n > 0 {
				s.CompactKeep = n
			}
		}
	}
	s.Dedupe = b("dedupeEnabled", false, nil)
	if b("headroomEnabled", false, o.Headroom) && s.HeadroomURL != "" {
		timeout := 3 * time.Second
		if o.HeadroomTimeout != nil {
			timeout = time.Duration(*o.HeadroomTimeout)
		} else if raw, ok := settings["headroomTimeoutMs"]; ok {
			var ms int
			if json.Unmarshal(raw, &ms) == nil && ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
		}
		s.Headroom = NewHeadroom(s.HeadroomURL, timeout, b("headroomCompressUserMessages", false, o.CompressUserMessages))
	}
	return s
}

// Enabled reports whether any saver is on.
func (s *Saver) Enabled() bool {
	return s != nil && (s.RTK || s.caveman != "" || s.ponytail != "" || s.custom != "" || s.Headroom != nil || s.CompactKeep > 0 || s.Dedupe)
}

// estimateTokens approximates tokens from bytes (4 bytes per token, rounded up).
func estimateTokens(n int) int64 {
	if n <= 0 {
		return 0
	}
	return int64((n + 3) / 4)
}

// PromptCost estimates the input tokens each prompt saver adds per request.
func (s *Saver) PromptCost() PromptCost {
	if s == nil {
		return PromptCost{}
	}
	return PromptCost{
		Caveman:  estimateTokens(len(s.caveman)),
		Ponytail: estimateTokens(len(s.ponytail)),
		Custom:   estimateTokens(len(s.custom)),
	}
}

// OptedOut reports whether the client turned savers off for this request.
func OptedOut(h http.Header) bool {
	for _, k := range OptOutHeaders {
		if strings.EqualFold(h.Get(k), "off") {
			return true
		}
	}
	return false
}

// Apply runs compaction, RTK, Headroom, Caveman and Ponytail (9Router's order,
// with compaction first) on the final upstream body b in format f. It returns
// the new body and what changed, including the tag such as
// "COMPACT:120,RTK:2,CAVEMAN:ultra".
func (s *Saver) Apply(ctx context.Context, b []byte, f tr.Format, model string) ([]byte, Stats) {
	st := Stats{}
	if !s.Enabled() {
		return b, st
	}
	var tags []string
	if s.CompactKeep > 0 || s.Dedupe {
		st.CompactIn = len(b)
		b = compactBody(b, s.CompactKeep, s.Dedupe)
		st.CompactOut = len(b)
		if saved := st.CompactIn - st.CompactOut; saved > 0 {
			tags = append(tags, "COMPACT:"+strconv.Itoa(saved))
		}
	}
	if s.RTK {
		st.RTKIn = len(b)
		var hits []string
		if b, hits = rtkBody(b); len(hits) > 0 {
			tags = append(tags, "RTK:"+strconv.Itoa(len(hits)))
		}
		st.RTKOut = len(b)
	}
	if s.Headroom != nil {
		st.HeadroomIn = len(b)
		var ok bool
		if b, ok = s.Headroom.Compress(ctx, b, f, model); ok {
			tags = append(tags, "HEADROOM")
		}
		st.HeadroomOut = len(b)
	}
	if s.caveman != "" {
		before := len(b)
		b = injectPrompt(b, f, s.caveman)
		st.PromptAdded += len(b) - before
		tags = append(tags, "CAVEMAN:"+s.CavemanLevel)
	}
	if s.ponytail != "" {
		before := len(b)
		b = injectPrompt(b, f, s.ponytail)
		st.PromptAdded += len(b) - before
		tags = append(tags, "PONYTAIL:"+s.PonytailLevel)
	}
	if s.custom != "" {
		before := len(b)
		b = injectPrompt(b, f, s.custom)
		st.PromptAdded += len(b) - before
		tags = append(tags, "CUSTOMPROMPT:"+strconv.Itoa(len(s.custom))+"c")
	}
	st.Tag = strings.Join(tags, ",")
	return b, st
}
