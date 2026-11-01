package document

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/textproto"
)

func headerRead(r io.Reader, l Limits) (textproto.MIMEHeader, io.Reader, int64, error) {
	br := bufio.NewReaderSize(r, 4096)
	var raw []byte
	fields, lineBytes := 0, 0
	for {
		line, e := br.ReadSlice('\n')
		if lineBytes == 0 && len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '\r' && line[0] != '\n' {
			fields++
			if fields > l.HeaderFields {
				return nil, nil, 0, incomplete("header field limit exceeded", nil)
			}
		}
		lineBytes += len(line)
		if int64(len(line)) > l.HeaderBytes-int64(len(raw)) {
			return nil, nil, 0, incomplete("header byte limit exceeded", nil)
		}
		raw = append(raw, line...)
		if errors.Is(e, bufio.ErrBufferFull) {
			continue
		}
		if e != nil {
			return nil, nil, 0, incomplete("incomplete header", e)
		}
		if lineBytes <= 2 && (bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n"))) {
			break
		}
		lineBytes = 0
	}
	h, e := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	if e != nil {
		e = incomplete("malformed header", e)
	}
	return h, br, int64(len(raw)), e
}

// headerGate prevents the stdlib multipart header reader from reaching its much
// larger internal allocation limit. Existing 4KiB read-ahead remains bounded.
type headerGate struct {
	r                 io.Reader
	remaining         int64
	on                bool
	fields, maxFields int
	boundary          string
	limit, total      int64
	charge            *int64
	inHeader          bool
	headerBytes       int64
	prefix            [80]byte
	stored, lineBytes int
	tailOK            bool
}

func (g *headerGate) Read(b []byte) (int, error) {
	if g.on {
		if g.remaining <= 0 {
			return 0, incomplete("multipart header limit exceeded", nil)
		}
		if int64(len(b)) > g.remaining {
			b = b[:g.remaining]
		}
	}
	n, e := g.r.Read(b)
	for _, v := range b[:n] {
		wasHeader := g.inHeader
		if wasHeader {
			if g.lineBytes == 0 && v != ' ' && v != '\t' && v != '\r' && v != '\n' {
				g.fields++
				if g.fields > g.maxFields {
					return 0, incomplete("wire header field limit exceeded", nil)
				}
			}
			g.headerBytes++
			*g.charge++
			if g.headerBytes > g.limit || *g.charge > g.total {
				return 0, incomplete("wire header byte limit exceeded", nil)
			}
		}
		g.lineBytes++
		if g.stored < len(g.prefix) {
			g.prefix[g.stored] = v
			g.stored++
		} else if v != ' ' && v != '\t' && v != '\r' && v != '\n' {
			g.tailOK = false
		}
		if v == '\n' {
			line := bytes.TrimRight(g.prefix[:g.stored], " \t\r\n")
			if wasHeader {
				if g.lineBytes <= 2 && (bytes.Equal(g.prefix[:g.stored], []byte("\r\n")) || bytes.Equal(g.prefix[:g.stored], []byte("\n"))) {
					g.inHeader = false
				}
			} else if g.tailOK && len(line) == len(g.boundary)+2 && bytes.HasPrefix(line, []byte("--")) && bytes.Equal(line[2:], []byte(g.boundary)) {
				g.inHeader = true
				g.headerBytes = 0
				g.fields = 0
			}
			g.lineBytes = 0
			g.stored = 0
			g.tailOK = true
		}
	}
	if g.on {
		g.remaining -= int64(n)
	}
	return n, e
}
func (p *parser) header(h textproto.MIMEHeader) error {
	var n int64
	fields := 0
	for k, vs := range h {
		for _, v := range vs {
			fields++
			n += int64(len(k) + len(v) + 4)
		}
	}
	if fields > p.l.HeaderFields || n > p.l.HeaderBytes || p.headers > p.l.TotalHeaderBytes {
		return incomplete("header aggregate limit exceeded", nil)
	}
	return nil
}
