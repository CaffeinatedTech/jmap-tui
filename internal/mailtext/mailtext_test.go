package mailtext

import (
	"strings"
	"testing"
)

func TestParagraphsBecomeLines(t *testing.T) {
	out := HTMLToText("<p>First paragraph.</p><p>Second paragraph.</p>")
	want := "First paragraph.\n\nSecond paragraph."
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestScriptAndStyleStripped(t *testing.T) {
	out := HTMLToText("<style>p { color: red }</style><p>Hello</p><script>alert(1)</script>")
	if out != "Hello" {
		t.Fatalf("out = %q, want %q", out, "Hello")
	}
}

func TestLineBreaks(t *testing.T) {
	out := HTMLToText("line one<br>line two<br/>line three")
	want := "line one\nline two\nline three"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestLinkFootnotes(t *testing.T) {
	out := HTMLToText(`<p>See <a href="https://example.test/docs">the docs</a> and <a href="https://example.test/docs">the docs</a> plus <a href="https://other.test">other</a>.</p>`)
	if !strings.Contains(out, "the docs [1]") {
		t.Fatalf("missing footnote marker: %q", out)
	}
	if !strings.Contains(out, "other [2]") {
		t.Fatalf("missing second marker: %q", out)
	}
	if strings.Count(out, "[1] https://example.test/docs") != 1 {
		t.Fatalf("footnote section wrong: %q", out)
	}
	// Repeated URL reuses its number instead of duplicating the footnote.
	if strings.Contains(out, "[3]") {
		t.Fatalf("duplicate footnote assigned: %q", out)
	}
	if !strings.HasSuffix(out, "[2] https://other.test") {
		t.Fatalf("footnotes not last: %q", out)
	}
}

func TestLinkWithoutHrefIsPlain(t *testing.T) {
	out := HTMLToText(`<a name="anchor">just text</a>`)
	if out != "just text" {
		t.Fatalf("out = %q", out)
	}
}

func TestEntitiesDecoded(t *testing.T) {
	out := HTMLToText("<p>Fish &amp; chips &mdash; 5 &lt; 6</p>")
	if out != "Fish & chips — 5 < 6" {
		t.Fatalf("out = %q", out)
	}
}

func TestListsBullets(t *testing.T) {
	out := HTMLToText("<ul><li>one</li><li>two<ul><li>nested</li></ul></li></ul>")
	want := "- one\n- two\n  - nested"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestPrePreserved(t *testing.T) {
	src := "<pre>func main() {\n\treturn\n}</pre>"
	out := HTMLToText(src)
	if !strings.Contains(out, "func main() {") || !strings.Contains(out, "\n\treturn") {
		t.Fatalf("pre content mangled: %q", out)
	}
}

func TestImageAltOnly(t *testing.T) {
	out := HTMLToText(`<p><img src="https://tracker.example.test/pixel.gif" alt="photo of a cat">text after</p>`)
	if !strings.Contains(out, "[image: photo of a cat]") {
		t.Fatalf("alt text missing: %q", out)
	}
	if strings.Contains(out, "tracker.example.test") {
		t.Fatalf("image src leaked into text: %q", out)
	}
}

func TestImageWithoutAltDropped(t *testing.T) {
	out := HTMLToText(`<p>a<img src="x.png">b</p>`)
	if out != "ab" {
		t.Fatalf("out = %q", out)
	}
}

func TestWhitespaceCollapsesWithoutFusing(t *testing.T) {
	out := HTMLToText("<p>foo <b>bar</b>   baz\n\nqux</p>")
	want := "foo bar baz qux"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestInlineSpacingAcrossTags(t *testing.T) {
	out := HTMLToText("<p>Visit <a href=\"https://x.test\">our site</a> today</p>")
	if !strings.Contains(out, "Visit our site [1] today") {
		t.Fatalf("out = %q", out)
	}
}

func TestTableRowsAndCells(t *testing.T) {
	out := HTMLToText("<table><tr><td>a</td><td>b</td></tr><tr><td>c</td><td>d</td></tr></table>")
	if !strings.Contains(out, "a  b") || !strings.Contains(out, "  c  d") {
		t.Fatalf("out = %q", out)
	}
}

func TestFragmentWithoutDocumentShell(t *testing.T) {
	out := HTMLToText("plain-ish <b>bold</b> tail")
	if out != "plain-ish bold tail" {
		t.Fatalf("out = %q", out)
	}
}

func TestEmptyInput(t *testing.T) {
	if out := HTMLToText(""); out != "" {
		t.Fatalf("out = %q, want empty", out)
	}
	if out := HTMLToText("<html><body></body></html>"); out != "" {
		t.Fatalf("out = %q, want empty", out)
	}
}

func TestHeadingsAreLines(t *testing.T) {
	out := HTMLToText("<h2>News</h2><p>body</p>")
	want := "News\n\nbody"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestExcessiveNewlinesCollapsed(t *testing.T) {
	out := HTMLToText("<div>a</div><div></div><div></div><div></div><div>b</div>")
	if strings.Count(out, "\n\n") != 1 || strings.Contains(out, "\n\n\n") {
		t.Fatalf("out = %q", out)
	}
}
