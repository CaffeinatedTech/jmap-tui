package mailtext

// Audit probes for T-2/T-8: HTML→text must strip control characters.
// FAIL = regression confirmed.

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var hostileHTML = []struct{ name, html string }{
	{"raw-esc", "<p>hello \x1b[2J world</p>"},
	{"raw-osc", "<p>click \x1b]8;;https://evil.test\x07here\x1b]8;;\x07</p>"},
	{"entity-esc", "<p>esc &#27;[2J here</p>"},
	{"entity-hex-esc", "<p>esc &#x1b;[2J here</p>"},
	{"entity-bel", "<p>ding &#7; dong</p>"},
	{"entity-cr", "<p>line1 &#13;EVIL</p>"},
	{"c1-csi", "<p>c1 \x9b2J here</p>"},
	{"nul", "<p>a &#0; b</p>"},
	{"bidi", "<p>invoice &#8238;gnp.exe</p>"},
	{"zero-width", "<p>pass&#8203;word</p>"},
	{"script-strip", "<script>alert(1)</script><p>ok</p>"},
	{"style-strip", "<style>p{color:red}</style><p>ok</p>"},
	{"img-no-fetch", `<img src="https://evil.test/tracker.gif" alt="ALT">`},
	{"deep-nesting", strings.Repeat("<div>", 200) + "deep" + strings.Repeat("</div>", 200)},
	{"long-word", "<p>" + strings.Repeat("a", 5000) + "</p>"},
}

func TestAuditT2HTMLToTextStripsControls(t *testing.T) {
	for _, tc := range hostileHTML {
		out := HTMLToText(tc.html)
		if !utf8.ValidString(out) {
			t.Errorf("[%s] HTMLToText output is not valid UTF-8", tc.name)
		}
		for i, r := range out {
			if r == '\n' || r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f {
				t.Errorf("[%s] control %#x survived HTMLToText at offset %d: %q", tc.name, r, i, snippet(out))
				break
			}
			if r == 0x1b {
				t.Errorf("[%s] ESC survived HTMLToText at offset %d: %q", tc.name, i, snippet(out))
				break
			}
			if r >= 0x80 && r <= 0x9f {
				t.Errorf("[%s] C1 control %#x survived HTMLToText at offset %d", tc.name, r, i)
				break
			}
			if r == 0x202e || r == 0x200b || r == 0xfeff {
				t.Errorf("[%s] format control %#x survived HTMLToText at offset %d", tc.name, r, i)
				break
			}
		}
	}
}

func TestAuditT8HTMLToTextTerminates(t *testing.T) {
	cases := []string{
		"<div>" + strings.Repeat("<span>", 5000) + "x",
		strings.Repeat("<p>", 10000),
		"\x00\x01\x02",
		strings.Repeat("<!--", 500) + "unterminated",
	}
	for i, in := range cases {
		done := make(chan string, 1)
		go func() { done <- HTMLToText(in) }()
		select {
		case <-done:
		case <-timeAfter():
			t.Errorf("case %d: HTMLToText never returned", i)
		}
	}
}

func snippet(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '.'
		}
		return r
	}, s)
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// timeAfter is a 3s deadline for the termination probe.
func timeAfter() <-chan time.Time { return time.After(3 * time.Second) }

// --- fuzz target ---

// FuzzHTMLToText: hostile HTML must never panic, never hang (enforced by
// the fuzz engine's per-input timeout), and never emit control bytes
// (T-2).
func FuzzHTMLToText(f *testing.F) {
	for _, s := range []string{
		"<p>hello</p>",
		"<script>x</script><p>ok</p>",
		"\x1b[2J&#27;[2J",
		"<div><span><a href='http://x'>y</a>",
		"&#0;&#7;&#27;&#x1b;",
		"\x00\x01\x07\x1b\x9b",
		"\u202epassword\u200b",
		"<img src=//evil.test/x.gif alt='a'>",
		"",
		"<p>" + string([]byte{0xff, 0xfe, 0xfd}) + "</p>",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := HTMLToText(in)
		for _, r := range out {
			if r == '\n' || r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f || r == 0x1b || (r >= 0x80 && r <= 0x9f) {
				t.Fatalf("control %#x in HTMLToText output: %q", r, clip(out))
			}
			if r == 0x202e || r == 0x200b || r == 0xfeff {
				t.Fatalf("format control %#x in HTMLToText output: %q", r, clip(out))
			}
		}
	})
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
