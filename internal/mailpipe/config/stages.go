package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/mrothroc/mailpipe/pkg/policy"
	"github.com/mrothroc/mailpipe/pkg/stage"
)

// DefaultJevModel is the decision model used when a jev stage names none.
const DefaultJevModel = "typesafe/jev-1.14"

const (
	stagesEnv          = "MAILPIPE_STAGES"
	defaultExecTimeout = 30
	defaultThreshold   = 0.5
	// A template match files a document by itself, so Jev must be surer of
	// it than of a label.
	defaultTemplateThreshold = 0.7
	templatesEnv             = "MAILPIPE_TEMPLATES"
	defaultMinVerdicts       = 3
)

var stageName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Stages is the private stage configuration (~/mail/stages.json): it names the
// owner's addresses, honeypots and questions, so it never lives in a repository.
type Stages struct {
	SelfAddresses []string `json:"self_addresses"`
	SelfDomains   []string `json:"self_domains"`
	Honeypots     []string `json:"honeypots,omitempty"`
	// AutomatedSenders are agents or scripts, even on the owner's domains or
	// written to by the owner, that the allowlist never keeps.
	AutomatedSenders []string      `json:"automated_senders,omitempty"`
	OpenRouter       OpenRouter    `json:"openrouter"`
	Policy           policy.Policy `json:"policy"`
	Stages           []StageDef    `json:"stages"`
}

// OpenRouter names the Keychain item holding the OpenRouter API key.
type OpenRouter struct {
	KeychainService string `json:"keychain_service"`
	KeychainAccount string `json:"keychain_account"`
}

// StageDef defines one stage.
type StageDef struct {
	Name           string           `json:"name"`
	Type           string           `json:"type"` // allowlist, honeypot, jev, exec, hook, sender_cache, template
	Command        []string         `json:"command,omitempty"`
	TimeoutSeconds int              `json:"timeout_seconds,omitempty"`
	NeedsBody      bool             `json:"needs_body,omitempty"`
	NeedsSource    bool             `json:"needs_source,omitempty"` // hook: temporary MIME source file
	Senders        []string         `json:"senders,omitempty"`      // hook: exact addresses, empty matches all
	Question       *stage.Question  `json:"question,omitempty"`     // one question...
	Questions      []stage.Question `json:"questions,omitempty"`    // ...or several, asked in one call
	Threshold      float64          `json:"threshold,omitempty"`
	MaxInputBytes  int              `json:"max_input_bytes,omitempty"`
	Model          string           `json:"model,omitempty"`
	URL            string           `json:"url,omitempty"`
	Source         string           `json:"source,omitempty"`       // sender_cache: the stage whose verdicts are reused
	Label          string           `json:"label,omitempty"`        // sender_cache: the source label that is reused
	MinVerdicts    int              `json:"min_verdicts,omitempty"` // sender_cache: history needed, default 3
	Templates      string           `json:"templates,omitempty"`    // template: the template file, default DefaultTemplatesPath
}

// DefaultTemplatesPath is $MAILPIPE_TEMPLATES, else ~/mail/templates.json.
func DefaultTemplatesPath() string {
	if p := strings.TrimSpace(os.Getenv(templatesEnv)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "templates.json"
	}
	return filepath.Join(home, "mail", "templates.json")
}

// DefaultStagesPath is $MAILPIPE_STAGES, else ~/mail/stages.json.
func DefaultStagesPath() string {
	if p := strings.TrimSpace(os.Getenv(stagesEnv)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "stages.json"
	}
	return filepath.Join(home, "mail", "stages.json")
}

// LoadStages reads and validates the stage configuration at path.
func LoadStages(path string) (Stages, error) {
	var c Stages
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, fmt.Errorf("%w: %s does not exist; create it (mode 0600) from examples/stages.example.json", ErrNotConfigured, path)
	}
	if err != nil {
		return c, fmt.Errorf("config: read %s: %v", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("config: %s: %v", path, err)
	}
	for _, a := range c.AutomatedSenders {
		a = strings.TrimSpace(a)
		if local, domain, ok := strings.Cut(a, "@"); !ok || local == "" || domain == "" || strings.Contains(domain, "@") || strings.ContainsFunc(a, unicode.IsSpace) {
			return c, fmt.Errorf("config: %s: automated sender %q must be an address", path, a)
		}
	}
	seen := map[string]bool{}
	for i := range c.Stages {
		d := &c.Stages[i]
		if !stageName.MatchString(d.Name) {
			return c, fmt.Errorf("config: %s: stage name %q must be lower-case letters, digits, '-' or '_'", path, d.Name)
		}
		if seen[d.Name] {
			return c, fmt.Errorf("config: %s: stage %q is defined twice", path, d.Name)
		}
		seen[d.Name] = true
		if err := d.normalize(); err != nil {
			return c, fmt.Errorf("config: %s: stage %q: %v", path, d.Name, err)
		}
	}
	for _, d := range c.Stages {
		// The source may be any stage name, including one whose verdicts were
		// imported rather than produced by a configured stage.
		if d.Type == "sender_cache" && d.Source == d.Name {
			return c, fmt.Errorf("config: %s: stage %q cannot use its own verdicts as its source", path, d.Name)
		}
	}
	if err := c.Policy.Validate(); err != nil {
		return c, fmt.Errorf("config: %s: %v", path, err)
	}
	return c, nil
}

func (d *StageDef) normalize() error {
	if d.Type != "hook" && (d.NeedsSource || len(d.Senders) > 0) {
		return errors.New("needs_source and senders are only supported by hook stages")
	}
	switch d.Type {
	case "allowlist", "honeypot":
	case "exec", "hook":
		if len(d.Command) == 0 || strings.TrimSpace(d.Command[0]) == "" {
			return fmt.Errorf(`a %s stage needs "command": [program, args...]`, d.Type)
		}
		if d.TimeoutSeconds <= 0 {
			d.TimeoutSeconds = defaultExecTimeout
		}
		if d.Type == "hook" {
			if !filepath.IsAbs(d.Command[0]) {
				return errors.New("a hook command must use an absolute executable path")
			}
			for _, a := range d.Senders {
				local, domain, ok := strings.Cut(strings.TrimSpace(a), "@")
				if !ok || local == "" || domain == "" || strings.Contains(domain, "@") || strings.ContainsFunc(strings.TrimSpace(a), unicode.IsSpace) {
					return errors.New("hook senders must be plain addresses")
				}
			}
		}
	case "jev":
		if d.Question != nil && d.Questions != nil {
			return errors.New(`a jev stage takes "question" or "questions", not both`)
		}
		if d.Question != nil {
			d.Questions, d.Question = []stage.Question{*d.Question}, nil
		}
		if len(d.Questions) == 0 {
			return errors.New(`a jev stage needs "question" (or "questions") with key, instructions, criteria_true and criteria_false`)
		}
		blank := func(s string) bool { return strings.TrimSpace(s) == "" }
		keys := map[string]bool{}
		for i := range d.Questions {
			q := &d.Questions[i]
			q.Key = strings.TrimSpace(q.Key) // the key becomes a label that policy rules match exactly
			if blank(q.Key) || blank(q.Instructions) || blank(q.True) || blank(q.False) {
				return errors.New(`every jev question needs key, instructions, criteria_true and criteria_false`)
			}
			if keys[q.Key] {
				return fmt.Errorf("jev question key %q is repeated", q.Key)
			}
			if q.Threshold < 0 || q.Threshold > 1 {
				return fmt.Errorf("question %q threshold must be in (0, 1], or omitted", q.Key)
			}
			keys[q.Key] = true
		}
		return d.jevDefaults(defaultThreshold)
	case "template":
		if d.Question != nil || d.Questions != nil {
			return errors.New("a template stage asks its own question: which of the sender's templates fits")
		}
		if d.Templates == "" {
			d.Templates = DefaultTemplatesPath()
		}
		return d.jevDefaults(defaultTemplateThreshold)
	case "sender_cache":
		if !stageName.MatchString(d.Source) || strings.TrimSpace(d.Label) == "" {
			return errors.New(`a sender_cache stage needs "source" (a stage name: lower-case letters, digits, '-' or '_') and "label"`)
		}
		if d.MinVerdicts == 0 {
			d.MinVerdicts = defaultMinVerdicts
		}
		if d.MinVerdicts < 1 {
			return errors.New("min_verdicts must be at least 1")
		}
	default:
		return fmt.Errorf("unknown type %q (want allowlist, honeypot, jev, exec, hook, sender_cache or template)", d.Type)
	}
	return nil
}

// jevDefaults fills in and checks the threshold and model of a stage that
// calls Jev.
func (d *StageDef) jevDefaults(threshold float64) error {
	if d.MaxInputBytes < 0 {
		return errors.New("max_input_bytes must be positive, or omitted")
	}
	if d.MaxInputBytes == 0 {
		d.MaxInputBytes = stage.DefaultMaxInputBytes
	}
	if d.Threshold == 0 {
		d.Threshold = threshold
	}
	if d.Threshold <= 0 || d.Threshold > 1 {
		return fmt.Errorf("threshold %v must be in (0, 1]", d.Threshold)
	}
	if d.Model == "" {
		d.Model = DefaultJevModel
	}
	return nil
}

// Stage returns the definition named name.
func (c Stages) Stage(name string) (StageDef, bool) {
	for _, d := range c.Stages {
		if d.Name == name {
			return d, true
		}
	}
	return StageDef{}, false
}
