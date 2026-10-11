package main

import (
	"strings"
	"time"
)

const dateLayout = "2006-01-02"

// cleanDue checks a due date from a form or an import: empty, or a real
// calendar date as YYYY-MM-DD, which is what <input type="date"> sends.
func cleanDue(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if _, err := time.Parse(dateLayout, s); err != nil {
		return "", invalid("%q is not a date (YYYY-MM-DD)", s)
	}
	return s, nil
}

// dueBadge is what the card face shows for a due date.
type dueBadge struct {
	Label string // "Today", "Tomorrow", "Mon 12 Oct", ...
	State string // overdue, today, soon or later; the CSS colours them
	Date  string // YYYY-MM-DD, for the tooltip
}

// soonDays is how far ahead a due date counts as soon.
const soonDays = 2

// due grades a due date against today in the server's time zone (TZ). It is
// a template function; ok is false for a card without a date.
func due(date string, now time.Time) (dueBadge, bool) {
	// Both days as midnight UTC, so the difference is whole days even across
	// a daylight saving change; "today" is still the server's local date.
	d, err := time.Parse(dateLayout, date)
	if err != nil {
		return dueBadge{}, false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := int(d.Sub(today) / (24 * time.Hour))
	b := dueBadge{Date: date}
	switch {
	case days < 0:
		b.State = "overdue"
	case days == 0:
		b.State = "today"
	case days <= soonDays:
		b.State = "soon"
	default:
		b.State = "later"
	}
	switch days {
	case -1:
		b.Label = "Yesterday"
	case 0:
		b.Label = "Today"
	case 1:
		b.Label = "Tomorrow"
	default:
		if d.Year() == today.Year() {
			b.Label = d.Format("Mon 2 Jan")
		} else {
			b.Label = d.Format("2 Jan 2006")
		}
	}
	return b, true
}
