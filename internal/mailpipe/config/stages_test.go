package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
)

func writeStages(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stages.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadStagesAppliesDefaults(t *testing.T) {
	p := writeStages(t, `{
	  "self_addresses": ["alex@example.com"], "self_domains": ["example.com"], "honeypots": ["trap@example.com"],
	  "openrouter": {"keychain_service": "mailpipe-openrouter", "keychain_account": "default"},
	  "stages": [
	    {"name": "known", "type": "allowlist"},
	    {"name": "honeypot", "type": "honeypot"},
	    {"name": "political", "type": "jev", "question": {"key": "political_appeal", "instructions": "Is this a political appeal?",
	      "criteria_true": "asks to act", "criteria_false": "anything else"}},
	    {"name": "newsletter", "type": "exec", "command": ["/bin/echo", "{}"]}
	  ]}`)
	c, err := config.LoadStages(p)
	if err != nil {
		t.Fatal(err)
	}
	j, ok := c.Stage("political")
	if !ok || j.Model != config.DefaultJevModel || j.Threshold != 0.5 || len(j.Questions) != 1 || j.Questions[0].Key != "political_appeal" {
		t.Fatalf("jev stage %+v", j)
	}
	e, _ := c.Stage("newsletter")
	if e.TimeoutSeconds != 30 {
		t.Fatalf("exec timeout %d, want the default 30", e.TimeoutSeconds)
	}
	if _, ok := c.Stage("nope"); ok {
		t.Fatal("unknown stage found")
	}
}

func TestLoadStagesRejectsBadDefinitions(t *testing.T) {
	cases := map[string]string{
		"duplicate name":       `{"stages":[{"name":"a","type":"allowlist"},{"name":"a","type":"allowlist"}]}`,
		"unknown type":         `{"stages":[{"name":"a","type":"magic"}]}`,
		"exec no command":      `{"stages":[{"name":"a","type":"exec"}]}`,
		"jev no question":      `{"stages":[{"name":"a","type":"jev"}]}`,
		"jev bad threshold":    `{"stages":[{"name":"a","type":"jev","threshold":1.5,"question":{"key":"k","instructions":"i","criteria_true":"t","criteria_false":"f"}}]}`,
		"bad name":             `{"stages":[{"name":"two words","type":"allowlist"}]}`,
		"jev both forms":       `{"stages":[{"name":"a","type":"jev","question":{"key":"k","instructions":"i","criteria_true":"t","criteria_false":"f"},"questions":[{"key":"j","instructions":"i","criteria_true":"t","criteria_false":"f"}]}]}`,
		"jev repeated key":     `{"stages":[{"name":"a","type":"jev","questions":[{"key":"k","instructions":"i","criteria_true":"t","criteria_false":"f"},{"key":"k","instructions":"i","criteria_true":"t","criteria_false":"f"}]}]}`,
		"jev both, list empty": `{"stages":[{"name":"a","type":"jev","question":{"key":"k","instructions":"i","criteria_true":"t","criteria_false":"f"},"questions":[]}]}`,
		"jev key spacing":      `{"stages":[{"name":"a","type":"jev","questions":[{"key":"k","instructions":"i","criteria_true":"t","criteria_false":"f"},{"key":" k ","instructions":"i","criteria_true":"t","criteria_false":"f"}]}]}`,
		"jev blank in list":    `{"stages":[{"name":"a","type":"jev","questions":[{"key":"k","instructions":"i","criteria_true":"t","criteria_false":"f"},{"key":" ","instructions":"i","criteria_true":"t","criteria_false":"f"}]}]}`,
		"unknown field":        `{"stages":[{"name":"a","type":"allowlist","comand":["x"]}]}`,
	}
	for name, body := range cases {
		if _, err := config.LoadStages(writeStages(t, body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestMissingStagesSaysWhatToCreate(t *testing.T) {
	_, err := config.LoadStages(filepath.Join(t.TempDir(), "stages.json"))
	if !errors.Is(err, config.ErrNotConfigured) || !strings.Contains(err.Error(), "examples/stages.example.json") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheShippedExampleIsValidAndInvented(t *testing.T) {
	c, err := config.LoadStages("../../../examples/stages.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range append(append(c.SelfAddresses, c.Honeypots...), c.SelfDomains...) {
		if !strings.Contains(a, "example.") {
			t.Errorf("example config holds a non-example value %q", a)
		}
	}
}

func TestWhitespaceOnlyQuestionFieldsAreRejected(t *testing.T) {
	for _, field := range []string{"key", "instructions", "criteria_true", "criteria_false"} {
		q := map[string]string{"key": "k", "instructions": "i", "criteria_true": "t", "criteria_false": "f"}
		q[field] = "   "
		body := `{"stages":[{"name":"a","type":"jev","question":{"key":"` + q["key"] + `","instructions":"` + q["instructions"] +
			`","criteria_true":"` + q["criteria_true"] + `","criteria_false":"` + q["criteria_false"] + `"}}]}`
		if _, err := config.LoadStages(writeStages(t, body)); err == nil {
			t.Errorf("whitespace-only %s accepted", field)
		}
	}
}

func TestPolicyAndSenderCacheLoad(t *testing.T) {
	p := writeStages(t, `{
	  "policy": {"keep_senders": ["news@example.org"], "rules": [{"stage": "political", "label": "political_appeal", "action": "delete"}]},
	  "stages": [
	    {"name": "political", "type": "jev", "question": {"key": "political_appeal", "instructions": "i", "criteria_true": "t", "criteria_false": "f"}},
	    {"name": "political-cached", "type": "sender_cache", "source": "political", "label": "political_appeal"}
	  ]}`)
	c, err := config.LoadStages(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Policy.Rules) != 1 || c.Policy.KeepSenders[0] != "news@example.org" {
		t.Fatalf("policy %+v", c.Policy)
	}
	sc, _ := c.Stage("political-cached")
	if sc.MinVerdicts != 3 || sc.Source != "political" {
		t.Fatalf("sender_cache %+v", sc)
	}
}

func TestBadPolicyOrSenderCacheIsRejected(t *testing.T) {
	cases := map[string]string{
		"bad rule action":      `{"policy":{"rules":[{"stage":"s","label":"l","action":"explode"}]},"stages":[]}`,
		"cache without source": `{"stages":[{"name":"c","type":"sender_cache","label":"l"}]}`,
		"cache of itself":      `{"stages":[{"name":"c","type":"sender_cache","source":"c","label":"l"}]}`,
		"cache without label":  `{"stages":[{"name":"a","type":"allowlist"},{"name":"c","type":"sender_cache","source":"a"}]}`,
	}
	for name, body := range cases {
		if _, err := config.LoadStages(writeStages(t, body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSenderCacheSourceMustBeAStageName(t *testing.T) {
	if _, err := config.LoadStages(writeStages(t, `{"stages":[{"name":"c","type":"sender_cache","source":"Legacy Political","label":"l"}]}`)); err == nil {
		t.Fatal("malformed source accepted")
	}
}

func TestAJevStageCanAskSeveralQuestions(t *testing.T) {
	p := writeStages(t, `{"stages":[{"name":"category","type":"jev","questions":[
	  {"key":"promotion","instructions":"i","criteria_true":"t","criteria_false":"f"},
	  {"key":"newsletter","instructions":"i","criteria_true":"t","criteria_false":"f"}]}]}`)
	c, err := config.LoadStages(p)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := c.Stage("category"); len(d.Questions) != 2 || d.Questions[1].Key != "newsletter" {
		t.Fatalf("stage %+v", d)
	}
}

func TestATemplateStageGetsDefaults(t *testing.T) {
	t.Setenv("MAILPIPE_TEMPLATES", "")
	c, err := config.LoadStages(writeStages(t, `{"stages":[{"name":"template","type":"template"},
	  {"name":"other","type":"template","templates":"/x/t.json","threshold":0.9,"model":"m"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	d, _ := c.Stage("template")
	if d.Templates != filepath.Join(home, "mail", "templates.json") || d.Threshold != 0.7 || d.Model != config.DefaultJevModel {
		t.Fatalf("defaults %+v", d)
	}
	o, _ := c.Stage("other")
	if o.Templates != "/x/t.json" || o.Threshold != 0.9 || o.Model != "m" {
		t.Fatalf("explicit %+v", o)
	}
	t.Setenv("MAILPIPE_TEMPLATES", "/elsewhere/t.json")
	if got := config.DefaultTemplatesPath(); got != "/elsewhere/t.json" {
		t.Fatalf("DefaultTemplatesPath = %q", got)
	}
}

func TestBadTemplateStagesAreRejected(t *testing.T) {
	for name, body := range map[string]string{
		"threshold": `{"stages":[{"name":"a","type":"template","threshold":1.5}]}`,
		"question":  `{"stages":[{"name":"a","type":"template","question":{"key":"k","instructions":"i","criteria_true":"t","criteria_false":"f"}}]}`,
	} {
		if _, err := config.LoadStages(writeStages(t, body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAutomatedSendersAreReadAndChecked(t *testing.T) {
	c, err := config.LoadStages(writeStages(t, `{"automated_senders":["Bot@Example.com"],"stages":[]}`))
	if err != nil || len(c.AutomatedSenders) != 1 {
		t.Fatalf("config %+v err %v", c, err)
	}
	for _, bad := range []string{"example.com", "bot @example.com", "a@b@example.com", "@example.com", "bot@"} {
		if _, err := config.LoadStages(writeStages(t, `{"automated_senders":["`+bad+`"],"stages":[]}`)); err == nil {
			t.Errorf("accepted automated sender %q", bad)
		}
	}
}

func TestHoneypotIsOptionalAndHookSettingsAreExplicit(t *testing.T) {
	c, err := config.LoadStages(writeStages(t, `{"stages":[{"name":"custom","type":"hook","command":["/private/classifier"],"senders":["alerts@service.example.net"],"needs_source":true}]}`))
	if err != nil || len(c.Honeypots) != 0 {
		t.Fatalf("config %+v err %v", c, err)
	}
	h, ok := c.Stage("custom")
	if !ok || !h.NeedsSource || h.TimeoutSeconds != 30 || len(h.Senders) != 1 {
		t.Fatalf("hook %+v", h)
	}
	for _, body := range []string{
		`{"stages":[{"name":"custom","type":"hook","command":["relative/classifier"]}]}`,
		`{"stages":[{"name":"custom","type":"hook","command":["/classifier"],"senders":["@example.org"]}]}`,
		`{"stages":[{"name":"custom","type":"exec","command":["/classifier"],"needs_source":true}]}`,
	} {
		if _, err := config.LoadStages(writeStages(t, body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	example, err := config.LoadStages("../../../examples/stages.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(example.Honeypots) != 0 {
		t.Fatal("shipped example enables optional recipient filtering")
	}
	for _, d := range example.Stages {
		if d.Type == "honeypot" {
			t.Fatal("shipped example enables optional stage")
		}
	}
}

func TestModelInputAndQuestionThresholdConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		invalid      bool
		limit        int
	}{
		{"default", "", false, 1048576},
		{"override", `,"max_input_bytes":262144`, false, 262144},
		{"negative limit", `,"max_input_bytes":-1`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"stages":[{"name":"schedule","type":"jev"` + tc.fields + `,"questions":[{"key":"past","instructions":"Today is {today}; are all dates past?","criteria_true":"all dated items expired","criteria_false":"any future or unclear date","threshold":0.9}]}]}`
			c, err := config.LoadStages(writeStages(t, body))
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid definition accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			d, _ := c.Stage("schedule")
			if d.MaxInputBytes != tc.limit || d.Questions[0].Threshold != .9 {
				t.Fatalf("definition %+v", d)
			}
		})
	}
	for _, threshold := range []string{"-0.1", "1.01"} {
		body := `{"stages":[{"name":"schedule","type":"jev","question":{"key":"past","instructions":"i","criteria_true":"t","criteria_false":"f","threshold":` + threshold + `}}]}`
		if _, err := config.LoadStages(writeStages(t, body)); err == nil {
			t.Fatal("invalid question threshold accepted")
		}
	}
}
