package secret

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	b, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := b.Seal([]byte("value"))
	b2, _ := LoadOrCreate(path) // same key from disk
	got, err := b2.Open(sealed)
	if err != nil || string(got) != "value" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestRedact(t *testing.T) {
	for _, in := range []string{
		"Authorization: Bearer abcdef123456",
		`{"api_key":"zz-secret-value"}`,
		"key sk-proj-ABCDEFGHIJKLMNOP leaked",
	} {
		out := Redact(in)
		if strings.Contains(out, "abcdef123456") || strings.Contains(out, "zz-secret-value") || strings.Contains(out, "ABCDEFGHIJKLMNOP") {
			t.Errorf("not redacted: %q -> %q", in, out)
		}
	}
}
