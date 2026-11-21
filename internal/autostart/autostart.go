// Package autostart registers FastRouter to start when the current user logs
// in. Everything is per-user; nothing needs admin/root:
//
//	linux   systemd user unit  ~/.config/systemd/user/fastrouter.service
//	darwin  launchd agent      ~/Library/LaunchAgents/com.zenpo.fastrouter.plist
//	windows Startup shortcut   %APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\FastRouter.lnk
//
// The dashboard's Settings page and `fastrouter service` both go through Build,
// so the two can never disagree about what "start at login" means.
package autostart

import (
	"errors"
	"fmt"
	"io"
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

// Supported reports whether this OS has an autostart mechanism here.
func Supported(goos string) bool {
	switch goos {
	case "linux", "darwin", "windows":
		return true
	}
	return false
}

// Path is the file that exists while autostart is installed.
func Path(goos string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch goos {
	case "linux":
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".config")
		}
		return filepath.Join(dir, "systemd", "user", unitName), nil
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", winLinkName), nil
	}
	return "", fmt.Errorf("autostart: unsupported OS %q", goos)
}

// Installed reports whether this user starts FastRouter at login, and where
// that is recorded. It only looks at the file, so it never spawns anything.
func Installed(goos string) (path string, ok bool) {
	p, err := Path(goos)
	if err != nil {
		return "", false
	}
	_, err = os.Stat(p)
	return p, err == nil
}

// Step is one file write/remove or command.
type Step struct {
	Write, Remove string // file path
	Content       string
	Cmd           []string
	Env           []string // extra env for Cmd
	IgnoreErr     bool
	Note          string
}

// Plan is an ordered list of steps to get from here to the wanted state.
type Plan []Step

// Print describes the plan without touching anything.
func (p Plan) Print(w io.Writer) {
	for _, s := range p {
		switch {
		case s.Write != "":
			fmt.Fprintf(w, "write %s:\n%s\n", s.Write, s.Content)
		case s.Remove != "":
			fmt.Fprintf(w, "remove %s\n", s.Remove)
		case s.Cmd != nil:
			fmt.Fprintf(w, "run: %s\n", strings.Join(append(append([]string{}, s.Env...), s.Cmd...), " "))
		case s.Note != "":
			fmt.Fprintln(w, s.Note)
		}
	}
}

// Apply runs the plan.
func (p Plan) Apply(w io.Writer) error {
	if w == nil {
		w = io.Discard
	}
	for _, s := range p {
		var err error
		switch {
		case s.Write != "":
			if err = os.MkdirAll(filepath.Dir(s.Write), 0o700); err == nil {
				err = os.WriteFile(s.Write, []byte(s.Content), 0o644)
			}
			if err == nil {
				fmt.Fprintln(w, "wrote", s.Write)
			}
		case s.Remove != "":
			err = os.Remove(s.Remove)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			} else if err == nil {
				fmt.Fprintln(w, "removed", s.Remove)
			}
		case s.Cmd != nil:
			c := exec.Command(s.Cmd[0], s.Cmd[1:]...)
			c.Env = append(os.Environ(), s.Env...)
			c.Stdout, c.Stderr = w, w
			err = c.Run()
			if err != nil {
				err = fmt.Errorf("%s: %w", strings.Join(s.Cmd, " "), err)
			}
		case s.Note != "":
			fmt.Fprintln(w, s.Note)
		}
		if err != nil && !s.IgnoreErr {
			return err
		}
	}
	return nil
}

// Build returns the plan for action (install, uninstall or status) on goos.
// serveArgs are the arguments the unit/shortcut should run.
func Build(goos, action, exe string, serveArgs []string) (Plan, error) {
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
			return Plan{
				{Write: unit, Content: systemdUnit(exe, serveArgs)},
				{Cmd: sc("daemon-reload")},
				{Cmd: sc("enable", "--now", unitName)},
				{Note: "Starts at login. To keep it running while logged out: loginctl enable-linger $USER"},
			}, nil
		case "uninstall":
			return Plan{
				{Cmd: sc("disable", "--now", unitName), IgnoreErr: true},
				{Remove: unit},
				{Cmd: sc("daemon-reload"), IgnoreErr: true},
			}, nil
		case "status":
			return Plan{{Cmd: sc("status", "--no-pager", unitName)}}, nil
		}
	case "darwin":
		pl := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		domain := "gui/" + strconv.Itoa(os.Getuid())
		logFile := filepath.Join(home, "Library", "Logs", "fastrouter.log")
		switch action {
		case "install":
			return Plan{
				{Write: pl, Content: launchdPlist(exe, serveArgs, logFile)},
				{Cmd: []string{"launchctl", "bootout", domain + "/" + launchdLabel}, IgnoreErr: true},
				{Cmd: []string{"launchctl", "bootstrap", domain, pl}},
				{Note: "Starts at login. Logs: " + logFile},
			}, nil
		case "uninstall":
			return Plan{
				{Cmd: []string{"launchctl", "bootout", domain + "/" + launchdLabel}, IgnoreErr: true},
				{Remove: pl},
			}, nil
		case "status":
			return Plan{{Cmd: []string{"launchctl", "print", domain + "/" + launchdLabel}}}, nil
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
			return Plan{
				{
					Cmd: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps},
					Env: []string{"FR_LNK=" + lnk, "FR_EXE=" + exe, "FR_ARGS=" + winJoin(serveArgs), "FR_DIR=" + filepath.Dir(exe)},
				},
				{Note: "Created " + lnk + " (runs minimized at your next logon). To start now: " + winJoin(append([]string{exe}, serveArgs...))},
			}, nil
		case "uninstall":
			return Plan{{Remove: lnk}}, nil
		case "status":
			if _, err := os.Stat(lnk); err == nil {
				return Plan{{Note: "installed: " + lnk}}, nil
			}
			return Plan{{Note: "not installed (" + lnk + " not found)"}}, nil
		}
	default:
		return nil, fmt.Errorf("autostart: unsupported OS %q", goos)
	}
	return nil, fmt.Errorf("autostart: unknown action %q (want install, uninstall or status)", action)
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

// Goos is the OS this build targets (tests override it).
func Goos() string { return runtime.GOOS }
