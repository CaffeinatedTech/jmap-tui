package mailtext

import (
	"strings"
	"testing"
)

func TestSanitizeStyledKeepsSGR(t *testing.T) {
	cases := []string{
		"\x1b[m",
		"\x1b[1m",
		"\x1b[1;3;38;2;130;170;255mtext\x1b[m",
		"\x1b[38;5;123mbold\x1b[0m",
		"plain \x1b[32mgreen\x1b[0m plain",
	}
	for _, in := range cases {
		if got := SanitizeStyled(in); got != in {
			t.Errorf("SanitizeStyled(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestSanitizeStyledNoESCFastPath(t *testing.T) {
	// A clean string must take the Sanitize path untouched.
	if got := SanitizeStyled("hello\n\tworld"); got != "hello\n\tworld" {
		t.Errorf("got %q", got)
	}
	// And the Sanitize policy still applies on that path.
	if got := SanitizeStyled("a\x07b\x1b"); strings.ContainsAny(got, "\x07\x1b") {
		t.Errorf("controls survived fast path: %q", got)
	}
}

func TestSanitizeStyledStripsNonSGR(t *testing.T) {
	cases := map[string]string{
		// Cursor / screen control: the CSI is dropped whole.
		"a\x1b[2Jb":        "ab",
		"a\x1b[1;5Hb":      "ab",
		"a\x1b[?25lb":      "ab",
		"a\x1b[2J\x1b[0mb": "a\x1b[0mb",
		// OSC hyperlink and title: dropped whole, payload included —
		// the URL is already visible as text and the wrapper would
		// only add a sender-chosen click target.
		"a\x1b]8;;https://evil\x07b": "ab",
		"a\x1b]0;title\x1b\\b":       "ab",
		// Bare ESC and a truncated sequence: only the introducer is
		// gone, the printable tail stays as ordinary text.
		"a\x1bb":     "ab",
		"a\x1b[31":   "a[31",
		"a\x1b[31Xb": "ab", // non-SGR final byte: whole CSI dropped
		// Charset selection and other ESC- sequences.
		"a\x1b(Bb": "a(Bb",
		// The Sanitize policy rides along.
		"a\x07\x1b[0m":    "a\x1b[0m",
		"a\u202eb\x1b[1m": "ab\x1b[1m",
	}
	for in, want := range cases {
		if got := SanitizeStyled(in); got != want {
			t.Errorf("SanitizeStyled(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeStyledSurvivesSplitSequence(t *testing.T) {
	// A sequence chopped at the end of the string must degrade to
	// visible text, never to a live escape.
	for _, in := range []string{"\x1b", "\x1b[", "\x1b[3", "\x1b["} {
		out := SanitizeStyled(in)
		if strings.ContainsRune(out, '\x1b') {
			t.Errorf("SanitizeStyled(%q) = %q kept ESC", in, out)
		}
	}
}

// Styled body views reach the frame with the renderer's SGR already in
// them; the hostile input is the *content*, which is Sanitize'd before
// rendering. This asserts the two layers compose: whatever the content
// was, only SGR survives SanitizeStyled.
func TestSanitizeStyledOnlyEmitsSGR(t *testing.T) {
	inputs := []string{
		"\x1b[1mok\x1b[0m",
		"pre\x1b[2Jpost\x1b[31mgreen",
		"\x1b]8;;http://x\x07link\x1b]8;;\x07",
		"clean",
		"\x1b[38;2;1;2;3m",
	}
	for _, in := range inputs {
		out := SanitizeStyled(in)
		for i := 0; i < len(out); i++ {
			if out[i] != '\x1b' {
				continue
			}
			n, final, ok := csiLen(out[i:])
			if !ok || final != 'm' {
				t.Fatalf("SanitizeStyled(%q) leaked non-SGR escape at %d: %q", in, i, snippet(out))
			}
			i += n - 1
		}
	}
}
