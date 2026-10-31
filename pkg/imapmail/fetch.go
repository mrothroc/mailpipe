package imapmail

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	imap "github.com/emersion/go-imap"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type fetched struct {
	uid    uint32
	wire   WireMetadata
	body   []byte
	offset uint32
}

func parseFetch(r imap.Resp, source bool) (out fetched, isFetch bool, err error) {
	data, ok := r.(*imap.DataResp)
	if !ok {
		return out, false, nil
	}
	if len(data.Fields) != 3 {
		return out, false, nil
	}
	kind := atom(data.Fields[1])
	if kind != "FETCH" && kind != "UIDFETCH" {
		return out, false, nil
	}
	isFetch = true
	prefix, e := imap.ParseNumber(data.Fields[0])
	if e != nil || prefix == 0 {
		return out, true, fault("invalid FETCH prefix", false)
	}
	fields, ok := data.Fields[2].([]any)
	if !ok || len(fields)%2 != 0 {
		return out, true, fault("invalid FETCH grammar", false)
	}
	seen := map[string]bool{}
	bodyFields := 0
	for i := 0; i < len(fields); i += 2 {
		k := atom(fields[i])
		v := fields[i+1]
		if k == "" || seen[k] {
			return out, true, fault("duplicate FETCH field", false)
		}
		seen[k] = true
		switch k {
		case "UID":
			out.uid, e = imap.ParseNumber(v)
		case "FLAGS":
			out.wire.Flags, e = imap.ParseStringList(v)
			flags := map[string]bool{}
			for _, flag := range out.wire.Flags {
				key := strings.ToUpper(flag)
				if flags[key] || !validText(flag, 8192) {
					return out, true, fault("invalid or duplicate flag", false)
				}
				flags[key] = true
			}
		case "RFC822.SIZE":
			out.wire.Size, e = imap.ParseNumber(v)
		case "INTERNALDATE":
			var date string
			date, e = imap.ParseString(v)
			if e == nil {
				out.wire.InternalDate, e = time.Parse(imap.DateTimeLayout, date)
			}
		case "ENVELOPE":
			var envelope imap.Envelope
			var values []any
			values, ok = v.([]any)
			if !ok || len(values) != 10 {
				return out, true, fault("invalid envelope", false)
			}
			e = envelope.Parse(values)
			out.wire.Envelope = Envelope{Subject: envelope.Subject, MessageID: envelope.MessageId, InReplyTo: envelope.InReplyTo, Date: envelope.Date, From: addresses(envelope.From), To: addresses(envelope.To), Cc: addresses(envelope.Cc), Bcc: addresses(envelope.Bcc), ReplyTo: addresses(envelope.ReplyTo)}
		default:
			if source && strings.HasPrefix(k, "BODY[]<") && strings.HasSuffix(k, ">") {
				bodyFields++
				if bodyFields != 1 {
					return out, true, fault("extra source literal", false)
				}
				rawOffset := strings.TrimSuffix(strings.TrimPrefix(k, "BODY[]<"), ">")
				offset, parseErr := strconv.ParseUint(rawOffset, 10, 32)
				if parseErr != nil {
					return out, true, fault("invalid source offset", false)
				}
				out.offset = uint32(offset)
				literal, ok := v.(imap.Literal)
				if !ok || literal.Len() > 32768 {
					return out, true, fault("invalid source literal", false)
				}
				out.body = make([]byte, literal.Len())
				_, e = io.ReadFull(literal, out.body)
			} else {
				return out, true, fault("unknown FETCH field", false)
			}
		}
		if e != nil {
			return out, true, fault("invalid FETCH field", false)
		}
	}
	if !seen["UID"] || out.uid == 0 {
		return out, true, fault("untargeted FETCH update", true)
	}
	if kind == "UIDFETCH" && prefix != out.uid {
		return out, true, fault("UIDFETCH prefix mismatch", false)
	}
	// Legacy FETCH prefix is a sequence number and NEVER used as identity.
	if !seen["RFC822.SIZE"] {
		return out, true, fault("missing message size", false)
	}
	if source {
		if out.body == nil {
			return out, true, fault("missing source literal", false)
		}
	} else {
		for _, key := range []string{"FLAGS", "INTERNALDATE", "ENVELOPE"} {
			if !seen[key] {
				return out, true, fault("incomplete metadata response", false)
			}
		}
	}
	return out, true, nil
}
func addresses(values []*imap.Address) []mailbox.Address {
	out := make([]mailbox.Address, 0, len(values))
	for _, v := range values {
		// A NIL/empty host denotes structural group syntax, not a mailbox.
		// The flat projection retains qualified members, never group labels or
		// incomplete components promoted into a sender/recipient identity.
		if v != nil && v.MailboxName != "" && v.HostName != "" {
			out = append(out, mailbox.Address{Name: v.PersonalName, Address: v.MailboxName + "@" + v.HostName})
		}
	}
	return out
}
func (s *session) fetch(lo, hi uint32, offset *uint32, length uint32, consume func(fetched) error) error {
	rangeAtom := fmt.Sprintf("%d:%d", lo, hi)
	items := "UID FLAGS INTERNALDATE RFC822.SIZE ENVELOPE"
	if offset != nil {
		items = fmt.Sprintf("UID RFC822.SIZE BODY.PEEK[]<%d.%d>", *offset, length)
	}
	return s.command("UID FETCH", []any{imap.RawString(rangeAtom), imap.RawString("(" + items + ")")}, func(r imap.Resp) error {
		if data, ok := r.(*imap.DataResp); ok && len(data.Fields) >= 2 {
			switch atom(data.Fields[1]) {
			case "EXISTS", "EXPUNGE":
				s.dirty = true
				return fault("mailbox changed during scan", true)
			}
			if atom(data.Fields[0]) == "VANISHED" {
				s.dirty = true
				return fault("mailbox changed during scan", true)
			}
		}
		f, ok, e := parseFetch(r, offset != nil)
		if e != nil {
			return e
		}
		if !ok {
			if _, ok := r.(*imap.DataResp); ok {
				return fault("unexpected scan response", false)
			}
			return nil
		}
		if s.uidOnly {
			if data, ok := r.(*imap.DataResp); ok && atom(data.Fields[1]) == "FETCH" {
				prefix, e := imap.ParseNumber(data.Fields[0])
				if e != nil || prefix != f.uid {
					return fault("UIDONLY legacy prefix mismatch", false)
				}
			}
		}
		if f.uid < lo || f.uid > hi {
			return fault("untargeted UID update", true)
		}
		return consume(f)
	})
}
