package graphmail

import (
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// Recovery metadata equality permits address casing and sender presentation
// changes on an already exact immutable reference. This is neither RFC mailbox
// equivalence nor the action policy.
// Clearing fields changes value copies only, never input recipient arrays.
func reconciliationMetadataMatches(a, b mailbox.Metadata) bool {
	return reconciliationMetadataMatchesSender(a, b, false)
}

// The caller may exclude enumeration's sender address only after source
// integrity and the original historical sender/header checks have passed.
func reconciliationMetadataMatchesSender(a, b mailbox.Metadata, senderDisagrees bool) bool {
	// Changed names must survive JSON receipt encoding without byte replacement.
	// Identical names retain existing address-only comparison semantics.
	if a.Sender.Name != b.Sender.Name && (!utf8.ValidString(a.Sender.Name) || !utf8.ValidString(b.Sender.Name)) {
		return false
	}
	if !utf8.ValidString(a.Sender.Address) || !utf8.ValidString(b.Sender.Address) {
		return false
	}
	if senderDisagrees {
		if a.Sender.Address == "" || b.Sender.Address == "" {
			return false
		}
	} else if !strings.EqualFold(a.Sender.Address, b.Sender.Address) {
		return false
	}
	if (a.Recipients == nil) != (b.Recipients == nil) || len(a.Recipients) != len(b.Recipients) {
		return false
	}
	for i, left := range a.Recipients {
		right := b.Recipients[i]
		if !utf8.ValidString(left.Address) || !utf8.ValidString(right.Address) || !strings.EqualFold(left.Address, right.Address) {
			return false
		}
		left.Address, right.Address = "", ""
		if !reflect.DeepEqual(left, right) {
			return false
		}
	}
	a.Sender.Address, b.Sender.Address = "", ""
	a.Sender.Name, b.Sender.Name = "", ""
	a.Recipients, b.Recipients = nil, nil
	return reflect.DeepEqual(a, b)
}

// Diagnostics run only after metadata rejection. Their finite vocabulary never
// includes values, counts, reflected names or raw errors. TrimSpace here is NOT
// an acceptance rule and these relations never supply evidence or authority.
func metadataDifference(a, b mailbox.Metadata) string {
	fields := []string{}
	if a.InternetMessageID != b.InternetMessageID {
		fields = append(fields, "internet-message-id")
	}
	if !reflect.DeepEqual(a.Received, b.Received) {
		fields = append(fields, "received")
	}
	if a.Sender.Address != b.Sender.Address {
		fields = append(fields, "sender-address")
	}
	if a.Sender.Name != b.Sender.Name {
		fields = append(fields, "sender-name")
	}
	if !reflect.DeepEqual(a.Recipients, b.Recipients) {
		fields = append(fields, "recipients")
	}
	if a.Subject != b.Subject {
		fields = append(fields, "subject")
	}
	if a.Preview != b.Preview {
		fields = append(fields, "preview")
	}
	if a.HasAttachments != b.HasAttachments {
		fields = append(fields, "has-attachments")
	}
	if a.IsRead != b.IsRead {
		fields = append(fields, "is-read")
	}
	if !reflect.DeepEqual(metadataUnclassified(a), metadataUnclassified(b)) {
		fields = append(fields, "unknown-field")
	}
	return "; fields=" + strings.Join(fields, ",") + "; sender-address=" + addressRelation(a.Sender.Address, b.Sender.Address) + "; recipient-addresses=" + recipientRelation(a.Recipients, b.Recipients, false) + "; recipient-names=" + recipientRelation(a.Recipients, b.Recipients, true)
}

func metadataUnclassified(m mailbox.Metadata) mailbox.Metadata {
	m.InternetMessageID = ""
	m.Received = time.Time{}
	m.Sender.Address = ""
	m.Sender.Name = ""
	m.Recipients = nil
	m.Subject = ""
	m.Preview = ""
	m.HasAttachments = false
	m.IsRead = false
	return m
}

// Scalar precedence: encoding, exact, untrimmed fold, trim-only, joint, different.
func addressRelation(a, b string) string {
	if !utf8.ValidString(a) || !utf8.ValidString(b) {
		return "invalid-utf8"
	}
	return addressRelationResult(a == b, strings.EqualFold(a, b), strings.TrimSpace(a) == strings.TrimSpace(b), strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)))
}
func addressRelationResult(exact, fold, trim, joint bool) string {
	switch {
	case exact:
		return "exact"
	case fold:
		return "case-only"
	case trim:
		return "trim-only"
	case joint:
		return "trim-and-case"
	default:
		return "different"
	}
}

// Sequence precedence is nilness, cardinality, relevant string encoding, then
// the same address precedence at corresponding indexes. Names stay byte-exact.
// "different" can include order/content, never a semantic mailbox assertion.
func recipientRelation(a, b []mailbox.Address, names bool) string {
	if (a == nil) != (b == nil) {
		return "representation-different"
	}
	if len(a) != len(b) {
		return "cardinality-different"
	}
	for i, left := range a {
		x, y := left.Address, b[i].Address
		if names {
			x, y = left.Name, b[i].Name
		}
		if !utf8.ValidString(x) || !utf8.ValidString(y) {
			return "invalid-utf8"
		}
	}
	exact, fold, trim, joint := true, true, true, true
	for i, left := range a {
		x, y := left.Address, b[i].Address
		if names {
			x, y = left.Name, b[i].Name
		}
		exact = exact && x == y
		if !names {
			fold = fold && strings.EqualFold(x, y)
			trim = trim && strings.TrimSpace(x) == strings.TrimSpace(y)
			joint = joint && strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(y))
		}
	}
	if names {
		if exact {
			return "exact"
		}
		return "different"
	}
	return addressRelationResult(exact, fold, trim, joint)
}
