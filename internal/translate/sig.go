package translate

import (
	"bytes"
	"strings"
)

// Thinking signatures are only valid at the provider family that issued
// them (H4). When FastRouter hands a signature to a client in a different
// format, it tags it with the issuing family: "frsig:<format>:<signature>".
// On the way back the tag decides whether the signature is restored (same
// family) or dropped (any other family); untagged signatures are native to
// the client's own format.

const sigTag = "frsig:"

// TagSignature tags sig as issued by an upstream speaking from.
func TagSignature(from Format, sig string) string {
	if sig == "" || strings.HasPrefix(sig, sigTag) {
		return sig
	}
	return sigTag + from.String() + ":" + sig
}

// TagEvents tags thinking signatures in evs that came from an upstream in
// format from and are going to a client in another format.
func TagEvents(evs []Event, from Format) {
	for i := range evs {
		if evs[i].Kind == EvSignature && evs[i].Name != "tool" {
			evs[i].Text = TagSignature(from, evs[i].Text)
		}
	}
}

// TagResponse does the same for a complete response.
func TagResponse(r *Response, from Format) {
	for i := range r.Parts {
		if r.Parts[i].Type == PartThinking {
			r.Parts[i].Signature = TagSignature(from, r.Parts[i].Signature)
		}
	}
}

// HasTaggedSignature is a cheap check for client bodies that carry tagged
// signatures (so passthrough must re-encode them).
func HasTaggedSignature(body []byte) bool { return bytes.Contains(body, []byte(sigTag)) }

// sigFor returns the signature to send to an upstream speaking to, or "".
func sigFor(sig string, from, to Format) string {
	if sig == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(sig, sigTag); ok {
		fam, s, ok := strings.Cut(rest, ":")
		if ok && fam == to.String() {
			return s
		}
		return ""
	}
	if from == to {
		return sig
	}
	return ""
}

// resolveSignatures returns r with every signature either restored for `to`
// or removed. r itself is not modified (it may be reused for other targets).
func resolveSignatures(r *Request, to Format) *Request {
	changed := false
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			if p.Signature != "" && sigFor(p.Signature, r.From, to) != p.Signature {
				changed = true
			}
		}
	}
	out := *r
	out.From = to // signatures are now valid for `to`
	if !changed {
		return &out
	}
	out.Messages = make([]Message, len(r.Messages))
	for i, m := range r.Messages {
		parts := make([]Part, len(m.Parts))
		copy(parts, m.Parts)
		for j := range parts {
			parts[j].Signature = sigFor(parts[j].Signature, r.From, to)
		}
		out.Messages[i] = Message{Role: m.Role, Parts: parts}
	}
	return &out
}
