package runner

import (
	"strings"
	"unicode/utf8"
)

// isMorePrompt reports whether output ends with a TADS/frob [MORE] pagination marker.
// frob has no --no-more flag; without a real terminal it defaults to a 1-line page
// height, emitting [MORE] after every line of output.
func isMorePrompt(s string) bool {
	return strings.HasSuffix(strings.ToUpper(strings.TrimRight(s, " \t\r\n")), "[MORE]")
}

// stripMoreSuffix removes the trailing [MORE] marker and normalises trailing whitespace.
func stripMoreSuffix(s string) string {
	trimmed := strings.TrimRight(s, " \t\r\n")
	if idx := strings.LastIndex(strings.ToUpper(trimmed), "[MORE]"); idx >= 0 {
		return strings.TrimRight(trimmed[:idx], " \t\r\n") + "\n"
	}
	return s
}

// sanitizeUTF8 replaces invalid UTF-8 sequences with U+FFFD.
func sanitizeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	sb := &strings.Builder{}
	sb.Grow(len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		sb.WriteRune(r) // DecodeRune returns RuneError for invalid sequences
		b = b[size:]
	}
	return sb.String()
}
