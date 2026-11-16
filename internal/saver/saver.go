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

	"github.com/zenpo/fastrouter/internal/config"
	tr "github.com/zenpo/fastrouter/internal/translate"
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
	caveman, ponytail     string
	custom                string    // user's own system prompt (9Router's customPrompt)
	Headroom              *Headroom // nil: off
	HeadroomURL           string
	HeadroomCodeAware     bool
	HeadroomKompress      bool
	HeadroomAutostartWant bool
}

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
	return s != nil && (s.RTK || s.caveman != "" || s.ponytail != "" || s.custom != "" || s.Headroom != nil)
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

// Apply runs RTK, Headroom, Caveman and Ponytail (9Router's order) on the
// final upstream body b in format f. It returns the new body and a tag such
// as "RTK:2,CAVEMAN:ultra" ("" when nothing ran).
func (s *Saver) Apply(ctx context.Context, b []byte, f tr.Format, model string) ([]byte, string) {
	if !s.Enabled() {
		return b, ""
	}
	var tags []string
	if s.RTK {
		var hits []string
		if b, hits = rtkBody(b); len(hits) > 0 {
			tags = append(tags, "RTK:"+strconv.Itoa(len(hits)))
		}
	}
	if s.Headroom != nil {
		var ok bool
		if b, ok = s.Headroom.Compress(ctx, b, f, model); ok {
			tags = append(tags, "HEADROOM")
		}
	}
	if s.caveman != "" {
		b = injectPrompt(b, f, s.caveman)
		tags = append(tags, "CAVEMAN:"+s.CavemanLevel)
	}
	if s.ponytail != "" {
		b = injectPrompt(b, f, s.ponytail)
		tags = append(tags, "PONYTAIL:"+s.PonytailLevel)
	}
	if s.custom != "" {
		b = injectPrompt(b, f, s.custom)
		tags = append(tags, "CUSTOMPROMPT:"+strconv.Itoa(len(s.custom))+"c")
	}
	return b, strings.Join(tags, ",")
}
