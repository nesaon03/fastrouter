package oauth

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCursorChecksumAndSession(t *testing.T) {
	// Same math as 9Router's generateCursorChecksum for a fixed time.
	got := cursorChecksum("mid", time.UnixMilli(1_763_370_000_000))
	if !strings.HasSuffix(got, "mid") || len(got) != 8+3 {
		t.Fatalf("checksum %q", got)
	}
	// RFC 4122 uuid v5 of "www.example.com" in the DNS namespace.
	if u := uuidV5DNS("www.example.com"); u != "2ed6657d-e927-568b-95e1-2665a8aea6a2" {
		t.Fatalf("uuid v5 %s", u)
	}
	h := cursorHeaders("pre::tok", "m1")
	if h["authorization"] != "Bearer tok" || h["x-client-key"] != sha256Hex("tok") || h["content-type"] != "application/connect+proto" {
		t.Fatalf("headers %v", h)
	}
}

func TestCursorPasteParsing(t *testing.T) {
	tok := "aaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.cccccccccccccccccccc"
	mid := "d6eec8e1-1287-4047-a14b-2aa565ffe0b0"
	for _, in := range []string{
		`{"accessToken":"` + tok + `","machineId":"` + mid + `"}`,
		tok + "\n" + mid,
		"cursorAuth/accessToken|" + tok + "\nstorage.serviceMachineId|" + mid,
	} {
		gt, gm := parseCursorPaste(in)
		if gt != tok || gm != mid {
			t.Fatalf("%q -> %q %q", in, gt, gm)
		}
	}
}

func TestCursorRunFrame(t *testing.T) {
	var b cursorBody
	_ = json.Unmarshal([]byte(`{"model":"gpt-5","reasoning_effort":"high","messages":[{"role":"system","content":"Be brief"},
		{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":[{"type":"text","text":"Say OK"}]}],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]}`), &b)
	f := cursorRunFrame(&b, "gpt-5")
	if f[0] != 0 || int(f[1])<<24|int(f[2])<<16|int(f[3])<<8|int(f[4]) != len(f)-5 {
		t.Fatal("bad connect frame")
	}
	run := pbDecode(f[5:]).msg(1)
	user := run.msg(2).msg(1).msg(1)
	if txt := user.str(1); txt != "Be brief\n\nSay OK" {
		t.Fatalf("user text %q", txt)
	}
	if hist := run.msg(2).msg(1).msg(7).all(1); len(hist) != 2 {
		t.Fatalf("history %d", len(hist))
	}
	if run.msg(3).str(1) != "gpt-5" || run.msg(9).str(1) != "gpt-5" || run.msg(9).msg(3).str(1) != "reasoning" {
		t.Fatal("model fields")
	}
	tool := run.msg(4).msg(1)
	if tool.str(1) != "f" {
		t.Fatal("tool")
	}
	sf, _ := tool.get(3)
	if v, _ := pbValueDecode(sf.b).(map[string]any); v["type"] != "object" {
		t.Fatalf("schema %v", v)
	}
}

func serverFrame(msg []byte) []byte { return connectFrame(msg) }

func TestCursorTurnTextAndAcks(t *testing.T) {
	var in bytes.Buffer
	in.Write(serverFrame(pbLen(2, pbCat(pbUint(1, 7), pbStr(15, "e1"), pbLen(10, nil))))) // request_context
	in.Write(serverFrame(pbLen(4, pbCat(pbUint(1, 3), pbLen(2, nil)))))                   // kv get
	in.Write(serverFrame(pbLen(1, pbLen(1, pbStr(1, "O")))))                              // text delta
	in.Write(serverFrame(pbLen(1, pbLen(1, pbStr(1, "K")))))                              // text delta
	in.Write(serverFrame(pbLen(1, pbLen(14, nil))))                                       // turn ended
	var acks bytes.Buffer
	var text, kinds string
	cursorTurn(&in, &acks, "gpt-5", func(e cursorEvent) { kinds += e.kind + ","; text += e.text })
	if text != "OK" || kinds != "text,text,done," {
		t.Fatalf("%q %q", text, kinds)
	}
	ack := pbDecode(acks.Bytes()[5:]).msg(2)
	if id, _ := ack.get(1); id.n != 7 || ack.str(15) != "e1" || !ack.has(10) {
		t.Fatal("request_context ack")
	}
}

func TestCursorTurnToolCallAndEmpty(t *testing.T) {
	entry := pbLen(2, pbCat(pbStr(1, "path"), pbLen(2, pbValue("a.txt"))))
	var in bytes.Buffer
	in.Write(serverFrame(pbLen(2, pbCat(pbUint(1, 1), pbStr(15, "x"), pbLen(11, pbCat(pbStr(1, "read"), entry, pbStr(3, "call_1"), pbStr(5, "read")))))))
	var got []cursorEvent
	cursorTurn(&in, &bytes.Buffer{}, "m", func(e cursorEvent) { got = append(got, e) })
	if len(got) != 2 || got[0].kind != "tool" || got[0].tool != [3]string{"call_1", "read", `{"path":"a.txt"}`} || got[1].finish != "tool_calls" {
		t.Fatalf("%+v", got)
	}
	got = nil
	cursorTurn(bytes.NewReader(serverFrame(pbLen(1, pbLen(14, nil)))), &bytes.Buffer{}, "m", func(e cursorEvent) { got = append(got, e) })
	if len(got) != 1 || got[0].kind != "error" {
		t.Fatalf("empty turn: %+v", got)
	}
}
