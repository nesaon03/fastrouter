package pricing

// Rate cards for models the compiled table does not know yet.
//
// The table shipped in pricing_9router.json only covers models that existed
// when the file was cut. A model discovered at run time (or a vendor's renamed
// id) therefore priced at $0.00, which reads as "free" in the dashboard and in
// the usage ledger. OpenRouter publishes its whole catalog with per-token
// pricing in one unauthenticated JSON document, so the same discovery pass that
// learns model ids can learn their rates without a key, an account or a rebuild.
//
// Nothing here is authoritative: a learned rate only fills a gap, so a compiled
// or user-overridden price always wins (Learn refuses to replace an entry).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// OpenRouterModelsURL is the public catalog document. It is a variable only so
// tests can point it at a local server.
var OpenRouterModelsURL = "https://openrouter.ai/api/v1/models"

// maxHarvest caps the document read (it is a few MB today and grows slowly).
const maxHarvest = 16 << 20

// openRouterDoc is the part of OpenRouter's /api/v1/models we use. Rates are
// strings holding USD per token, not per million, and a missing rate is "".
type openRouterDoc struct {
	Data []orModel `json:"data"`
}

type orModel struct {
	ID      string    `json:"id"`
	Pricing orPricing `json:"pricing"`
}

type orPricing struct {
	Prompt         string `json:"prompt"`
	Completion     string `json:"completion"`
	InputCacheRead string `json:"input_cache_read"`
	CacheWrite     string `json:"input_cache_write"`
	Reasoning      string `json:"internal_reasoning"`
}

// hasDeadRate reports whether the row carries a rate string that does not parse.
// Such a row is skipped whole: storing it with zeros would look like a free
// model and hide the real (unknown) price.
func hasDeadRate(m orModel) bool {
	for _, s := range []string{m.Pricing.Prompt, m.Pricing.Completion, m.Pricing.InputCacheRead,
		m.Pricing.CacheWrite, m.Pricing.Reasoning} {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if v, err := strconv.ParseFloat(s, 64); err != nil || v < 0 {
			return true
		}
	}
	return false
}

// FetchOpenRouter downloads the public rate table, keyed by OpenRouter model id
// ("anthropic/claude-opus-4-6"). Values are USD per 1M tokens, the unit Price
// uses. Models without a positive input or output rate are skipped: a zero
// rate from this document is more likely a missing field than a free model.
func FetchOpenRouter(ctx context.Context, client *http.Client) (map[string]Price, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenRouterModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter models: http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxHarvest))
	if err != nil {
		return nil, err
	}
	var doc openRouterDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := make(map[string]Price, len(doc.Data))
	for _, m := range doc.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || (m.Pricing.Prompt == "" && m.Pricing.Completion == "") {
			continue // no usable pricing block
		}
		p := Price{
			Input:         perMillion(m.Pricing.Prompt),
			Output:        perMillion(m.Pricing.Completion),
			Cached:        perMillion(m.Pricing.InputCacheRead),
			CacheCreation: perMillion(m.Pricing.CacheWrite),
			Reasoning:     perMillion(m.Pricing.Reasoning),
		}
		if hasDeadRate(m) {
			continue // an unparsable rate is not a free model
		}
		out[id] = p
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("openrouter models: no usable rates")
	}
	return out, nil
}

// perMillion converts OpenRouter's per-token string to USD per 1M tokens.
func perMillion(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0
	}
	return v * 1e6
}
