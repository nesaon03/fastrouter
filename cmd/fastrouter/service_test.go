package main

import (
	"strings"
	"testing"
)

func TestWinJoin(t *testing.T) {
	cases := map[string][]string{
		`serve --config "C:\Users\A B\cfg.json"`: {"serve", "--config", `C:\Users\A B\cfg.json`},
		`"C:\dir with space\\"`:                  {`C:\dir with space\`},
		`"say \"hi\""`:                           {`say "hi"`},
		`""`:                                     {""},
	}
	for want, in := range cases {
		if got := winJoin(in); got != want {
			t.Errorf("winJoin(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSystemdUnitQuoting(t *testing.T) {
	u := systemdUnit("/opt/fast router/fastrouter", []string{"serve", "--config", `/home/u/100%$x"y.json`})
	want := `ExecStart="/opt/fast router/fastrouter" "serve" "--config" "/home/u/100%%$$x\"y.json"`
	if !strings.Contains(u, want+"\n") {
		t.Fatalf("unit missing %s:\n%s", want, u)
	}
	if !strings.Contains(u, "WantedBy=default.target") {
		t.Fatal("unit not wanted by default.target")
	}
}

func TestLaunchdPlistEscaping(t *testing.T) {
	p := launchdPlist("/Users/a&b/fastrouter", []string{"serve", "--config", "/x/<c>.json"}, "/tmp/l.log")
	for _, want := range []string{
		"<string>/Users/a&amp;b/fastrouter</string>",
		"<string>/x/&lt;c&gt;.json</string>",
		"<key>RunAtLoad</key><true/>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %s", want)
		}
	}
}

func TestServicePlanAllOS(t *testing.T) {
	args := []string{"serve", "--config", "/c.json"}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, action := range []string{"install", "uninstall", "status"} {
			p, err := servicePlan(goos, action, "/bin/fastrouter", args)
			if err != nil || len(p) == 0 {
				t.Errorf("%s %s: plan=%v err=%v", goos, action, p, err)
			}
			for _, s := range p {
				if len(s.cmd) > 0 && (s.cmd[0] == "sudo" || strings.Contains(strings.Join(s.cmd, " "), "--system")) {
					t.Errorf("%s %s: needs privileges: %v", goos, action, s.cmd)
				}
			}
		}
	}
	if _, err := servicePlan("linux", "bogus", "/x", args); err == nil {
		t.Error("expected error for unknown action")
	}
	if _, err := servicePlan("plan9", "install", "/x", args); err == nil {
		t.Error("expected error for unsupported OS")
	}
}
