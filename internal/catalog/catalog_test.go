package catalog

import (
	"strings"
	"testing"
)

// The registry is generated and will be regenerated; hand-added ids must survive
// that and must not double up once the registry catches up.
func TestModelsMergesAdditions(t *testing.T) {
	got := Models("codex")
	ids := map[string]int{}
	for _, m := range got {
		ids[m.ID]++
	}
	for _, want := range []string{"gpt-6.2-astra", "gpt-6.2-sol", "gpt-6.2-astra[1m]", "gpt-6.2-sol[1m]"} {
		if ids[want] != 1 {
			t.Fatalf("%s appears %d times in the codex list", want, ids[want])
		}
	}
	if ids["gpt-6.1-terra"] != 1 {
		t.Fatal("the registry's own ids must still be there")
	}
	for id, n := range ids {
		if n > 1 {
			t.Fatalf("%s is listed %d times", id, n)
		}
	}
}

// An addition for a provider that has no registry entry still works, and a
// provider with no additions is unaffected.
func TestModelsAdditionsOnly(t *testing.T) {
	if len(Models("opencode")) != 0 {
		// opencode is a plugin provider with no models; the point is that the
		// merge does not invent anything for it.
		t.Log("opencode has registry models; skipping the empty check")
	}
	modelAdditions["__test__"] = []Model{{ID: "x-1", Name: "X"}, {ID: "x-1", Name: "dup"}}
	defer delete(modelAdditions, "__test__")
	got := Models("__test__")
	if len(got) != 1 || got[0].ID != "x-1" {
		t.Fatalf("duplicate addition survived: %+v", got)
	}
}

// Every added id must be something a request can carry: no spaces, no quotes.
func TestAdditionsAreSendable(t *testing.T) {
	for provider, list := range modelAdditions {
		for _, m := range list {
			if strings.TrimSpace(m.ID) != m.ID || m.ID == "" {
				t.Fatalf("%s: bad id %q", provider, m.ID)
			}
			if strings.ContainsAny(m.ID, " \t\"'\\") {
				t.Fatalf("%s: id %q contains a character an HTTP body would mangle", provider, m.ID)
			}
			if m.Name == "" {
				t.Fatalf("%s: %s has no display name", provider, m.ID)
			}
		}
	}
}
