package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// M6: the request head is read with one byte scan of the top-level object
// instead of a full json.Unmarshal, so large agent contexts (0.5-2 MB) cost
// one pass; nested values are skipped, not decoded.

var errNotObject = errors.New("request body must be a JSON object")
var errDupModel = errors.New(`request body has more than one top-level "model"`)

// scanHead fills h from the top-level fields of body.
func scanHead(body []byte, h *reqHead) error {
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return errNotObject
	}
	i = skipSpace(body, i+1)
	if i < len(body) && body[i] == '}' {
		return nil
	}
	sawModel := false
	for i < len(body) {
		if body[i] != '"' {
			return errNotObject
		}
		kend := skipString(body, i)
		if kend < 0 {
			return errNotObject
		}
		key := body[i+1 : kend-1]
		j := skipSpace(body, kend)
		if j >= len(body) || body[j] != ':' {
			return errNotObject
		}
		v := skipSpace(body, j+1)
		vend := skipValue(body, v)
		if vend < 0 {
			return errNotObject
		}
		raw := body[v:vend]
		switch string(key) {
		case "model":
			if sawModel {
				return errDupModel // N2: routers and upstreams could disagree on which one wins
			}
			sawModel = true
			if len(raw) > 0 && raw[0] == '"' {
				if err := json.Unmarshal(raw, &h.Model); err != nil {
					return errNotObject
				}
			}
		case "stream":
			h.Stream = string(raw) == "true"
		case "max_tokens":
			h.MaxTokens, _ = strconv.Atoi(string(raw))
		case "temperature":
			h.Temperature = raw
		case "reasoning_effort":
			h.ReasoningEffort = raw
		case "thinking":
			h.Thinking = raw
		case "reasoning":
			h.Reasoning = raw
		case "output_config":
			h.OutputConfig = raw
		case "stream_options":
			h.StreamOptions = raw
		}
		i = skipSpace(body, vend)
		if i < len(body) && body[i] == ',' {
			i = skipSpace(body, i+1)
			continue
		}
		if i < len(body) && body[i] == '}' {
			return nil
		}
		return errNotObject
	}
	return errNotObject
}

// skipValue returns the index just past the JSON value starting at body[i], or -1.
func skipValue(body []byte, i int) int {
	if i >= len(body) {
		return -1
	}
	switch body[i] {
	case '"':
		return skipString(body, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(body); j++ {
			switch body[j] {
			case '"':
				end := skipString(body, j)
				if end < 0 {
					return -1
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	}
	j := i
	for j < len(body) && body[j] != ',' && body[j] != '}' && body[j] != ']' && body[j] != ' ' && body[j] != '\n' && body[j] != '\r' && body[j] != '\t' {
		j++
	}
	if j == i {
		return -1
	}
	return j
}

var errTooLarge = errors.New("request body too large")

// readBody reads the request body into a buffer sized from Content-Length
// (no doubling growth), refusing bodies over limit (L2: 413, not truncation).
func readBody(r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, errTooLarge
	}
	if r.ContentLength > 0 {
		buf := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, err
}

// patchBody sets the top-level string `model` (when newModel != "") and adds
// a top-level field in a single output copy.
func patchBody(b []byte, newModel, addKey, addRaw string) []byte {
	start := skipSpace(b, 0)
	if start >= len(b) || b[start] != '{' {
		return b
	}
	v, vend := -1, -1
	if newModel != "" {
		v, vend = findTopLevelString(b, "model")
	}
	extra := 0
	if addKey != "" {
		extra = len(addKey) + len(addRaw) + 4
	}
	out := make([]byte, 0, len(b)+len(newModel)+extra+2)
	out = append(out, b[:start+1]...)
	if addKey != "" {
		out = append(out, strconv.Quote(addKey)...)
		out = append(out, ':')
		out = append(out, addRaw...)
		if j := skipSpace(b, start+1); j < len(b) && b[j] != '}' {
			out = append(out, ',')
		}
	}
	if v < 0 {
		return append(out, b[start+1:]...)
	}
	out = append(out, b[start+1:v]...)
	out = append(out, strconv.Quote(newModel)...)
	return append(out, b[vend:]...)
}

// findTopLevelString returns the span of the top-level string value of key, or -1s.
func findTopLevelString(body []byte, key string) (int, int) {
	out, ok := replaceTopLevelStringSpan(body, key)
	if !ok {
		return -1, -1
	}
	return out[0], out[1]
}
