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
	"text/tabwriter"
	"time"

	"github.com/zenpo/fastrouter/internal/bench"
	"github.com/zenpo/fastrouter/internal/config"
	"github.com/zenpo/fastrouter/internal/server"
	"github.com/zenpo/fastrouter/internal/store"
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
		fmt.Fprintf(os.Stderr, "benchmarking %s with %s: %d request(s), %s prompt, stream=%v, ide=%v\n",
			o.BaseURL, label, o.Total(), o.Prompt, o.Stream, o.IDE)
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
			fmt.Fprintf(os.Stderr, "  #%-3d %-40s %-9s %-8s ttft %8.1fms  total %8.1fms  %6.1f tok/s  overhead %6.2fms  %s\n",
				r.Seq, r.Model, r.Endpoint, res, r.TTFTMs, r.TotalMs, r.TokPerSec, r.OverheadMs, r.Account)
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
	fmt.Fprintln(tw, "\nMODEL\tENDPOINT\tOK\tTTFT min/avg/p50/p95 ms\tTOTAL avg/p95 ms\tTOK/S\tOVERHEAD avg/p95 ms\tACCOUNTS\tERRORS")
	for _, s := range sum {
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%.0f / %.0f / %.0f / %.0f\t%.0f / %.0f\t%.1f\t%.2f / %.2f\t%s\t%s\n", s.Model, s.Endpoint, s.OK, s.Runs,
			s.TTFT.Min, s.TTFT.Avg, s.TTFT.P50, s.TTFT.P95, s.Total.Avg, s.Total.P95, s.TokPS, s.Overhead.Avg, s.Overhead.P95, s.Accounts, s.Errors)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, r := range rs {
		if r.NoText {
			fmt.Println("\n† only hidden reasoning tokens came back; those runs are left out of TTFT")
			break
		}
	}
	return nil
}
