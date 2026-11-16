package importer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zenpo/fastrouter/internal/store"
)

// M7/M8: proxy credentials and credential URLs in settings are sealed, and
// accounts that used a proxy in 9Router are flagged at import.
func TestM7M8ProxySecretsSealedAndWarned(t *testing.T) {
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	conns := b["providerConnections"].([]any)
	c0 := conns[0].(map[string]any)
	c0["isActive"] = true
	psd, _ := c0["providerSpecificData"].(map[string]any)
	if psd == nil {
		psd = map[string]any{}
		c0["providerSpecificData"] = psd
	}
	psd["connectionProxyEnabled"] = true
	psd["connectionProxyUrl"] = "http://puser:PROXYPASS123@10.0.0.1:8080"
	b["proxyPools"] = []any{map[string]any{"name": "p", "url": "socks5://u:POOLPASS456@1.2.3.4:1080"}}
	b["settings"].(map[string]any)["tunnelUrl"] = "https://t.example/x?token=TUNNELTOK789"
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.json")
	out, _ := json.Marshal(b)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(st, path); err != nil {
		t.Fatal(err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if !strings.Contains(string(snap.Settings["tunnelUrl"]), "TUNNELTOK789") || !strings.Contains(string(snap.Settings["backup.proxyPools"]), "POOLPASS456") {
		t.Fatal("sealed settings did not round-trip")
	}
	db, _ := os.ReadFile(filepath.Join(dir, "fastrouter.db"))
	for _, secret := range []string{"PROXYPASS123", "POOLPASS456", "TUNNELTOK789"} {
		if bytes.Contains(db, []byte(secret)) {
			t.Errorf("%s stored in plaintext", secret)
		}
	}
	var warned, pools bool
	for _, w := range snap.Warnings {
		warned = warned || (w.Kind == "proxy" && w.Subject == "accounts")
		pools = pools || (w.Kind == "proxy" && w.Subject == "pools")
	}
	if !warned || !pools {
		t.Fatalf("missing proxy warnings: %+v", snap.Warnings)
	}
}
