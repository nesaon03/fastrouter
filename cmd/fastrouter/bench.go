package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nesaon03/fastrouter/internal/bench"
	"github.com/nesaon03/fastrouter/internal/config"
	"github.com/nesaon03/fastrouter/internal/server"
	"github.com/nesaon03/fastrouter/internal/store"
)

// runBench measures an already running FastRouter over HTTP. It reads the
// config and database only to pick a local API key (never printed) and to
// save the run in the Speed Test history.
func runBench(cfgPath, host string, port int, o bench.Options, working, asJSON, save bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if port != 0 {
		cfg.Port = port
	}
	if host != "" {
		cfg.Host = host
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
	log.SetOutput(io.Discard)
	srv := server.New(cfg, snap, st)
	defer srv.Close()
	if working {
		o.Models = append(o.Models, srv.BenchWorkingModels()...)
	}
	if err := o.Normalize(); err != nil {
		return err
	}
	key, label, err := srv.BenchKey(o.Models)
	if err != nil {
		return err
	}
	h := cfg.Host
	if ip := net.ParseIP(h); h == "" || (ip != nil && ip.IsUnspecified()) {
		h = "127.0.0.1"
	}
	o.BaseURL, o.APIKey = "http://"+net.JoinHostPort(h, strconv.Itoa(cfg.Port)), key

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	started := time.Now()
	if !asJSON {
		fmt.Fprintf(os.Stderr, "benchmarking %s with %s: %d request(s), %s prompt, stream=%v, effort=%s, clients=%s\n",
			o.BaseURL, label, o.Total(), o.Prompt, o.Stream, o.Effort, strings.Join(o.Clients, ","))
	}
	var rs []bench.Result
	bench.Run(ctx, nil, o, func(r bench.Result) {
		r.Account = srv.AccountLabel(r.AccountID)
		rs = append(rs, r)
		if !asJSON {
			res := "PASS"
			if r.NoText {
				res = "PASS†" // only hidden reasoning tokens: TTFT = total
			}
			if !r.OK {
				res = "FAIL " + r.Class
			}
			ttft, speed := fmt.Sprintf("%8.1fms", r.TTFTMs), bench.SpeedLabel(r.TTFTMs)
			if r.NoText {
				ttft, speed = "       —  ", bench.SpeedLabel(r.TotalMs)
			}
			if !r.OK {
				speed = ""
			}
			fmt.Fprintf(os.Stderr, "  #%-3d %-12s %-36s %-8s ttft %s  total %8.1fms  %6.1f tok/s  overhead %6.2fms (%s)  %-11s %s\n",
				r.Seq, r.Client, r.Model, res, ttft, r.TotalMs, r.TokPerSec, r.OverheadMs, bench.OverheadLabel(r.OverheadMs), speed, r.Account)
		}
	})
	if save {
		_ = srv.BenchRecord(o, started, rs, label)
	}
	sum := bench.Summarize(rs)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"options": o, "results": rs, "summary": sum})
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nCLIENT\tMODEL\tENDPOINT\tOK\tTTFT min/avg/p50/p95 ms\tSPEED\tTOTAL avg/p95 ms\tTOK/S\tOVERHEAD avg/p95 ms\tACCOUNTS\tERRORS")
	for _, s := range sum {
		ttft, speed := fmt.Sprintf("%.0f / %.0f / %.0f / %.0f", s.TTFT.Min, s.TTFT.Avg, s.TTFT.P50, s.TTFT.P95), bench.SpeedLabel(s.TTFT.Avg)
		switch {
		case s.OK == 0:
			ttft, speed = "—", "—"
		case s.TTFTRuns == 0:
			ttft, speed = "— (no visible output)", bench.SpeedLabel(s.Total.Avg)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\t%.0f / %.0f\t%.1f\t%.2f / %.2f (%s)\t%s\t%s\n", s.Client, s.Model, s.Endpoint, s.OK, s.Runs,
			ttft, speed, s.Total.Avg, s.Total.P95, s.TokPS, s.Overhead.Avg, s.Overhead.P95, bench.OverheadLabel(s.Overhead.Avg), s.Accounts, s.Errors)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, r := range rs {
		if r.NoText {
			fmt.Println("\n† no visible output (reasoning used the budget); those runs are left out of TTFT")
			break
		}
	}
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
