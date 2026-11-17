package tunnel

import (
	"context"
	"strings"
	"testing"
)

const tsStatusJSON = `{"Version":"1.104.7-t","BackendState":"Running","CertDomains":[],
"Self":{"DNSName":"pc.tail.ts.net.","TailscaleIPs":["100.1.2.3","fd7a::1"],"Online":true}}`

func fake(serve *string, calls *[]string, extra func(args []string) (string, error)) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		a := strings.Join(args, " ")
		*calls = append(*calls, a)
		switch {
		case a == "status --json":
			return []byte(tsStatusJSON), nil
		case a == "serve status --json":
			return []byte(*serve), nil
		}
		if extra != nil {
			o, err := extra(args)
			return []byte(o), err
		}
		return nil, nil
	}
}

func TestTailscaleKeepsOtherServes(t *testing.T) {
	serve := `{"TCP":{"8100":{"TCPForward":"127.0.0.1:8100"},"80":{"TCPForward":"127.0.0.1:80"}}}`
	var calls []string
	ts := &Tailscale{Port: 20129, Bin: "ts"}
	ts.run = fake(&serve, &calls, nil)
	st := ts.Status(context.Background())
	if !st.LoggedIn || st.DNSName != "pc.tail.ts.net" || st.Mode != "off" || st.HTTPS || strings.Join(st.OtherPorts, ",") != "80,8100" {
		t.Fatalf("status: %+v", st)
	}
	if err := ts.Enable(context.Background(), "serve"); err != nil {
		t.Fatal(err)
	}
	last := calls[len(calls)-1]
	if last != "serve --bg --yes --http=20129 http://127.0.0.1:20129" {
		t.Fatalf("no certs → plain HTTP serve on our port, got %q", last)
	}
	if err := ts.Enable(context.Background(), "funnel"); err == nil {
		t.Fatal("funnel without HTTPS must ask the user to enable it")
	}
	for _, c := range calls {
		if strings.Contains(c, "reset") {
			t.Fatalf("must never reset serve config: %q", c)
		}
	}

	serve = `{"TCP":{"8100":{"TCPForward":"127.0.0.1:8100"},"20129":{"HTTP":true}},
"Web":{"pc.tail.ts.net:20129":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:20129"}}}}}`
	st = ts.Status(context.Background())
	if st.Mode != "serve" || st.URL != "http://pc.tail.ts.net:20129" || strings.Join(st.OtherPorts, ",") != "8100" {
		t.Fatalf("serving: %+v", st)
	}
	calls = nil
	if err := ts.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "serve --http=20129 off" {
		t.Fatalf("disable must remove only our entry: %v", calls)
	}
}

func TestTailscaleFunnelOff(t *testing.T) {
	serve := `{"TCP":{"443":{"HTTPS":true}},"Web":{"pc.tail.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:20129"}}}},"AllowFunnel":{"pc.tail.ts.net:443":true}}`
	var calls []string
	ts := &Tailscale{Port: 20129, Bin: "ts"}
	ts.run = fake(&serve, &calls, nil)
	st := ts.Status(context.Background())
	if st.Mode != "funnel" || st.URL != "https://pc.tail.ts.net" {
		t.Fatalf("%+v", st)
	}
	if err := ts.Disable(context.Background()); err != nil || calls[len(calls)-1] != "funnel --https=443 off" {
		t.Fatalf("%v %v", err, calls)
	}
}

func TestQuickURL(t *testing.T) {
	line := `2026-11-17T10:00:00Z INF |  https://brave-lion-cat-dog.trycloudflare.com                    |`
	if m := quickURL.FindString(line); m != "https://brave-lion-cat-dog.trycloudflare.com" {
		t.Fatal(m)
	}
}
