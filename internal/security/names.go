// Package security holds the input sanitisation, rate limiting and admission
// control used by the public SSH service.
package security

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MinNameLen and MaxNameLen bound display names (in bytes; names are
	// ASCII-only so bytes equal columns).
	MinNameLen = 3
	MaxNameLen = 16
)

// Name validation errors. They are safe to show to players.
var (
	ErrNameLength   = fmt.Errorf("name must be %d to %d characters", MinNameLen, MaxNameLen)
	ErrNameChars    = errors.New("use only letters, digits, '-', '_' and '.'")
	ErrNameStart    = errors.New("name must start with a letter or digit")
	ErrNameReserved = errors.New("that name is reserved")
)

// reservedSkeletons are name skeletons nobody may claim, to make
// impersonating the operator or the guest naming scheme harder.
var reservedSkeletons = []string{
	"admin", "administrator", "root", "system", "server", "sysop", "mod",
	"moderator", "staff", "support", "official", "owner", "operator",
	"flap", "flapd", "anonymous", "null", "nil", "undefined", "you", "me",
}

// NameAllowedRune reports whether r may appear in a display name. Only a
// small ASCII alphabet is allowed: this rules out control characters, escape
// sequences, bidirectional overrides, homoglyphs from other scripts and any
// width ambiguity.
func NameAllowedRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-', r == '_', r == '.':
		return true
	}
	return false
}

// ValidateName checks a display name chosen by a player.
func ValidateName(name string) error {
	if !utf8.ValidString(name) {
		return ErrNameChars
	}
	if len(name) < MinNameLen || len(name) > MaxNameLen {
		return ErrNameLength
	}
	for _, r := range name {
		if !NameAllowedRune(r) {
			return ErrNameChars
		}
	}
	if c := name[0]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
		return ErrNameStart
	}
	sk := NameKey(name)
	if strings.HasPrefix(sk, "guest") {
		return ErrNameReserved
	}
	for _, r := range reservedSkeletons {
		if sk == NameKey(r) {
			return ErrNameReserved
		}
	}
	return nil
}

// NameKey returns the uniqueness key of a name. Names that look alike in a
// monospace terminal map to the same key, so "Alice", "A1ice", "ALICE" and
// "a.lice" collide and cannot be registered by different players.
func NameKey(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(name) {
		switch r {
		case '-', '_', '.':
			continue
		case '0':
			r = 'o'
		case '1', 'i':
			r = 'l'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// SanitizeName reduces untrusted input to a displayable name: disallowed
// runes are removed and the result is truncated. The output may still fail
// ValidateName (for example if it is too short).
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= MaxNameLen {
			break
		}
		if NameAllowedRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// LogString makes an untrusted string safe and bounded for structured logs:
// non-printable and non-ASCII runes are replaced and the length is capped.
// Structured log handlers escape quotes, but stripping everything unusual
// keeps logs readable and prevents fake entries in plain-text viewers.
func LogString(s string, maxLen int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxLen {
			b.WriteString("…")
			break
		}
		if r < 0x20 || r >= 0x7f {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
