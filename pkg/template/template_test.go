package template_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/template"
)

var pacific = time.FixedZone("PDT", -7*3600)

const giftJSON = `{"templates": [
  {"id": "gift-receipt", "senders": ["gifts@school.example.org"],
   "describe": "A gift receipt for a donation, with a receipt number and a gift date",
   "subject": "^Gift Receipt",
   "anchors": ["Please keep this email as your receipt for tax purposes."],
   "fields": {
     "date": {"pattern": "Gift date: (\\S+)", "type": "date"},
     "receipt": {"pattern": "Receipt number: (\\d+)"}
   },
   "file": {"folder": "Archive/Records/{date.year}", "name": "{date}_School_Gift Receipt_{receipt}"}},
  {"id": "invoice", "senders": ["@vendor.example.com"],
   "describe": "An invoice for a software subscription",
   "subject": "Invoice (INV\\d+)",
   "fields": {
     "date": {"from": "received"},
     "invoice": {"from": "subject", "pattern": "(INV\\d+)"}
   },
   "file": {"folder": "Archive/Records/{date.year}/Business", "name": "{date}_Vendor_Invoice_{invoice}"}},
  {"id": "ride", "senders": ["rides@example.net"],
   "describe": "A receipt for a ride", "subject": "Your ride"}
]}`

func giftMessage() stage.Message {
	return stage.Message{N: 7, Sender: "Gifts@School.example.org", Subject: "Gift Receipt - Thank You!",
		Received: "2026-01-05T17:00:00Z",
		Body: "Please keep this email\n  as your receipt for tax purposes.\n\nGift details\nReceipt number:\t10011680\n" +
			"Receipt date:  01/05/2026\nGift date:   12/24/2025\n<https://track.example.org/x>"}
}

func mustParse(t *testing.T, data string) *template.Set {
	t.Helper()
	s, err := template.Parse([]byte(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

func TestForMatchesAddressesAndDomainsIgnoringCase(t *testing.T) {
	s := mustParse(t, giftJSON)
	ids := func(sender string) []string {
		var out []string
		for _, tp := range s.For(sender) {
			out = append(out, tp.ID)
		}
		return out
	}
	if got := ids("GIFTS@school.example.org"); !reflect.DeepEqual(got, []string{"gift-receipt"}) {
		t.Errorf("address: %v", got)
	}
	if got := ids("billing@vendor.example.com"); !reflect.DeepEqual(got, []string{"invoice"}) {
		t.Errorf("domain: %v", got)
	}
	// A domain matches that domain only, not one that merely ends with it.
	for _, sender := range []string{"x@notvendor.example.com", "x@mail.vendor.example.com", "gifts@other.example.org", ""} {
		if got := ids(sender); got != nil {
			t.Errorf("%q: %v, want none", sender, got)
		}
	}
}

func TestMatchExtractsFieldsFromBodyAndRendersTheDestination(t *testing.T) {
	s := mustParse(t, giftJSON)
	tp := s.For("gifts@school.example.org")[0]
	m := tp.Match(giftMessage(), pacific)
	if !m.OK {
		t.Fatalf("no match: %s", m.Why)
	}
	want := map[string]string{"date": "2025-12-24", "receipt": "10011680"}
	if !reflect.DeepEqual(m.Fields, want) {
		t.Errorf("fields %v, want %v", m.Fields, want)
	}
	folder, name, err := tp.Destination(m.Fields)
	if err != nil {
		t.Fatal(err)
	}
	// The year comes from the document's date (the gift), not the email's.
	if folder != "Archive/Records/2025" || name != "2025-12-24_School_Gift Receipt_10011680" {
		t.Errorf("destination %q %q", folder, name)
	}
}

func TestMatchTakesFieldsFromSubjectAndLocalReceivedDate(t *testing.T) {
	s := mustParse(t, giftJSON)
	tp := s.For("billing@vendor.example.com")[0]
	// 05:54 UTC on the 24th is the evening of the 23rd in California.
	m := tp.Match(stage.Message{Sender: "billing@vendor.example.com", Subject: "Vendor: Invoice INV2623", Received: "2026-03-24T05:54:31Z", Body: "Attached."}, pacific)
	if !m.OK {
		t.Fatalf("no match: %s", m.Why)
	}
	if m.Fields["date"] != "2026-03-23" || m.Fields["invoice"] != "INV2623" {
		t.Errorf("fields %v", m.Fields)
	}
	folder, name, err := tp.Destination(m.Fields)
	if err != nil || folder != "Archive/Records/2026/Business" || name != "2026-03-23_Vendor_Invoice_INV2623" {
		t.Errorf("destination %q %q %v", folder, name, err)
	}
}

func TestMatchFailsWithAReason(t *testing.T) {
	s := mustParse(t, giftJSON)
	tp := s.For("gifts@school.example.org")[0]
	cases := map[string]struct {
		edit func(*stage.Message)
		why  string
	}{
		"subject":        {func(m *stage.Message) { m.Subject = "Your annual report" }, "subject"},
		"anchor":         {func(m *stage.Message) { m.Body = strings.Replace(m.Body, "tax purposes", "your records", 1) }, "anchor"},
		"missing field":  {func(m *stage.Message) { m.Body = strings.Replace(m.Body, "Receipt number", "Ref", 1) }, "receipt"},
		"bad date":       {func(m *stage.Message) { m.Body = strings.Replace(m.Body, "12/24/2025", "soon", 1) }, "date"},
		"date too early": {func(m *stage.Message) { m.Body = strings.Replace(m.Body, "12/24/2025", "12/24/2019", 1) }, "date"},
		"date too late":  {func(m *stage.Message) { m.Body = strings.Replace(m.Body, "12/24/2025", "01/09/2026", 1) }, "date"},
		"unsafe text":    {func(m *stage.Message) { m.Body = strings.Replace(m.Body, "10011680", "1001/680", 1) }, "receipt"},
	}
	for name, c := range cases {
		msg := giftMessage()
		c.edit(&msg)
		// "unsafe text" needs a pattern that admits the slash.
		tpl := tp
		if name == "unsafe text" {
			tpl = mustParse(t, strings.Replace(giftJSON, `Receipt number: (\\d+)`, `Receipt number: (\\S+)`, 1)).For("gifts@school.example.org")[0]
		}
		m := tpl.Match(msg, pacific)
		if m.OK || !strings.Contains(m.Why, c.why) {
			t.Errorf("%s: %+v, want a failure naming %q", name, m, c.why)
		}
	}
}

func TestDatesInCommonFormats(t *testing.T) {
	for text, want := range map[string]string{
		"2025-12-24":                   "2025-12-24",
		"12/24/2025":                   "2025-12-24",
		"1/5/2026":                     "2026-01-05",
		"December 24, 2025":            "2025-12-24",
		"Dec 24, 2025":                 "2025-12-24",
		"Dec. 24, 2025":                "2025-12-24",
		"24 December 2025":             "2025-12-24",
		"24 Dec 2025":                  "2025-12-24",
		"Wednesday, December 24, 2025": "2025-12-24",
		"Wed, Dec 24, 2025":            "2025-12-24",
		"Wed., Dec. 24, 2025":          "2025-12-24",
	} {
		data := strings.Replace(giftJSON, `Gift date: (\\S+)`, `Gift date: (.+)`, 1)
		tp := mustParse(t, data).For("gifts@school.example.org")[0]
		msg := giftMessage()
		msg.Body = strings.Replace(msg.Body, "12/24/2025", text, 1)
		m := tp.Match(msg, pacific)
		if !m.OK || m.Fields["date"] != want {
			t.Errorf("%q: %+v, want %s", text, m, want)
		}
	}
}

func TestATemplateWithoutAFileHasNoDestination(t *testing.T) {
	tp := mustParse(t, giftJSON).For("rides@example.net")[0]
	if tp.Files() {
		t.Error("ride template should not file")
	}
	m := tp.Match(stage.Message{Sender: "rides@example.net", Subject: "Your ride with Sam", Received: "2026-01-05T17:00:00Z"}, pacific)
	if !m.OK {
		t.Fatalf("no match: %s", m.Why)
	}
	if _, _, err := tp.Destination(m.Fields); err == nil {
		t.Error("want an error for a template that is not filed")
	}
}

func TestParseRejectsBadTemplates(t *testing.T) {
	base := `{"id": "a", "senders": ["x@example.com"], "describe": "d", "subject": "s"`
	cases := map[string]string{
		"no templates":       `{"templates": []}`,
		"unknown key":        `{"templates": [` + base + `, "anchor": ["typo"]}]}`,
		"no id":              `{"templates": [{"senders": ["x@example.com"], "describe": "d", "subject": "s"}]}`,
		"duplicate id":       `{"templates": [` + base + `}, ` + base + `}]}`,
		"no senders":         `{"templates": [{"id": "a", "describe": "d", "subject": "s"}]}`,
		"bad sender":         `{"templates": [{"id": "a", "senders": ["example.com"], "describe": "d", "subject": "s"}]}`,
		"no description":     `{"templates": [{"id": "a", "senders": ["x@example.com"], "subject": "s"}]}`,
		"no subject":         `{"templates": [{"id": "a", "senders": ["x@example.com"], "describe": "d"}]}`,
		"bad subject":        `{"templates": [` + base + `[}]}`,
		"no group":           `{"templates": [` + base + `, "fields": {"x": {"pattern": "\\d+"}}}]}`,
		"two groups":         `{"templates": [` + base + `, "fields": {"x": {"pattern": "(\\d)(\\d)"}}}]}`,
		"no pattern":         `{"templates": [` + base + `, "fields": {"x": {"from": "body"}}}]}`,
		"bad from":           `{"templates": [` + base + `, "fields": {"x": {"from": "header", "pattern": "(x)"}}}]}`,
		"bad type":           `{"templates": [` + base + `, "fields": {"x": {"pattern": "(x)", "type": "money"}}}]}`,
		"received not date":  `{"templates": [` + base + `, "fields": {"date": {"from": "received", "pattern": "(x)"}}}]}`,
		"blank anchor":       `{"templates": [` + base + `, "anchors": [" "]}]}`,
		"filed without date": `{"templates": [` + base + `, "fields": {"x": {"pattern": "(x)"}}, "file": {"folder": "A", "name": "{x}"}}]}`,
		"unknown field":      `{"templates": [` + base + `, "fields": {"date": {"from": "received"}}, "file": {"folder": "A", "name": "{date}_{nope}"}}]}`,
		"year of text":       `{"templates": [` + base + `, "fields": {"date": {"from": "received"}, "x": {"pattern": "(x)"}}, "file": {"folder": "{x.year}", "name": "{date}"}}]}`,
		"name not dated":     `{"templates": [` + base + `, "fields": {"date": {"from": "received"}}, "file": {"folder": "A", "name": "Receipt_{date}"}}]}`,
		"folder escapes":     `{"templates": [` + base + `, "fields": {"date": {"from": "received"}}, "file": {"folder": "../A", "name": "{date}"}}]}`,
		"absolute folder":    `{"templates": [` + base + `, "fields": {"date": {"from": "received"}}, "file": {"folder": "/A", "name": "{date}"}}]}`,
		"slash in name":      `{"templates": [` + base + `, "fields": {"date": {"from": "received"}}, "file": {"folder": "A", "name": "{date}/x"}}]}`,
		"stray brace":        `{"templates": [` + base + `, "fields": {"date": {"from": "received"}}, "file": {"folder": "A", "name": "{date}_{"}}]}`,
	}
	for name, data := range cases {
		if _, err := template.Parse([]byte(data)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}

func TestLoadNamesTheFileInErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "templates.json")
	if _, err := template.Load(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("missing file: %v", err)
	}
	if err := os.WriteFile(path, []byte(giftJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := template.Load(path)
	if err != nil || len(s.Templates) != 3 {
		t.Fatalf("load: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"templates": [{}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := template.Load(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("bad file: %v", err)
	}
}

func TestCommonLinesAreThoseInEveryExample(t *testing.T) {
	texts := []string{
		"Dear customer,\nThank you for your purchase A1.\nAttached you will find our invoice.\nBest regards,\nVendor Inc.",
		"Dear customer,\nThank you for your purchase B2.\n\nAttached  you will find our invoice.\nBest regards,\nVendor Inc.\nP.S. sale",
		"Dear customer,\nAttached you will find our invoice.\nVendor Inc.\nBest regards,\nVendor Inc.",
	}
	got := template.CommonLines(texts)
	// In first-example order, once each, whitespace collapsed; lines with
	// varying details (the purchase number) drop out.
	want := []string{"Dear customer,", "Attached you will find our invoice.", "Best regards,", "Vendor Inc."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := template.CommonLines(nil); got != nil {
		t.Errorf("no examples: %q", got)
	}
}

func TestGetFindsATemplateByID(t *testing.T) {
	s := mustParse(t, giftJSON)
	if tp := s.Get("invoice"); tp == nil || tp.ID != "invoice" {
		t.Fatalf("got %+v", tp)
	}
	if s.Get("nope") != nil {
		t.Fatal("found an unknown id")
	}
}

func TestTheShippedExampleIsValidAndInvented(t *testing.T) {
	s, err := template.Load("../../examples/templates.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range s.Templates {
		for _, sender := range tp.Senders {
			if !strings.Contains(sender, "example.") {
				t.Errorf("template %s names a non-example sender %q", tp.ID, sender)
			}
		}
	}
}

// A field's value may not steer a destination out of the filing root, even
// one the folder pattern places first.
func TestDestinationStaysInsideTheRoot(t *testing.T) {
	data := strings.Replace(giftJSON, `"folder": "Archive/Records/{date.year}"`, `"folder": "{receipt}/Taxes"`, 1)
	data = strings.Replace(data, `Receipt number: (\\d+)`, `Receipt number: (\\S+)`, 1)
	tp := mustParse(t, data).For("gifts@school.example.org")[0]
	for _, v := range []string{"..", ".", "..."} {
		msg := giftMessage()
		msg.Body = strings.Replace(msg.Body, "10011680", v, 1)
		if m := tp.Match(msg, pacific); m.OK {
			t.Errorf("%q matched: %+v", v, m)
		}
	}
	if _, _, err := tp.Destination(map[string]string{"date": "2025-12-24", "receipt": ".."}); err == nil {
		t.Error("rendered a folder outside the root")
	}
}
