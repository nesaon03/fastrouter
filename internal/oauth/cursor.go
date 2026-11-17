package oauth

// Cursor IDE: 9Router's import-token sign-in and its AgentService executor
// (open-sse/executors/cursor.js, utils/cursorProtobuf.js, utils/cursorChecksum.js,
// services/cursorModels.js and src/lib/oauth/services/cursor.js; MIT,
// decolua/9router). Cursor speaks Connect-RPC protobuf over HTTP/2: one
// bidirectional Run stream per turn, where FastRouter answers the server's
// context/KV requests and turns its text deltas into OpenAI SSE.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zenpo/fastrouter/internal/store"
	_ "modernc.org/sqlite"
)

const (
	CursorAgentBase     = "https://agent.api5.cursor.sh"
	cursorRunPath       = "/agent.v1.AgentService/Run"
	cursorModelsPath    = "/agent.v1.AgentService/GetUsableModels"
	cursorClientVersion = "3.12.17"
	cursorClientCommit  = "0fb762053c34788bb7760d5673f8a6d4c8589d50"
)

// CursorRunURL is the AgentService Run endpoint.
const CursorRunURL = CursorAgentBase + cursorRunPath

// cursorClient talks HTTP/2 (the agent host is h2-only) with no overall
// timeout: the proxy's first-byte and idle timers bound each stream.
var cursorClient = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = true
	return &http.Client{Transport: t}
}()

func init() {
	register(&Spec{ID: "cursor", Kind: KindPaste, Risk: true, RefreshLead: 24 * time.Hour,
		Paste: func(ctx context.Context, e Env, text string) (*Result, error) {
			var tok, mid, email string
			if t := strings.ToLower(strings.TrimSpace(text)); t == "import" || t == "auto" {
				app, err := ReadCursorApp()
				if err != nil {
					return nil, err
				}
				tok, mid, email = app.AccessToken, app.MachineID, app.Email
			} else {
				tok, mid = parseCursorPaste(text)
			}
			if len(tok) < 50 {
				return nil, errors.New("no Cursor access token found (expected the cursorAuth/accessToken value)")
			}
			if len(strings.ReplaceAll(mid, "-", "")) < 32 {
				return nil, errors.New("no Cursor machine id found (expected the storage.serviceMachineId value)")
			}
			if _, err := CursorModels(ctx, tok, mid, true); err != nil {
				return nil, fmt.Errorf("Cursor rejected the token: %w", err)
			}
			return cursorResult(tok, mid, email), nil
		},
		// Cursor has no public refresh endpoint. The IDE keeps its session
		// fresh in its local database, so a refresh re-reads it from there.
		Refresh: func(ctx context.Context, e Env, c store.Connection) (*Result, error) {
			cur := AccessKey(c)
			mid := Meta(c, "machineId")
			if app, err := ReadCursorApp(); err == nil && app.AccessToken != "" && app.MachineID == mid &&
				jwtExp(app.AccessToken).After(jwtExp(cur)) {
				return cursorResult(app.AccessToken, app.MachineID, app.Email), nil
			}
			if exp := jwtExp(cur); exp.After(time.Now()) {
				return &Result{ExpiresIn: int(time.Until(exp).Seconds())}, nil
			}
			return nil, ErrReauth
		},
	})
	adapters["cursor"] = &Adapter{
		Route: func(string, bool, Account) (string, string) { return FmtOpenAI, CursorRunURL },
		Headers: func(h http.Header, key string, _ bool, a Account) {
			for k := range h {
				h.Del(k)
			}
			for k, v := range cursorHeaders(key, a.Extra["machineId"]) {
				h.Set(k, v)
			}
		},
		Do: cursorDo,
	}
}

func cursorResult(tok, mid, email string) *Result {
	r := &Result{Secrets: map[string]string{"accessToken": tok},
		PSD: map[string]any{"machineId": mid, "authMethod": "imported"}}
	if sub := jwtClaim(tok, "sub"); sub != "" {
		r.PSD["userId"] = sub
	}
	r.Email = firstNonEmpty(email, jwtClaim(tok, "email"))
	r.Name = firstNonEmpty(r.Email, "Cursor")
	if exp := jwtExp(tok); exp.After(time.Now()) {
		r.ExpiresIn = int(time.Until(exp).Seconds())
	}
	return r
}

// parseCursorPaste accepts {"accessToken","machineId"} JSON, the two values on
// separate lines, or sqlite3's "key|value" output.
func parseCursorPaste(text string) (tok, mid string) {
	var j struct {
		AccessToken string `json:"accessToken"`
		MachineID   string `json:"machineId"`
	}
	if json.Unmarshal([]byte(text), &j) == nil && j.AccessToken != "" {
		return strings.TrimSpace(j.AccessToken), strings.TrimSpace(j.MachineID)
	}
	for _, f := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' || r == ' ' || r == '\t' || r == '|' || r == ',' }) {
		f = strings.Trim(f, `"'`)
		switch {
		case strings.Count(f, ".") == 2 && len(f) > 50:
			tok = f
		case len(strings.ReplaceAll(f, "-", "")) >= 32 && strings.Trim(strings.ReplaceAll(f, "-", ""), "0123456789abcdefABCDEF") == "":
			mid = f
		}
	}
	return tok, mid
}

func jwtExp(tok string) time.Time {
	if n, err := strconv.ParseInt(jwtClaim(tok, "exp"), 10, 64); err == nil {
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// CursorApp is the session the Cursor IDE on this machine is signed in with.
type CursorApp struct{ AccessToken, MachineID, Email string }

// cursorStatePath is where Cursor keeps its session (9Router CURSOR_CONFIG.tokenStoragePaths).
func cursorStatePath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "Cursor", "User", "globalStorage", "state.vscdb")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	}
	return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
}

// ReadCursorApp reads the Cursor IDE's session from its local database (read-only).
func ReadCursorApp() (*CursorApp, error) {
	p := cursorStatePath()
	if _, err := os.Stat(p); err != nil {
		return nil, errors.New("Cursor IDE is not installed or never signed in on this machine (no state.vscdb)")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(p)+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, errors.New("could not open Cursor's state database")
	}
	defer db.Close()
	get := func(k string) string {
		var v []byte
		_ = db.QueryRow("SELECT value FROM ItemTable WHERE key = ?", k).Scan(&v)
		return strings.Trim(strings.TrimSpace(string(v)), `"`)
	}
	app := &CursorApp{AccessToken: get("cursorAuth/accessToken"), MachineID: get("storage.serviceMachineId"), Email: get("cursorAuth/cachedEmail")}
	if app.AccessToken == "" {
		return nil, errors.New("Cursor IDE is not signed in on this machine; sign in to Cursor first")
	}
	return app, nil
}

// ---- headers (cursorChecksum.js) ----

func cursorHeaders(token, machineID string) map[string]string {
	if i := strings.Index(token, "::"); i >= 0 {
		token = token[i+2:]
	}
	if machineID == "" {
		machineID = sha256Hex(token + "machineId")
	}
	osName, arch := "linux", "x64"
	switch runtime.GOOS {
	case "windows":
		osName = "windows"
	case "darwin":
		osName = "macos"
	}
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	return map[string]string{
		"authorization":               "Bearer " + token,
		"connect-accept-encoding":     "gzip",
		"connect-protocol-version":    "1",
		"content-type":                "application/connect+proto",
		"user-agent":                  "connect-es/1.6.1",
		"x-amzn-trace-id":             "Root=" + uuid4(),
		"x-client-key":                sha256Hex(token),
		"x-cursor-checksum":           cursorChecksum(machineID, time.Now()),
		"x-cursor-client-version":     cursorClientVersion,
		"x-cursor-client-commit":      cursorClientCommit,
		"x-cursor-client-type":        "ide",
		"x-cursor-client-os":          osName,
		"x-cursor-client-arch":        arch,
		"x-cursor-client-device-type": "desktop",
		"x-cursor-config-version":     uuid4(),
		"x-cursor-timezone":           localZoneName(),
		"x-ghost-mode":                "true",
		"x-request-id":                uuid4(),
		"x-session-id":                uuidV5DNS(token),
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// cursorChecksum is the "Jyh cipher": a 6-byte timestamp, XOR-chained, then
// URL-safe base64, followed by the machine id.
func cursorChecksum(machineID string, now time.Time) string {
	ts := uint64(now.UnixMilli() / 1_000_000)
	b := []byte{byte(ts >> 40), byte(ts >> 32), byte(ts >> 24), byte(ts >> 16), byte(ts >> 8), byte(ts)}
	t := byte(165)
	for i := range b {
		b[i] = (b[i] ^ t) + byte(i)
		t = b[i]
	}
	return base64.RawURLEncoding.EncodeToString(b) + machineID
}

func uuidV5DNS(name string) string {
	ns, _ := hex.DecodeString("6ba7b8109dad11d180b400c04fd430c8")
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(name))
	b := h.Sum(nil)[:16]
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	x := hex.EncodeToString(b)
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

func localZoneName() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if n := time.Local.String(); n != "" && n != "Local" {
		return n
	}
	return "UTC"
}

// ---- protobuf (cursorProtobuf.js) ----

func pbVarint(v uint64) []byte { return binary.AppendUvarint(nil, v) }

func pbTag(field, wire int) []byte { return pbVarint(uint64(field<<3 | wire)) }

func pbLen(field int, b []byte) []byte {
	out := append(pbTag(field, 2), pbVarint(uint64(len(b)))...)
	return append(out, b...)
}

func pbStr(field int, s string) []byte { return pbLen(field, []byte(s)) }

func pbUint(field int, v uint64) []byte { return append(pbTag(field, 0), pbVarint(v)...) }

func pbCat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

type pbField struct {
	num  int
	wire int
	n    uint64
	b    []byte
}

type pbMsg []pbField

func pbDecode(b []byte) pbMsg {
	var out pbMsg
	for len(b) > 0 {
		tag, k := binary.Uvarint(b)
		if k <= 0 {
			break
		}
		b = b[k:]
		f := pbField{num: int(tag >> 3), wire: int(tag & 7)}
		switch f.wire {
		case 0:
			v, k := binary.Uvarint(b)
			if k <= 0 {
				return out
			}
			f.n, b = v, b[k:]
		case 1:
			if len(b) < 8 {
				return out
			}
			f.b, b = b[:8], b[8:]
		case 2:
			l, k := binary.Uvarint(b)
			if k <= 0 || uint64(len(b)-k) < l {
				return out
			}
			f.b, b = b[k:k+int(l)], b[k+int(l):]
		case 5:
			if len(b) < 4 {
				return out
			}
			f.b, b = b[:4], b[4:]
		default:
			return out
		}
		out = append(out, f)
	}
	return out
}

func (m pbMsg) get(n int) (pbField, bool) {
	for _, f := range m {
		if f.num == n {
			return f, true
		}
	}
	return pbField{}, false
}

func (m pbMsg) has(n int) bool { _, ok := m.get(n); return ok }

func (m pbMsg) str(n int) string { f, _ := m.get(n); return string(f.b) }

func (m pbMsg) msg(n int) pbMsg { f, _ := m.get(n); return pbDecode(f.b) }

func (m pbMsg) all(n int) []pbField {
	var out []pbField
	for _, f := range m {
		if f.num == n {
			out = append(out, f)
		}
	}
	return out
}

func connectFrame(payload []byte) []byte {
	out := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}

// pbValue encodes a JSON value as google.protobuf.Value.
func pbValue(v any) []byte {
	switch x := v.(type) {
	case nil:
		return pbUint(1, 0)
	case bool:
		n := uint64(0)
		if x {
			n = 1
		}
		return pbUint(4, n)
	case float64:
		return append(pbTag(2, 1), binary.LittleEndian.AppendUint64(nil, math.Float64bits(x))...)
	case json.Number:
		f, _ := x.Float64()
		return pbValue(f)
	case string:
		return pbStr(3, x)
	case []any:
		var items []byte
		for _, it := range x {
			items = append(items, pbLen(1, pbValue(it))...)
		}
		return pbLen(6, items)
	case map[string]any:
		var entries []byte
		for k, val := range x {
			entries = append(entries, pbLen(1, pbCat(pbStr(1, k), pbLen(2, pbValue(val))))...)
		}
		return pbLen(5, entries)
	}
	return pbStr(3, fmt.Sprint(v))
}

func pbValueDecode(b []byte) any {
	m := pbDecode(b)
	if len(m) == 0 {
		return nil
	}
	f := m[0]
	switch f.num {
	case 2:
		if len(f.b) == 8 {
			return math.Float64frombits(binary.LittleEndian.Uint64(f.b))
		}
	case 3:
		return string(f.b)
	case 4:
		return f.n != 0
	case 5:
		out := map[string]any{}
		for _, e := range pbDecode(f.b).all(1) {
			pair := pbDecode(e.b)
			if k := pair.str(1); k != "" {
				vf, _ := pair.get(2)
				out[k] = pbValueDecode(vf.b)
			}
		}
		return out
	case 6:
		out := []any{}
		for _, e := range pbDecode(f.b).all(1) {
			out = append(out, pbValueDecode(e.b))
		}
		return out
	}
	return nil
}

// ---- Run request (executors/cursor.js buildAgentRunFrame) ----

type cursorMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type cursorBody struct {
	Model    string      `json:"model"`
	Messages []cursorMsg `json:"messages"`
	Tools    []struct {
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  any    `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	ReasoningEffort string `json:"reasoning_effort"`
}

// cursorText returns a message's text, or ok=false when it has non-text parts.
func cursorText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var out []string
	ok := true
	for _, p := range parts {
		if p.Type != "" && p.Type != "text" {
			ok = false
			continue
		}
		out = append(out, p.Text)
	}
	return strings.Join(out, "\n"), ok
}

func cursorHistory(m cursorMsg) []byte {
	text, _ := cursorText(m.Content)
	parts := []string{}
	if text != "" {
		parts = append(parts, text)
	}
	if m.Role == "assistant" {
		for _, tc := range m.ToolCalls {
			parts = append(parts, fmt.Sprintf("[tool_call id=%s name=%s args=%s]", tc.ID, firstNonEmpty(tc.Function.Name, "tool"), firstNonEmpty(tc.Function.Arguments, "{}")))
		}
	}
	if m.Role == "tool" {
		parts = append(parts, "[tool_result id="+m.ToolCallID+"]")
	}
	body := strings.Join(parts, "\n")
	if body == "" {
		return nil
	}
	t := pbStr(1, body)
	if m.Role == "assistant" {
		return pbLen(2, pbLen(1, pbLen(1, t)))
	}
	return pbLen(1, pbLen(1, pbLen(1, t)))
}

func cursorRunFrame(b *cursorBody, model string) []byte {
	var system []string
	var chat []cursorMsg
	for _, m := range b.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if t, _ := cursorText(m.Content); t != "" {
				system = append(system, t)
			}
			continue
		}
		chat = append(chat, m)
	}
	cur := -1
	for i, m := range chat {
		if m.Role == "user" {
			cur = i
		}
	}
	var current cursorMsg
	hist := chat
	switch {
	case cur >= 0:
		current, hist = chat[cur], chat[:cur]
	case len(chat) > 0:
		current, hist = chat[len(chat)-1], chat[:len(chat)-1]
	}
	userText, _ := cursorText(current.Content)
	if userText == "" {
		userText = "Continue."
	}
	if len(system) > 0 {
		userText = strings.Join(system, "\n\n") + "\n\n" + userText
	}
	userMessage := pbCat(pbStr(1, userText), pbStr(2, uuid4()), pbLen(3, nil), pbUint(4, 1))
	userAction := pbLen(1, userMessage)
	var history []byte
	for _, m := range hist {
		if h := cursorHistory(m); h != nil {
			history = append(history, pbLen(1, h)...)
		}
	}
	if len(history) > 0 {
		userAction = append(userAction, pbLen(7, history)...)
	}
	conversationAction := pbLen(1, userAction)

	effort := strings.ToLower(b.ReasoningEffort)
	switch effort {
	case "ultra":
		effort = "max"
	case "minimal":
		effort = "low"
	}
	requested := pbCat(pbStr(1, model), pbUint(7, 1))
	if effort != "" && effort != "none" {
		key := "effort"
		if strings.Contains(strings.ToLower(model), "gpt-") {
			key = "reasoning"
		}
		requested = append(requested, pbLen(3, pbCat(pbStr(1, key), pbStr(2, effort)))...)
	}
	details := pbCat(pbStr(1, model), pbStr(3, model), pbStr(4, model))
	var tools []byte
	for _, t := range b.Tools {
		schema := t.Function.Parameters
		if schema == nil {
			schema = map[string]any{}
		}
		def := pbCat(pbStr(1, t.Function.Name), pbStr(2, t.Function.Description), pbLen(3, pbValue(schema)),
			pbStr(4, "fastrouter"), pbStr(5, t.Function.Name))
		tools = append(tools, pbLen(1, def)...)
	}
	run := pbCat(pbLen(1, nil), pbLen(2, conversationAction), pbLen(3, details))
	if len(tools) > 0 {
		run = append(run, pbLen(4, tools)...)
	}
	run = append(run, pbLen(9, requested)...)
	return connectFrame(pbLen(1, run))
}

// ---- Run stream ----

type cursorEvent struct {
	kind    string // text | tool | done | error
	text    string
	tool    [3]string // id, name, arguments
	finish  string
	status  int    // error: HTTP status to report
	suggest string // error: model Cursor suggests instead
}

func wrapExec(id uint64, execID string, field int, payload []byte) []byte {
	var parts []byte
	if id != 0 {
		parts = pbUint(1, id)
	}
	parts = append(parts, pbStr(15, execID)...)
	parts = append(parts, pbLen(field, payload)...)
	return connectFrame(pbLen(2, parts))
}

var execResultField = map[int]int{2: 2, 3: 3, 4: 4, 5: 5, 7: 7, 8: 8, 9: 9, 16: 16, 20: 20, 23: 23}

func visibleAfterThink(t string) string {
	i := strings.LastIndex(t, "</think>")
	if i < 0 {
		return ""
	}
	return strings.TrimLeft(t[i+len("</think>"):], " \t\r\n")
}

// cursorTurn reads one Run stream, answering the server's requests on w, and
// reports events until the turn ends.
func cursorTurn(body io.Reader, w io.Writer, model string, emit func(cursorEvent)) {
	composer := strings.HasPrefix(strings.ToLower(model[strings.LastIndex(model, "/")+1:]), "composer")
	var thinking string
	emitted, sawText := 0, false
	finished := false
	end := func() {
		if finished {
			return
		}
		finished = true
		if !sawText && thinking != "" {
			fb := strings.TrimSpace(thinking)
			if composer {
				fb = visibleAfterThink(thinking)
			}
			if fb != "" {
				sawText = true
				emit(cursorEvent{kind: "text", text: fb})
			}
		}
		if sawText {
			emit(cursorEvent{kind: "done", finish: "stop"})
		} else {
			emit(cursorEvent{kind: "error", text: "Cursor AgentService returned an empty turn"})
		}
	}
	hdr := make([]byte, 5)
	for !finished {
		if _, err := io.ReadFull(body, hdr); err != nil {
			break
		}
		flags, n := hdr[0], binary.BigEndian.Uint32(hdr[1:])
		if n > 64<<20 {
			break
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(body, payload); err != nil {
			break
		}
		if flags&1 != 0 {
			if zr, err := gzip.NewReader(bytes.NewReader(payload)); err == nil {
				if p, err := io.ReadAll(zr); err == nil {
					payload = p
				}
			}
		}
		if flags&2 != 0 { // end-of-stream trailer (JSON)
			var t struct {
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(payload, &t) == nil && t.Error != nil && !sawText {
				finished = true
				text := cursorTrailerError(payload, t.Error.Code, t.Error.Message)
				ev := cursorEvent{kind: "error", text: text, status: connectStatus(t.Error.Code, text)}
				if m := suggestRe.FindStringSubmatch(text); m != nil {
					ev.suggest = m[1]
				}
				emit(ev)
				return
			}
			break
		}
		msg := pbDecode(payload)
		if msg.has(1) { // interaction_update
			up := msg.msg(1)
			if up.has(1) {
				if d := up.msg(1).str(1); d != "" {
					sawText = true
					emit(cursorEvent{kind: "text", text: d})
				}
			}
			if up.has(4) {
				if d := up.msg(4).str(1); d != "" {
					thinking += d
					if composer {
						if v := visibleAfterThink(thinking); len(v) > emitted {
							sawText = true
							emit(cursorEvent{kind: "text", text: v[emitted:]})
							emitted = len(v)
						}
					}
				}
			}
			if up.has(14) {
				end()
			}
		}
		if msg.has(4) { // KV get/set: ack so the stream continues
			kv := msg.msg(4)
			id, _ := kv.get(1)
			meta, _ := kv.get(4)
			field, payload := 0, []byte(nil)
			if kv.has(2) {
				field, payload = 2, pbLen(1, nil)
			} else if kv.has(3) {
				field = 3
			}
			if field != 0 {
				var parts []byte
				if id.n != 0 {
					parts = pbUint(1, id.n)
				}
				parts = append(parts, pbLen(field, payload)...)
				if len(meta.b) > 0 {
					parts = append(parts, pbLen(4, meta.b)...)
				}
				_, _ = w.Write(connectFrame(pbLen(3, parts)))
			}
		}
		if msg.has(2) { // exec request
			ex := msg.msg(2)
			id, _ := ex.get(1)
			execID := ex.str(15)
			switch {
			case ex.has(10): // request_context
				_, _ = w.Write(wrapExec(id.n, execID, 10, pbLen(1, pbLen(1, nil))))
			case ex.has(11): // MCP tool call
				args := ex.msg(11)
				m := map[string]any{}
				for _, e := range args.all(2) {
					pair := pbDecode(e.b)
					if k := pair.str(1); k != "" {
						vf, _ := pair.get(2)
						m[k] = pbValueDecode(vf.b)
					}
				}
				name := firstNonEmpty(args.str(5), args.str(1))
				if name == "" {
					finished = true
					emit(cursorEvent{kind: "error", text: "Cursor AgentService requested an unsupported IDE tool"})
					return
				}
				a, _ := json.Marshal(m)
				finished = true
				emit(cursorEvent{kind: "tool", tool: [3]string{firstNonEmpty(args.str(3), "call_"+strings.ReplaceAll(uuid4(), "-", "")[:24]), name, string(a)}})
				emit(cursorEvent{kind: "done", finish: "tool_calls"})
				return
			default: // IDE builtins (shell, read, ...): reject so the model continues
				variant := 0
				for _, f := range ex {
					if f.num != 1 && f.num != 15 {
						variant = f.num
						break
					}
				}
				rf := execResultField[variant]
				switch {
				case variant == 9:
					_, _ = w.Write(wrapExec(id.n, execID, 9, nil))
				case rf != 0:
					_, _ = w.Write(wrapExec(id.n, execID, rf, pbLen(2, pbStr(2, "Tool not available in this environment. Use the MCP tools provided instead."))))
				default:
					finished = true
					emit(cursorEvent{kind: "error", text: "Cursor AgentService requested an unsupported IDE tool"})
					return
				}
			}
		}
	}
	end()
}

// cursorTrailerError describes a Connect end-of-stream error. Cursor puts the
// readable reason in a base64 ErrorDetails proto; its strings are pulled out.
func cursorTrailerError(raw []byte, code, msg string) string {
	var t struct {
		Error struct {
			Details []struct {
				Type  string          `json:"type"`
				Value string          `json:"value"`
				Debug json.RawMessage `json:"debug"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &t)
	parts := []string{}
	for _, d := range t.Error.Details {
		if len(d.Debug) > 0 && string(d.Debug) != "null" {
			parts = append(parts, string(d.Debug))
			continue
		}
		if b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(d.Value, "=")); err == nil {
			parts = append(parts, pbStrings(b)...)
		}
	}
	out := "Cursor " + firstNonEmpty(code, "error")
	if msg != "" && !strings.EqualFold(msg, "error") {
		out += ": " + msg
	}
	if len(parts) > 0 {
		out += " (" + strings.Join(parts, "; ") + ")"
	}
	if len(out) > 600 {
		out = out[:600]
	}
	return out
}

// pbStrings collects the printable strings in a protobuf message, depth first.
func pbStrings(b []byte) []string {
	var out []string
	for _, f := range pbDecode(b) {
		if f.wire != 2 || len(f.b) == 0 {
			continue
		}
		if inner := pbDecode(f.b); len(inner) > 0 && pbConsumes(f.b) {
			out = append(out, pbStrings(f.b)...)
			continue
		}
		s := string(f.b)
		if utf8ok(s) {
			out = append(out, s)
		}
	}
	return out
}

func pbConsumes(b []byte) bool {
	n := 0
	for _, f := range pbDecode(b) {
		n += len(pbTag(f.num, f.wire))
		switch f.wire {
		case 0:
			n += len(pbVarint(f.n))
		case 2:
			n += len(pbVarint(uint64(len(f.b)))) + len(f.b)
		default:
			n += len(f.b)
		}
	}
	return n == len(b)
}

func utf8ok(s string) bool {
	for _, r := range s {
		if r == 0xFFFD || (r < 0x20 && r != '\n' && r != '\t') {
			return false
		}
	}
	return true
}

// cursorStart opens a Run stream for model and waits for its first event.
func cursorStart(ctx context.Context, hdr http.Header, in *cursorBody, model string) (<-chan cursorEvent, cursorEvent, *http.Response, error) {
	pr, pw := io.Pipe()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, CursorRunURL, pr)
	if err != nil {
		return nil, cursorEvent{}, nil, err
	}
	r.Header = hdr.Clone()
	var wmu sync.Mutex
	write := func(b []byte) (int, error) { wmu.Lock(); defer wmu.Unlock(); return pw.Write(b) }
	go func() { _, _ = write(cursorRunFrame(in, model)) }()
	resp, err := cursorClient.Do(r)
	if err != nil {
		pw.Close()
		return nil, cursorEvent{}, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		resp.Body.Close()
		pw.Close()
		return nil, cursorEvent{}, cursorErr(resp.StatusCode, fmt.Sprintf("Cursor AgentService %d: %s", resp.StatusCode, firstNonEmpty(strings.TrimSpace(string(eb)), "request failed"))), nil
	}
	events := make(chan cursorEvent, 64)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		defer pw.Close()
		cursorTurn(resp.Body, writerFunc(write), model, func(e cursorEvent) {
			select {
			case events <- e:
			case <-ctx.Done():
			}
		})
	}()
	drain := func() {
		go func() {
			for range events {
			}
		}()
	}
	select {
	case e, ok := <-events:
		if !ok {
			return nil, cursorEvent{}, cursorErr(http.StatusBadGateway, "Cursor AgentService closed the stream"), nil
		}
		if e.kind == "error" {
			drain()
		}
		return events, e, nil, nil
	case <-ctx.Done():
		drain()
		return nil, cursorEvent{}, nil, context.Cause(ctx)
	}
}

func firstStatus(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// connectStatus maps a Connect error code to the HTTP status FastRouter reports.
func connectStatus(code, text string) int {
	if strings.Contains(text, "unavailable on") || strings.Contains(text, "Switch to an included model") {
		return http.StatusBadRequest // the plan does not include this model
	}
	switch code {
	case "resource_exhausted":
		return http.StatusTooManyRequests
	case "unauthenticated":
		return http.StatusUnauthorized
	case "permission_denied":
		return http.StatusForbidden
	case "not_found":
		return http.StatusNotFound
	case "invalid_argument", "failed_precondition", "out_of_range":
		return http.StatusBadRequest
	case "unavailable":
		return http.StatusServiceUnavailable
	case "deadline_exceeded":
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

var suggestRe = regexp.MustCompile(`"suggestedModel"\s*:\s*"([^"]+)"`)

// cursorDo runs one Cursor turn and answers with an OpenAI SSE stream. It
// waits for the first event so an empty or failed turn becomes an HTTP error
// the proxy can fail over on.
func cursorDo(req *http.Request, body []byte, a Account) (*http.Response, error) {
	var in cursorBody
	if err := json.Unmarshal(body, &in); err != nil {
		return cursorErr(http.StatusBadRequest, "could not read the request: "+err.Error()), nil
	}
	for _, m := range in.Messages {
		if _, ok := cursorText(m.Content); !ok {
			return cursorErr(http.StatusBadRequest, "Cursor: image and file parts are not supported by FastRouter's Cursor path yet; send text only"), nil
		}
	}
	model := in.Model
	ctx := req.Context()
	key := strings.TrimPrefix(req.Header.Get("authorization"), "Bearer ")
	if model == "" || model == "default" || model == "auto" {
		if ms, err := CursorModels(ctx, key, a.Extra["machineId"], false); err == nil {
			if pick := cursorAutoModel(ms); pick != "" {
				model = pick
			}
		}
	}
	auto := in.Model == "" || in.Model == "default" || in.Model == "auto"
	events, first, errResp, err := cursorStart(ctx, req.Header, &in, model)
	if err != nil {
		return nil, err
	}
	if first.kind == "error" && auto && first.suggest != "" && first.suggest != model {
		// "Auto" picked a model the plan does not include: take Cursor's suggestion.
		events, first, errResp, err = cursorStart(ctx, req.Header, &in, first.suggest)
		if err != nil {
			return nil, err
		}
	}
	if errResp != nil {
		return errResp, nil
	}
	if first.kind == "error" {
		return cursorErr(firstStatus(first.status, http.StatusBadGateway), first.text), nil
	}
	out, ow := io.Pipe()
	go func() {
		defer ow.Close()
		id := "chatcmpl-cursor-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		created := time.Now().Unix()
		chunk := func(delta map[string]any, finish any, usage map[string]int) {
			c := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": in.Model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
			if usage != nil {
				c["choices"] = []any{}
				c["usage"] = usage
			}
			b, _ := json.Marshal(c)
			_, _ = ow.Write(append(append([]byte("data: "), b...), '\n', '\n'))
		}
		chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
		outChars := 0
		handle := func(e cursorEvent) bool {
			switch e.kind {
			case "text":
				outChars += len(e.text)
				chunk(map[string]any{"content": e.text}, nil, nil)
			case "tool":
				outChars += len(e.tool[2])
				chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": e.tool[0], "type": "function",
					"function": map[string]any{"name": e.tool[1], "arguments": e.tool[2]}}}}, nil, nil)
			case "error":
				b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": e.text, "type": "api_error"}})
				_, _ = ow.Write(append(append([]byte("data: "), b...), '\n', '\n'))
				return false
			case "done":
				chunk(map[string]any{}, firstNonEmpty(e.finish, "stop"), nil)
				in := len(body) / 4
				chunk(nil, nil, map[string]int{"prompt_tokens": in, "completion_tokens": outChars/4 + 1, "total_tokens": in + outChars/4 + 1})
				return false
			}
			return true
		}
		if handle(first) {
			for e := range events {
				if !handle(e) {
					break
				}
			}
		}
		_, _ = ow.Write([]byte("data: [DONE]\n\n"))
		go func() {
			for range events {
			}
		}()
	}()
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: h, Body: out, Request: req,
		Proto: "HTTP/2.0", ProtoMajor: 2}, nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func cursorErr(status int, msg string) *http.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "api_error"}})
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Status: strconv.Itoa(status) + " " + http.StatusText(status), Header: h,
		Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b))}
}

// ---- models (services/cursorModels.js) ----

// CursorModel is one entry of the account's live model picker.
type CursorModel struct {
	ID, Name                                       string
	DefaultOn, ChatOnly, SupportsAgent, Hidden, ok bool
}

var cursorModelCache sync.Map // sha256(token+machine) -> cursorModelsEntry

type cursorModelsEntry struct {
	at     time.Time
	models []CursorModel
}

// CursorModels lists the models the account can use (GetUsableModels).
func CursorModels(ctx context.Context, token, machineID string, fresh bool) ([]CursorModel, error) {
	ck := sha256Hex("cursor:" + machineID + ":" + token)
	if v, ok := cursorModelCache.Load(ck); ok && !fresh && time.Since(v.(cursorModelsEntry).at) < 5*time.Minute {
		return v.(cursorModelsEntry).models, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, CursorAgentBase+cursorModelsPath, bytes.NewReader(nil))
	if err != nil {
		return nil, err
	}
	for k, v := range cursorHeaders(token, machineID) {
		r.Header.Set(k, v)
	}
	r.Header.Del("connect-accept-encoding")
	r.Header.Del("connect-protocol-version")
	r.Header.Set("accept", "application/proto")
	r.Header.Set("content-type", "application/proto")
	resp, err := cursorClient.Do(r)
	if err != nil {
		return nil, errors.New("could not reach Cursor")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{Status: resp.StatusCode, Code: errCode(b)}
	}
	msg := pbDecode(b)
	flags := map[string]CursorModel{}
	for _, f := range msg.all(2) {
		m := pbDecode(f.b)
		boolOf := func(n int) bool { x, _ := m.get(n); return x.n == 1 }
		id := strings.TrimSpace(m.str(1))
		flags[id] = CursorModel{DefaultOn: boolOf(2), ChatOnly: boolOf(4), SupportsAgent: boolOf(5), Hidden: boolOf(35), ok: true}
	}
	var out []CursorModel
	seen := map[string]bool{}
	for _, f := range msg.all(1) {
		d := pbDecode(f.b)
		id := strings.TrimSpace(d.str(1))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		cm := flags[id]
		cm.ID, cm.Name = id, strings.TrimSpace(firstNonEmpty(d.str(4), d.str(5), d.str(3), id))
		out = append(out, cm)
	}
	cursorModelCache.Store(ck, cursorModelsEntry{at: time.Now(), models: out})
	return out, nil
}

// cursorAutoModel resolves "default"/"auto" to the account's default agent model.
func cursorAutoModel(ms []CursorModel) string {
	var first string
	for _, m := range ms {
		if !m.ok || !m.SupportsAgent || m.ChatOnly || m.Hidden {
			continue
		}
		if m.DefaultOn {
			return m.ID
		}
		if first == "" {
			first = m.ID
		}
	}
	if first == "" && len(ms) > 0 && !ms[0].ok { // no picker flags sent: the list is the plan's usable models
		first = ms[0].ID
	}
	return first
}
