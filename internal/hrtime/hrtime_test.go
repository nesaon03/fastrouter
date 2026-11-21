package hrtime

import (
	"testing"
	"time"
)

func TestMonotonicAndFine(t *testing.T) {
	a := Now()
	time.Sleep(20 * time.Millisecond)
	// Windows timers can return a little early, so allow a wide lower bound;
	// the point is that the clock advances with the sleep, not its exact value.
	if d := Since(a); d < 5*time.Millisecond || d > 2*time.Second {
		t.Fatalf("since %v", d)
	}
	// At least one of many back-to-back reads must be sub-millisecond: a coarse
	// clock (the reason this package exists) never is. Sampling avoids failing
	// when the scheduler preempts one pair.
	fine := false
	for i := 0; i < 200; i++ {
		b, c := Now(), Now()
		if c < b {
			t.Fatalf("clock went backwards: %d -> %d", b, c)
		}
		if c-b <= int64(time.Millisecond) {
			fine = true
		}
	}
	if !fine {
		t.Fatal("no sub-millisecond resolution in 200 samples")
	}
}
