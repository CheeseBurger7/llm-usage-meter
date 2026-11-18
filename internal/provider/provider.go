// Package provider holds the rule for provider labels (OPEN_QUESTIONS Q20),
// shared by ingest and the capture proxy so both accept the same values.
//
// A provider label is folded to lowercase ASCII and must then match
// ^[a-z0-9._-]{1,256}$. Every allowed value is ASCII, so Go, SQL (the CHECK
// constraint in migration 00007) and any later price book or dashboard code
// compare the same bytes, with no Unicode case mapping or collation involved.
package provider

import "fmt"

// MaxLen is the longest allowed label, in bytes. All allowed bytes are
// ASCII, so it is also the length in characters.
const MaxLen = 256

// Normalize lowercases the ASCII letters A to Z and leaves every other byte
// as it is. It does not trim or fold non-ASCII letters: a value that still
// holds anything outside the allowed set is refused by Check, not rewritten.
func Normalize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// Check reports why a normalized label is not allowed, or "" when it is.
// The reason is a short field error and never repeats the value.
func Check(s string) string {
	switch {
	case s == "":
		return "required"
	case len(s) > MaxLen:
		return fmt.Sprintf("must be at most %d bytes", MaxLen)
	}
	for i := 0; i < len(s); i++ {
		if !allowed(s[i]) {
			return "must contain only a-z, 0-9, '.', '_' and '-' (A-Z is lowercased)"
		}
	}
	return ""
}

func allowed(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}
