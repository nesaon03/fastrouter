package importer

import (
	"github.com/nesaon03/fastrouter/internal/store"
	"strings"
	"testing"

	"github.com/nesaon03/fastrouter/internal/catalog"
	"github.com/nesaon03/fastrouter/internal/router"
	"github.com/nesaon03/fastrouter/internal/translate"
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
			if tg.Label != "xmtp/mimo-v2.5-pro" && !strings.HasPrefix(tg.Label, "ag/") {
				t.Fatalf("unexpected usable vision target %s", tg.Label)
			}
		}
	}
	if len(pool) == 0 || usable < 1 {
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

func TestDuplicatePrefixesFixed(t *testing.T) {
	snap, sum, err := Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, n := range snap.Nodes {
		got[n.Name] = n.Prefix + "|" + strings.Join(n.Aliases, ",")
	}
	for name, want := range map[string]string{"deepseek": "custom|ds2", "neon": "neon|", "atria": "atria|", "deepseek ultr fast": "og|"} {
		if got[name] != want {
			t.Errorf("%s: %q, want %q", name, got[name], want)
		}
	}
	found := false
	for _, w := range sum.Warnings {
		if strings.Contains(w.Message, "duplicate prefixes fixed") && strings.Contains(w.Message, "neon: custom -> neon") {
			found = true
		}
		if strings.Contains(w.Message, `prefix "custom" is shared`) {
			t.Errorf("still shared: %s", w.Message)
		}
	}
	if !found {
		t.Error("no rename warning")
	}
	st := router.Build(snap, nil)
	for _, m := range []string{"custom/x", "ds2/x", "neon/x", "atria/x", "og/x"} {
		ts, err := st.Resolve(m)
		if err != nil || len(ts) != 1 {
			t.Fatalf("%s: %v", m, err)
		}
	}
	a, _ := st.Resolve("ds2/x")
	b, _ := st.Resolve("custom/x")
	if a[0].Provider != b[0].Provider || a[0].Provider.Name != "deepseek" {
		t.Fatal("ds2 and custom should both reach the deepseek endpoint")
	}
}

func TestNodeAliasesPersist(t *testing.T) {
	snap, _, err := Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReplaceAll(snap); err != nil {
		t.Fatal(err)
	}
	back, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range back.Nodes {
		if n.Name == "deepseek" && (n.Prefix != "custom" || strings.Join(n.Aliases, ",") != "ds2") {
			t.Fatalf("after reload: %+v", n)
		}
	}
}
