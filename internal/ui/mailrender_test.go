package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The dark accent is #82aaff, the light accent #2a5db0 (theme.go); the
// renderer emits them as truecolor SGR.
const (
	darkAccentSGR  = "\x1b[38;2;130;170;255m"
	lightAccentSGR = "\x1b[38;2;42;93;176m"
)

func TestRenderBodyStylesStructureWithThePalette(t *testing.T) {
	md := "## Heading\n\nHello **world** and *friends* with `code`.\n\n> quoted"
	for _, tc := range []struct {
		name   string
		p      Palette
		accent string
	}{
		{"dark", DarkTheme(), darkAccentSGR},
		{"light", LightTheme(), lightAccentSGR},
	} {
		out := RenderBody(md, tc.p)
		// The heading is the accent plus bold: the palette colour with
		// an SGR bold parameter appended.
		if want := strings.TrimSuffix(tc.accent, "m") + ";1mHeading"; !strings.Contains(out, want) {
			t.Errorf("%s: heading not in the palette accent+bold (%s): %q", tc.name, want, out)
		}
		if !strings.Contains(out, tc.accent) {
			t.Errorf("%s: accent never used: %q", tc.name, out)
		}
		visible := ansi.Strip(out)
		for _, want := range []string{"Heading", "Hello world and friends with code.", "quoted"} {
			if !strings.Contains(visible, want) {
				t.Errorf("%s: %q missing from visible text: %q", tc.name, want, visible)
			}
		}
		// The markup that produced the styling must not survive it.
		for _, marker := range []string{"##", "**", "*", "`", "> "} {
			if strings.Contains(visible, marker) {
				t.Errorf("%s: markdown marker %q leaked: %q", tc.name, marker, visible)
			}
		}
	}
}

func TestRenderBodyIsStyledOnly(t *testing.T) {
	md := "## H\n\nbody [1]\n\n-- \n\n[1] https://example.test/x?a=1&b=2"
	out := RenderBody(md, DarkTheme())
	// The gate is an identity on the renderer's own output: nothing but
	// SGR survives it.
	if got := SanitizeStyled(out); got != out {
		t.Errorf("SanitizeStyled changed renderer output:\n in %q\nout %q", out, got)
	}
	// No hyperlink wrapper reaches the pane: the URL is text.
	if strings.Contains(out, "]8;") {
		t.Errorf("OSC hyperlink leaked into the body: %q", out)
	}
	// The output is width-independent: no line is padded to any width.
	for i, l := range strings.Split(out, "\n") {
		if strings.HasSuffix(l, " ") {
			t.Errorf("line %d carries trailing padding: %q", i, l)
		}
	}
}

func TestRenderBodyFallsBackToSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		md   string
	}{
		{"empty", ""},
		{"oversized", strings.Repeat("x", maxStyledBodyBytes+1)},
	} {
		if got, want := RenderBody(tc.md, DarkTheme()), tc.md; got != want {
			t.Errorf("%s: got %q, want the source %q", tc.name, got, want)
		}
	}
	// A body just inside the guard is still styled.
	inside := "# Head\n\n" + strings.Repeat("word ", maxStyledBodyBytes/5-20)
	if got := RenderBody(inside, DarkTheme()); !strings.ContainsRune(got, 0x1b) {
		t.Error("body under the guard was not styled")
	}
}

func TestRenderBodyIsDeterministic(t *testing.T) {
	md := "## A\n\n- one\n- two\n\n| a | b |\n| --- | --- |\n| 1 | 2 |"
	a := RenderBody(md, DarkTheme())
	b := RenderBody(md, DarkTheme())
	if a != b {
		t.Fatal("render is not deterministic")
	}
}

// The frame boundary keeps the pane's styling and still strips anything
// else that looks like an escape.
func TestSanitizeStateKeepsBodyStyling(t *testing.T) {
	st := baseFrame(true)
	st.VpView = "\x1b[38;2;130;170;255mstyled\x1b[m\x1b]8;;https://evil\x07\x1b[2J"
	out := sanitizeState(st)
	if !strings.Contains(out.VpView, "\x1b[38;2;130;170;255mstyled\x1b[m") {
		t.Errorf("stypping dropped the pane's SGR: %q", out.VpView)
	}
	if strings.ContainsRune(out.VpView, 0x1b) && !onlySGR(out.VpView) {
		t.Errorf("non-SGR escape survived the frame: %q", out.VpView)
	}
}

// The search query is the textinput's rendered view: its placeholder and
// cursor styling must survive the frame boundary, and only that styling.
func TestSanitizeStateKeepsSearchQueryStyling(t *testing.T) {
	st := baseFrame(true)
	st.Search = &SearchView{
		Query: "\x1b[38;5;240mtype to search…\x1b[m\x1b]8;;https://evil\x07\x1b[2J",
		Scope: "Inbox",
	}
	out := sanitizeState(st)
	if !strings.Contains(out.Search.Query, "\x1b[38;5;240m") {
		t.Errorf("frame boundary dropped the query's SGR: %q", out.Search.Query)
	}
	if strings.ContainsRune(out.Search.Query, 0x1b) && !onlySGR(out.Search.Query) {
		t.Errorf("non-SGR escape survived the frame: %q", out.Search.Query)
	}
}

// onlySGR reports whether every escape in s is a well-formed CSI SGR.
func onlySGR(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			continue
		}
		n := 0
		for i+2+n < len(s) && s[i+2+n] >= 0x30 && s[i+2+n] <= 0x3f {
			n++
		}
		j := i + 2 + n
		if j >= len(s) || s[j] != 'm' {
			return false
		}
		i = j
	}
	return true
}
