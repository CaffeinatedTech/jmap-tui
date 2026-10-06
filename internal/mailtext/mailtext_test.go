package mailtext

import (
	"slices"
	"strings"
	"testing"
)

// TestLinkListMatchesFootnotes pins the contract the open-link picker
// relies on: the returned hrefs are numbered exactly as the [n] footnotes
// are, in first-seen order and de-duplicated the same way.
func TestLinkListMatchesFootnotes(t *testing.T) {
	src := `<p><a href="https://a.test">A</a> <a href="mailto:b@test">B</a> <a href="https://a.test">A again</a></p>`
	want := []string{"https://a.test", "mailto:b@test"}

	md, links := HTMLToMarkdownLinks(src)
	if !slices.Equal(links, want) {
		t.Fatalf("markdown links = %q, want %q", links, want)
	}
	if !strings.HasSuffix(md, "[2] mailto:b@test") {
		t.Fatalf("footnotes out of order: %q", md)
	}

	_, plainLinks := HTMLToTextLinks(src)
	if !slices.Equal(plainLinks, want) {
		t.Fatalf("plain links = %q, want %q", plainLinks, want)
	}
}

// TestLinkListSanitized proves a control character smuggled into an href
// never reaches the picker (it would be handed to the browser otherwise).
func TestLinkListSanitized(t *testing.T) {
	_, links := HTMLToTextLinks("<a href=\"https://x.test/\x01path\">x</a>")
	if len(links) != 1 {
		t.Fatalf("links = %q, want one", links)
	}
	if strings.ContainsRune(links[0], '\x01') {
		t.Fatalf("control character survived: %q", links[0])
	}
}

// TestFindURLs: bare URLs in a plain-text body are collected in order,
// de-duplicated, and stripped of the punctuation prose leaves on them.
func TestFindURLs(t *testing.T) {
	text := "Reset it at https://dashboard.test/reset?token=abc, then see\n" +
		"(https://wiki.test/Page_(disambiguation)) or mailto:help@test.\n" +
		"Again: https://dashboard.test/reset?token=abc"
	want := []string{
		"https://dashboard.test/reset?token=abc",
		"https://wiki.test/Page_(disambiguation)",
		"mailto:help@test",
	}
	if got := FindURLs(text); !slices.Equal(got, want) {
		t.Fatalf("FindURLs = %q, want %q", got, want)
	}
	if got := FindURLs("no links here"); len(got) != 0 {
		t.Fatalf("FindURLs = %q, want none", got)
	}
}

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

func TestOrphanListItemDoesNotPanic(t *testing.T) {
	// A malformed body can contain <li> with no enclosing list; the
	// nesting indent must clamp at zero rather than going negative.
	out := HTMLToText("<div><li>orphan</li></div>")
	if !strings.Contains(out, "- orphan") {
		t.Fatalf("out = %q, want it to contain %q", out, "- orphan")
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
