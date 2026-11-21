package router

import (
	"strings"
	"testing"

	"github.com/zenpo/fastrouter/internal/store"
)

// deepseekSnap is one active DeepSeek account with no catalog or custom models
// of its own, so only the discovery overlay can contribute model names.
func deepseekSnap(discovered ...store.DiscoveredModel) *store.Snapshot {
	return &store.Snapshot{
		Connections: []store.Connection{
			{ID: "a", Provider: "deepseek", Active: true, Status: "ok", Priority: 1,
				Secrets: map[string]string{"apiKey": "k1"}},
		},
		Discovered: discovered,
	}
}

func hasModel(st *State, name string) bool {
	for _, m := range st.Models {
		if m == name {
			return true
		}
	}
	return false
}

// A model released after this build shows up in /v1/models with no code change.
func TestDiscoveredModelIsListed(t *testing.T) {
	st := Build(deepseekSnap(store.DiscoveredModel{
		Provider: "deepseek", Model: "deepseek-v9-ultra", Status: store.DiscoveredListed,
	}), nil)
	if !hasModel(st, "ds/deepseek-v9-ultra") {
		t.Fatalf("discovered model missing from %v", st.Models)
	}
	if !st.IsDiscovered("deepseek", "deepseek-v9-ultra") {
		t.Fatal("IsDiscovered should report the overlay entry")
	}
}

// The trap: DeepSeek's own list returns the *served* id, which the catalog maps
// onto a public id. Advertising it would give two names for one model.
func TestDiscoveredWireIDIsSkipped(t *testing.T) {
	st := Build(deepseekSnap(
		store.DiscoveredModel{Provider: "deepseek", Model: "deepseek-flash", Status: store.DiscoveredListed},
		store.DiscoveredModel{Provider: "deepseek", Model: "deepseek-v4-pro", Status: store.DiscoveredListed},
	), nil)
	for _, m := range st.Models {
		if strings.Contains(m, "deepseek-flash") || strings.Contains(m, "deepseek-v4-pro") {
			t.Fatalf("wire id advertised: %v", st.Models)
		}
	}
	if st.IsDiscovered("deepseek", "deepseek-flash") {
		t.Fatal("a wire id must not be tracked as discovered")
	}
}

func TestDiscoveredDeadIsSkipped(t *testing.T) {
	st := Build(deepseekSnap(store.DiscoveredModel{
		Provider: "deepseek", Model: "gone-model", Status: store.DiscoveredDead,
	}), nil)
	if hasModel(st, "ds/gone-model") {
		t.Fatalf("dead model still listed: %v", st.Models)
	}
}

// A discovered id that a custom model already lists must not appear twice.
func TestDiscoveredDoesNotDuplicate(t *testing.T) {
	snap := deepseekSnap(store.DiscoveredModel{
		Provider: "deepseek", Model: "mine", Status: store.DiscoveredListed,
	})
	snap.CustomModels = []store.CustomModel{{ProviderAlias: "ds", ID: "mine", Type: "llm"}}
	st := Build(snap, nil)
	n := 0
	for _, m := range st.Models {
		if m == "ds/mine" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("ds/mine listed %d times: %v", n, st.Models)
	}
}

func TestDiscoveredVisionCap(t *testing.T) {
	st := Build(deepseekSnap(store.DiscoveredModel{
		Provider: "deepseek", Model: "vision-1", Status: store.DiscoveredListed,
		Caps: `{"vision":true}`,
	}), nil)
	p, _ := st.Provider("ds")
	if !st.SupportsVision(Target{Provider: p, Model: "vision-1", Label: "ds/vision-1"}) {
		t.Fatal("declared vision capability ignored")
	}
}
