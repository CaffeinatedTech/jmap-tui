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

// SanitizeStyled is Sanitize for output that carries renderer-generated
// styling: it passes well-formed CSI SGR sequences (ESC [ … m — colours,
// bold, reset) through untouched and applies the Sanitize policy to
// everything else. Every other escape is dropped whole: no cursor move,
// no screen clear, no OSC — title or hyperlink — survives. Only
// appearance reaches the terminal.
//
// OSC is dropped rather than passed because a hyperlink's payload is a
// URL the reader already sees as text: keeping the wrapper would add
// nothing but a clickable target whose destination the sender chose.
//
// It is the frame-boundary choke point for the styled body view: the
// content string is built from Sanitize'd source plus the renderer's own
// SGR, and this is what verifies that claim before the frame is written.
// Sanitize remains the policy for everything that is not styled.
func SanitizeStyled(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return Sanitize(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '\x1b' {
			r, size := utf8.DecodeRuneInString(s[i:])
			if keepRune(r) {
				b.WriteRune(r)
			}
			// A malformed byte maps to RuneError, which keepRune
			// passes and WriteRune emits as U+FFFD — same contract
			// as Sanitize.
			i += size
			continue
		}
		if n, ok := sgrLen(s[i:]); ok {
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		if n, _, ok := csiLen(s[i:]); ok {
			i += n // a complete non-SGR control sequence: dropped
			continue
		}
		if n, ok := oscLen(s[i:]); ok {
			i += n // OSC (hyperlink, title): dropped whole
			continue
		}
		i++ // a bare ESC: dropped; its payload is ordinary text
	}
	return b.String()
}

// oscLen returns the byte length of the OSC sequence at the start of s
// (ESC ] … BEL, or ESC ] … ST). An unterminated OSC runs to the end of
// the string: a terminal would swallow the remainder too, so dropping it
// keeps the payload from ever reaching one.
func oscLen(s string) (int, bool) {
	if len(s) < 2 || s[0] != '\x1b' || s[1] != ']' {
		return 0, false
	}
	for i := 2; i < len(s); i++ {
		switch s[i] {
		case 0x07:
			return i + 1, true
		case '\x1b':
			if i+1 < len(s) && s[i+1] == '\\' {
				return i + 2, true
			}
			return len(s), true // a fresh escape inside: malformed
		}
	}
	return len(s), true
}

// SeqLen returns the byte length of the escape sequence at the start of
// s — a CSI, an OSC, or ESC followed by one byte — or 0 when s does not
// begin with one. It lets a caller walk styled text and skip the
// sequences without having to judge which of them are safe; deciding that
// is SanitizeStyled's job.
func SeqLen(s string) int {
	if len(s) == 0 || s[0] != '\x1b' {
		return 0
	}
	if len(s) > 1 && s[1] == '[' {
		if n, _, ok := csiLen(s); ok {
			return n
		}
		return 1
	}
	if len(s) > 1 && s[1] == ']' {
		if n, ok := oscLen(s); ok {
			return n
		}
		return 1
	}
	if len(s) == 1 {
		return 1
	}
	return 2
}

// sgrLen returns the byte length of the CSI SGR sequence at the start of
// s, which must begin with one.
func sgrLen(s string) (int, bool) {
	n, final, ok := csiLen(s)
	if !ok || final != 'm' {
		return 0, false
	}
	return n, true
}

// csiLen parses a CSI sequence (ESC [ params intermediates final) at the
// start of s, returning its byte length and final byte. ok is false when
// s does not start with a complete, well-formed CSI.
func csiLen(s string) (n int, final byte, ok bool) {
	if len(s) < 2 || s[0] != '\x1b' || s[1] != '[' {
		return 0, 0, false
	}
	for i := 2; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 0x30 && c <= 0x3f: // parameter bytes
		case c >= 0x20 && c <= 0x2f: // intermediate bytes
		case c >= 0x40 && c <= 0x7e: // final byte
			return i + 1, c, true
		default: // malformed: not a CSI after all
			return 0, 0, false
		}
	}
	return 0, 0, false // unterminated
}
