package store

import (
	"testing"
	"time"
)

func TestUsageSaverColumns(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	events := []UsageEvent{
		{At: now.Add(-2 * time.Minute), ConnectionID: "c1", Provider: "p", Model: "m", Status: 200,
			Input: 100, Output: 50, Cost: 0.01, SavedInput: 80, AddedInput: 20, SavedCost: 0.002, SaverTag: "RTK:1"},
		{At: now.Add(-time.Minute), ConnectionID: "c1", Provider: "p", Model: "m", Status: 200,
			Input: 10, Output: 10, Cost: 0.001, SavedInput: 0, AddedInput: 5},
	}
	if err := st.WriteUsage(events); err != nil {
		t.Fatal(err)
	}
	tot, err := st.SaverTotals(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if tot.Requests != 2 || tot.OK != 2 || tot.Output != 60 || tot.SavedInput != 80 || tot.AddedInput != 25 {
		t.Fatalf("totals = %+v", tot)
	}
	empty, err := st.SaverTotals(now.Add(time.Hour))
	if err != nil || empty.Requests != 0 || empty.OK != 0 || empty.SavedInput != 0 {
		t.Fatalf("empty totals = %+v err=%v", empty, err)
	}
	if tot.SavedCost != 0.002 {
		t.Fatalf("saved cost = %v", tot.SavedCost)
	}
	rows, err := st.UsageReport(now.Add(-time.Hour), "provider")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SavedInput != 80 || rows[0].AddedInput != 25 || rows[0].SavedCost != 0.002 {
		t.Fatalf("rows = %+v", rows)
	}
	entries, total, err := st.UsageEntries(now.Add(-time.Hour), "", 10, 0)
	if err != nil || total != 2 {
		t.Fatalf("entries: %v total=%d", err, total)
	}
	if entries[0].SaverTag != "" || entries[0].AddedInput != 5 {
		t.Fatalf("newest entry = %+v", entries[0])
	}
	if entries[1].SaverTag != "RTK:1" || entries[1].SavedInput != 80 {
		t.Fatalf("oldest entry = %+v", entries[1])
	}
}
