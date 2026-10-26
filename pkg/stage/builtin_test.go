package stage_test

import (
	"context"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/stage"
)

func TestAllowlistKeepsSelfOwnedDomainsAndContacts(t *testing.T) {
	a := stage.NewAllowlist("known", []string{"Alex@Example.COM"}, []string{"example.com"}, []string{"friend@example.org"}, nil)
	cases := map[string]string{
		"alex@example.com":          "self",
		"other@mail.example.com":    "self",
		"friend@example.org":        "known_contact",
		"FRIEND@example.org":        "known_contact",
		"news@campaign.example.org": "",
		"x@unrelated.example.net":   "",
	}
	for sender, label := range cases {
		m := msg()
		m.Sender = sender
		v, err := a.Evaluate(context.Background(), m)
		if err != nil {
			t.Fatal(err)
		}
		if label == "" {
			if len(v.Labels) != 0 || v.Final || v.Action != "" {
				t.Errorf("%s: verdict %+v, want none", sender, v)
			}
			continue
		}
		if len(v.Labels) != 1 || v.Labels[0] != label || v.Action != stage.Keep || !v.Final || v.Validate() != nil {
			t.Errorf("%s: verdict %+v, want %s/keep/final", sender, v, label)
		}
	}
}

func TestHoneypotFlagsMailToTheHoneypotFromOthers(t *testing.T) {
	self := stage.NewAllowlist("known", []string{"alex@example.com"}, []string{"example.com"}, nil, nil)
	h := stage.NewHoneypot("honeypot", []string{"Trap@Example.COM"}, self)
	m := msg()
	m.Recipients = []string{"someone@example.net", "trap@example.com"}
	v, err := h.Evaluate(context.Background(), m)
	if err != nil || len(v.Labels) != 1 || v.Labels[0] != "honeypot" || v.Scores["honeypot"] != 1 {
		t.Fatalf("verdict %+v err %v", v, err)
	}
	m.Sender = "alex@example.com" // the owner mailing their own honeypot is not a hit
	if v, _ := h.Evaluate(context.Background(), m); len(v.Labels) != 0 {
		t.Fatalf("self mail flagged: %+v", v)
	}
	m = msg() // not addressed to the honeypot
	if v, _ := h.Evaluate(context.Background(), m); len(v.Labels) != 0 {
		t.Fatalf("ordinary mail flagged: %+v", v)
	}
}

// An automated sender on the owner's own domain (an agent, a script) is
// neither the owner nor a contact, so policy rules apply to its mail.
func TestAllowlistNeverKeepsAutomatedSenders(t *testing.T) {
	a := stage.NewAllowlist("known", []string{"alex@example.com"}, []string{"example.com"}, []string{"bot@example.com"}, []string{"Bot@Example.com"})
	for sender, keep := range map[string]bool{"bot@example.com": false, "alex@example.com": true, "other@example.com": true} {
		m := msg()
		m.Sender = sender
		v, err := a.Evaluate(context.Background(), m)
		if err != nil {
			t.Fatal(err)
		}
		if (v.Action == stage.Keep) != keep || a.IsSelf(sender) != keep {
			t.Errorf("%s: verdict %+v self %v, want kept %v", sender, v, a.IsSelf(sender), keep)
		}
	}
}

func TestHoneypotWithoutAddressesProducesNoVerdict(t *testing.T) {
	h := stage.NewHoneypot("optional", nil, nil)
	m := msg()
	m.Recipients = []string{"recipient@example.net"}
	v, err := h.Evaluate(context.Background(), m)
	if err != nil || len(v.Labels) != 0 || v.Action != "" {
		t.Fatalf("disabled optional stage: %+v %v", v, err)
	}
}
