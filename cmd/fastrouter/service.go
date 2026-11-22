package main

// "fastrouter service" registers FastRouter to start when the current user logs
// in. The mechanism itself lives in internal/autostart, so the dashboard's
// Settings page and this command can never disagree.

import (
	"flag"
	"os"
	"path/filepath"

	"github.com/nesaon03/fastrouter/internal/autostart"
)

// serviceOS is overridable in tests.
var serviceOS = autostart.Goos()

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

	plan, err := autostart.Build(serviceOS, action, exe, serveArgs)
	if err != nil {
		return err
	}
	if *dry {
		plan.Print(os.Stdout)
		return nil
	}
	return plan.Apply(os.Stdout)
}
