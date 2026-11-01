package imapmail

import (
	"fmt"
	"mime"
	"strings"

	imap "github.com/emersion/go-imap"
	"github.com/mrothroc/mailpipe/pkg/document"
)

func (s *session) structure(uid, size uint32, l document.Limits) (out ContentStructure, err error) {
	l, err = l.Resolve()
	if err != nil {
		return out, err
	}
	seen := false
	err = s.command("UID FETCH", []any{imap.RawString(fmt.Sprintf("%d:%d", uid, uid)), imap.RawString("(UID RFC822.SIZE BODYSTRUCTURE)")}, func(r imap.Resp) error {
		d, ok := r.(*imap.DataResp)
		if !ok {
			return nil
		}
		if len(d.Fields) != 3 || (atom(d.Fields[1]) != "FETCH" && atom(d.Fields[1]) != "UIDFETCH") {
			return failure("unexpected structure response")
		}
		prefix, e := imap.ParseNumber(d.Fields[0])
		if e != nil || prefix == 0 {
			return failure("invalid structure prefix")
		}
		fields, ok := d.Fields[2].([]any)
		if !ok || len(fields) != 6 || seen {
			return failure("invalid structure fields")
		}
		var gotUID, gotSize uint32
		var structure []any
		keys := map[string]bool{}
		for i := 0; i < len(fields); i += 2 {
			k := atom(fields[i])
			if keys[k] {
				return failure("duplicate structure field")
			}
			keys[k] = true
			switch k {
			case "UID":
				gotUID, e = imap.ParseNumber(fields[i+1])
			case "RFC822.SIZE":
				gotSize, e = imap.ParseNumber(fields[i+1])
			case "BODYSTRUCTURE":
				structure, ok = fields[i+1].([]any)
				if !ok {
					return failure("missing body structure")
				}
			default:
				return failure("unexpected structure field")
			}
			if e != nil {
				return failure("invalid structure value")
			}
		}
		if !keys["UID"] || !keys["RFC822.SIZE"] || !keys["BODYSTRUCTURE"] || gotUID != uid || gotSize != size || ((s.uidOnly || atom(d.Fields[1]) == "UIDFETCH") && prefix != uid) {
			return failure("structure identity changed")
		}
		seen = true
		var nodes int
		attached, e := structureAttachments(structure, l, 0, &nodes)
		out = ContentStructure{attached, nodes}
		return e
	})
	if err == nil && !seen {
		err = failure("structure occurrence missing")
	}
	if err != nil {
		return ContentStructure{}, err
	}
	return out, nil
}
func structureAttachments(fields []any, l document.Limits, depth int, nodes *int) (bool, error) {
	if depth > l.Depth || *nodes >= l.Parts {
		return false, failure("structure depth or part limit")
	}
	*nodes++
	if len(fields) == 0 {
		return false, failure("empty body structure")
	}
	var childAttached bool
	inlineImage := false
	var params, disposition any
	if _, multi := fields[0].([]any); multi {
		i := 0
		for i < len(fields) {
			child, ok := fields[i].([]any)
			if !ok {
				break
			}
			a, e := structureAttachments(child, l, depth+1, nodes)
			if e != nil {
				return false, e
			}
			childAttached = childAttached || a
			i++
		}
		if i == 0 || len(fields) < i+3 {
			return false, failure("incomplete multipart structure extensions")
		}
		if subtype, e := structureString(fields[i], l); e != nil || !structureToken(subtype) {
			return false, failure("invalid multipart subtype")
		}
		params = fields[i+1]
		disposition = fields[i+2]
	} else {
		if len(fields) < 7 {
			return false, failure("incomplete leaf structure")
		}
		typ, e := structureString(fields[0], l)
		if e != nil || !structureToken(typ) || strings.EqualFold(typ, "multipart") {
			return false, failure("invalid structure type")
		}
		sub, e := structureString(fields[1], l)
		if e != nil || !structureToken(sub) {
			return false, failure("invalid structure subtype")
		}
		if _, e := imap.ParseNumber(fields[6]); e != nil {
			return false, failure("invalid structure size")
		}
		params = fields[2]
		id := ""
		for _, index := range []int{3, 4} {
			if fields[index] != nil {
				value, e := structureString(fields[index], l)
				if e != nil {
					return false, failure("invalid structure nstring")
				}
				if index == 3 {
					id = value
				}
			}
		}
		enc, e := structureString(fields[5], l)
		if e != nil || !structureToken(enc) {
			return false, failure("invalid structure encoding")
		}
		inlineImage = strings.EqualFold(typ, "image") && strings.TrimSpace(id) != ""
		extension := 7
		switch strings.ToLower(typ + "/" + sub) {
		case "message/rfc822":
			if len(fields) < 10 {
				return false, failure("incomplete nested structure")
			}
			envelope, ok := fields[7].([]any)
			if !ok || len(envelope) != 10 {
				return false, failure("invalid nested envelope")
			}
			if _, e := imap.ParseNumber(fields[9]); e != nil {
				return false, failure("invalid nested line count")
			}
			child, ok := fields[8].([]any)
			if !ok {
				return false, failure("missing nested structure")
			}
			a, e := structureAttachments(child, l, depth+1, nodes)
			if e != nil {
				return false, e
			}
			childAttached = a
			extension = 10
		case "text/plain", "text/html":
			extension = 8
		default:
			if strings.EqualFold(typ, "text") {
				extension = 8
			}
		}
		if strings.EqualFold(typ, "text") {
			if len(fields) <= 7 {
				return false, failure("missing text line count")
			}
			if _, e := imap.ParseNumber(fields[7]); e != nil {
				return false, failure("invalid text line count")
			}
		}
		if len(fields) < extension+2 {
			return false, failure("incomplete disposition evidence")
		}
		if fields[extension] != nil {
			if _, e := structureString(fields[extension], l); e != nil {
				return false, failure("invalid structure checksum")
			}
		}
		disposition = fields[extension+1]
	}
	name, e := structureName(params, l)
	if e != nil {
		return false, e
	}
	disp := ""
	filename := ""
	if disposition != nil {
		d, ok := disposition.([]any)
		if !ok || len(d) != 2 {
			return false, failure("invalid disposition evidence")
		}
		disp, e = structureString(d[0], l)
		if e != nil || !structureToken(disp) {
			return false, failure("invalid disposition")
		}
		filename, e = structureName(d[1], l)
		if e != nil {
			return false, e
		}
	}
	if strings.EqualFold(disp, "attachment") {
		return true, nil
	}
	if inlineImage {
		return childAttached, nil
	}
	if name != "" || filename != "" {
		return true, nil
	}
	return childAttached, nil
}
func structureName(value any, l document.Limits) (string, error) {
	if value == nil {
		return "", nil
	}
	fields, ok := value.([]any)
	if !ok || len(fields)%2 != 0 || len(fields)/2 > l.HeaderFields {
		return "", failure("invalid structure parameters")
	}
	name := ""
	keys := map[string]bool{}
	var bytes int64
	for i := 0; i < len(fields); i += 2 {
		k, e := structureString(fields[i], l)
		v, e2 := structureString(fields[i+1], l)
		if e != nil || e2 != nil || !structureToken(k) {
			return "", failure("invalid structure parameter")
		}
		k = strings.ToLower(k)
		if keys[k] {
			return "", failure("duplicate structure parameter")
		}
		keys[k] = true
		bytes += int64(len(k) + len(v))
		if bytes > l.HeaderBytes {
			return "", failure("structure parameter limit")
		}
		if v != "" && (k == "filename" || k == "name" || strings.HasPrefix(k, "filename*") || strings.HasPrefix(k, "name*")) {
			name = v
		}
	}
	return name, nil
}

// Reuse the codec's string/literal mapping, checking its finite size before a
// literal-to-string allocation. NIL is accepted only by explicit nstring callers.
func structureString(value any, l document.Limits) (string, error) {
	if literal, ok := value.(imap.Literal); ok && (literal.Len() < 0 || int64(literal.Len()) > l.HeaderBytes) {
		return "", failure("structure string limit")
	}
	text, e := imap.ParseString(value)
	if e != nil || int64(len(text)) > l.HeaderBytes {
		return "", failure("invalid structure string")
	}
	return text, nil
}
func structureToken(value string) bool {
	if value == "" || value == "*" {
		return false
	}
	typ, params, e := mime.ParseMediaType("application/" + value)
	return e == nil && len(params) == 0 && strings.EqualFold(typ, "application/"+value)
}
