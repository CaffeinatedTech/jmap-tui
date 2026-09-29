package mailtext

import (
	"strings"
	"testing"
)

func TestMDHeadings(t *testing.T) {
	cases := map[string]string{
		"<h1>Top</h1>":       "# Top",
		"<h2>News</h2>":      "## News",
		"<h6>Tiny</h6>":      "###### Tiny",
		"<h3>a<br>b</h3>":    "### a  \nb", // hard break, not a soft join
		"<h2>News</h2><p>x>": "## News\n\nx\\>",
	}
	for in, want := range cases {
		if got := HTMLToMarkdown(in); got != want {
			t.Errorf("HTMLToMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMDEmphasis(t *testing.T) {
	cases := map[string]string{
		"<p>a <b>bold</b> b</p>":         "a **bold** b",
		"<p>a <strong>bold</strong></p>": "a **bold**",
		"<p><i>it</i>al</p>":             "*it*al",
		"<p><em>x</em></p>":              "*x*",
		"<p>1 < 2 and 3 > 2</p>":         "1 \\< 2 and 3 \\> 2",
		// Boundary spaces stay outside the delimiters: CommonMark
		// will not emphasise "** bar **".
		"<p>foo<b> bar </b>baz</p>": "foo **bar** baz",
		// Empty emphasis emits nothing rather than stray markers.
		"<p>a <b></b> b</p>": "a  b",
	}
	for in, want := range cases {
		if got := HTMLToMarkdown(in); got != want {
			t.Errorf("HTMLToMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMDEscapingBlocksStructure(t *testing.T) {
	cases := map[string]string{
		"<p># not a heading</p>": "\\# not a heading",
		"<p>- not a list</p>":    "\\- not a list",
		"<p>1. not ordered</p>":  "1\\. not ordered",
		"<p>a * b _ c</p>":       "a \\* b \\_ c",
		"<p>[not a link](x)</p>": "\\[not a link\\](x)",
		"<p>`not code`</p>":      "\\`not code\\`",
		"<p>a | b</p>":           "a \\| b",
		"<p>~~not struck~~</p>":  "\\~\\~not struck\\~\\~",
		"<p>&lt;script&gt;</p>":  "\\<script\\>",
		"<p>&amp;amp;</p>":       "&amp;amp;", // one decode round-trips
		"<p>AT&amp;T</p>":        "AT&amp;T",
		"<p>5 &lt; 6</p>":        "5 \\< 6",
		"<p>2 * 3 = 6</p>":       "2 \\* 3 = 6",
		"<p>hello world</p>":     "hello world", // clean prose is untouched
		"<p>a=b</p>":             "a=b",         // only a line-start "=" opens a setext heading
	}
	for in, want := range cases {
		if got := HTMLToMarkdown(in); got != want {
			t.Errorf("HTMLToMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMDBreaksAreHard(t *testing.T) {
	// A soft break would let markdown rejoin the lines and silently
	// drop the sender's <br>.
	cases := map[string]string{
		"<p>Cheers,<br>The Team</p>":             "Cheers,  \nThe Team",
		"<p>a<br/>b<br/>c</p>":                   "a  \nb  \nc",
		"<li>x<br>y</li>":                        "- x  \ny",
		"<blockquote><p>a<br>b</p></blockquote>": "> a  \n> b",
	}
	for in, want := range cases {
		if got := HTMLToMarkdown(in); got != want {
			t.Errorf("HTMLToMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
	// A trailing <br> must not leave a dangling break (a stray blank line).
	if got := HTMLToMarkdown("<p>line<br></p><p>next</p>"); got != "line\n\nnext" {
		t.Errorf("trailing br = %q, want %q", got, "line\n\nnext")
	}
	// The plain rendering is unchanged.
	if got := HTMLToText("<p>a<br>b</p>"); got != "a\nb" {
		t.Errorf("HTMLToText br = %q, want %q", got, "a\nb")
	}
}

func TestMDLists(t *testing.T) {
	cases := map[string]string{
		"<ul><li>a</li><li>b</li></ul>":                    "- a\n- b",
		"<ol><li>a</li><li>b</li></ol>":                    "1. a\n2. b",
		"<ol><li>a<ul><li>b</li></ul></li></ol>":           "1. a\n    - b",
		"<ul><li>a<ol><li>b</li><li>c</li></ol></li></ul>": "- a\n    1. b\n    2. c",
		// A second ordered list restarts its numbering.
		"<ol><li>a</li></ol><ol><li>b</li></ol>": "1. a\n\n1. b",
	}
	for in, want := range cases {
		if got := HTMLToMarkdown(in); got != want {
			t.Errorf("HTMLToMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMDBlockquoteAndRule(t *testing.T) {
	if got, want := HTMLToMarkdown("<blockquote><p>one</p><p>two</p></blockquote>"), "> one\n>\n> two"; got != want {
		t.Errorf("blockquote = %q, want %q", got, want)
	}
	if got, want := HTMLToMarkdown("<hr>"), "---"; got != want {
		t.Errorf("hr = %q, want %q", got, want)
	}
	// The rule must not fuse with the paragraph above it.
	if got := HTMLToMarkdown("<p>x</p><hr><p>y</p>"); !strings.Contains(got, "x\n\n---\n\ny") {
		t.Errorf("hr did not separate blocks: %q", got)
	}
}

func TestMDPreFenced(t *testing.T) {
	got := HTMLToMarkdown("<pre>func main() {\n\treturn\n}</pre>")
	want := "```\nfunc main() {\n\treturn\n}\n```"
	if got != want {
		t.Fatalf("pre = %q, want %q", got, want)
	}
	// Content containing its own fence gets a longer one.
	got = HTMLToMarkdown("<pre>``` inside</pre>")
	if !strings.Contains(got, "````\n``` inside\n````") {
		t.Errorf("fence not extended: %q", got)
	}
}

func TestMDCodeSpan(t *testing.T) {
	if got, want := HTMLToMarkdown("<p>a <code>*x*</code> b</p>"), "a `*x*` b"; got != want {
		t.Errorf("code span = %q, want %q", got, want)
	}
	// Escaping must not leak into the span.
	if got, want := HTMLToMarkdown("<p>a <code>x < y</code> b</p>"), "a `x < y` b"; got != want {
		t.Errorf("code span escape = %q, want %q", got, want)
	}
}

func TestMDDataTable(t *testing.T) {
	got := HTMLToMarkdown("<table><tr><th>Name</th><th>Qty</th></tr><tr><td>Widget</td><td>3</td></tr></table>")
	want := "| Name | Qty |\n| --- | --- |\n| Widget | 3 |"
	if got != want {
		t.Fatalf("data table = %q, want %q", got, want)
	}
	// A literal pipe inside a cell stays inside its cell.
	got = HTMLToMarkdown("<table><tr><th>a</th><th>b</th></tr><tr><td>x|y</td><td>z</td></tr></table>")
	if !strings.Contains(got, "x\\|y") {
		t.Errorf("cell pipe not escaped: %q", got)
	}
}

func TestMDLayoutTableBecomesFlow(t *testing.T) {
	cases := []string{
		// Spacer cell: nothing to read.
		"<table><tr><td><img src='x'></td><td>text</td></tr><tr><td></td><td>more</td></tr></table>",
		// colspan is a layout device, not data.
		"<table><tr><td colspan='2'>banner</td></tr><tr><td>a</td><td>b</td></tr></table>",
		// Nested tables are wrappers.
		"<table><tr><td><table><tr><td>a</td><td>b</td></tr></table></td></tr></table>",
		// A lone row with no header is a strip, not a table.
		"<table><tr><td>left</td><td>right</td></tr></table>",
		// Ragged columns.
		"<table><tr><td>a</td><td>b</td></tr><tr><td>c</td></tr></table>",
		// Single column.
		"<table><tr><td>a</td></tr><tr><td>b</td></tr></table>",
	}
	for _, in := range cases {
		got := HTMLToMarkdown(in)
		if strings.Contains(got, "| ") {
			t.Errorf("layout table rendered as grid: %q\ninput: %s", got, in)
		}
		if !strings.Contains(got, "a") && strings.Contains(in, ">a<") {
			t.Errorf("content lost: %q\ninput: %s", got, in)
		}
	}
}

func TestMDLinksKeepFootnotes(t *testing.T) {
	got := HTMLToMarkdown(`<p>See <a href="https://example.test/docs">the docs</a> today</p>`)
	if !strings.Contains(got, "See the docs [1] today") {
		t.Fatalf("footnote marker missing: %q", got)
	}
	if !strings.Contains(got, "[1] https://example.test/docs") {
		t.Fatalf("footnote URL missing: %q", got)
	}
	// The URL is plain text, not a markdown link: nothing downstream
	// should re-parse it into an embeddable construct.
	if strings.Contains(got, "](https") {
		t.Errorf("markdown link syntax emitted: %q", got)
	}
}

func TestMDImagesAltOnly(t *testing.T) {
	got := HTMLToMarkdown(`<p><img src="https://t.test/p.gif" alt="a cat"> after</p>`)
	if !strings.Contains(got, "[image: a cat] after") {
		t.Errorf("alt text missing: %q", got)
	}
	if strings.Contains(got, "t.test") {
		t.Errorf("image src leaked: %q", got)
	}
}

func TestMDHiddenElementsDropped(t *testing.T) {
	cases := map[string]string{
		"<span style='display:none'>preheader text</span>Visible":           "Visible",
		"<div style=\"visibility: hidden\">gone</div>here":                  "here",
		"<table><tr><td style='display: none'>spacer</td></tr></table>real": "real",
		"<div style='height:0;overflow:hidden'>hidden</div>show":            "show",
		// A class-based rule we cannot evaluate must not hide anything.
		"<div class='hidden'>shown</div>": "shown",
		// overflow:hidden alone is a clipping container, not a hider.
		"<div style='overflow:hidden;width:300px'>kept</div>": "kept",
	}
	for in, want := range cases {
		if got := HTMLToMarkdown(in); got != want {
			t.Errorf("HTMLToMarkdown(%q) = %q, want %q", in, got, want)
		}
		if got := HTMLToText(in); got != want {
			t.Errorf("HTMLToText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMDScriptStyleStripped(t *testing.T) {
	got := HTMLToMarkdown("<style>p{color:red}</style><p>Hello</p><script>alert(1)</script>")
	if got != "Hello" {
		t.Fatalf("got %q, want %q", got, "Hello")
	}
}

func TestMDEmptyInput(t *testing.T) {
	if out := HTMLToMarkdown(""); out != "" {
		t.Fatalf("out = %q, want empty", out)
	}
	if out := HTMLToMarkdown("<html><body></body></html>"); out != "" {
		t.Fatalf("out = %q, want empty", out)
	}
}

// Plain conversion must be unaffected by the markdown mode: the composer
// quotes through HTMLToText and its output is a tested contract.
func TestMDPlainPathUnchanged(t *testing.T) {
	cases := map[string]string{
		"<h2>News</h2><p>body</p>":          "News\n\nbody",
		"<p>a <b>b</b> c</p>":               "a b c",
		"<ol><li>x</li><li>y</li></ol>":     "- x\n- y",
		"<blockquote><p>q</p></blockquote>": "q",
	}
	for in, want := range cases {
		if got := HTMLToText(in); got != want {
			t.Errorf("HTMLToText(%q) = %q, want %q", in, got, want)
		}
	}
}
