package runner

import (
	"strings"
	"unicode/utf8"
)

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
