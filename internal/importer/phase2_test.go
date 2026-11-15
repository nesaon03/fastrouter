package importer

import (
	"testing"

	"github.com/zenpo/fastrouter/internal/catalog"
	"github.com/zenpo/fastrouter/internal/router"
	"github.com/zenpo/fastrouter/internal/translate"
)

// zenpo's imported settings drive Phase 2 behaviour.
func TestImportedPhase2Settings(t *testing.T) {
	snap, _, err := Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	st := router.Build(snap, nil)
	if p, ok := st.Provider("deepseek"); !ok || p.ThinkingMode != "max" {
		t.Fatalf("deepseek thinking mode: %+v", p)
	}
	ds, _ := st.Provider("deepseek")
	if f, u := ds.Pick(translate.Anthropic); f != translate.Anthropic || u == "" {
		t.Fatalf("deepseek should take Anthropic natively, got %v %q", f, u)
	}
	if f, _ := ds.Pick(translate.Gemini); f != translate.OpenAI {
		t.Fatalf("deepseek for a Gemini client should translate to OpenAI, got %v", f)
	}
	pool := st.VisionFallback()
	usable := 0
	for _, tg := range pool {
		if !tg.Provider.Plugin && tg.Provider.Pool.Size() > 0 {
			usable++
			if tg.Label != "xmtp/mimo-v2.5-pro" {
				t.Fatalf("unexpected usable vision target %s", tg.Label)
			}
		}
	}
	if len(pool) == 0 || usable != 1 {
		t.Fatalf("vision pool %d targets, %d usable in core", len(pool), usable)
	}
}

func TestVisionNames(t *testing.T) {
	for m, want := range map[string]bool{
		"claude-sonnet-4-6": true, "gemini-3.8-flash-high": true, "qwen3-vl-plus": true, "glm-4.6v": true,
		"mimo-v2-omni": true, "gpt-5.4": true, "deepseek-v4-flash": false, "gemini-2.5-flash-image": false,
		"text-embedding-3": false, "kimi-k2.6": false, "minimax-m2.7": false,
	} {
		if got := catalog.LooksLikeVision(m); got != want {
			t.Errorf("%s: %v, want %v", m, got, want)
		}
	}
}
