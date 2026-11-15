package catalog

import (
	"regexp"
	"strings"
)

// Name-based vision detection, after 9Router's visionPatterns.js (MIT):
// only ever used to turn vision ON for a model nobody declared.
var (
	notVision  = regexp.MustCompile(`(?i)(^|[-_/:.])(image|img)([-_/:.]|$)|stable-image|nanobanana|imagine|t2v|i2v|flux|dall|sdxl|diffusion|embed|rerank|guard|moderation|tts|stt|whisper|voice|speech|audio`)
	visionName = regexp.MustCompile(`(?i)(^|[-_/:.])(vision|vl|vlm|multimodal|omni|visual)([-_/:.]|$)|[0-9]\.[0-9]+v([-_/:.]|$)|(^|[-_/:.])glm-[0-9]+v([-_/:.]|$)|(^|[-_/:.])(llava|pixtral|internvl|cogvlm|minicpm-v|moondream|idefics|fuyu)`)
	// Families that read images.
	visionFamilies = []string{"claude-", "gemini-", "gpt-4o", "gpt-4.1", "gpt-5", "gpt-6", "grok-4", "grok-5", "o3", "o4", "qwen3-vl", "kimi-k3", "llama-4"}
)

// LooksLikeVision reports whether a model id probably accepts image input.
func LooksLikeVision(model string) bool {
	id := strings.ToLower(model)
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	if notVision.MatchString(id) {
		return false
	}
	if visionName.MatchString(id) {
		return true
	}
	for _, f := range visionFamilies {
		if strings.HasPrefix(id, f) {
			return true
		}
	}
	return false
}
