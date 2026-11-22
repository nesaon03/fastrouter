package importer

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/nesaon03/fastrouter/internal/catalog"
	"github.com/nesaon03/fastrouter/internal/store"
)

// knownPrefixFixes are agreed renames for endpoints that 9Router left sharing
// a prefix. They match on name and host so they only touch those endpoints.
var knownPrefixFixes = []struct {
	name, host string
	prefix     string   // new prefix
	aliases    []string // extra prefixes
}{
	{name: "deepseek", host: "api.deepseek.com", prefix: "custom", aliases: []string{"ds2"}}, // keeps "custom" for old configs
	{name: "neon", host: "api.v2.neokens.com", prefix: "neon"},
	{name: "atria", host: "api.atria-asi.ai", prefix: "atria"},
	{name: "deepseek ultr fast", host: "apis.opengateway.ai", prefix: "og"}, // 9Router's original prefix
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// fixPrefixes gives every custom endpoint a unique prefix: the known renames
// first, then any remaining duplicate gets a prefix made from its name. The
// first endpoint keeps a shared prefix. It returns human-readable renames.
func fixPrefixes(nodes []store.Node) []string {
	var out []string
	for i := range nodes {
		n := &nodes[i]
		u, _ := url.Parse(n.BaseURL)
		for _, f := range knownPrefixFixes {
			if !strings.EqualFold(strings.TrimSpace(n.Name), f.name) || u == nil || !strings.EqualFold(u.Hostname(), f.host) {
				continue
			}
			if n.Prefix != f.prefix {
				out = append(out, fmt.Sprintf("%s: %s -> %s", n.Name, firstNonEmpty(n.Prefix, "(none)"), f.prefix))
				n.Prefix = f.prefix
			}
			for _, a := range f.aliases {
				if !contains(n.Aliases, a) {
					n.Aliases = append(n.Aliases, a)
					out = append(out, fmt.Sprintf("%s: also answers to %s", n.Name, a))
				}
			}
		}
	}
	used := map[string]bool{}
	for _, n := range nodes {
		for _, a := range n.Aliases {
			used[a] = true
		}
	}
	for i := range nodes {
		n := &nodes[i]
		if !used[n.Prefix] {
			used[n.Prefix] = true
			continue
		}
		base := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(n.Name), "-"), "-")
		if base == "" {
			base = "node"
		}
		cand := base
		for k := 2; used[cand] || isBuiltinKey(cand); k++ {
			cand = fmt.Sprintf("%s%d", base, k)
		}
		out = append(out, fmt.Sprintf("%s: %s -> %s", n.Name, n.Prefix, cand))
		n.Prefix = cand
		used[cand] = true
	}
	return out
}

func isBuiltinKey(k string) bool {
	return catalog.Reserved(k)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
