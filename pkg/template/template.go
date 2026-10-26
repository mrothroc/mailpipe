// Package template recognises the kinds of email a sender sends, so a record
// can be filed by rule instead of by hand. A template describes one kind: how
// to recognise it deterministically (its fingerprint), which fields to take
// from it, and, when it is worth filing, the folder and name to file it under.
//
// Templates name real senders and the owner's folders, so they live in user
// configuration, never in this repository.
package template

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/pkg/stage"
)

// Where a field's value comes from.
const (
	FromBody     = "body"
	FromSubject  = "subject"
	FromReceived = "received"
)

// Field types.
const (
	Text = "text"
	Date = "date"
)

// A date field must fall within this window around the email: documents are
// dated before they are sent, rarely long before, and never well after.
const (
	maxDateAge  = 3 * 365 * 24 * time.Hour
	maxDateLead = 24 * time.Hour
)

// maxTextField bounds a text field, which ends up in a file name.
const maxTextField = 80

// Field says how to take one value from a message.
type Field struct {
	From    string `json:"from,omitempty"`    // body (default), subject or received
	Pattern string `json:"pattern,omitempty"` // one capture group; not used with received
	Type    string `json:"type,omitempty"`    // text (default) or date; received is always a date

	re *regexp.Regexp
}

// File is where a filed template's messages go, relative to the filing root.
// "{field}" is a field's value and "{field.year}" a date field's year.
type File struct {
	Folder string `json:"folder"`
	Name   string `json:"name"`
}

// Template is one kind of email from one sender.
type Template struct {
	ID       string           `json:"id"`
	Senders  []string         `json:"senders"`
	Describe string           `json:"describe"`
	Subject  string           `json:"subject"`
	Anchors  []string         `json:"anchors,omitempty"`
	Fields   map[string]Field `json:"fields,omitempty"`
	File     *File            `json:"file,omitempty"`

	subject *regexp.Regexp
	anchors []string // whitespace collapsed
}

// Set is every template.
type Set struct {
	Templates []*Template `json:"templates"`
}

// Load reads and checks a template file.
func Load(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Parse checks templates: every pattern compiles, every placeholder names a
// field, and every destination stays inside the filing root.
func Parse(data []byte) (*Set, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Set
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if len(s.Templates) == 0 {
		return nil, errors.New("no templates")
	}
	seen := map[string]bool{}
	for i, t := range s.Templates {
		if t == nil {
			return nil, fmt.Errorf("template %d is empty", i+1)
		}
		if err := t.check(); err != nil {
			if t.ID != "" {
				return nil, fmt.Errorf("template %s: %w", t.ID, err)
			}
			return nil, fmt.Errorf("template %d: %w", i+1, err)
		}
		if seen[t.ID] {
			return nil, fmt.Errorf("template id %s is used twice", t.ID)
		}
		seen[t.ID] = true
	}
	return &s, nil
}

var (
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	placeholder = regexp.MustCompile(`\{([a-z_][a-z0-9_]*)(\.year)?\}`)
	// unsafeName holds the characters a file name may not contain (';'
	// separates the files recorded in a filing verdict).
	unsafeName = "/\\:;"
)

func (t *Template) check() error {
	if t.ID == none {
		return errors.New(`"none" is reserved: it is Jev's answer when no template fits`)
	}
	if !idPattern.MatchString(t.ID) {
		return fmt.Errorf("id %q must be lowercase letters, digits and dashes", t.ID)
	}
	if len(t.Senders) == 0 {
		return errors.New("no senders")
	}
	for i, s := range t.Senders {
		s = strings.ToLower(strings.TrimSpace(s))
		if !strings.Contains(s, "@") || strings.HasSuffix(s, "@") {
			return fmt.Errorf("sender %q must be an address or @domain", t.Senders[i])
		}
		t.Senders[i] = s
	}
	if strings.TrimSpace(t.Describe) == "" {
		return errors.New("no description (Jev chooses between descriptions)")
	}
	if t.Subject == "" {
		return errors.New("no subject pattern")
	}
	var err error
	if t.subject, err = regexp.Compile(t.Subject); err != nil {
		return fmt.Errorf("subject: %w", err)
	}
	t.anchors = nil
	for _, a := range t.Anchors {
		a = collapse(a)
		if a == "" {
			return errors.New("an anchor is blank")
		}
		t.anchors = append(t.anchors, a)
	}
	for name, f := range t.Fields {
		if err := f.check(); err != nil {
			return fmt.Errorf("field %s: %w", name, err)
		}
		t.Fields[name] = f
	}
	if t.File != nil {
		return t.checkFile()
	}
	return nil
}

func (f *Field) check() error {
	if f.From == "" {
		f.From = FromBody
	}
	switch f.From {
	case FromReceived:
		if f.Pattern != "" || (f.Type != "" && f.Type != Date) {
			return errors.New("the received date takes no pattern and is always a date")
		}
		f.Type = Date
		return nil
	case FromBody, FromSubject:
	default:
		return fmt.Errorf("from %q (want body, subject or received)", f.From)
	}
	if f.Type == "" {
		f.Type = Text
	}
	if f.Type != Text && f.Type != Date {
		return fmt.Errorf("type %q (want text or date)", f.Type)
	}
	if f.Pattern == "" {
		return errors.New("no pattern")
	}
	re, err := regexp.Compile(f.Pattern)
	if err != nil {
		return err
	}
	if re.NumSubexp() != 1 {
		return fmt.Errorf("pattern %q must have exactly one capture group", f.Pattern)
	}
	f.re = re
	return nil
}

func (t *Template) checkFile() error {
	if d, ok := t.Fields["date"]; !ok || d.Type != Date {
		return errors.New("a filed template needs a date field: names start with the document's date")
	}
	if !strings.HasPrefix(t.File.Name, "{date}") {
		return errors.New(`the name must start with "{date}" (see the naming rules)`)
	}
	for _, part := range []string{t.File.Folder, t.File.Name} {
		for _, ph := range placeholder.FindAllStringSubmatch(part, -1) {
			f, ok := t.Fields[ph[1]]
			if !ok {
				return fmt.Errorf("%s names no field", ph[0])
			}
			if ph[2] != "" && f.Type != Date {
				return fmt.Errorf("%s: only a date has a year", ph[0])
			}
		}
		if strings.ContainsAny(placeholder.ReplaceAllString(part, ""), "{}") {
			return fmt.Errorf("%q has a brace that is not a placeholder", part)
		}
	}
	if t.File.Folder == "" || !insideRoot(t.File.Folder) {
		return fmt.Errorf("folder %q must be inside the filing root", t.File.Folder)
	}
	if strings.ContainsAny(t.File.Name, unsafeName) {
		return fmt.Errorf("name %q must be a plain file name (no / \\ : or ;)", t.File.Name)
	}
	return nil
}

// insideRoot reports a relative folder that stays inside the filing root.
func insideRoot(folder string) bool {
	clean := filepath.Clean(folder)
	return !filepath.IsAbs(folder) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// For returns the templates for a sender, in file order.
func (s *Set) For(sender string) []*Template {
	sender = strings.ToLower(strings.TrimSpace(sender))
	_, domain, ok := strings.Cut(sender, "@")
	if !ok {
		return nil
	}
	var out []*Template
	for _, t := range s.Templates {
		for _, want := range t.Senders {
			if want == sender || want == "@"+domain {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// Get returns the template with id, or nil.
func (s *Set) Get(id string) *Template {
	for _, t := range s.Templates {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Files reports whether the template's messages are filed.
func (t *Template) Files() bool { return t.File != nil }

// Match is the fingerprint's result for one template: OK, with the fields'
// values (dates as YYYY-MM-DD), or the first reason it failed.
type Match struct {
	OK     bool
	Why    string
	Fields map[string]string
}

var whitespace = regexp.MustCompile(`\s+`)

func collapse(s string) string { return strings.TrimSpace(whitespace.ReplaceAllString(s, " ")) }

// Match fingerprints m: the subject matches, every anchor appears in the
// readable body, and every field extracts and validates. loc is the zone
// dates are read in.
func (t *Template) Match(m stage.Message, loc *time.Location) Match {
	if !t.subject.MatchString(m.Subject) {
		return Match{Why: fmt.Sprintf("subject does not match %q", t.Subject)}
	}
	text := stage.Readable(m.Body)
	flat := collapse(text)
	for _, a := range t.anchors {
		if !strings.Contains(flat, a) {
			return Match{Why: fmt.Sprintf("anchor missing: %q", a)}
		}
	}
	received, err := time.Parse(time.RFC3339, m.Received)
	if err != nil {
		return Match{Why: fmt.Sprintf("received time %q is not a time", m.Received)}
	}
	fields := make(map[string]string, len(t.Fields))
	for _, name := range slices.Sorted(maps.Keys(t.Fields)) {
		f := t.Fields[name]
		v, err := f.value(m.Subject, text, received, loc)
		if err != nil {
			return Match{Why: fmt.Sprintf("field %s: %v", name, err)}
		}
		fields[name] = v
	}
	return Match{OK: true, Fields: fields}
}

func (f Field) value(subject, text string, received time.Time, loc *time.Location) (string, error) {
	if f.From == FromReceived {
		return received.In(loc).Format(time.DateOnly), nil
	}
	in := text
	if f.From == FromSubject {
		in = subject
	}
	sub := f.re.FindStringSubmatch(in)
	if sub == nil {
		return "", fmt.Errorf("pattern %q not found in the %s", f.Pattern, f.From)
	}
	v := collapse(sub[1])
	if f.Type == Date {
		d, err := parseDate(v, loc)
		if err != nil {
			return "", err
		}
		if d.Before(received.Add(-maxDateAge)) || d.After(received.Add(maxDateLead)) {
			return "", fmt.Errorf("date %s is too far from the email's date %s", d.Format(time.DateOnly), received.In(loc).Format(time.DateOnly))
		}
		return d.Format(time.DateOnly), nil
	}
	if strings.Trim(v, ".") == "" || len(v) > maxTextField || strings.ContainsAny(v, unsafeName) {
		return "", fmt.Errorf("value %q is empty, only dots, longer than %d characters, or has a character a file name cannot (/ \\ : ;)", v, maxTextField)
	}
	return v, nil
}

var dateLayouts = []string{
	"2006-01-02", "1/2/2006", "January 2, 2006", "Jan 2, 2006", "2 January 2006", "2 Jan 2006",
	"Monday, January 2, 2006", "Mon, Jan 2, 2006",
}

// parseDate reads the common ways mail spells a date ("Wed., Dec." as
// "Wed, Dec").
func parseDate(s string, loc *time.Location) (time.Time, error) {
	norm := strings.ReplaceAll(s, ".", "")
	for _, layout := range dateLayouts {
		if d, err := time.ParseInLocation(layout, norm, loc); err == nil {
			return d, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a date", s)
}

// Destination renders a filed template's folder and name from a match's
// fields.
func (t *Template) Destination(fields map[string]string) (folder, name string, err error) {
	if t.File == nil {
		return "", "", fmt.Errorf("template %s is not filed", t.ID)
	}
	render := func(pattern string) string {
		return placeholder.ReplaceAllStringFunc(pattern, func(ph string) string {
			sub := placeholder.FindStringSubmatch(ph)
			v, ok := fields[sub[1]]
			if !ok && err == nil {
				err = fmt.Errorf("template %s: no value for %s", t.ID, sub[1])
			}
			if sub[2] != "" {
				v, _, _ = strings.Cut(v, "-")
			}
			return v
		})
	}
	folder, name = render(t.File.Folder), render(t.File.Name)
	if err != nil {
		return "", "", err
	}
	// Fields are validated when matched; this guards values from elsewhere.
	if !insideRoot(folder) || strings.Trim(name, ".") == "" || strings.ContainsAny(name, unsafeName) {
		return "", "", fmt.Errorf("template %s: %q / %q is not a file inside the filing root", t.ID, folder, name)
	}
	return folder, name, nil
}

// CommonLines returns the lines (whitespace collapsed) found in every one of
// the texts, in the first text's order and once each: candidate anchors for a
// sender's template.
func CommonLines(texts []string) []string {
	if len(texts) == 0 {
		return nil
	}
	count := map[string]int{}
	for _, text := range texts {
		seen := map[string]bool{}
		for _, l := range strings.Split(stage.Readable(text), "\n") {
			if l = collapse(l); l != "" && !seen[l] {
				seen[l] = true
				count[l]++
			}
		}
	}
	var out []string
	done := map[string]bool{}
	for _, l := range strings.Split(stage.Readable(texts[0]), "\n") {
		if l = collapse(l); count[l] == len(texts) && !done[l] {
			done[l] = true
			out = append(out, l)
		}
	}
	return out
}
