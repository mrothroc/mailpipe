package imapmail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type occurrenceKey struct {
	Account       mailbox.AccountID
	Collection    mailbox.CollectionID
	Mailbox       string
	Validity, UID uint32
	Generation    int64
}

func occurrence(account mailbox.Account, folder Folder, validity, uid uint32, generation int64) Occurrence {
	k := occurrenceKey{account.ID, folder.Collection.Ref.CollectionID, folder.MailboxID, validity, uid, generation}
	b, _ := json.Marshal(k)
	v := "imap-occurrence-v1:" + base64.RawURLEncoding.EncodeToString(b)
	return Occurrence{Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(v), Locator: mailbox.Locator(v)}, Collection: folder.Collection.Ref, MailboxID: folder.MailboxID, UIDValidity: validity, UID: uid, EpochGeneration: generation}
}
func (c *Client) decode(ref mailbox.MessageRef) (Occurrence, Folder, error) {
	if e := ref.Validate(c.Account().ID); e != nil {
		return Occurrence{}, Folder{}, e
	}
	const prefix = "imap-occurrence-v1:"
	v := string(ref.MessageID)
	if len(v) > 8192 || len(v) < len(prefix) || v[:len(prefix)] != prefix || string(ref.Locator) != v {
		return Occurrence{}, Folder{}, failure("invalid occurrence identity")
	}
	b, e := base64.RawURLEncoding.DecodeString(v[len(prefix):])
	if e != nil {
		return Occurrence{}, Folder{}, failure("invalid occurrence identity")
	}
	var k occurrenceKey
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&k); e != nil {
		return Occurrence{}, Folder{}, failure("invalid occurrence identity")
	}
	var trailing any
	if e = dec.Decode(&trailing); e != io.EOF {
		return Occurrence{}, Folder{}, failure("invalid occurrence identity")
	}
	const fp = "imap-folder-v1:"
	collection := string(k.Collection)
	if len(collection) < len(fp) || collection[:len(fp)] != fp {
		return Occurrence{}, Folder{}, failure("invalid occurrence scope")
	}
	name, e := base64.RawURLEncoding.DecodeString(collection[len(fp):])
	if e != nil || !validText(string(name), 4096) {
		return Occurrence{}, Folder{}, failure("invalid occurrence scope")
	}
	f := c.folder(string(name), "")
	if k.Account != c.Account().ID || k.Collection != f.Collection.Ref.CollectionID || k.Mailbox != f.MailboxID || k.Validity == 0 || k.UID == 0 || k.Generation <= 0 {
		return Occurrence{}, Folder{}, failure("invalid occurrence tuple")
	}
	o := occurrence(c.Account(), f, k.Validity, k.UID, k.Generation)
	if o.Ref != ref {
		return Occurrence{}, Folder{}, failure("noncanonical occurrence")
	}
	return o, f, nil
}
