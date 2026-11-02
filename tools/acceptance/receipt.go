package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"
)

type technical struct {
	Archive      bool `json:"archive_hash_verified"`
	Help         bool `json:"installed_help"`
	Examples     bool `json:"installed_examples"`
	Graph        bool `json:"graph_config"`
	Yahoo        bool `json:"yahoo_config"`
	Capabilities bool `json:"yahoo_read_only_capabilities"`
	Cleanup      bool `json:"owned_cleanup"`
}
type checks struct {
	Consent     string `json:"consent"`
	Stage1      string `json:"stage1"`
	Initial     string `json:"initial_sync"`
	Repeat      string `json:"repeated_sync"`
	Incremental string `json:"incremental_change"`
	Body        string `json:"complete_readable_body"`
	MIME        string `json:"full_mime_transport"`
	Flags       string `json:"read_flags_unchanged"`
	Isolation   string `json:"account_isolation"`
	Apply       string `json:"execute_apply"`
	Rules       string `json:"execute_rules"`
	Save        string `json:"execute_save"`
	Processing  string `json:"processing_disabled"`
	Recovery    string `json:"recovery_reviewed"`
}
type aggregates struct {
	Initial     uint64 `json:"initial"`
	Repeat      uint64 `json:"repeat"`
	Incremental uint64 `json:"incremental"`
}
type receipt struct {
	Version   int        `json:"version"`
	Candidate string     `json:"candidate_commit"`
	Provider  string     `json:"provider"`
	Platform  string     `json:"platform"`
	Consent   string     `json:"consent_reference"`
	Checks    checks     `json:"checks"`
	Counts    aggregates `json:"aggregate_counts"`
	Technical technical  `json:"stage1_technical"`
}
type publicSummary struct {
	Version   int       `json:"version"`
	Candidate string    `json:"candidate_commit"`
	Provider  string    `json:"provider"`
	Platform  string    `json:"declared_stage1_platform"`
	Technical technical `json:"stage1"`
	Pilot     string    `json:"pilot"`
}
type consent struct {
	Version        int    `json:"version"`
	Reference      string `json:"reference"`
	OwnerConfirmed bool   `json:"owner_confirmed"`
	ReadOnlySync   bool   `json:"read_only_sync"`
	Content        bool   `json:"content_reads"`
	LocalIsolation bool   `json:"local_isolation_check"`
	DeniedExecute  bool   `json:"denied_execute_checks"`
	Date           string `json:"date"`
}

func pendingChecks() checks {
	return checks{"pending", "pending", "pending", "pending", "pending", "pending", "pending", "pending", "pending", "pending", "pending", "pending", "pending", "pending"}
}
func strictJSON(raw []byte, out any) error {
	if len(raw) > 65536 || len(raw) == 0 {
		return errInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if checkTokens(d) != nil {
		return errInvalid
	}
	if _, e := d.Token(); e != io.EOF {
		return errInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var shape any
	if d.Decode(&shape) != nil || !exactShape(shape, reflect.TypeOf(out).Elem()) {
		return errInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return errInvalid
	}
	return nil
}
func checkTokens(d *json.Decoder) error { return checkDepth(d, 0) }
func checkDepth(d *json.Decoder, depth int) error {
	if depth > 8 {
		return errInvalid
	}
	t, e := d.Token()
	if e != nil {
		return errInvalid
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			s, ok := k.(string)
			if e != nil || !ok || seen[s] {
				return errInvalid
			}
			seen[s] = true
			if checkDepth(d, depth+1) != nil {
				return errInvalid
			}
		}
	case '[':
		for d.More() {
			if checkDepth(d, depth+1) != nil {
				return errInvalid
			}
		}
	default:
		return errInvalid
	}
	_, e = d.Token()
	return e
}
func validateReceipt(r receipt, candidate string) error {
	if r.Version != 1 || !validHex(candidate, 40) || r.Candidate != candidate || r.Provider != "yahoo" || (r.Platform != "macos-15" && r.Platform != "macos") || r.Consent != "" && !validHex(r.Consent, 32) {
		return errInvalid
	}
	raw, _ := json.Marshal(r.Checks)
	var values map[string]string
	json.Unmarshal(raw, &values)
	for _, v := range values {
		if v != "pending" && v != "pass" && v != "fail" {
			return errInvalid
		}
	}
	return nil
}
func receiptCommand(verb, root, candidate string, approved bool) error {
	if !validHex(candidate, 40) {
		return errInvalid
	}
	p, e := openPrivate(root)
	if e != nil {
		return errInvalid
	}
	defer p.close()
	switch verb {
	case "receipt-init":
		r := receipt{Version: 1, Candidate: candidate, Provider: "yahoo", Platform: "macos-15", Checks: pendingChecks()}
		b, _ := json.MarshalIndent(r, "", "  ")
		if p.write("receipt.json", append(b, '\n')) != nil {
			return errInvalid
		}
		c, _ := json.MarshalIndent(consent{Version: 1}, "", "  ")
		if p.write("consent.json", append(c, '\n')) != nil {
			// A concurrent editor may have replaced or changed the receipt. Never
			// roll back a named artifact whose continued ownership is uncertain.
			return errors.Join(errInvalid, errors.New("acceptance: initialization incomplete; retain artifacts for private review"))
		}
		return nil
	case "receipt-validate", "summary-proposal":
		b, e := p.read("receipt.json")
		if e != nil {
			return errInvalid
		}
		var r receipt
		if strictJSON(b, &r) != nil || validateReceipt(r, candidate) != nil {
			return errInvalid
		}
		consentBytes, x := p.read("consent.json")
		var c consent
		if x != nil || strictJSON(consentBytes, &c) != nil || validateConsent(c) != nil {
			return errInvalid
		}
		if verb == "receipt-validate" {
			return nil
		}
		pilot := "pending"
		if approved {
			raw, _ := json.Marshal(r.Checks)
			var v map[string]string
			json.Unmarshal(raw, &v)
			for _, s := range v {
				if s != "pass" {
					return errors.New("acceptance: private verification incomplete")
				}
			}
			if !allTechnical(r.Technical) {
				return errors.New("acceptance: private verification incomplete")
			}
			if c.Version != 1 || !validHex(c.Reference, 32) || c.Reference != r.Consent || !c.OwnerConfirmed || !c.ReadOnlySync || !c.Content || !c.LocalIsolation || !c.DeniedExecute || len(c.Date) != 10 {
				return errInvalid
			}
			if _, e := time.Parse("2006-01-02", c.Date); e != nil {
				return errInvalid
			}
			pilot = "privately_verified"
		}
		out := publicSummary{1, candidate, "yahoo", "macos-15", r.Technical, pilot}
		b, _ = json.MarshalIndent(out, "", "  ")
		return p.write("summary-proposal.json", append(b, '\n'))
	}
	return errInvalid
}

func exactShape(value any, t reflect.Type) bool {
	if value == nil {
		return false
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := value.(map[string]any)
		if !ok || len(m) != t.NumField() {
			return false
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			v, ok := m[f.Tag.Get("json")]
			if !ok || !exactShape(v, f.Type) {
				return false
			}
		}
		return true
	case reflect.Slice:
		a, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range a {
			if !exactShape(v, t.Elem()) {
				return false
			}
		}
		return true
	case reflect.String:
		_, ok := value.(string)
		return ok
	case reflect.Bool:
		_, ok := value.(bool)
		return ok
	case reflect.Int, reflect.Uint64:
		_, ok := value.(json.Number)
		return ok
	}
	return false
}

func validateConsent(c consent) error {
	if c.Version != 1 || c.Reference != "" && !validHex(c.Reference, 32) {
		return errInvalid
	}
	if c.Date != "" {
		if _, e := time.Parse("2006-01-02", c.Date); e != nil {
			return errInvalid
		}
	}
	if c.OwnerConfirmed && (c.Reference == "" || c.Date == "") {
		return errInvalid
	}
	return nil
}
func allTechnical(t technical) bool {
	return t.Archive && t.Help && t.Examples && t.Graph && t.Yahoo && t.Capabilities && t.Cleanup
}
