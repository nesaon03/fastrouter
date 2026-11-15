package main

// "fastrouter service" registers FastRouter to start when the current user
// logs in. Everything is per-user; nothing needs admin/root:
//
//	linux   systemd user unit  ~/.config/systemd/user/fastrouter.service
//	darwin  launchd agent      ~/Library/LaunchAgents/com.zenpo.fastrouter.plist
//	windows Startup shortcut   %APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\FastRouter.lnk

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	unitName     = "fastrouter.service"
	launchdLabel = "com.zenpo.fastrouter"
	winLinkName  = "FastRouter.lnk"
)

// serviceOS is overridable in tests.
var serviceOS = runtime.GOOS

func runService(fs *flag.FlagSet, args []string) error {
	dry := fs.Bool("dry-run", false, "print what would be done without changing anything")
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	action := args[0]
	_ = fs.Parse(args[1:])

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	cfg, err := filepath.Abs(fs.Lookup("config").Value.String())
	if err != nil {
		return err
	}
	serveArgs := []string{"serve", "--config", cfg}
	if h := fs.Lookup("host").Value.String(); h != "" {
		serveArgs = append(serveArgs, "--host", h)
	}
	if p := fs.Lookup("port").Value.String(); p != "" && p != "0" {
		serveArgs = append(serveArgs, "--port", p)
	}

	plan, err := servicePlan(serviceOS, action, exe, serveArgs)
	if err != nil {
		return err
	}
	if *dry {
		plan.print()
		return nil
	}
	return plan.apply()
}

// step is one file write/remove or command.
type step struct {
	write, remove string // file path
	content       string
	cmd           []string
	env           []string // extra env for cmd
	ignoreErr     bool
	note          string
}

type plan []step

func (p plan) print() {
	for _, s := range p {
		switch {
		case s.write != "":
			fmt.Printf("write %s:\n%s\n", s.write, s.content)
		case s.remove != "":
			fmt.Printf("remove %s\n", s.remove)
		case s.cmd != nil:
			fmt.Printf("run: %s\n", strings.Join(append(append([]string{}, s.env...), s.cmd...), " "))
		case s.note != "":
			fmt.Println(s.note)
		}
	}
}

func (p plan) apply() error {
	for _, s := range p {
		var err error
		switch {
		case s.write != "":
			if err = os.MkdirAll(filepath.Dir(s.write), 0o700); err == nil {
				err = os.WriteFile(s.write, []byte(s.content), 0o644)
			}
			if err == nil {
				fmt.Println("wrote", s.write)
			}
		case s.remove != "":
			err = os.Remove(s.remove)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			} else if err == nil {
				fmt.Println("removed", s.remove)
			}
		case s.cmd != nil:
			c := exec.Command(s.cmd[0], s.cmd[1:]...)
			c.Env = append(os.Environ(), s.env...)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			err = c.Run()
			if err != nil {
				err = fmt.Errorf("%s: %w", strings.Join(s.cmd, " "), err)
			}
		case s.note != "":
			fmt.Println(s.note)
		}
		if err != nil && !s.ignoreErr {
			return err
		}
	}
	return nil
}

func servicePlan(goos, action, exe string, serveArgs []string) (plan, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	switch goos {
	case "linux":
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".config")
		}
		unit := filepath.Join(dir, "systemd", "user", unitName)
		sc := func(a ...string) []string { return append([]string{"systemctl", "--user"}, a...) }
		switch action {
		case "install":
			return plan{
				{write: unit, content: systemdUnit(exe, serveArgs)},
				{cmd: sc("daemon-reload")},
				{cmd: sc("enable", "--now", unitName)},
				{note: "Starts at login. To keep it running while logged out: loginctl enable-linger $USER"},
			}, nil
		case "uninstall":
			return plan{
				{cmd: sc("disable", "--now", unitName), ignoreErr: true},
				{remove: unit},
				{cmd: sc("daemon-reload"), ignoreErr: true},
			}, nil
		case "status":
			return plan{{cmd: sc("status", "--no-pager", unitName)}}, nil
		}
	case "darwin":
		pl := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		domain := "gui/" + strconv.Itoa(os.Getuid())
		logFile := filepath.Join(home, "Library", "Logs", "fastrouter.log")
		switch action {
		case "install":
			return plan{
				{write: pl, content: launchdPlist(exe, serveArgs, logFile)},
				{cmd: []string{"launchctl", "bootout", domain + "/" + launchdLabel}, ignoreErr: true},
				{cmd: []string{"launchctl", "bootstrap", domain, pl}},
				{note: "Starts at login. Logs: " + logFile},
			}, nil
		case "uninstall":
			return plan{
				{cmd: []string{"launchctl", "bootout", domain + "/" + launchdLabel}, ignoreErr: true},
				{remove: pl},
			}, nil
		case "status":
			return plan{{cmd: []string{"launchctl", "print", domain + "/" + launchdLabel}}}, nil
		}
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		lnk := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", winLinkName)
		switch action {
		case "install":
			// Values go through env vars so nothing is spliced into the script.
			const ps = `$s=(New-Object -ComObject WScript.Shell).CreateShortcut($env:FR_LNK);` +
				`$s.TargetPath=$env:FR_EXE;$s.Arguments=$env:FR_ARGS;$s.WorkingDirectory=$env:FR_DIR;` +
				`$s.WindowStyle=7;$s.Description='FastRouter local AI gateway';$s.Save()`
			return plan{
				{
					cmd: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps},
					env: []string{"FR_LNK=" + lnk, "FR_EXE=" + exe, "FR_ARGS=" + winJoin(serveArgs), "FR_DIR=" + filepath.Dir(exe)},
				},
				{note: "Created " + lnk + " (runs minimized at your next logon). To start now: " + winJoin(append([]string{exe}, serveArgs...))},
			}, nil
		case "uninstall":
			return plan{{remove: lnk}}, nil
		case "status":
			if _, err := os.Stat(lnk); err == nil {
				return plan{{note: "installed: " + lnk}}, nil
			}
			return plan{{note: "not installed (" + lnk + " not found)"}}, nil
		}
	default:
		return nil, fmt.Errorf("service: unsupported OS %q", goos)
	}
	return nil, fmt.Errorf("service: unknown action %q (want install, uninstall or status)", action)
}

func systemdUnit(exe string, args []string) string {
	q := make([]string, 0, len(args)+1)
	for _, a := range append([]string{exe}, args...) {
		a = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(a)
		q = append(q, `"`+a+`"`)
	}
	return `[Unit]
Description=FastRouter local AI gateway
After=network-online.target

[Service]
Type=simple
ExecStart=` + strings.Join(q, " ") + `
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`
}

func launchdPlist(exe string, args []string, logFile string) string {
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range append([]string{exe}, args...) {
		b.WriteString("    <string>" + esc(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>` + esc(logFile) + `</string>
  <key>StandardErrorPath</key><string>` + esc(logFile) + `</string>
</dict>
</plist>
`)
	return b.String()
}

// winJoin quotes args per the CommandLineToArgvW rules.
func winJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && !strings.ContainsAny(a, " \t\"") {
			out[i] = a
			continue
		}
		var b strings.Builder
		b.WriteByte('"')
		slashes := 0
		for _, r := range a {
			switch r {
			case '\\':
				slashes++
				continue
			case '"':
				b.WriteString(strings.Repeat(`\`, slashes*2+1))
			default:
				b.WriteString(strings.Repeat(`\`, slashes))
			}
			slashes = 0
			b.WriteRune(r)
		}
		b.WriteString(strings.Repeat(`\`, slashes*2))
		b.WriteByte('"')
		out[i] = b.String()
	}
	return strings.Join(out, " ")
}
