package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.6.0", "0.6.0", 0},
		{"v0.6.0", "0.6.0", 0},
		{"0.6.1", "0.6.0", 1},
		{"0.6.0", "0.6.1", -1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.6.0-beta.1", "0.6.0", 0}, // suffixes are ignored
		{"0.6", "0.6.0", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCheckFetchesOnceAndCaches(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("accept header %q", r.Header.Get("Accept"))
		}
		_, _ = w.Write([]byte(`{"name":"fastrouter","version":"0.7.0"}`))
	}))
	defer srv.Close()

	c := New()
	c.URL = srv.URL
	first := c.Check(context.Background(), "0.6.0")
	if !first.UpdateAvailable || first.Latest != "0.7.0" || first.Command != Command {
		t.Fatalf("first check %+v", first)
	}
	second := c.Check(context.Background(), "0.6.0")
	if !second.UpdateAvailable || calls.Load() != 1 {
		t.Fatalf("second check %+v calls=%d", second, calls.Load())
	}
}

func TestCheckSameVersionIsNotAnUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.6.0"}`))
	}))
	defer srv.Close()
	c := New()
	c.URL = srv.URL
	info := c.Check(context.Background(), "0.6.0")
	if info.UpdateAvailable || info.Latest != "0.6.0" {
		t.Fatalf("%+v", info)
	}
}

func TestCheckErrorIsRemembered(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := New()
	c.URL = srv.URL
	first := c.Check(context.Background(), "0.6.0")
	if first.Error == "" || first.UpdateAvailable {
		t.Fatalf("first %+v", first)
	}
	second := c.Check(context.Background(), "0.6.0")
	if second.Error == "" || calls.Load() != 1 {
		t.Fatalf("second %+v calls=%d", second, calls.Load())
	}
	// After ErrorTTL the check retries.
	c.mu.Lock()
	c.at = time.Now().Add(-c.ErrorTTL - time.Minute)
	c.mu.Unlock()
	c.Check(context.Background(), "0.6.0")
	if calls.Load() != 2 {
		t.Fatalf("no retry after ErrorTTL: %d calls", calls.Load())
	}
}
