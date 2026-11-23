package store

import "testing"

func TestSwarmMemoryPersistence(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveSwarmMemory("k1", "code", []byte(`{"runs":3,"scores":{"a/m1":0.7}}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSwarmMemory("k2", "code", []byte(`{"runs":1}`)); err != nil {
		t.Fatal(err)
	}
	doc, err := st.SwarmMemory("k1", "code")
	if err != nil || string(doc) != `{"runs":3,"scores":{"a/m1":0.7}}` {
		t.Fatalf("doc = %s err=%v", doc, err)
	}
	if _, err := st.SwarmMemory("k1", "math"); err == nil {
		t.Fatal("missing class should fail")
	}
	if _, err := st.SwarmMemory("k3", "code"); err == nil {
		t.Fatal("missing owner should fail")
	}
	// Replacing one lesson leaves the other intact.
	if err := st.SaveSwarmMemory("k1", "code", []byte(`{"runs":4}`)); err != nil {
		t.Fatal(err)
	}
	doc, _ = st.SwarmMemory("k2", "code")
	if string(doc) != `{"runs":1}` {
		t.Fatalf("other key changed: %s", doc)
	}
	if err := st.ClearSwarmMemory(""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SwarmMemory("k1", "code"); err == nil {
		t.Fatal("clear-all did not remove lessons")
	}
}
