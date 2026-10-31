// Package safetext makes untrusted text safe to print in a terminal or a CI
// log, where it could otherwise carry escape sequences, bidirectional
// overrides or GitHub workflow commands.
package safetext

import (
	"strings"
	"unicode"
)

// Text returns s without control characters, such as line breaks and escape
// sequences, and without bidirectional formatting characters. Tabs become
// spaces.
func Text(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case unicode.IsControl(r), isBidi(r):
			return -1
		}
		return r
	}, s)
}

// isBidi reports whether r is a bidirectional formatting character, which can
// make text display in a different order than it is stored.
func isBidi(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		('\u202a' <= r && r <= '\u202e') || ('\u2066' <= r && r <= '\u2069')
}
