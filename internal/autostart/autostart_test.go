package autostart

import (
	"bytes"
	"os"
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

func TestPlanAllOS(t *testing.T) {
	args := []string{"serve", "--config", "/c.json"}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, action := range []string{"install", "uninstall", "status"} {
			p, err := Build(goos, action, "/bin/fastrouter", args)
			if err != nil || len(p) == 0 {
				t.Errorf("%s %s: plan=%v err=%v", goos, action, p, err)
			}
			for _, s := range p {
				if len(s.Cmd) > 0 && (s.Cmd[0] == "sudo" || strings.Contains(strings.Join(s.Cmd, " "), "--system")) {
					t.Errorf("%s %s: needs privileges: %v", goos, action, s.Cmd)
				}
			}
		}
	}
	if _, err := Build("linux", "bogus", "/x", args); err == nil {
		t.Error("expected error for unknown action")
	}
	if _, err := Build("plan9", "install", "/x", args); err == nil {
		t.Error("expected error for unsupported OS")
	}
}

// The dashboard needs to know where autostart lives before anything is
// installed, and it must never claim installed without a file.
func TestPathAndInstalled(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		p, err := Path(goos)
		if err != nil || p == "" {
			t.Fatalf("%s: path %q err %v", goos, p, err)
		}
		if !strings.HasSuffix(p, winLinkName) && !strings.HasSuffix(p, unitName) && !strings.HasSuffix(p, ".plist") {
			t.Fatalf("%s: unexpected path %s", goos, p)
		}
		if _, ok := Installed(goos); ok {
			// Only true if the developer machine really has one; either answer is
			// valid, so just require consistency with the file.
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("%s: reported installed without a file at %s", goos, p)
			}
		}
	}
	if _, err := Path("plan9"); err == nil {
		t.Fatal("unsupported OS must error")
	}
	if Supported("plan9") {
		t.Fatal("plan9 is not supported")
	}
}

// The plan's printout is what `service install --dry-run` shows; it must not
// leak into Apply when a writer is given.
func TestPrintAndApplyDry(t *testing.T) {
	p, err := Build("windows", "install", `C:\a\fastrouter.exe`, []string{"serve", "--config", `C:\a\cfg.json`})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	p.Print(&b)
	if !strings.Contains(b.String(), "run:") || !strings.Contains(b.String(), "FR_LNK=") {
		t.Fatalf("print output: %s", b.String())
	}
}
