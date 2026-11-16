package logring

import (
	"fmt"
	"strings"
	"testing"
)

func TestRingRedactsAndBounds(t *testing.T) {
	r := New(3)
	fmt.Fprintf(r, "a\nupstream failed: Authorization: Bearer sk-abcdefghijklmnopqrstuv for someone@example.com\n")
	_, ch, stop := r.Subscribe()
	defer stop()
	fmt.Fprint(r, "par")
	fmt.Fprint(r, "tial\nd\ne\n")
	lines := r.Snapshot()
	if len(lines) != 3 || lines[0].Text != "partial" || lines[2].Text != "e" {
		t.Fatalf("%+v", lines)
	}
	all := New(10)
	fmt.Fprintln(all, "upstream failed: Authorization: Bearer sk-abcdefghijklmnopqrstuv for someone@example.com")
	got := all.Snapshot()[0]
	if strings.Contains(got.Text, "sk-abc") || strings.Contains(got.Text, "someone@") || got.Level != "error" {
		t.Fatalf("%+v", got)
	}
	if ev := <-ch; ev.Line.Text != "partial" {
		t.Fatalf("%+v", ev)
	}
	r.Clear()
	if len(r.Snapshot()) != 0 {
		t.Fatal("not cleared")
	}
}
