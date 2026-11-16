package server

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
)

func (s *Server) inflightFor(provider string) *atomic.Int64 {
	if v, ok := s.inflight.Load(provider); ok {
		return v.(*atomic.Int64)
	}
	v, _ := s.inflight.LoadOrStore(provider, new(atomic.Int64))
	return v.(*atomic.Int64)
}

// usagePeriod maps 9Router's period ids to a start time (local midnight for today).
func usagePeriod(p string, now time.Time) (string, time.Time) {
	switch p {
	case "today":
		y, m, d := now.Date()
		return p, time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	case "7d", "30d", "60d":
		n, _ := strconv.Atoi(p[:len(p)-1])
		return p, now.Add(-time.Duration(n) * 24 * time.Hour)
	case "all":
		return p, time.Time{}
	}
	return "24h", now.Add(-24 * time.Hour)
}

type seriesPoint struct {
	Label    string  `json:"label"`
	At       string  `json:"at"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

// usageSeries buckets hourly rows by local hour (today/24h) or local day.
func usageSeries(rows []store.UsageBucket, period string, since, now time.Time) []seriesPoint {
	hourly := period == "today" || period == "24h"
	if period == "all" {
		since = now.Add(-30 * 24 * time.Hour)
		if len(rows) > 0 && rows[0].Hour.Before(since) {
			since = rows[0].Hour
		}
	}
	trunc := func(t time.Time) time.Time {
		t = t.In(now.Location())
		if hourly {
			return t.Truncate(time.Hour)
		}
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	}
	idx := map[int64]int{}
	out := []seriesPoint{}
	for t := trunc(since); !t.After(now); {
		idx[t.Unix()] = len(out)
		label := t.Format("Jan 2")
		if hourly {
			label = t.Format("15:04")
		}
		out = append(out, seriesPoint{Label: label, At: t.UTC().Format(time.RFC3339)})
		if hourly {
			t = t.Add(time.Hour)
		} else {
			t = t.AddDate(0, 0, 1)
		}
		if len(out) > 400 {
			break
		}
	}
	for _, r := range rows {
		if i, ok := idx[trunc(r.Hour).Unix()]; ok {
			out[i].Requests += r.Requests
			out[i].Tokens += r.Tokens
			out[i].Cost += r.Cost
		}
	}
	return out
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		writeJSON(w, map[string]any{"error": "no database"})
		return
	}
	q := r.URL.Query()
	now := time.Now()
	period, since := usagePeriod(firstNonEmpty(q.Get("period"), q.Get("window")), now)
	out := map[string]any{"window": period, "period": period, "since": since.UTC().Format(time.RFC3339),
		"note": "Estimated, not actual billing (9Router's price table)"}
	for _, by := range []string{"account", "model", "key", "provider"} {
		rows, err := s.st.UsageReport(since, by)
		if err != nil {
			http.Error(w, "usage query failed", http.StatusInternalServerError)
			return
		}
		if by == "account" {
			for i := range rows {
				rows[i].Name = maskEmail(rows[i].Name)
			}
		}
		out[by+"s"] = rows
	}
	hours, err := s.st.UsageHours(since)
	if err != nil {
		http.Error(w, "usage query failed", http.StatusInternalServerError)
		return
	}
	out["series"] = usageSeries(hours, period, since, now)
	recent, _, err := s.st.UsageEntries(time.Time{}, "", 20, 0)
	if err != nil {
		http.Error(w, "usage query failed", http.StatusInternalServerError)
		return
	}
	for i := range recent {
		recent[i].Account = maskEmail(recent[i].Account)
	}
	out["recent"] = recent
	active := map[string]int64{}
	s.inflight.Range(func(k, v any) bool {
		if n := v.(*atomic.Int64).Load(); n > 0 {
			active[k.(string)] = n
		}
		return true
	})
	out["active"] = active
	writeJSON(w, out)
}

// handleUsageRequests is the Details tab: one row per attempt, paginated.
func (s *Server) handleUsageRequests(w http.ResponseWriter, r *http.Request) {
	if s.st == nil {
		writeJSON(w, map[string]any{"error": "no database"})
		return
	}
	q := r.URL.Query()
	period, since := usagePeriod(q.Get("period"), time.Now())
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	rows, total, err := s.st.UsageEntries(since, q.Get("provider"), limit, offset)
	if err != nil {
		http.Error(w, "usage query failed", http.StatusInternalServerError)
		return
	}
	for i := range rows {
		rows[i].Account = maskEmail(rows[i].Account)
	}
	writeJSON(w, map[string]any{"period": period, "total": total, "limit": limit, "offset": offset, "requests": rows})
}
