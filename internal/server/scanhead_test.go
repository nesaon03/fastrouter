package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/zenpo/fastrouter/internal/store"
)

func TestScanHeadMatchesUnmarshal(t *testing.T) {
	for _, b := range []string{
		`{}`,
		`{"model":"ds/x","stream":true,"max_tokens":12,"messages":[{"role":"user","content":"{\"model\":\"nope\"}"}]}`,
		` { "stream" : false , "model":"a\u00e9\"b", "temperature":0.5, "thinking":{"type":"enabled","budget_tokens":10},
		   "reasoning_effort":"high","reasoning":{"effort":"low"},"output_config":{},"stream_options":{"include_usage":true},"x":[1,[2,{"y":"]"}]] }`,
	} {
		var want, got reqHead
		if err := json.Unmarshal([]byte(b), &want); err != nil {
			t.Fatal(err)
		}
		if err := scanHead([]byte(b), &got); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		norm := func(h reqHead) string { out, _ := json.Marshal(h); return string(out) }
		if norm(got) != norm(want) {
			t.Errorf("scan %s\n got %s\nwant %s", b, norm(got), norm(want))
		}
	}
	for _, bad := range []string{``, `[]`, `{"model":}`, `{"model":"x"`, `{"a":1,}`, `{"model":"a","model":"b"}`} {
		var h reqHead
		if scanHead([]byte(bad), &h) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestPatchBody(t *testing.T) {
	got := patchBody([]byte(`{"model":"p/m","stream":true}`), "m", "stream_options", `{"include_usage":true}`)
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil || v["model"] != "m" || v["stream_options"] == nil {
		t.Fatalf("%s %v", got, err)
	}
	if !reflect.DeepEqual(patchBody([]byte(`{}`), "", "k", `1`), []byte(`{"k":1}`)) {
		t.Fatal("empty object")
	}
}

// L2: oversized bodies get a 413 instead of being truncated.
func TestL2TooLargeIs413(t *testing.T) {
	_, hs := newTestServer(t, &store.Snapshot{})
	req, _ := http.NewRequest("POST", hs.URL+"/v1/chat/completions", strings.NewReader("{}"))
	req.ContentLength = maxBody + 1
	req.Header.Set("Authorization", "Bearer "+localKey)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
}

func largeBody(kb int) []byte {
	var msgs []string
	for i := 0; len(msgs)*1100 < kb*1024; i++ {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":%q}`, strings.Repeat("some agent context text \"quoted\" ", 30)))
	}
	return []byte(`{"model":"ds/deepseek-v4.1-flash","stream":true,"max_tokens":4096,"messages":[` + strings.Join(msgs, ",") + `]}`)
}

// M6: per-request head cost on a ~540 KB body (the review measured 0.47 ms
// for json.Unmarshal plus 0.35 ms for two body copies).
func BenchmarkHeadLarge_Unmarshal(b *testing.B) {
	body := largeBody(540)
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		var h reqHead
		_ = json.Unmarshal(body, &h)
	}
}

func BenchmarkHeadLarge_Scan(b *testing.B) {
	body := largeBody(540)
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		var h reqHead
		_ = scanHead(body, &h)
	}
}

func BenchmarkPassthroughPatchLarge_TwoCopies(b *testing.B) {
	body := largeBody(540)
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		x, _ := replaceTopLevelString(body, "model", "deepseek-v4.1-flash")
		_ = injectTopLevel(x, "stream_options", `{"include_usage":true}`)
	}
}

func BenchmarkPassthroughPatchLarge_OneCopy(b *testing.B) {
	body := largeBody(540)
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		_ = patchBody(body, "deepseek-v4.1-flash", "stream_options", `{"include_usage":true}`)
	}
}
