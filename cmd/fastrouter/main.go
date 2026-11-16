// Command fastrouter is a fast local AI gateway compatible with 9Router setups.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/zenpo/fastrouter/internal/config"
	"github.com/zenpo/fastrouter/internal/importer"
	"github.com/zenpo/fastrouter/internal/logring"
	"github.com/zenpo/fastrouter/internal/server"
	"github.com/zenpo/fastrouter/internal/store"
)

const usage = `fastrouter — local AI gateway

Usage:
  fastrouter [serve] [--config path] [--host 127.0.0.1] [--port 20128]
  fastrouter import <9router-backup.json> [--config path]
  fastrouter test-providers [--config path] [--provider id] [--model id=model]... [--json] [--no-refresh]
      sends one "Say OK" (max 5 tokens) per active account and prints
      provider, account, PASS/FAIL, latency and error class (never keys or bodies)
  fastrouter service install|uninstall|status [--config path] [--host h] [--port n] [--dry-run]
  fastrouter version | --version

Config defaults to ~/.fastrouter/fastrouter.json (created on first run).
Binding to anything but 127.0.0.1 is opt-in via --host or the config file.
Send SIGHUP to reload the database after an import.
`

// Set at build time: -ldflags "-X main.commit=... -X main.date=...".
// The version itself is server.Version (see scripts/build.sh).
var (
	commit = ""
	date   = ""
)

func main() {
	log.SetOutput(io.MultiWriter(os.Stderr, logring.Default)) // Console Log page
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	} else if len(args) > 0 && (args[0] == "--version" || args[0] == "-version" || args[0] == "-v") {
		cmd = "version"
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	host := fs.String("host", "", "listen host (overrides config)")
	port := fs.Int("port", 0, "listen port (overrides config)")

	var err error
	switch cmd {
	case "serve":
		_ = fs.Parse(args)
		err = serve(*cfgPath, *host, *port)
	case "import":
		// allow flags before or after the file name
		_ = fs.Parse(args)
		file := fs.Arg(0)
		if fs.NArg() > 1 {
			_ = fs.Parse(fs.Args()[1:])
		}
		if file == "" {
			fs.Usage()
			os.Exit(2)
		}
		err = runImport(*cfgPath, file)
	case "test-providers":
		prov := fs.String("provider", "", "only this provider id")
		asJSON := fs.Bool("json", false, "print JSON")
		noRefresh := fs.Bool("no-refresh", false, "do not refresh due OAuth tokens first")
		models := map[string]string{}
		fs.Func("model", "provider=model to test with (repeatable)", func(v string) error {
			k, m, ok := strings.Cut(v, "=")
			if !ok || k == "" || m == "" {
				return fmt.Errorf("want provider=model")
			}
			models[k] = m
			return nil
		})
		_ = fs.Parse(args)
		err = runProviderTest(*cfgPath, *prov, models, *asJSON, !*noRefresh)
	case "version":
		fmt.Println(versionString())
	case "service":
		err = runService(fs, args)
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runProviderTest(cfgPath, provider string, models map[string]string, asJSON, refresh bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	snap, err := st.Load()
	if err != nil {
		return err
	}
	log.SetOutput(io.Discard) // keep the table clean; log lines never carry secrets anyway
	srv := server.New(cfg, snap, st)
	defer srv.Close()
	ctx := context.Background()
	if refresh {
		if n := srv.RefreshDue(ctx); n > 0 {
			fmt.Fprintf(os.Stderr, "refreshed %d expired sign-in token(s)\n", n)
		}
	}
	res := srv.LiveTest(ctx, server.LiveOptions{Provider: provider, Models: models})
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tACCOUNT\tMODEL\tRESULT\tLATENCY\tERROR")
	pass := 0
	for _, r := range res {
		result, errText := "PASS", ""
		if r.OK {
			pass++
		} else {
			result, errText = "FAIL", r.Class+": "+r.Message
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%dms\t%s\n", r.Provider, firstNonEmptyStr(r.Name, r.Account), r.Model, result, r.Ms, errText)
	}
	_ = tw.Flush()
	fmt.Printf("\n%d/%d accounts passed\n", pass, len(res))
	return nil
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func runImport(cfgPath, file string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	sum, err := importer.Run(st, file)
	if err != nil {
		return err
	}
	sum.Print(os.Stdout)
	fmt.Printf("\nSaved to %s (secrets encrypted with %s/secret.key)\n", cfg.DataDir, cfg.DataDir)
	return nil
}

func serve(cfgPath, host string, port int) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if host != "" {
		cfg.Host = host
	}
	if port != 0 {
		cfg.Port = port
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	snap, err := st.Load()
	if err != nil {
		return err
	}
	srv := server.New(cfg, snap, st)
	defer srv.Close()
	bg, stopBG := context.WithCancel(context.Background())
	defer stopBG()
	go srv.RunTokenRefresh(bg, time.Minute) // Codex accounts signed in through FastRouter
	if hr, err := srv.Saver().StartHeadroom(bg, filepath.Join(cfg.DataDir, "headroom")); err != nil {
		log.Printf("headroom autostart skipped: %v", err)
	} else if hr != nil {
		log.Printf("started headroom proxy (pid %d)", hr.Process.Pid)
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if ip := net.ParseIP(cfg.Host); ip == nil || !ip.IsLoopback() {
		log.Printf("WARNING: listening on %s exposes the router beyond this machine", cfg.Host)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	drained := make(chan struct{}) // closed once Shutdown has finished draining (M2)
	go func() {
		defer close(drained)
		for s := range sig {
			if s == syscall.SIGHUP {
				if snap, err := st.Load(); err != nil {
					log.Printf("reload failed: %v", err)
				} else {
					srv.Reload(snap)
					log.Printf("reloaded configuration")
				}
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = hs.Shutdown(ctx)
			cancel()
			return
		}
	}()

	log.Printf("fastrouter %s listening on http://%s (dashboard: /dashboard, API: /v1)", server.Version, addr)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// ListenAndServe returns as soon as Shutdown starts; wait for in-flight
	// requests to finish before the deferred recorder and store close.
	<-drained
	return nil
}

func versionString() string {
	c, d := commit, date
	if bi, ok := debug.ReadBuildInfo(); ok && c == "" {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				c = s.Value
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			}
		}
	}
	if len(c) > 12 {
		c = c[:12]
	}
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	return fmt.Sprintf("fastrouter %s (commit %s, built %s, %s %s/%s)",
		server.Version, c, d, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
