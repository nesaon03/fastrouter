package clitools

import (
	"regexp"
	"strings"
)

// Hermes Agent config.yaml editing, ported from 9Router's hermesYaml.js.

var (
	hModelBlock = regexp.MustCompile(`(?m)^model:[ \t]*\n((?:[ \t]+.*\n?|[ \t]*\n)*)`)
	hModelScal  = regexp.MustCompile(`(?m)^model:[ \t]*[^\n]*\n?`)
	hDelegation = regexp.MustCompile(`(?m)^delegation:[ \t]*\n((?:[ \t]+.*\n?|[ \t]*\n)*)`)
	hAux        = regexp.MustCompile(`(?m)^auxiliary:[ \t]*\n((?:[ \t]+.*\n?|[ \t]*\n)*)`)
	hAuxSub     = regexp.MustCompile(`(?m)^  ([A-Za-z0-9_]+):[ \t]*\n((?:[ \t]{4,}.*\n?|[ \t]*\n)*)`)
	hStt        = regexp.MustCompile(`(?m)^stt:[ \t]*\n((?:[ \t]+.*\n?|[ \t]*\n)*)`)
)

const hermesKeyRef = "${OPENAI_API_KEY}"

func hAuxRole(role string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(role) + `:[ \t]*\n(?:[ \t]{4,}.*\n?|[ \t]*\n)*`)
}

func yget(body, key string) string {
	m := regexp.MustCompile(`(?m)^[ \t]+` + regexp.QuoteMeta(key) + `:[ \t]*["']?([^"'\n]+)["']?`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

type hBlock struct{ Model, Provider, BaseURL string }

func hermesModel(y string) *hBlock {
	m := hModelBlock.FindStringSubmatch(y)
	if m == nil {
		return nil
	}
	return &hBlock{yget(m[1], "default"), yget(m[1], "provider"), yget(m[1], "base_url")}
}

func hermesDelegation(y string) *hBlock {
	m := hDelegation.FindStringSubmatch(y)
	if m == nil {
		return nil
	}
	return &hBlock{yget(m[1], "model"), yget(m[1], "provider"), yget(m[1], "base_url")}
}

func hermesAux(y string) map[string]hBlock {
	out := map[string]hBlock{}
	m := hAux.FindStringSubmatch(y)
	if m == nil {
		return out
	}
	for _, sm := range hAuxSub.FindAllStringSubmatch(m[1], -1) {
		out[sm[1]] = hBlock{yget(sm[2], "model"), yget(sm[2], "provider"), yget(sm[2], "base_url")}
	}
	return out
}

func ensureNL(y string) string {
	if y == "" || strings.HasSuffix(y, "\n") {
		return y
	}
	return y + "\n"
}

func hermesSetModel(y, model, base string) string {
	block := "model:\n  default: \"" + model + "\"\n  provider: \"custom\"\n  base_url: \"" + base + "\"\n  api_key: " + hermesKeyRef + "\n"
	if hModelBlock.MatchString(y) {
		return replaceFirst(hModelBlock, y, block)
	}
	if hModelScal.MatchString(y) {
		return replaceFirst(hModelScal, y, block)
	}
	if y == "" {
		return block
	}
	return block + "\n" + y
}

func hermesSetDelegation(y, model, base string) string {
	block := "delegation:\n  model: \"" + model + "\"\n  provider: \"custom\"\n  base_url: \"" + base + "\"\n  api_key: " + hermesKeyRef + "\n"
	if hDelegation.MatchString(y) {
		return replaceFirst(hDelegation, y, block)
	}
	return ensureNL(y) + block
}

func hermesSetAux(y, role, model, base string) string {
	block := "  " + role + ":\n    provider: \"custom\"\n    model: \"" + model + "\"\n    base_url: \"" + base + "\"\n    api_key: " + hermesKeyRef + "\n"
	m := hAux.FindStringSubmatch(y)
	if m == nil {
		return ensureNL(y) + "auxiliary:\n" + block
	}
	re := hAuxRole(role)
	body := m[1]
	if re.MatchString(body) {
		body = replaceFirst(re, body, block)
	} else {
		body = ensureNL(body) + block
	}
	return replaceFirst(hAux, y, "auxiliary:\n"+body)
}

func hermesDelAux(y, role string) string {
	m := hAux.FindStringSubmatch(y)
	if m == nil {
		return y
	}
	body := replaceFirst(hAuxRole(role), m[1], "")
	if strings.TrimSpace(body) == "" {
		return replaceFirst(hAux, y, "")
	}
	return replaceFirst(hAux, y, "auxiliary:\n"+body)
}

type hSTT struct{ Enabled, Provider, Model, BaseURL, APIKey string }

func hermesSTT(y string) *hSTT {
	m := hStt.FindStringSubmatch(y)
	if m == nil {
		return nil
	}
	s := &hSTT{}
	cur := ""
	for _, l := range strings.Split(m[1], "\n") {
		if regexp.MustCompile(`^  \S`).MatchString(l) {
			kv := regexp.MustCompile(`^  ([A-Za-z0-9_]+):[ \t]*(.*)$`).FindStringSubmatch(l)
			cur = ""
			if kv != nil && kv[2] != "" {
				v := strings.Trim(strings.TrimSpace(kv[2]), `"'`)
				switch kv[1] {
				case "enabled":
					s.Enabled = v
				case "provider":
					s.Provider = v
				}
			} else if kv != nil {
				cur = kv[1]
			}
		} else if cur == "openai" {
			kv := regexp.MustCompile(`^ {4,}([A-Za-z0-9_]+):[ \t]*(.*)$`).FindStringSubmatch(l)
			if kv != nil && kv[2] != "" {
				v := strings.Trim(strings.TrimSpace(kv[2]), `"'`)
				switch kv[1] {
				case "model":
					s.Model = v
				case "base_url":
					s.BaseURL = v
				case "api_key":
					s.APIKey = v
				}
			}
		}
	}
	return s
}

func (s *hSTT) ours() bool {
	return s != nil && s.Provider == "openai" && s.BaseURL != "" && s.APIKey == hermesKeyRef
}

// hermesSetSTT replaces the stt block with FastRouter's audio transcription settings.
func hermesSetSTT(y, model, base string) string {
	block := "stt:\n  enabled: true\n  provider: \"openai\"\n  openai:\n    model: \"" + model + "\"\n    base_url: \"" + base + "\"\n    api_key: " + hermesKeyRef + "\n"
	if hStt.MatchString(y) {
		return replaceFirst(hStt, y, block)
	}
	return ensureNL(y) + block
}

func hermesDelSTT(y string) string { return strings.TrimLeft(replaceFirst(hStt, y, ""), "\n") }

func replaceFirst(re *regexp.Regexp, s, repl string) string {
	loc := re.FindStringIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[0]] + repl + s[loc[1]:]
}

func upsertEnv(text, key, value string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=.*$`)
	line := key + "=" + value
	if re.MatchString(text) {
		return replaceFirst(re, text, line)
	}
	return ensureNL(text) + line + "\n"
}

func removeEnv(text, key string) string {
	return replaceFirst(regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(key)+`=.*\n?`), text, "")
}
