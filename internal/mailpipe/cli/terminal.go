package cli

import (
	"io"
	"strconv"
	"sync"
	"unicode/utf8"
)

// terminalWriter makes control characters visible without allowing the terminal
// to interpret them. Keep layout and Unicode, including split UTF-8 writes.
// JSON encoders already escape C0 controls; escaping Unicode C1 controls with
// JSON-compatible escapes also preserves their decoded values.
type terminalWriter struct {
	mu      sync.Mutex
	out     io.Writer
	pending []byte
}

func (w *terminalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data := append(w.pending, p...)
	w.pending = nil
	var out []byte
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			w.pending = append(w.pending, data...)
			break
		}
		r, n := utf8.DecodeRune(data)
		if r == utf8.RuneError && n == 1 {
			out = append(out, '\\', 'x', "0123456789abcdef"[data[0]>>4], "0123456789abcdef"[data[0]&15])
		} else if (r < 32 && r != '\n' && r != '\t') || (r >= 127 && r <= 159) || r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			// Always use Unicode escapes for non-ASCII controls so JSON remains
			// valid and round trips to exactly the original string.
			if r >= 127 {
				out = append(out, '\\', 'u')
				for shift := 12; shift >= 0; shift -= 4 {
					out = append(out, "0123456789abcdef"[(r>>shift)&15])
				}
			} else {
				quoted := strconv.QuoteRune(r)
				out = append(out, quoted[1:len(quoted)-1]...)
			}
		} else {
			out = append(out, data[:n]...)
		}
		data = data[n:]
	}
	if len(out) > 0 {
		n, err := w.out.Write(out)
		if err != nil {
			return 0, err
		}
		if n != len(out) {
			return 0, io.ErrShortWrite
		}
	}
	return len(p), nil
}

func (w *terminalWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	// A final truncated UTF-8 sequence is data, never a terminal control.
	for _, b := range w.pending {
		_, _ = io.WriteString(w.out, "\\x"+string([]byte{"0123456789abcdef"[b>>4], "0123456789abcdef"[b&15]}))
	}
	w.pending = nil
}
