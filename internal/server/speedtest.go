package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/bench"
	"github.com/zenpo/fastrouter/internal/store"
)

// Speed Test tab: runs bench against this server's own /v1 endpoints over
// loopback HTTP, the same path an IDE or CLI takes. The FastRouter API key it
// uses stays on the server; the browser only ever sees the key's name.

type benchJob struct {
	mu       sync.Mutex
	run      store.BenchRun
	opts     bench.Options
	total    int
	results  []bench.Result
	keyLabel string
	cancel   context.CancelFunc
}

type benchState struct {
	mu   sync.Mutex
	cur  *benchJob            // the running job, if any
	jobs map[string]*benchJob // recent jobs (live view)
	base string               // tests: override the loopback URL
}

func (j *benchJob) view() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	rs := append([]bench.Result(nil), j.results...)
	return map[string]any{"id": j.run.ID, "status": j.run.Status, "startedAt": j.run.StartedAt, "finishedAt": j.run.FinishedAt,
		"source": j.run.Source, "options": j.opts, "total": j.total, "done": len(rs), "results": rs,
		"summary": bench.Summarize(rs), "key": j.keyLabel}
}

// benchBase is the loopback URL of this server's own listener.
func (s *Server) benchBase() string {
	s.bench.mu.Lock()
	b := s.bench.base
	s.bench.mu.Unlock()
	if b != "" {
		return b
	}
	h := s.host
	if ip := net.ParseIP(h); h == "" || h == "localhost" || (ip != nil && ip.IsUnspecified()) {
		h = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(h, strconv.Itoa(s.port))
}

// benchKey picks an active local API key that may call every model. It
// returns the key (server-side only) and a label that is safe to show.
func (s *Server) benchKey(models []string) (key, label string, err error) {
	st := s.state.Load()
	if !st.HasAPIKeys() {
		return "", "no API keys set: local open mode", nil
	}
	for _, k := range st.Snap.APIKeys {
		if !k.Active || k.Key == "" {
			continue
		}
		ki := st.CheckAPIKey(k.Key)
		ok := ki != nil
		for _, m := range models {
			ok = ok && ki.Allows(m)
		}
		if ok {
			return k.Key, "API key “" + k.Name + "”", nil
		}
	}
	return "", "", errors.New("no active FastRouter API key may call all of these models; create one under API Keys or pick fewer models")
}

type benchCandidate struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // model | combo
	Working  bool   `json:"working"`
	Accounts int    `json:"accounts"`
}

// benchCandidates lists one test model per provider with usable accounts
// (the same model the live test uses) plus every combo. "Working" means an
// account of that provider answered 200 on its last request.
func (s *Server) benchCandidates() []benchCandidate {
	st := s.state.Load()
	var working map[string]string
	var stats map[string]store.ConnStats
	if s.st != nil {
		working, _ = s.st.WorkingModels()
		stats, _ = s.st.ConnectionStats()
	}
	ok := map[string]bool{}
	for _, c := range st.Snap.Connections {
		if c.Active && stats[c.ID].LastStatus == 200 {
			ok[c.Provider] = true
		}
	}
	out := []benchCandidate{}
	for _, p := range st.List {
		if p.Plugin || p.Pool.Size() == 0 {
			continue
		}
		m := testModel(st, p, "", working)
		if m == "" {
			continue
		}
		out = append(out, benchCandidate{ID: p.Prefix + "/" + m, Provider: p.ID, Name: p.Name, Kind: "model", Working: ok[p.ID], Accounts: p.Pool.Size()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Working && !out[j].Working })
	for _, c := range st.Snap.Combos {
		out = append(out, benchCandidate{ID: c.Name, Name: c.Name, Kind: "combo", Working: true})
	}
	return out
}

type benchProvider struct {
	ID       string `json:"id"`
	Status   string `json:"status"` // ok | fail | "" (no recent request)
	Accounts int    `json:"accounts"`
}

// benchProviders lists the providers the Speed Test can call (usable
// accounts) with the status of their accounts' last request.
func (s *Server) benchProviders() []benchProvider {
	st := s.state.Load()
	var stats map[string]store.ConnStats
	if s.st != nil {
		stats, _ = s.st.ConnectionStats()
	}
	status := map[string]string{}
	for _, c := range st.Snap.Connections {
		switch code := stats[c.ID].LastStatus; {
		case !c.Active || code == 0:
		case code == 200:
			status[c.Provider] = "ok"
		case status[c.Provider] == "":
			status[c.Provider] = "fail"
		}
	}
	out := []benchProvider{}
	for _, p := range st.List {
		if !p.Plugin && p.Pool.Size() > 0 {
			out = append(out, benchProvider{ID: p.ID, Status: status[p.ID], Accounts: p.Pool.Size()})
		}
	}
	return out
}

// benchModelStatus maps "prefix/model" to the status of its latest request.
func (s *Server) benchModelStatus() map[string]int {
	out := map[string]int{}
	if s.st == nil {
		return out
	}
	last, _ := s.st.LastModelStatus()
	for _, p := range s.state.Load().List {
		for m, code := range last[p.ID] {
			out[p.Prefix+"/"+m] = code
		}
	}
	return out
}

func (s *Server) handleBenchOptions(w http.ResponseWriter, r *http.Request) {
	_, label, _ := s.benchKey(nil)
	s.bench.mu.Lock()
	cur := ""
	if s.bench.cur != nil {
		cur = s.bench.cur.run.ID
	}
	s.bench.mu.Unlock()
	adminJSON(w, http.StatusOK, map[string]any{"candidates": s.benchCandidates(), "key": label, "base": s.benchBase(), "running": cur,
		"clients": bench.Profiles, "providers": s.benchProviders(), "modelStatus": s.benchModelStatus()})
}

func (s *Server) handleBenchStart(w http.ResponseWriter, r *http.Request) {
	var o bench.Options
	if !decodeStrict(w, r, &o) {
		return
	}
	if len(o.Models) == 1 && o.Models[0] == "@working" {
		o.Models = nil
		for _, c := range s.benchCandidates() {
			if c.Working && c.Kind == "model" {
				o.Models = append(o.Models, c.ID)
			}
		}
	}
	if err := o.Normalize(); err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if o.Total() > 300 {
		adminErr(w, http.StatusBadRequest, fmt.Sprintf("%d requests is too many for one run (max 300)", o.Total()))
		return
	}
	key, label, err := s.benchKey(o.Models)
	if err != nil {
		adminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.bench.mu.Lock()
	if s.bench.cur != nil {
		s.bench.mu.Unlock()
		adminErr(w, http.StatusConflict, "a speed test is already running")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &benchJob{opts: o, total: o.Total(), keyLabel: label, cancel: cancel,
		run: store.BenchRun{ID: newID(), StartedAt: time.Now(), Status: "running", Source: "dashboard"}}
	if s.bench.jobs == nil {
		s.bench.jobs = map[string]*benchJob{}
	}
	s.bench.cur, s.bench.jobs[j.run.ID] = j, j
	s.bench.mu.Unlock()

	o.BaseURL, o.APIKey = s.benchBase(), key
	s.saveBench(j)
	go func() {
		defer cancel()
		bench.Run(ctx, nil, o, func(res bench.Result) {
			res.Account = s.accountLabel(res.AccountID)
			j.mu.Lock()
			j.results = append(j.results, res)
			j.mu.Unlock()
		})
		j.mu.Lock()
		j.run.FinishedAt = time.Now()
		if ctx.Err() != nil {
			j.run.Status = "stopped"
		} else {
			j.run.Status = "done"
		}
		j.mu.Unlock()
		s.saveBench(j)
		s.bench.mu.Lock()
		s.bench.cur = nil
		for id, old := range s.bench.jobs { // keep memory small; history lives in SQLite
			if id != j.run.ID && old.run.Status != "running" {
				delete(s.bench.jobs, id)
			}
		}
		s.bench.mu.Unlock()
	}()
	adminJSON(w, http.StatusOK, map[string]any{"id": j.run.ID, "total": j.total, "key": label})
}

func (s *Server) saveBench(j *benchJob) {
	if s.st == nil {
		return
	}
	j.mu.Lock()
	run := j.run
	run.Options, _ = json.Marshal(j.opts)
	sum := bench.Summarize(j.results)
	run.Summary, _ = json.Marshal(map[string]any{"total": j.total, "done": len(j.results), "ok": countOK(j.results), "stats": sum, "key": j.keyLabel})
	run.Results, _ = json.Marshal(j.results)
	j.mu.Unlock()
	if err := s.st.SaveBenchRun(run); err != nil {
		log.Printf("speed test save failed: %v", err)
	}
}

func countOK(rs []bench.Result) int {
	n := 0
	for _, r := range rs {
		if r.OK {
			n++
		}
	}
	return n
}

// accountLabel shows which account served a request without exposing it fully.
func (s *Server) accountLabel(id string) string {
	if id == "" {
		return ""
	}
	st := s.state.Load()
	for _, c := range st.Snap.Connections {
		if c.ID == id {
			prov := c.Provider
			for _, p := range st.List {
				if p.ID == c.Provider && p.Name != "" {
					prov = p.Name
				}
			}
			return prov + " · " + maskEmail(c.Name)
		}
	}
	return id
}

func (s *Server) handleBenchStop(w http.ResponseWriter, r *http.Request) {
	s.bench.mu.Lock()
	j := s.bench.jobs[r.PathValue("id")]
	s.bench.mu.Unlock()
	if j == nil || j.cancel == nil {
		adminErr(w, http.StatusNotFound, "that run is not running")
		return
	}
	j.cancel()
	adminJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// benchView returns a live job, or a stored one.
func (s *Server) benchView(id string) (map[string]any, error) {
	s.bench.mu.Lock()
	j := s.bench.jobs[id]
	s.bench.mu.Unlock()
	if j != nil {
		return j.view(), nil
	}
	if s.st == nil {
		return nil, store.ErrNotFound
	}
	run, err := s.st.BenchRun(id)
	if err != nil {
		return nil, err
	}
	var o bench.Options
	var rs []bench.Result
	var sum struct {
		Total int    `json:"total"`
		Key   string `json:"key"`
	}
	_ = json.Unmarshal(run.Options, &o)
	_ = json.Unmarshal(run.Results, &rs)
	_ = json.Unmarshal(run.Summary, &sum)
	if run.Status == "running" {
		run.Status = "interrupted" // the server restarted mid-run
	}
	return map[string]any{"id": run.ID, "status": run.Status, "startedAt": run.StartedAt, "finishedAt": run.FinishedAt,
		"source": run.Source, "options": o, "total": sum.Total, "done": len(rs), "results": rs,
		"summary": bench.Summarize(rs), "key": sum.Key}, nil
}

func (s *Server) handleBenchGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.benchView(r.PathValue("id"))
	if err != nil {
		adminErr(w, http.StatusNotFound, "run not found")
		return
	}
	adminJSON(w, http.StatusOK, v)
}

func (s *Server) handleBenchHistory(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		adminJSON(w, http.StatusOK, map[string]any{"runs": []any{}})
		return
	}
	runs, err := s.st.BenchRuns(100)
	if err != nil {
		adminErr(w, http.StatusInternalServerError, "could not read history")
		return
	}
	s.bench.mu.Lock()
	for i := range runs {
		if runs[i].Status == "running" && s.bench.jobs[runs[i].ID] == nil {
			runs[i].Status = "interrupted"
		}
	}
	s.bench.mu.Unlock()
	adminJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleBenchDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "all" {
		id = ""
	}
	s.bench.mu.Lock()
	if cur := s.bench.cur; cur != nil && (id == "" || cur.run.ID == id) {
		s.bench.mu.Unlock()
		adminErr(w, http.StatusConflict, "stop the running speed test first")
		return
	}
	if id == "" {
		s.bench.jobs = map[string]*benchJob{}
	} else {
		delete(s.bench.jobs, id)
	}
	s.bench.mu.Unlock()
	if s.st != nil {
		if err := s.st.DeleteBenchRun(id); err != nil {
			adminErr(w, http.StatusInternalServerError, "could not delete")
			return
		}
	}
	adminJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleBenchExport downloads one run (or "all") as JSON or CSV.
func (s *Server) handleBenchExport(w http.ResponseWriter, r *http.Request) {
	id, format := r.PathValue("id"), r.URL.Query().Get("format")
	var runs []map[string]any
	if id == "all" {
		if s.st != nil {
			list, _ := s.st.BenchRuns(keepBenchExport)
			for _, x := range list {
				if v, err := s.benchView(x.ID); err == nil {
					runs = append(runs, v)
				}
			}
		}
	} else {
		v, err := s.benchView(id)
		if err != nil {
			adminErr(w, http.StatusNotFound, "run not found")
			return
		}
		runs = append(runs, v)
	}
	name := "fastrouter-speedtest-" + id + "-" + time.Now().Format("20060102-150405")
	if format == "csv" {
		var buf bytes.Buffer
		cw := csv.NewWriter(&buf)
		_ = cw.Write([]string{"run_id", "run_started", "seq", "at", "client", "model", "endpoint", "stream", "ok", "status", "error_class", "error",
			"ttfb_ms", "ttft_ms", "total_ms", "in_tokens", "out_tokens", "tokens_estimated", "tok_per_sec",
			"overhead_ms", "pre_ms", "post_ms", "hold_ms", "upstream_ttfb_ms", "attempts", "account", "upstream", "token_saver"})
		f := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
		for _, run := range runs {
			rs, _ := run["results"].([]bench.Result)
			started, _ := run["startedAt"].(time.Time)
			for _, x := range rs {
				_ = cw.Write([]string{fmt.Sprint(run["id"]), started.Format(time.RFC3339), strconv.Itoa(x.Seq), x.At.Format(time.RFC3339Nano),
					x.Client, x.Model, x.Endpoint, strconv.FormatBool(x.Stream), strconv.FormatBool(x.OK), strconv.Itoa(x.Status), x.Class, x.Message,
					f(x.TTFBMs), f(x.TTFTMs), f(x.TotalMs), strconv.Itoa(x.InTokens), strconv.Itoa(x.OutTokens), strconv.FormatBool(x.Estimated), f(x.TokPerSec),
					f(x.OverheadMs), f(x.PreMs), f(x.PostMs), f(x.HoldMs), f(x.UpTTFBMs), strconv.Itoa(x.Attempts), x.Account, x.Upstream, x.Savers})
			}
		}
		cw.Flush()
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
		_, _ = w.Write(buf.Bytes())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if id == "all" {
		_ = enc.Encode(map[string]any{"runs": runs})
	} else {
		_ = enc.Encode(runs[0])
	}
}

const keepBenchExport = 200

// BenchRecord stores a CLI run in the same history as the dashboard.
func (s *Server) BenchRecord(o bench.Options, started time.Time, rs []bench.Result, keyLabel string) error {
	if s.st == nil {
		return nil
	}
	j := &benchJob{opts: o, total: o.Total(), results: rs, keyLabel: keyLabel,
		run: store.BenchRun{ID: newID(), StartedAt: started, FinishedAt: time.Now(), Status: "done", Source: "cli"}}
	if len(rs) < j.total {
		j.run.Status = "stopped"
	}
	s.saveBench(j)
	return nil
}

// BenchKey and AccountLabel let `fastrouter bench` reuse the dashboard's choices.
func (s *Server) BenchKey(models []string) (string, string, error) { return s.benchKey(models) }
func (s *Server) AccountLabel(id string) string                    { return s.accountLabel(id) }

// BenchWorkingModels is the "all working" list.
func (s *Server) BenchWorkingModels() []string {
	var out []string
	for _, c := range s.benchCandidates() {
		if c.Working && c.Kind == "model" {
			out = append(out, c.ID)
		}
	}
	return out
}
