package dashboard

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestIconFontHasEveryUsedIcon fails when the dashboard uses a Material Symbol that
// the shipped subset font lacks (it would render as text such as "zoom_out").
// Fix: python3 scripts/icons.py (rebuilds the subset and the manifest).
func TestIconFontHasEveryUsedIcon(t *testing.T) {
	valid := words(t, "testdata/material-symbols-all.txt")
	sha, subset := manifest(t)
	font, err := os.ReadFile("static/fonts/material-symbols.woff2")
	if err != nil {
		t.Fatal(err)
	}
	if h := sha256.Sum256(font); hex.EncodeToString(h[:]) != sha {
		t.Fatal("material-symbols.woff2 changed without its manifest; run python3 scripts/icons.py")
	}
	lit := regexp.MustCompile("[\"'`]([a-z0-9_]+)[\"'`]")
	span := regexp.MustCompile(`class="ms[^"]*"[^>]*>\s*([a-z0-9_]+)\s*<`)
	files, _ := filepath.Glob("static/*.js")
	html, _ := filepath.Glob("static/*.html")
	var missing []string
	seen := map[string]bool{}
	for _, f := range append(files, html...) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range []*regexp.Regexp{lit, span} {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				n := m[1]
				if valid[n] && !subset[n] && !seen[n] {
					seen[n] = true
					missing = append(missing, n)
				}
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("icons used but missing from the font subset: %s\nrun: python3 scripts/icons.py", strings.Join(missing, ", "))
	}
}

func words(t *testing.T, p string) map[string]bool {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, w := range strings.Fields(string(b)) {
		out[w] = true
	}
	return out
}

func manifest(t *testing.T) (string, map[string]bool) {
	f, err := os.Open("testdata/material-symbols-subset.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sha, names := "", map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "# sha256 "):
			sha = strings.Fields(l)[2]
		case l != "" && !strings.HasPrefix(l, "#"):
			names[l] = true
		}
	}
	return sha, names
}
