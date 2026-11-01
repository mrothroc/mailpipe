package document

import (
	"bufio"
	"encoding/base64"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"unicode/utf8"
)

func decodeHeader(v string) (string, error) {
	dec := mime.WordDecoder{CharsetReader: func(label string, r io.Reader) (io.Reader, error) { return textReader(r, label) }}
	out, e := dec.DecodeHeader(v)
	if e == nil && !utf8.ValidString(out) {
		e = incomplete("invalid UTF8 header", nil)
	}
	return out, e
}
func textReader(r io.Reader, label string) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "", "utf-8", "utf8":
		return transform.NewReader(r, encoding.UTF8Validator), nil
	case "us-ascii", "ascii":
		return asciiReader{r}, nil
	}
	r, e := charset.NewReaderLabel(label, r)
	if e != nil {
		return nil, incomplete("unsupported charset", nil)
	}
	return &replacementGuard{r: r}, nil
}

// Legacy decoders can substitute U+FFFD for malformed input. Refuse that
// ambiguous conversion rather than classify silently repaired text.
type replacementGuard struct {
	r    io.Reader
	tail [2]byte
	have int
}

func (r *replacementGuard) Read(b []byte) (int, error) {
	n, e := r.r.Read(b)
	for _, v := range b[:n] {
		if r.have == 2 && r.tail[0] == 0xef && r.tail[1] == 0xbf && v == 0xbd {
			return 0, incomplete("ambiguous charset replacement", nil)
		}
		r.tail[0] = r.tail[1]
		r.tail[1] = v
		if r.have < 2 {
			r.have++
		}
	}
	return n, e
}

type asciiReader struct{ io.Reader }

func (r asciiReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	for _, b := range p[:n] {
		if b > 127 {
			return 0, incomplete("invalid ASCII", nil)
		}
	}
	return n, e
}
func transfer(h textproto.MIMEHeader, r io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding"))) {
	case "", "7bit", "8bit", "binary":
		return r, nil
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r), nil
	case "quoted-printable":
		return quotedprintable.NewReader(&strictQP{r: bufio.NewReaderSize(r, 4096)}), nil
	default:
		return nil, incomplete("unsupported transfer encoding", nil)
	}
}

type strictQP struct {
	r       *bufio.Reader
	pending []byte
}

func (r *strictQP) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(r.pending) > 0 {
			p[n] = r.pending[0]
			r.pending = r.pending[1:]
			n++
			continue
		}
		b, e := r.r.ReadByte()
		if e != nil {
			return n, e
		}
		if b == '=' {
			next, e := r.r.Peek(1)
			if e == nil && next[0] == '\n' {
				r.pending = []byte{'=', '\n'}
				_, _ = r.r.Discard(1)
				continue
			}
			next, e = r.r.Peek(2)
			if e != nil {
				return n, incomplete("incomplete quoted printable", e)
			}
			hex := func(b byte) bool { return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' }
			if next[0] == '\n' {
				r.pending = []byte{'=', '\n'}
				_, _ = r.r.Discard(1)
			} else if next[0] == '\r' && next[1] == '\n' || hex(next[0]) && hex(next[1]) {
				r.pending = []byte{'=', next[0], next[1]}
				_, _ = r.r.Discard(2)
			} else {
				return n, incomplete("invalid quoted printable", nil)
			}
			continue
		}
		p[n] = b
		n++
	}
	return n, nil
}
