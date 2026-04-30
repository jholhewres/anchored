package memory

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func TruncateAtBoundary(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}

	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	boundary := cut
	for boundary > 0 {
		c := s[boundary-1]
		if c == ' ' || c == '\n' || c == '\t' || c == '\r' {
			break
		}
		boundary--
	}

	if boundary > 0 {
		return strings.TrimRight(s[:boundary], " \t\r\n")
	}
	return s[:cut]
}

func SplitOnPunctuation(text string) []string {
	var words []string
	var current strings.Builder
	for _, r := range text {
		if unicode.IsSpace(r) {
			if current.Len() > 0 {
				words = append(words, current.String())
				current.Reset()
			}
			continue
		}
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			if current.Len() > 0 {
				words = append(words, current.String())
				current.Reset()
			}
			words = append(words, string(r))
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	return words
}
