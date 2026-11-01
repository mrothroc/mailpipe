package document

import (
	"bufio"
	"context"
	"errors"
	"golang.org/x/net/html/charset"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"text/template"
)

type parser struct {
	ctx              context.Context
	d                *FileDoc
	l                Limits
	decoded, headers int64
	nodes            int
	inlineIDs        []string
}
type countedDecoded struct {
	p    *parser
	r    io.Reader
	part int64
}

func (r *countedDecoded) Read(b []byte) (int, error) {
	if e := r.p.ctx.Err(); e != nil {
		return 0, e
	}
	n, e := r.r.Read(b)
	if int64(n) > r.p.l.PartBytes-r.part || int64(n) > r.p.l.DecodedBytes-r.p.decoded {
		return 0, incomplete("decoded byte limit exceeded", nil)
	}
	r.part += int64(n)
	r.p.decoded += int64(n)
	return n, e
}
func (p *parser) spool(r io.Reader, max int64) (*Blob, error) {
	b, e := p.d.scope.NewBlob(p.ctx, max, 0)
	if e != nil {
		return nil, e
	}
	p.d.files = append(p.d.files, b)
	e = copyChunks(p.ctx, b, r)
	if e != nil {
		return nil, incomplete("part decoding failed", e)
	}
	if e = b.Seal(); e != nil {
		return nil, e
	}
	return b, nil
}
func boundedParameters(value string, max int) error {
	quoted, escaped := false, false
	count := 0
	for _, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if quoted && r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
		}
		if r == ';' && !quoted {
			count++
			if count > max {
				return incomplete("MIME parameter count limit exceeded", nil)
			}
		}
	}
	return nil
}
func (p *parser) walk(h textproto.MIMEHeader, r io.Reader, depth int) (b bodies, err error) {
	if depth > p.l.Depth || p.nodes >= p.l.Parts {
		return b, incomplete("MIME depth or part limit exceeded", nil)
	}
	p.nodes++
	if err = p.header(h); err != nil {
		return b, err
	}
	if e := boundedParameters(h.Get("Content-Type"), p.l.HeaderFields); e != nil {
		return b, e
	}
	if e := boundedParameters(h.Get("Content-Disposition"), p.l.HeaderFields); e != nil {
		return b, e
	}
	ct, params, e := mime.ParseMediaType(h.Get("Content-Type"))
	if e != nil {
		if h.Get("Content-Type") != "" {
			return b, incomplete("invalid content type", e)
		}
		ct = "text/plain"
		params = map[string]string{}
	}
	disp, dp, e := mime.ParseMediaType(h.Get("Content-Disposition"))
	if e != nil && h.Get("Content-Disposition") != "" {
		return b, incomplete("invalid disposition", e)
	}
	name := dp["filename"]
	if name == "" {
		name = params["name"]
	}
	if name != "" {
		name, e = decodeHeader(name)
		if e != nil {
			return b, incomplete("invalid filename encoding", e)
		}
	}
	attached := disp == "attachment" || name != ""
	if strings.HasPrefix(ct, "multipart/") {
		boundary := params["boundary"]
		if len(boundary) == 0 || len(boundary) > 70 {
			return b, incomplete("invalid multipart boundary", nil)
		}
		enc := strings.ToLower(h.Get("Content-Transfer-Encoding"))
		if enc != "" && enc != "7bit" && enc != "8bit" && enc != "binary" {
			return b, incomplete("encoded multipart unsupported", nil)
		}
		var attachedBlob *Blob
		attachmentsBefore := len(p.d.Attachments)
		inlineBefore := len(p.inlineIDs)
		if attached {
			attachedBlob, e = p.spool(&countedDecoded{p: p, r: r}, p.l.PartBytes)
			if e != nil {
				return b, e
			}
			reader, e := attachedBlob.Open()
			if e != nil {
				return b, e
			}
			defer func() { err = errors.Join(err, reader.Close()) }()
			r = reader
		}
		gate := &headerGate{r: r, boundary: boundary, limit: p.l.HeaderBytes, total: p.l.TotalHeaderBytes, charge: &p.headers, tailOK: true, maxFields: p.l.HeaderFields}
		mr := multipart.NewReader(gate, boundary)
		start := strings.Trim(params["start"], "<>")
		index := 0
		foundRoot := false
		for {
			gate.on = true
			gate.remaining = p.l.HeaderBytes + 8192
			part, e := mr.NextRawPart()
			gate.on = false
			if e == io.EOF {
				break
			}
			if e != nil {
				return b, incomplete("incomplete multipart", e)
			}
			child, e := p.walk(part.Header, part, depth+1)
			if e != nil {
				return b, e
			}
			if e = copyChunks(p.ctx, io.Discard, part); e != nil {
				return b, incomplete("incomplete part", e)
			}
			switch ct {
			case "multipart/alternative":
				previous := b
				if len(child.read) > 0 && (len(b.read) == 0 || !child.read[0].html) {
					b.read = child.read
				}
				if len(child.render) > 0 && (len(b.render) == 0 || child.render[0].html) {
					b.render = child.render
				}
				if e = p.discardBodies(previous, b); e != nil {
					return b, e
				}
				if e = p.discardBodies(child, b); e != nil {
					return b, e
				}
			case "multipart/related":
				match := start == "" && index == 0 || start != "" && strings.Trim(part.Header.Get("Content-ID"), "<>") == start
				if match {
					if foundRoot {
						return b, incomplete("ambiguous related root", nil)
					}
					foundRoot = true
					b = child
				} else if e = p.discardBodies(child, b); e != nil {
					return b, e
				}
			default:
				if e = p.joinBodies(&b, child); e != nil {
					return b, e
				}
				if e = p.discardBodies(child, b); e != nil {
					return b, e
				}
			}
			index++
		}
		if ct == "multipart/related" && start != "" && !foundRoot {
			return b, incomplete("missing related root", nil)
		}
		if ct != "multipart/related" && ct != "multipart/alternative" {
			for _, frags := range [][]fragment{b.read, b.render} {
				for _, f := range frags {
					if e = f.data.Seal(); e != nil {
						return b, e
					}
				}
			}
		}
		if attached {
			if e = p.discardBodies(b, bodies{}); e != nil {
				return b, e
			}
			for _, id := range p.inlineIDs[inlineBefore:] {
				delete(p.d.Inline, id)
			}
			p.inlineIDs = p.inlineIDs[:inlineBefore]
			p.d.Attachments = p.d.Attachments[:attachmentsBefore]
			p.d.Attachments = append(p.d.Attachments, FilePart{name, ct, "", attachedBlob})
			return bodies{}, nil
		}
		return b, nil
	}
	decoded, e := transfer(h, r)
	if e != nil {
		return b, e
	}
	raw, e := p.spool(&countedDecoded{p: p, r: decoded}, p.l.PartBytes)
	if e != nil {
		return b, e
	}
	if ct == "message/rfc822" {
		attachmentsBefore := len(p.d.Attachments)
		inlineBefore := len(p.inlineIDs)
		rd, e := raw.Open()
		if e != nil {
			return b, e
		}
		nh, nr, headerBytes, e := headerRead(rd, p.l)
		p.headers += headerBytes
		if p.headers > p.l.TotalHeaderBytes {
			e = incomplete("total header limit exceeded", nil)
		}
		var prefix strings.Builder
		if e == nil && !attached {
			for _, key := range []string{"From", "To", "Date", "Subject"} {
				value, decodeErr := decodeHeader(nh.Get(key))
				if decodeErr != nil {
					e = incomplete("nested header decoding failed", decodeErr)
					break
				}
				if value != "" {
					prefix.WriteString(key + ": " + value + "\n")
				}
			}
		}
		if e == nil {
			b, e = p.walk(nh, nr, depth+1)
		}
		e = errors.Join(e, rd.Close())
		if e != nil {
			return b, e
		}
		if attached {
			for _, id := range p.inlineIDs[inlineBefore:] {
				delete(p.d.Inline, id)
			}
			p.inlineIDs = p.inlineIDs[:inlineBefore]
			p.d.Attachments = p.d.Attachments[:attachmentsBefore]
			p.d.Attachments = append(p.d.Attachments, FilePart{name, ct, "", raw})
			if e = p.discardBodies(b, bodies{}); e != nil {
				return b, e
			}
			return bodies{}, nil
		}
		if e = raw.CloseContext(p.ctx); e != nil {
			return b, e
		}
		if prefix.Len() > 0 {
			header, e := p.spool(strings.NewReader(prefix.String()), p.l.TextBytes)
			if e != nil {
				return b, e
			}
			f := fragment{header, false}
			b.read = append([]fragment{f}, b.read...)
			b.render = append([]fragment{f}, b.render...)
		}
		return b, nil
	}
	cid := strings.Trim(strings.TrimSpace(h.Get("Content-ID")), "<>")
	if cid != "" && strings.HasPrefix(ct, "image/") && disp != "attachment" {
		if _, exists := p.d.Inline[cid]; exists {
			return b, incomplete("ambiguous inline content ID", nil)
		}
		p.inlineIDs = append(p.inlineIDs, cid)
		p.d.Inline[cid] = FilePart{name, ct, cid, raw}
		return b, nil
	}
	if attached {
		p.d.Attachments = append(p.d.Attachments, FilePart{name, ct, cid, raw})
		return b, nil
	}
	if ct != "text/plain" && ct != "text/html" {
		return b, raw.CloseContext(p.ctx)
	}
	rd, e := raw.Open()
	if e != nil {
		return b, e
	}
	label := params["charset"]
	if label == "" && ct == "text/html" {
		br := bufio.NewReaderSize(rd, 4096)
		sample, _ := br.Peek(1024)
		_, found, certain := charset.DetermineEncoding(sample, "")
		if certain {
			label = found
		}
		decoded = br
	} else {
		decoded = rd
	}
	converted, e := textReader(decoded, label)
	var txt *Blob
	if e == nil {
		txt, e = p.spool(&countedDecoded{p: p, r: converted}, p.l.TextBytes)
	}
	e = errors.Join(e, rd.Close())
	if e != nil {
		return b, incomplete("text decoding failed", e)
	}
	if e = raw.CloseContext(p.ctx); e != nil {
		return b, e
	}
	f := fragment{txt, ct == "text/html"}
	return bodies{[]fragment{f}, []fragment{f}}, nil
}

// joinBodies consumes each mixed child promptly. Only compacted body streams
// and actual attachment/inline files survive to the next part/checkpoint.
func (p *parser) joinBodies(dst *bodies, child bodies) error {
	for mode, frags := range [][]fragment{child.read, child.render} {
		if len(frags) == 0 {
			continue
		}
		target := &dst.read
		limit := p.l.TextBytes
		if mode == 1 {
			target = &dst.render
			limit = p.l.PageBytes
		}
		if len(*target) == 0 {
			b, e := p.d.scope.NewBlob(p.ctx, limit, 0)
			if e != nil {
				return e
			}
			p.d.files = append(p.d.files, b)
			*target = []fragment{{b, mode == 1}}
		}
		out := (*target)[0].data
		for _, f := range frags {
			if out.Size() > 0 {
				if _, e := out.Write([]byte("\n\n")); e != nil {
					return e
				}
			}
			input, e := f.data.Open()
			if e != nil {
				return e
			}
			if mode == 0 && f.html {
				e = HTMLReadable(p.ctx, input, out, p.l)
			} else if mode == 1 && !f.html {
				checked := &contextWriter{ctx: p.ctx, w: out}
				_, e = io.WriteString(checked, "<pre>")
				buffer := make([]byte, 32768)
				for e == nil {
					n, re := input.Read(buffer)
					if n > 0 {
						template.HTMLEscape(checked, buffer[:n])
						e = checked.err
					}
					if re != nil {
						if re != io.EOF {
							e = re
						}
						break
					}
				}
				if e == nil {
					_, e = io.WriteString(checked, "</pre>")
				}
			} else {
				e = copyChunks(p.ctx, out, input)
			}
			e = errors.Join(e, input.Close())
			if e != nil {
				return e
			}
		}
	}
	return nil
}
func (p *parser) discardBodies(old, keep bodies) error {
	kept := map[*Blob]bool{}
	for _, list := range [][]fragment{keep.read, keep.render} {
		for _, f := range list {
			kept[f.data] = true
		}
	}
	var err error
	for _, list := range [][]fragment{old.read, old.render} {
		for _, f := range list {
			if !kept[f.data] {
				err = errors.Join(err, f.data.CloseContext(p.ctx))
				kept[f.data] = true
			}
		}
	}
	return err
}

// ParseReader shares the MIME traversal used by Parse. All raw input must be
// complete; callers with a provider use OpenSource before parsing.
func ParseReader(ctx context.Context, r io.Reader, s *Scope) (d *FileDoc, err error) {
	if s == nil {
		return nil, incomplete("scope required", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer cancel()
	var leave func()
	var e error
	if sr, ok := r.(*sourceReader); ok && sr.scope == s {
		leave = func() {}
	} else {
		leave, e = s.Enter(ctx)
		if e != nil {
			return nil, e
		}
	}
	l := s.limits
	estimate := parserReservation(l)
	release, e := s.reserveMemory(estimate)
	if e != nil {
		leave()
		return nil, e
	}
	d = &FileDoc{scope: s, ctx: ctx, Inline: map[string]FilePart{}, release: func() { release(); leave() }}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.Close())
			d = nil
		}
	}()
	h, body, headerBytes, e := headerRead(contextReader{ctx, &rawBoundReader{r: r, max: l.RawBytes}}, l)
	if e != nil {
		return d, incomplete("message header failed", e)
	}
	if h.Get("From") == "" && h.Get("Subject") == "" {
		return d, incomplete("no From or Subject header", nil)
	}
	d.MessageID = strings.TrimSpace(h.Get("Message-ID"))
	d.From, e = decodeHeader(h.Get("From"))
	if e != nil {
		return d, incomplete("invalid header encoding", e)
	}
	d.Subject, e = decodeHeader(h.Get("Subject"))
	if e != nil {
		return d, incomplete("invalid header encoding", e)
	}
	d.Date, _ = mail.Header(h).Date()
	p := &parser{ctx: ctx, d: d, l: l, headers: headerBytes}
	d.body, e = p.walk(h, body, 0)
	if e != nil {
		return d, e
	}
	if e = copyChunks(ctx, io.Discard, body); e != nil {
		return d, incomplete("incomplete message", e)
	}
	return d, nil
}
