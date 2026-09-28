package mailtext

import (
	"strings"
	"unicode/utf8"
)

// Sanitize strips characters that must never reach a terminal from server-
// or sender-controlled text (strip silently, at a single choke point).
// Removed: every C0 control except '\n' and '\t',
// DEL, every C1 control (U+0080–U+009F), the bidi embedding/isolate/
// override runes (U+202A–U+202E, U+2066–U+2069), zero-width and marking
// characters (U+200B–U+200F, U+FEFF, U+061C), and invalid UTF-8 bytes
// (replaced by U+FFFD, so the output is always valid UTF-8). Clean text is
// returned unchanged with no allocation; stripping is silent — no
// replacement glyphs, no escaping.
//
// This is the canonical implementation: it lives below the UI (internal/sync
// and HTMLToText must reach it and cannot import internal/ui). ui.Sanitize
// is the same policy under its display-layer name.
func Sanitize(s string) string {
	start := dirtyAt(s)
	if start < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:start])
	for _, r := range s[start:] {
		// range maps a malformed byte to RuneError, which keepRune
		// passes and WriteRune emits as U+FFFD — bad bytes are replaced,
		// policy-stripped runes are dropped, everything else is copied.
		if keepRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// dirtyAt returns the byte index of the first character Sanitize would
// remove, or -1 when s is already clean. Malformed UTF-8 counts as dirty:
// the slow path in Sanitize replaces each bad byte with U+FFFD, so callers
// that demand valid UTF-8 output get it whenever anything needed fixing.
func dirtyAt(s string) int {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b < utf8.RuneSelf {
			if b == '\n' || b == '\t' {
				continue
			}
			if b < 0x20 || b == 0x7f {
				return i
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		if !keepRune(r) {
			return i
		}
		i += size - 1
	}
	return -1
}

// keepRune reports whether r survives Sanitize. This is exactly the
// strip-silently policy: C0/C1 controls out, '\n' and '\t' in,
// bidi/zero-width marking characters out, ordinary text (including
// non-ASCII) in.
func keepRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return true
	case r < 0x20, r == 0x7f:
		return false
	case r >= 0x80 && r <= 0x9f: // C1 controls
		return false
	case r >= 0x202a && r <= 0x202e: // bidi embedding/override
		return false
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return false
	case r >= 0x200b && r <= 0x200f: // zero-width + LRM/RLM
		return false
	case r == 0xfeff, r == 0x061c:
		return false
	}
	return true
}

// Truncate sanitizes s (Sanitize) and cuts it to at most max bytes of
// output, the "…" tail included when a cut happened — so the result is
// always valid UTF-8 and control-free whatever the input was, unlike the
// old status/error truncators, which sliced at a fixed byte offset,
// splitting runes and shipping invalid UTF-8 into the status line. Text
// already within the budget comes back unchanged apart from
// sanitization; a max below the tail's own length yields "".
func Truncate(s string, max int) string {
	s = Sanitize(s)
	if len(s) <= max {
		return s
	}
	n := max - len("…")
	if n < 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
