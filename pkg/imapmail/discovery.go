package imapmail

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"

	imap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/utf7"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func canonical(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}
func (c *Client) folder(name, delimiter string) Folder {
	name = canonical(name)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(name))
	return Folder{Name: name, Delimiter: delimiter, Collection: mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: c.Account().ID, CollectionID: mailbox.CollectionID("imap-folder-v1:" + encoded)}, Name: name}, MailboxID: "imap-path-v1:" + encoded}
}
func (c *Client) Discover(ctx context.Context) (result Discovery, err error) {
	ctx, done, e := c.operation(ctx)
	if e != nil {
		return Discovery{}, safe("discovery unavailable", e)
	}
	defer done()
	s, e := c.connect(ctx)
	if e != nil {
		return Discovery{}, safe("discovery failed", e)
	}
	defer func() {
		if e := s.close(); e != nil {
			result = Discovery{}
			err = safe("discovery cleanup failed", errors.Join(err, e))
		}
	}()
	d, e := c.discover(s)
	if e != nil {
		return d, safe("folder roles require explicit validated mappings", e)
	}
	return d, nil
}
func (c *Client) discover(s *session) (Discovery, error) {
	d := Discovery{Capabilities: s.caps}
	names := map[string]int{}
	seenNames := map[string]bool{}
	roles := map[Role][]int{}
	args := []any{"", "*"}
	if s.caps.Has("SPECIAL-USE") && s.caps.Has("LIST-EXTENDED") {
		args = append(args, imap.RawString("RETURN"), []any{imap.RawString("SPECIAL-USE")})
	}
	e := s.command("LIST", args, func(r imap.Resp) error {
		data, ok := r.(*imap.DataResp)
		if !ok {
			return nil
		}
		if len(data.Fields) == 0 || atom(data.Fields[0]) != "LIST" {
			return fault("unexpected discovery data", false)
		}
		if len(data.Fields) != 4 {
			return fault("invalid folder response", false)
		}
		attrs, ok := data.Fields[1].([]any)
		if !ok {
			return fault("invalid folder attributes", false)
		}
		flags, e := imap.ParseStringList(attrs)
		if e != nil {
			return fault("invalid folder attributes", false)
		}
		name, e := imap.ParseString(data.Fields[3])
		if e != nil {
			return fault("invalid folder name", false)
		}
		name, e = utf7.Encoding.NewDecoder().String(name)
		if e != nil || !utf8.ValidString(name) || !validText(name, 4096) {
			return fault("invalid folder encoding", false)
		}
		name = canonical(name)
		if seenNames[name] {
			return fault("ambiguous folder identity", false)
		}
		seenNames[name] = true
		if hasFlag(flags, `\Noselect`) {
			return nil
		}
		if _, ok := names[name]; ok {
			return fault("ambiguous folder identity", false)
		}
		if len(d.Folders) >= 10000 {
			return fault("folder inventory limit", false)
		}
		delimiter := ""
		if data.Fields[2] != nil {
			delimiter, e = imap.ParseString(data.Fields[2])
			if e != nil || len(delimiter) > 32 {
				return fault("invalid folder delimiter", false)
			}
		}
		f := c.folder(name, delimiter)
		idx := len(d.Folders)
		names[name] = idx
		d.Folders = append(d.Folders, f)
		for _, role := range []Role{Sent, Trash, Junk} {
			if hasFlag(flags, `\`+string(role)) {
				roles[role] = append(roles[role], idx)
			}
		}
		return nil
	})
	if e != nil {
		return d, e
	}
	inbox, ok := names["INBOX"]
	if !ok {
		return d, fault("standard INBOX missing", false)
	}
	d.Folders[inbox].Role = Inbox
	used := map[int]bool{inbox: true}
	for _, role := range []Role{Sent, Trash, Junk} {
		idx := -1
		if explicit, ok := c.cfg.RoleMappings[role]; ok {
			var found bool
			idx, found = names[explicit]
			if !found {
				return d, fault("role mapping does not name existing selectable mailbox", false)
			}
		} else {
			if len(roles[role]) != 1 {
				return d, fault("required special-use role needs explicit mapping", false)
			}
			idx = roles[role][0]
		}
		if used[idx] {
			return d, fault("role mappings are ambiguous", false)
		}
		for other, indices := range roles {
			if other == role {
				continue
			}
			for _, i := range indices {
				if idx == i {
					return d, fault("mapping contradicts special-use attributes", false)
				}
			}
		}
		used[idx] = true
		d.Folders[idx].Role = role
	}
	return d, nil
}

type selected struct{ validity, next, count uint32 }

func (s *session) examine(f Folder) (selected, error) {
	var out selected
	var haveValidity, haveNext, haveCount, readonly bool
	name, e := utf7.Encoding.NewEncoder().String(f.Name)
	if e != nil {
		return out, fault("invalid folder encoding", false)
	}
	e = s.command("EXAMINE", []any{name}, func(r imap.Resp) error {
		switch r := r.(type) {
		case *imap.StatusResp:
			switch r.Code {
			case "UIDVALIDITY":
				if haveValidity || len(r.Arguments) != 1 {
					return fault("duplicate epoch evidence", false)
				}
				out.validity, e = imap.ParseNumber(r.Arguments[0])
				haveValidity = true
			case "UIDNEXT":
				if haveNext || len(r.Arguments) != 1 {
					return fault("duplicate high-water evidence", false)
				}
				out.next, e = imap.ParseNumber(r.Arguments[0])
				haveNext = true
			case "READ-ONLY":
				readonly = true
			}
			if e != nil {
				return fault("invalid selection evidence", false)
			}
		case *imap.DataResp:
			if len(r.Fields) == 2 && atom(r.Fields[1]) == "EXISTS" {
				if haveCount {
					return fault("selection count changed", true)
				}
				out.count, e = imap.ParseNumber(r.Fields[0])
				haveCount = true
				if e != nil {
					return fault("invalid mailbox count", false)
				}
			}
		}
		return nil
	})
	if e != nil {
		return out, e
	}
	if !haveValidity || !haveNext || !haveCount || !readonly || out.validity == 0 || out.next == 0 {
		return out, fault("read-only UID selection evidence missing", false)
	}
	return out, nil
}
