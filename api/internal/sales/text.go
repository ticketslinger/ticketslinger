package sales

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// cleanText turns CRLF into LF, checks s, then trims it (so a stray tab at
// the edge is refused, not trimmed away). It refuses invalid UTF-8, more
// than max runes, any control character but a newline, and the bidi controls
// that make text read differently from how it's stored.
func cleanText(s string, max int) (string, bool) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return "", false
	}
	for _, r := range s {
		if (unicode.Is(unicode.Cc, r) && r != '\n') || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return "", false
		}
	}
	return strings.TrimSpace(s), true
}
