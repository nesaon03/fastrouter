package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// Frame is one server-sent event.
type Frame struct {
	Event string // "event:" field, if any
	Data  []byte // joined "data:" lines
	Raw   []byte // the frame's bytes exactly as received, including the blank line
}

// FrameReader splits an SSE stream into frames without copying data it does not need to.
type FrameReader struct {
	br   *bufio.Reader
	raw  []byte
	data []byte
	ev   []byte
}

// NewFrameReader wraps r.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{br: bufio.NewReaderSize(r, 32<<10)}
}

// Reset reuses the reader (and its buffers) for a new stream.
func (fr *FrameReader) Reset(r io.Reader) {
	fr.br.Reset(r)
	fr.raw, fr.data, fr.ev = fr.raw[:0], fr.data[:0], fr.ev[:0]
}

// Buffered reports bytes already read from the network but not yet returned.
func (fr *FrameReader) Buffered() int { return fr.br.Buffered() }

// Next returns the next frame. Slices are valid until the next call.
// Comment-only frames (": ping") are returned with empty Event and Data.
func (fr *FrameReader) Next() (Frame, error) {
	fr.raw, fr.data, fr.ev = fr.raw[:0], fr.data[:0], fr.ev[:0]
	sawData := false
	for {
		line, err := fr.br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// very long line: accumulate
			buf := append([]byte(nil), line...)
			for err == bufio.ErrBufferFull {
				line, err = fr.br.ReadSlice('\n')
				buf = append(buf, line...)
			}
			line = buf
		}
		fr.raw = append(fr.raw, line...)
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			if len(fr.raw) > len(line) { // end of a non-empty frame
				return Frame{Event: string(fr.ev), Data: fr.data, Raw: fr.raw}, nil
			}
			if err != nil {
				return Frame{}, err
			}
			fr.raw = fr.raw[:0] // stray blank line
			continue
		}
		switch {
		case bytes.HasPrefix(trimmed, []byte("data:")):
			v := bytes.TrimPrefix(trimmed[5:], []byte(" "))
			if sawData {
				fr.data = append(fr.data, '\n')
			}
			fr.data = append(fr.data, v...)
			sawData = true
		case bytes.HasPrefix(trimmed, []byte("event:")):
			fr.ev = append(fr.ev[:0], bytes.TrimSpace(trimmed[6:])...)
		}
		if err != nil {
			if len(fr.raw) > 0 && (sawData || len(fr.ev) > 0) {
				fr.raw = append(fr.raw, '\n', '\n')
				return Frame{Event: string(fr.ev), Data: fr.data, Raw: fr.raw}, nil
			}
			return Frame{}, err
		}
	}
}

// sseBuf accumulates encoded SSE output.
type sseBuf struct{ b []byte }

func (s *sseBuf) data(v any) {
	s.b = append(s.b, "data: "...)
	s.b = appendJSON(s.b, v)
	s.b = append(s.b, '\n', '\n')
}

func (s *sseBuf) event(name string, v any) {
	s.b = append(s.b, "event: "...)
	s.b = append(s.b, name...)
	s.b = append(s.b, "\ndata: "...)
	s.b = appendJSON(s.b, v)
	s.b = append(s.b, '\n', '\n')
}

func (s *sseBuf) raw(str string) { s.b = append(s.b, str...) }

func (s *sseBuf) take() []byte {
	out := s.b
	s.b = nil
	return out
}

func appendJSON(b []byte, v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		return append(b, "{}"...)
	}
	return append(b, out...)
}

// appendJSONString appends s as a JSON string, escaped exactly like encoding/json.
func appendJSONString(b []byte, s string) []byte {
	out, err := json.Marshal(s)
	if err != nil {
		return append(b, `""`...)
	}
	return append(b, out...)
}
