package graphmail

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestMetadataRelationsAreDiagnosticOnly(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{{"same", "same", "exact"}, {"Reader", "reader", "case-only"}, {" reader ", "reader", "trim-only"}, {" READER ", "reader", "trim-and-case"}, {"one", "other", "different"}, {string([]byte{0xff}), "reader", "invalid-utf8"}, {"ß", "ẞ", "case-only"}, {"ß", "SS", "different"}, {"İ", "i", "different"}, {"I", "ı", "different"}, {"é", "e\u0301", "different"}} {
		if got := addressRelation(tc.a, tc.b); got != tc.want {
			t.Fatalf("wrong scalar relationship %s", got)
		}
	}
	a := mailbox.Metadata{Sender: mailbox.Address{Address: "Sender@Example.org", Name: "Sender"}, Recipients: []mailbox.Address{{Address: "Reader@Example.org", Name: "Reader"}}}
	b := reconciliationObservation(mailbox.State{Metadata: a}).Metadata
	b.Sender.Address = "sender@example.org"
	b.Recipients[0].Address = "reader@example.org"
	oldA, oldB := reconciliationObservation(mailbox.State{Metadata: a}), reconciliationObservation(mailbox.State{Metadata: b})
	if !reconciliationMetadataMatches(a, b) {
		t.Fatal("combined address case rejected")
	}
	_ = metadataDifference(a, b)
	if !reflect.DeepEqual(a, oldA.Metadata) || !reflect.DeepEqual(b, oldB.Metadata) {
		t.Fatal("comparison/diagnostics mutated metadata")
	}
	for _, kind := range []string{"sender-trim", "sender-invalid", "recipient-trim", "recipient-invalid", "sender-name-invalid", "recipient-name-invalid", "nil-empty"} {
		t.Run(kind, func(t *testing.T) {
			x, y := reconciliationObservation(mailbox.State{Metadata: a}).Metadata, reconciliationObservation(mailbox.State{Metadata: b}).Metadata
			switch kind {
			case "sender-trim":
				y.Sender.Address = " sender@example.org "
			case "sender-invalid":
				y.Sender.Address = string([]byte{0xff})
			case "recipient-trim":
				y.Recipients[0].Address = " reader@example.org "
			case "recipient-invalid":
				y.Recipients[0].Address = string([]byte{0xfe})
			case "sender-name-invalid":
				y.Sender.Name = string([]byte{0xff})
			case "recipient-name-invalid":
				y.Recipients[0].Name = "\xff"
			case "nil-empty":
				x.Recipients = nil
				y.Recipients = []mailbox.Address{}
			}
			if reconciliationMetadataMatches(x, y) {
				t.Fatal("diagnostic relation relaxed acceptance")
			}
		})
	}
}

func TestMetadataDiagnosticSequencePrecedence(t *testing.T) {
	a := []mailbox.Address{{Address: "Reader@example.org", Name: "Reader"}, {Address: "other@example.org", Name: "Other"}}
	copy := func() []mailbox.Address { return append([]mailbox.Address(nil), a...) }
	for _, tc := range []struct {
		name, want string
		edit       func([]mailbox.Address) []mailbox.Address
		names      bool
	}{
		{"representation", "representation-different", func([]mailbox.Address) []mailbox.Address { return nil }, false},
		{"cardinality", "cardinality-different", func(b []mailbox.Address) []mailbox.Address { return b[:1] }, false},
		{"encoding", "invalid-utf8", func(b []mailbox.Address) []mailbox.Address { b[0].Address = string([]byte{0xff}); return b }, false},
		{"case", "case-only", func(b []mailbox.Address) []mailbox.Address { b[0].Address = "reader@example.org"; return b }, false},
		{"trim", "trim-only", func(b []mailbox.Address) []mailbox.Address { b[0].Address = " Reader@example.org "; return b }, false},
		{"joint", "trim-and-case", func(b []mailbox.Address) []mailbox.Address { b[0].Address = " reader@example.org "; return b }, false},
		{"order", "different", func(b []mailbox.Address) []mailbox.Address { b[0], b[1] = b[1], b[0]; return b }, false},
		{"names-exact", "exact", func(b []mailbox.Address) []mailbox.Address { return b }, true},
		{"names-case", "different", func(b []mailbox.Address) []mailbox.Address { b[0].Name = "READER"; return b }, true},
		{"names-invalid", "invalid-utf8", func(b []mailbox.Address) []mailbox.Address { b[0].Name = string([]byte{0xff}); return b }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recipientRelation(a, tc.edit(copy()), tc.names); got != tc.want {
				t.Fatalf("wrong relation %s", got)
			}
		})
	}
}

func TestMetadataDiagnosticFieldsAndPrivacyBound(t *testing.T) {
	const hostile = "invented-secret\x1b\nhttps://host.example.org/private"
	a := mailbox.Metadata{}
	b := mailbox.Metadata{InternetMessageID: hostile, Received: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Sender: mailbox.Address{Address: hostile, Name: hostile}, Recipients: []mailbox.Address{{Address: hostile, Name: hostile}}, Subject: hostile, Preview: hostile, HasAttachments: true, IsRead: true}
	suffix := metadataDifference(a, b)
	want := "fields=internet-message-id,received,sender-address,sender-name,recipients,subject,preview,has-attachments,is-read;"
	if !strings.Contains(suffix, want) || !reflect.DeepEqual(metadataUnclassified(a), metadataUnclassified(b)) {
		t.Fatal("current metadata fields incompletely classified")
	}
	cause := errors.New(hostile)
	err := incompleteEvidence("consistency", "metadata-changed"+suffix, cause)
	if !errors.Is(err, cause) || len(err.Error()) > 1024 {
		t.Fatal("error bound or cause identity changed")
	}
	for _, value := range []string{"invented-secret", "https:", "host.example.org", "\x1b", "\n"} {
		if strings.Contains(err.Error(), value) {
			t.Fatal("diagnostic leaked untrusted value")
		}
	}
	// Future non-address Metadata fields remain in the fixed unknown-field
	// fallback; there is no such extra field to instantiate in this version.
}

func TestMetadataSenderNameEncodingAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		before, after string
		ok            bool
	}{{"Before", "After", true}, {"", "Name", true}, {" Name ", "Name", true}, {"Émetteur", "送信者", true}, {string([]byte{0xff}), "Name", false}, {"Name", string([]byte{0xff}), false}, {string([]byte{0xff}), string([]byte{0xfe}), false}, {string([]byte{0xff}), string([]byte{0xff}), true}} {
		a := mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org", Name: tc.before}}
		b := a
		b.Sender.Address = "SENDER@Example.org"
		b.Sender.Name = tc.after
		oldA, oldB := a, b
		if reconciliationMetadataMatches(a, b) != tc.ok {
			t.Fatal("wrong changed-name encoding policy")
		}
		if !reflect.DeepEqual(a, oldA) || !reflect.DeepEqual(b, oldB) {
			t.Fatal("comparison rewrote names")
		}
		b.Subject = "Other"
		if reconciliationMetadataMatches(a, b) {
			t.Fatal("name exception masked other metadata")
		}
	}
}
