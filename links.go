package main

import (
	"html/template"
	"net/url"
	"regexp"
	"strings"
)

// linkCandidate is a run of text that starts like a web address. What may
// follow is anything but whitespace and the characters that end an address
// in prose or in markup; trimLink then takes off trailing punctuation.
var linkCandidate = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `]+`)

// linkify escapes text for HTML and turns every http and https address in it
// into a link that opens in a new tab. It is the only place the templates get
// markup from Go, so everything that is not a link goes through
// HTMLEscapeString, and a link is only made of an address that parses with
// one of those two schemes and a host.
func linkify(text string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range linkCandidate.FindAllStringIndex(text, -1) {
		start, end := m[0], m[0]+len(trimLink(text[m[0]:m[1]]))
		u, err := url.Parse(text[start:end])
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		b.WriteString(template.HTMLEscapeString(text[last:start]))
		href := template.HTMLEscapeString(text[start:end])
		b.WriteString(`<a href="` + href + `" target="_blank" rel="noopener noreferrer nofollow">` + href + `</a>`)
		last = end
	}
	b.WriteString(template.HTMLEscapeString(text[last:]))
	return template.HTML(b.String())
}

// trimLink drops what prose puts after an address: a full stop, a comma, a
// closing bracket around the whole link. A closing parenthesis stays when the
// address opened one itself, as Wikipedia's do.
func trimLink(s string) string {
	for len(s) > 0 {
		last := s[len(s)-1]
		switch {
		case strings.IndexByte(".,;:!?]}", last) >= 0:
			s = s[:len(s)-1]
		case last == ')' && strings.Count(s, "(") < strings.Count(s, ")"):
			s = s[:len(s)-1]
		default:
			return s
		}
	}
	return s
}
