package main

import (
	"bytes"
	"fmt"
)

// maxPendingLine bounds how much unterminated upstream data is buffered while
// waiting for a newline.
const maxPendingLine = 32 << 20

// streamFramer converts raw upstream SSE bytes into the chunks the host
// expects from an executor, which depend on the protocol:
//
//   - Anthropic Messages: the host writes chunks verbatim, so they are passed
//     through, cut on line boundaries.
//   - OpenAI chat completions: the host wraps every chunk as "data: <chunk>"
//     and appends the final [DONE] itself, so each chunk must be one bare JSON
//     payload with the SSE framing and the upstream [DONE] removed.
//
// Network reads split lines anywhere; partial lines are carried over.
type streamFramer struct {
	anthropic bool
	buf       []byte
}

func newStreamFramer(anthropic bool) *streamFramer { return &streamFramer{anthropic: anthropic} }

// feed adds upstream bytes and returns the chunks that are now complete.
func (f *streamFramer) feed(p []byte) ([][]byte, error) {
	f.buf = append(f.buf, p...)
	if len(f.buf) > maxPendingLine && bytes.IndexByte(f.buf, '\n') < 0 {
		return nil, fmt.Errorf("upstream stream line exceeds %d bytes", maxPendingLine)
	}
	end := bytes.LastIndexByte(f.buf, '\n')
	if end < 0 {
		return nil, nil
	}
	complete := f.buf[:end+1]
	rest := append([]byte(nil), f.buf[end+1:]...)
	out := f.frame(complete)
	f.buf = rest
	return out, nil
}

// flush returns whatever remains once the upstream has ended.
func (f *streamFramer) flush() [][]byte {
	if len(f.buf) == 0 {
		return nil
	}
	tail := append(f.buf, '\n')
	f.buf = nil
	return f.frame(tail)
}

func (f *streamFramer) frame(complete []byte) [][]byte {
	if f.anthropic {
		return [][]byte{append([]byte(nil), complete...)}
	}
	var out [][]byte
	for _, line := range bytes.Split(complete, []byte("\n")) {
		line = bytes.TrimSpace(line)
		switch {
		case len(line) == 0, line[0] == ':':
			// blank separator or SSE comment / keep-alive
		case bytes.HasPrefix(line, []byte("data:")):
			payload := bytes.TrimSpace(line[len("data:"):])
			if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
				out = append(out, append([]byte(nil), payload...))
			}
		case line[0] == '{':
			// Some gateways send bare JSON lines instead of SSE.
			out = append(out, append([]byte(nil), line...))
		}
	}
	return out
}
