package mcpserver

import (
	"fmt"
	"strings"
	"time"
)

// validateDate parses YYYY-MM-DD strictly via time.Parse so
// nonsense like "2026-13-32" is rejected. Returns the parsed time
// (UTC, midnight) so callers can normalize to "YYYY-MM-DD HH:MM:SS"
// without re-parsing.
func validateDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("date is required (format: YYYY-MM-DD)")
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q must be YYYY-MM-DD: %w", s, err)
	}
	// Sanity bound: Paprika's product dates from 2011 and the meal
	// plan rarely projects more than a couple years out. Tighten so
	// callers don't accidentally schedule meals in 1970 or 2099 due
	// to a parsing mishap.
	min := time.Date(2011, 1, 1, 0, 0, 0, 0, time.UTC)
	max := time.Now().AddDate(5, 0, 0)
	if t.Before(min) {
		return time.Time{}, fmt.Errorf("date %s is before 2011-01-01; double-check the year", s)
	}
	if t.After(max) {
		return time.Time{}, fmt.Errorf("date %s is more than 5 years in the future; double-check the year", s)
	}
	return t, nil
}

// requireNonBlank returns the trimmed string or an error mentioning
// the field name. Whitespace-only inputs are rejected — Paprika's API
// would happily store them, producing invisible rows in the app.
func requireNonBlank(field, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("%s is required (got empty/whitespace)", field)
	}
	return v, nil
}

// boundInt clamps n to [min, max]. Returns (clamped, didClamp). The
// caller decides whether to reject on clamp or just use the bounded
// value silently.
func boundInt(n, min, max int) (int, bool) {
	clamped := n
	if clamped < min {
		clamped = min
	}
	if clamped > max {
		clamped = max
	}
	return clamped, clamped != n
}
