package textutil

import (
	"strings"
	"unicode"
)

// isSpace est la définition des blancs de Python (str.split, str.strip) : les blancs Unicode, plus les quatre
// séparateurs ASCII \x1c à \x1f.
func isSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// Strip reproduit str.strip() de Python.
func Strip(s string) string { return strings.TrimFunc(s, isSpace) }

// Fields reproduit str.split() de Python (découpage sur les blancs, sans éléments vides).
func Fields(s string) []string { return strings.FieldsFunc(s, isSpace) }

// Truncate reproduit s[:n] de Python : n caractères (et non n octets).
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// Slice reproduit s[a:b] de Python (bornes en caractères, jamais d'erreur).
func Slice(s string, a, b int) string {
	r := []rune(s)
	if a > len(r) {
		a = len(r)
	}
	if b > len(r) {
		b = len(r)
	}
	if a >= b {
		return ""
	}
	return string(r[a:b])
}
