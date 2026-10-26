package stage

import (
	"regexp"
	"strings"
)

var (
	// bracketedURL is how Graph's text rendering spells an image or link:
	// "[https://...]" or "<https://...>". Only the URL goes; text around it stays.
	bracketedURL = regexp.MustCompile(`[\[<]\s*(?:https?|mailto|cid):[^\]>\s]*\s*[\]>]`)
	bareURL      = regexp.MustCompile(`\bhttps?://\S+`)
	spaceRun     = regexp.MustCompile(`[ \t\f\v\x{00a0}]+`)
	blankRun     = regexp.MustCompile(`\n(?:[ \t]*\n)+`)
)

// Readable reduces a text body to what a person reads: URLs (which a text
// rendering of HTML mail spells out for every image and link) are removed,
// runs of spaces become one, and runs of blank lines become one blank line.
func Readable(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = bracketedURL.ReplaceAllString(body, "")
	body = bareURL.ReplaceAllString(body, "")
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaceRun.ReplaceAllString(l, " "))
	}
	body = strings.Join(lines, "\n")
	body = blankRun.ReplaceAllString(body, "\n\n")
	return strings.TrimSpace(body)
}

var (
	outlookRule  = regexp.MustCompile(`^_{10,}$`)
	fromHeader   = regexp.MustCompile(`^From:\s`)
	sentOrDate   = regexp.MustCompile(`^(?:Sent|Date):\s`)
	onWrote      = regexp.MustCompile(`^On\s.*\bwrote:$`)
	onStart      = regexp.MustCompile(`^On\s`)
	wroteEnd     = regexp.MustCompile(`\bwrote:$`)
	originalMark = regexp.MustCompile(`(?i)^-{2,}\s*original message\s*-{2,}$`)
)

// WithoutQuoted returns body cut at the start of its quoted reply history,
// and how many lines were cut. History starts at the first of: an Outlook
// header block ("From:" with "Sent:" or "Date:" just below, optionally after
// a rule of underscores), an "On ... wrote:" line (or one split over two
// lines), or "-----Original Message-----"; failing those, at a block of ">"
// lines that runs to the end. A ">" quote with the reply below it is kept.
func WithoutQuoted(body string) (string, int) {
	lines := strings.SplitAfter(body, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	at := func(i int) string {
		if i < len(lines) {
			return strings.TrimSpace(lines[i])
		}
		return ""
	}
	header := func(i int) bool {
		return fromHeader.MatchString(at(i)) && (sentOrDate.MatchString(at(i+1)) || sentOrDate.MatchString(at(i+2)))
	}
	cut := -1
	for i := range lines {
		l := at(i)
		if originalMark.MatchString(l) || onWrote.MatchString(l) || header(i) ||
			(outlookRule.MatchString(l) && header(i+1)) || (onStart.MatchString(l) && wroteEnd.MatchString(at(i+1))) {
			cut = i
			break
		}
	}
	if cut < 0 {
		j, quoted := len(lines), false
		for j > 0 && (at(j-1) == "" || strings.HasPrefix(at(j-1), ">")) {
			quoted = quoted || at(j-1) != ""
			j--
		}
		if !quoted {
			return body, 0
		}
		cut = j
	}
	kept := strings.TrimRight(strings.Join(lines[:cut], ""), " \t\r\n")
	if kept != "" {
		kept += "\n"
	}
	return kept, len(lines) - cut
}
