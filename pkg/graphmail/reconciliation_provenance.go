package graphmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

const reconciliationOriginLimit = 16 << 20

var (
	errOriginUnsupported = errors.New("unsupported selected-field provenance")
	errOriginMismatch    = errors.New("selected-field provenance disagrees with mapped state")
	errOriginLimit       = errors.New("selected-field provenance budget exceeded")
)

// ReconciliationAddressSource distinguishes absence from a present empty value.
// Address is untrusted private data; it is never ordinary diagnostic output.
type ReconciliationAddressSource struct {
	Presence string `json:"presence"`
	Address  string `json:"address"`
}

// ReconciliationSenderAddressOrigin describes only selected wire addresses and
// fixed integrity results. It contains no State, recipient, name or authority.
type ReconciliationSenderAddressOrigin struct {
	From                    ReconciliationAddressSource `json:"from"`
	Sender                  ReconciliationAddressSource `json:"sender"`
	SelectedSource          string                      `json:"selected_source"`
	MappedSenderMatchesWire bool                        `json:"mapped_sender_matches_wire"`
	RecipientsMatchWire     bool                        `json:"recipients_match_wire"`
}

// SenderAddress returns the raw choice, without inventing an empty fallback.
func (o ReconciliationSenderAddressOrigin) SenderAddress() (string, bool) {
	valid := func(a ReconciliationAddressSource) bool {
		switch a.Presence {
		case "missing", "null":
			return a.Address == ""
		case "present":
			return utf8.ValidString(a.Address)
		}
		return false
	}
	if !valid(o.From) || !valid(o.Sender) {
		return "", false
	}
	switch o.SelectedSource {
	case "from":
		return o.From.Address, o.From.Presence == "present"
	case "sender":
		return o.Sender.Address, o.From.Presence != "present" && o.Sender.Presence == "present"
	case "none":
		return "", o.From.Presence != "present" && o.Sender.Presence != "present"
	default:
		return "", false
	}
}

// MatchesMappedSender checks diagnostic integrity, not identity or ownership.
// Recheck at the handoff so a changed copied state cannot inherit wire origin.
func (o *ReconciliationSenderAddressOrigin) MatchesMappedSender(mapped string) bool {
	if o == nil || !o.MappedSenderMatchesWire || !o.RecipientsMatchWire {
		return false
	}
	address, valid := o.SenderAddress()
	return valid && address == mapped
}

// A second, fresh minimal view of the actual JSON prevents a typed decoder
// field from being re-labelled as independent wire provenance. Selected keys
// must be canonical and unique; unknown fields are ignored, never exported.
func originObject(raw []byte, selected ...string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, errOriginUnsupported
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, e := dec.Token()
	if e != nil || token != json.Delim('{') {
		return nil, errOriginUnsupported
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		token, e = dec.Token()
		if e != nil {
			return nil, errOriginUnsupported
		}
		key, ok := token.(string)
		if !ok {
			return nil, errOriginUnsupported
		}
		var value json.RawMessage
		if e = dec.Decode(&value); e != nil {
			return nil, errOriginUnsupported
		}
		for _, name := range selected {
			if strings.EqualFold(key, name) {
				if key != name || out[name] != nil {
					return nil, errOriginUnsupported
				}
				out[name] = value
				break
			}
		}
	}
	if token, e = dec.Token(); e != nil || token != json.Delim('}') {
		return nil, errOriginUnsupported
	}
	if _, e = dec.Token(); e != io.EOF {
		return nil, errOriginUnsupported
	}
	return out, nil
}
func originString(raw json.RawMessage) (string, error) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil || !utf8.ValidString(s) {
		return "", errOriginUnsupported
	}
	return s, nil
}
func originAddress(raw json.RawMessage) (mailbox.Address, error) {
	fields, e := originObject(raw, "emailAddress")
	if e != nil {
		return mailbox.Address{}, e
	}
	fields, e = originObject(fields["emailAddress"], "address", "name")
	if e != nil {
		return mailbox.Address{}, e
	}
	address, e := originString(fields["address"])
	if e != nil {
		return mailbox.Address{}, e
	}
	name := ""
	if value := fields["name"]; len(value) > 0 && string(value) != "null" {
		name, e = originString(value)
		if e != nil {
			return mailbox.Address{}, e
		}
	}
	return mailbox.Address{Address: address, Name: name}, nil
}
func originSource(raw json.RawMessage) (ReconciliationAddressSource, mailbox.Address, error) {
	if len(raw) == 0 {
		return ReconciliationAddressSource{Presence: "missing"}, mailbox.Address{}, nil
	}
	if string(raw) == "null" {
		return ReconciliationAddressSource{Presence: "null"}, mailbox.Address{}, nil
	}
	a, e := originAddress(raw)
	if e != nil {
		return ReconciliationAddressSource{}, mailbox.Address{}, e
	}
	return ReconciliationAddressSource{Presence: "present", Address: a.Address}, a, nil
}
func reconciliationOrigin(ctx context.Context, raw []byte, state mailbox.State) (ReconciliationSenderAddressOrigin, error) {
	var out ReconciliationSenderAddressOrigin
	if e := ctx.Err(); e != nil {
		return out, e
	}
	fields, e := originObject(raw, "id", "from", "sender", "toRecipients", "ccRecipients")
	if e != nil {
		return out, e
	}
	id, e := originString(fields["id"])
	if e != nil {
		return out, e
	}
	if id != string(state.Ref.MessageID) || id != string(state.Ref.Locator) {
		return out, errOriginMismatch
	}
	from, fromAddress, e := originSource(fields["from"])
	if e != nil {
		return out, e
	}
	sender, senderAddress, e := originSource(fields["sender"])
	if e != nil {
		return out, e
	}
	out.From, out.Sender = from, sender
	chosen := mailbox.Address{}
	out.SelectedSource = "none"
	if from.Presence == "present" {
		chosen = fromAddress
		out.SelectedSource = "from"
	} else if sender.Presence == "present" {
		chosen = senderAddress
		out.SelectedSource = "sender"
	}
	out.MappedSenderMatchesWire = chosen == state.Metadata.Sender
	index := 0
	for _, field := range []string{"toRecipients", "ccRecipients"} {
		var recipients []json.RawMessage
		if json.Unmarshal(fields[field], &recipients) != nil || recipients == nil {
			return out, errOriginUnsupported
		}
		for _, raw := range recipients {
			if e = ctx.Err(); e != nil {
				return out, e
			}
			a, e := originAddress(raw)
			if e != nil {
				return out, e
			}
			if index >= len(state.Metadata.Recipients) || a != state.Metadata.Recipients[index] {
				return out, errOriginMismatch
			}
			index++
		}
	}
	out.RecipientsMatchWire = index == len(state.Metadata.Recipients) && (index != 0 || state.Metadata.Recipients == nil)
	if !out.MappedSenderMatchesWire || !out.RecipientsMatchWire {
		return out, errOriginMismatch
	}
	return out, nil
}

type reconciliationOriginBudget struct{ used, limit int }

func (b *reconciliationOriginBudget) retain(ctx context.Context, id string, o ReconciliationSenderAddressOrigin, mapped string) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	for _, s := range []string{id, o.From.Address, o.Sender.Address, mapped} {
		if len(s) > b.limit-b.used {
			return errOriginLimit
		}
		b.used += len(s)
	}
	return nil
}
func originCategory(e error) string {
	switch {
	case errors.Is(e, errOriginUnsupported):
		return "unsupported-provenance"
	case errors.Is(e, errOriginMismatch):
		return "provenance-mismatch"
	case errors.Is(e, errOriginLimit):
		return "provenance-limit"
	}
	return ""
}
