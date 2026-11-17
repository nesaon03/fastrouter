package hrtime

import (
	"testing"
	"time"
)

func TestMonotonicAndFine(t *testing.T) {
	a := Now()
	time.Sleep(2 * time.Millisecond)
	if d := Since(a); d < 2*time.Millisecond || d > time.Second {
		t.Fatalf("since %v", d)
	}
	// Two back-to-back reads differ by far less than a millisecond.
	b, c := Now(), Now()
	if c < b || c-b > int64(time.Millisecond) {
		t.Fatalf("resolution %d", c-b)
	}
}
