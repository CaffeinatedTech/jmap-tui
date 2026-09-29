// Package mailtext converts HTML email bodies to text (FR-E2). It is
// deliberately conservative: script/style content is stripped, block
// elements become line breaks, links are unwrapped with footnote URLs, and
// nothing is ever fetched from the network.
//
// Two renderings share one walker:
//
//   - HTMLToText — plain text. It is what the composer quotes (FR-H1) and
//     what a non-styled view falls back to.
//   - HTMLToMarkdown — markdown for the preview pane, which the UI layer
//     then styles through glamour. Same traversal, same security posture;
//     only the emission points differ.
package mailtext

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// tags whose entire subtree is dropped.
var skipTags = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Head:     true,
	atom.Title:    true,
	atom.Noscript: true,
	atom.Template: true,
	atom.Svg:      true,
	atom.Iframe:   true,
	atom.Object:   true,
	atom.Select:   true,
	atom.Textarea: true,
}

// strong blocks are separated from surrounding content by a blank line;
// weak blocks (list items, table rows) just end their line.
var strongBlocks = map[atom.Atom]bool{
	atom.P:          true,
	atom.Div:        true,
	atom.H1:         true,
	atom.H2:         true,
	atom.H3:         true,
	atom.H4:         true,
	atom.H5:         true,
	atom.H6:         true,
	atom.Blockquote: true,
	atom.Table:      true,
	atom.Ul:         true,
	atom.Ol:         true,
	atom.Pre:        true,
	atom.Hr:         true,
	atom.Address:    true,
	atom.Article:    true,
	atom.Section:    true,
	atom.Header:     true,
	atom.Footer:     true,
	atom.Figure:     true,
	atom.Figcaption: true,
	atom.Dl:         true,
	atom.Form:       true,
	atom.Fieldset:   true,
	atom.Center:     true,
}

// weakBlocks end the current line only.
var weakBlocks = map[atom.Atom]bool{
	atom.Li: true,
	atom.Tr: true,
	atom.Dt: true,
	atom.Dd: true,
}

// hiddenByStyle reports whether an element's own style attribute says it
// is invisible — the preheader and tracking-pixel patterns email clients
// hide preview text and 1×1 images with. The checks are high-confidence
// substring matches against a whitespace-stripped, lowercased style: no
// CSS parsing and no selector matching, so a rule attached to a class can
// never hide visible content by accident.
func hiddenByStyle(style string) bool {
	if style == "" {
		return false
	}
	n := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return unicode.ToLower(r)
	}, style)
	if strings.Contains(n, "display:none") ||
		strings.Contains(n, "visibility:hidden") ||
		strings.Contains(n, "visibility:collapse") ||
		strings.Contains(n, "opacity:0") {
		return true
	}
	// A zero-height box still paints its overflow, so a height trick
	// only hides content when the box also clips.
	if strings.Contains(n, "overflow:hidden") || strings.Contains(n, "overflow:clip") {
		return strings.Contains(n, "height:0")
	}
	return false
}

var (
	trailingSpaceRe = regexp.MustCompile(`[ \t]+\n`)
	newlineRunRe    = regexp.MustCompile(`\n{3,}`)
	// hardBreakEdgeRe finds a markdown hard break (two trailing spaces)
	// sitting at the end of a block, where it would produce a stray
	// blank line instead of a line break.
	hardBreakEdgeRe = regexp.MustCompile(` +\n\n`)
)

type converter struct {
	buf      *strings.Builder
	pending  int            // consecutive newlines just written (0, 1, or 2+)
	links    map[string]int // URL → footnote number
	order    []string       // footnote numbers in first-seen order
	list     []string       // list-context stack ("ul"/"ol")
	counters []int          // ordered-list counters, one per open <ol> (md)
	md       bool           // emit markdown instead of plain text
	raw      int            // >0: text nodes are written verbatim (code spans)
}

func newConverter(md bool) *converter {
	return &converter{buf: &strings.Builder{}, links: map[string]int{}, md: md}
}

// HTMLToText renders an HTML document or fragment as plain text. Block
// elements become line breaks, <br> becomes a newline, list items gain
// bullets, and hyperlinks are unwrapped with numbered footnote URLs
// appended at the end. Malformed input is handled leniently by the parser;
// conversion never fails and never touches the network. The result is run
// through Sanitize: HTML character references decode to their control
// characters after parsing, so the strip happens on the final text.
func HTMLToText(src string) string {
	return convert(src, false)
}

// HTMLToMarkdown renders an HTML document or fragment as markdown for the
// preview pane: headings gain ATX markers, emphasis becomes */**, quotes
// and rules and fenced code get their block syntax, and text nodes are
// escaped so sender-authored markup cannot smuggle structure of its own.
// Link handling is unchanged from HTMLToText — the link text stays in
// place and URLs collect as numbered footnotes at the end — so the reader
// sees the same URLs the plain rendering shows.
//
// The output is run through Sanitize, so it carries no control characters:
// the only escapes a terminal ever sees downstream are the ones the
// renderer adds itself.
func HTMLToMarkdown(src string) string {
	return convert(src, true)
}

func convert(src string, md bool) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		// The HTML5 parser is error-tolerant; treat an impossible parse
		// failure as empty content rather than showing raw HTML.
		return ""
	}
	c := newConverter(md)
	c.walk(doc, false, 0)

	out := c.buf.String()
	if md {
		// Trailing spaces are load-bearing in markdown: they are the
		// hard break that <br> emits. Only a break left dangling at a
		// block's end is removed, where it would add a blank line
		// rather than break one.
		out = hardBreakEdgeRe.ReplaceAllString(out, "\n\n")
	} else {
		out = trailingSpaceRe.ReplaceAllString(out, "\n")
	}
	out = newlineRunRe.ReplaceAllString(out, "\n\n")
	out = strings.TrimSpace(out)

	if len(c.order) > 0 {
		// Markdown joins consecutive lines into one paragraph, so the
		// styled rendering separates the footnotes with a blank line
		// to keep each URL on a line of its own. Plain text already
		// has its newlines.
		head, sep := "\n\n-- \n", "\n"
		if md {
			head, sep = "\n\n-- \n\n", "\n\n"
		}
		var foot strings.Builder
		foot.WriteString(head)
		for i, u := range c.order {
			fmt.Fprintf(&foot, "[%d] %s%s", i+1, u, sep)
		}
		out += strings.TrimRight(foot.String(), "\n")
	}
	return Sanitize(out)
}

func (c *converter) walk(n *html.Node, inPre bool, depth int) {
	switch n.Type {
	case html.TextNode:
		if inPre || c.raw > 0 {
			c.write(n.Data)
			return
		}
		s := collapseSpace(n.Data)
		if c.md {
			s = escapeMD(s)
		}
		c.write(s)
		return
	case html.ElementNode:
		// fall through to tag handling below
	default:
		// comments, doctype: descend (children may hold content)
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
		return
	}

	a := n.DataAtom
	switch {
	case skipTags[a]:
		return

	case hiddenByStyle(attr(n, "style")):
		return

	case a == atom.Br:
		if c.md {
			// Two trailing spaces are CommonMark's hard break: a bare
			// newline between two words is a soft break, which joins
			// the lines back together and silently drops the <br>.
			c.write("  \n")
		} else {
			c.write("\n")
		}

	case a == atom.Hr:
		c.paraBoundary()
		c.write("\n---\n")

	case a == atom.Pre:
		c.paraBoundary()
		if c.md {
			inner := c.capture(func() {
				for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
					c.walk(ch, true, depth)
				}
			})
			inner = strings.Trim(inner, "\n")
			fence := fenceFor(inner, "```")
			c.write(fence + "\n" + inner + "\n" + fence)
		} else {
			c.write("\n")
			for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
				c.walk(ch, true, depth)
			}
			c.write("\n")
		}
		c.paraBoundary()

	case c.md && headingLevel(a) > 0:
		c.paraBoundary()
		c.write(strings.Repeat("#", headingLevel(a)) + " ")
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
		c.paraBoundary()

	case c.md && a == atom.Blockquote:
		c.paraBoundary()
		inner := c.capture(func() {
			for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
				c.walk(ch, inPre, depth)
			}
		})
		if inner = strings.TrimRight(inner, "\n"); inner != "" {
			c.write(quoteBlock(inner))
		}
		c.paraBoundary()

	case c.md && a == atom.Table:
		// Layout tables — the dominant shape in email — are not data:
		// isDataTable rejects them and they fall through to block flow,
		// which reads far better than a one-column grid of spacers.
		if isDataTable(n) {
			c.tableMD(n)
		} else {
			c.paraBoundary()
			for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
				c.walk(ch, inPre, depth)
			}
			c.paraBoundary()
		}

	case c.md && (a == atom.B || a == atom.Strong):
		c.emphasisMD("**", n, inPre, depth)

	case c.md && (a == atom.I || a == atom.Em):
		c.emphasisMD("*", n, inPre, depth)

	case c.md && a == atom.Code:
		c.codeSpanMD(n, inPre, depth)

	case c.md && (a == atom.Td || a == atom.Th):
		// Only reached when the enclosing table failed isDataTable:
		// each cell becomes its own line instead of a two-space gap.
		c.blockBoundary()
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
		c.blockBoundary()

	case a == atom.Ul || a == atom.Ol:
		// A nested list continues its parent item's line flow; a top-level
		// list is its own paragraph block.
		if len(c.list) > 0 {
			c.blockBoundary()
		} else {
			c.paraBoundary()
		}
		if c.md && a == atom.Ol {
			c.counters = append(c.counters, 0)
		}
		c.list = append(c.list, a.String())
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth+1)
		}
		c.list = c.list[:len(c.list)-1]
		if c.md && a == atom.Ol {
			c.counters = c.counters[:len(c.counters)-1]
		}
		if len(c.list) > 0 {
			c.blockBoundary()
		} else {
			c.paraBoundary()
		}

	case a == atom.Li:
		c.blockBoundary()
		c.write(c.listMarker())
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
		c.write("\n")

	case a == atom.Td || a == atom.Th:
		c.write("  ")
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}

	case a == atom.Img:
		c.image(n)

	case a == atom.A:
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
		if href := attr(n, "href"); href != "" {
			c.write(c.footnote(href))
		}

	case weakBlocks[a]:
		c.blockBoundary()
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
		c.blockBoundary()

	case strongBlocks[a]:
		c.paraBoundary()
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
		c.paraBoundary()

	default:
		// inline flow: just walk children
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
	}
}

// listMarker returns the indentation + bullet (or number) that opens the
// current list item. Markdown nests four spaces per level because an
// ordered marker like "10. " is four columns wide; plain text keeps the
// historical two.
func (c *converter) listMarker() string {
	unit := "  "
	if c.md {
		unit = "    "
	}
	indent := strings.Repeat(unit, max(len(c.list)-1, 0))
	if c.md && len(c.list) > 0 && c.list[len(c.list)-1] == "ol" && len(c.counters) > 0 {
		c.counters[len(c.counters)-1]++
		return fmt.Sprintf("%s%d. ", indent, c.counters[len(c.counters)-1])
	}
	return indent + "- "
}

// emphasisMD wraps the element's rendered children in a markdown emphasis
// marker. The children are captured first so the delimiters hug the text:
// CommonMark will not emphasise "** bar **", and the walker deliberately
// keeps boundary spaces on text nodes so adjacent inline nodes do not fuse.
func (c *converter) emphasisMD(mark string, n *html.Node, inPre bool, depth int) {
	inner := c.capture(func() {
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
	})
	body := strings.Trim(inner, " \t\n")
	if body == "" {
		return
	}
	lead := inner[:len(inner)-len(strings.TrimLeft(inner, " \t\n"))]
	tail := inner[len(strings.TrimRight(inner, " \t\n")):]
	c.write(lead + mark + body + mark + tail)
}

// codeSpanMD renders an inline <code> as a markdown code span. Children
// are captured raw — escaping inside backticks would show the backslashes.
func (c *converter) codeSpanMD(n *html.Node, inPre bool, depth int) {
	inner := c.capture(func() {
		c.raw++
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth)
		}
		c.raw--
	})
	if inner == "" {
		return
	}
	fence := "`"
	for strings.Contains(inner, fence) {
		fence += "`"
	}
	// A span whose content begins and ends with a space would lose one
	// space each side to CommonMark's stripping rule; pad it back.
	if strings.HasPrefix(inner, " ") && strings.HasSuffix(inner, " ") && strings.TrimSpace(inner) != "" {
		inner = " " + inner + " "
	}
	c.write(fence + inner + fence)
}

// capture runs f against a fresh buffer and returns what it wrote,
// restoring the converter's outer buffer and newline counter untouched.
// It is how a subtree is measured or transformed (quotes, table cells,
// emphasis) before it is committed to the output.
func (c *converter) capture(f func()) string {
	savedBuf, savedPending := c.buf, c.pending
	c.buf, c.pending = &strings.Builder{}, 0
	f()
	out := c.buf.String()
	c.buf, c.pending = savedBuf, savedPending
	return out
}

// write emits s and keeps the newline counter accurate: runs of only-
// newlines accumulate (capped), other content ends with 0 or 1 newline.
// Verbatim (pre) content may contain interior newlines — the counter only
// tracks the tail, and post-processing collapses any excess runs.
func (c *converter) write(s string) {
	c.buf.WriteString(s)
	if s == "" {
		return
	}
	if strings.Trim(s, "\n") == "" {
		c.pending += len(s)
		if c.pending > 2 {
			c.pending = 2
		}
		return
	}
	c.pending = 0
	if strings.HasSuffix(s, "\n") {
		c.pending = 1
	}
}

// blockBoundary ends the current line unless it is already ended or empty.
func (c *converter) blockBoundary() {
	if c.buf.Len() == 0 || c.pending >= 1 {
		return
	}
	c.buf.WriteString("\n")
	c.pending = 1
}

// paraBoundary guarantees exactly one blank line between blocks.
func (c *converter) paraBoundary() {
	if c.buf.Len() == 0 || c.pending >= 2 {
		return
	}
	c.buf.WriteString(strings.Repeat("\n", 2-c.pending))
	c.pending = 2
}

// image renders alt text in brackets; the image itself is never fetched.
func (c *converter) image(n *html.Node) {
	alt := strings.TrimSpace(attr(n, "alt"))
	if alt == "" {
		return
	}
	s := collapseSpace(alt)
	if c.md {
		s = escapeMD(s)
	}
	c.write("[image: " + s + "]")
}

// footnote returns the [n] marker for href, assigning numbers in first-seen
// order and reusing them for repeated URLs.
func (c *converter) footnote(href string) string {
	if idx, ok := c.links[href]; ok {
		return fmt.Sprintf(" [%d]", idx+1)
	}
	c.links[href] = len(c.order)
	c.order = append(c.order, href)
	return fmt.Sprintf(" [%d]", len(c.order))
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// collapseSpace collapses whitespace runs to single spaces while keeping
// boundary spaces so adjacent inline nodes do not fuse ("foo <b>bar</b>"
// must stay "foo bar", not "foobar").
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return b.String()
}
