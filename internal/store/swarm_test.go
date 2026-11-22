package store

import (
	"testing"
	"time"
)

func TestSwarmRunPersistence(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	if err := st.SaveSwarmRun("run-1", "team", now, "done", true, []byte(`{"id":"run-1","fullTexts":[["hello"]],"judgeText":"final"}`)); err != nil {
		t.Fatal(err)
	}
	rows, err := st.SwarmRuns(10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v err=%v", rows, err)
	}
	if rows[0].ID != "run-1" || rows[0].Combo != "team" || !rows[0].Consensus || rows[0].Doc != nil {
		t.Fatalf("row = %+v", rows[0])
	}
	doc, err := st.SwarmRun("run-1")
	if err != nil || string(doc) != `{"id":"run-1","fullTexts":[["hello"]],"judgeText":"final"}` {
		t.Fatalf("doc = %s err=%v", doc, err)
	}
	if _, err := st.SwarmRun("missing"); err == nil {
		t.Fatal("missing run should fail")
	}
	// Replacing the same id keeps one row.
	if err := st.SaveSwarmRun("run-1", "team", now, "error", false, []byte(`{"id":"run-1"}`)); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.SwarmRuns(10)
	if len(rows) != 1 || rows[0].Status != "error" || rows[0].Consensus {
		t.Fatalf("after replace = %+v", rows)
	}
}
