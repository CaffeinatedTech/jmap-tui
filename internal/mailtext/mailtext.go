// Package mailtext converts HTML email bodies to plain text (FR-E2). It is
// deliberately conservative: script/style content is stripped, block
// elements become line breaks, links are unwrapped with footnote URLs, and
// nothing is ever fetched from the network.
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

var (
	trailingSpaceRe = regexp.MustCompile(`[ \t]+\n`)
	newlineRunRe    = regexp.MustCompile(`\n{3,}`)
)

type converter struct {
	buf     strings.Builder
	pending int            // consecutive newlines just written (0, 1, or 2+)
	links   map[string]int // URL → footnote number
	order   []string       // footnote numbers in first-seen order
	list    []string       // list-context stack ("ul"/"ol")
}

// HTMLToText renders an HTML document or fragment as plain text. Block
// elements become line breaks, <br> becomes a newline, list items gain
// bullets, and hyperlinks are unwrapped with numbered footnote URLs
// appended at the end. Malformed input is handled leniently by the parser;
// conversion never fails and never touches the network. The result is run
// through Sanitize: HTML character references decode to their control
// characters after parsing (D-3), so the strip happens on the final text.
func HTMLToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		// The HTML5 parser is error-tolerant; treat an impossible parse
		// failure as empty content rather than showing raw HTML.
		return ""
	}
	c := &converter{links: map[string]int{}}
	c.walk(doc, false, 0)

	out := c.buf.String()
	out = trailingSpaceRe.ReplaceAllString(out, "\n")
	out = newlineRunRe.ReplaceAllString(out, "\n\n")
	out = strings.TrimSpace(out)

	if len(c.order) > 0 {
		var foot strings.Builder
		foot.WriteString("\n\n-- \n")
		for i, u := range c.order {
			fmt.Fprintf(&foot, "[%d] %s\n", i+1, u)
		}
		out += strings.TrimRight(foot.String(), "\n")
	}
	return Sanitize(out)
}

func (c *converter) walk(n *html.Node, inPre bool, depth int) {
	switch n.Type {
	case html.TextNode:
		if inPre {
			c.write(n.Data)
			return
		}
		c.write(collapseSpace(n.Data))
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

	case a == atom.Br:
		c.write("\n")

	case a == atom.Hr:
		c.paraBoundary()
		c.write("\n---\n")

	case a == atom.Pre:
		c.paraBoundary()
		c.write("\n")
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, true, depth)
		}
		c.write("\n")
		c.paraBoundary()

	case a == atom.Ul || a == atom.Ol:
		// A nested list continues its parent item's line flow; a top-level
		// list is its own paragraph block.
		if len(c.list) > 0 {
			c.blockBoundary()
		} else {
			c.paraBoundary()
		}
		c.list = append(c.list, a.String())
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, inPre, depth+1)
		}
		c.list = c.list[:len(c.list)-1]
		if len(c.list) > 0 {
			c.blockBoundary()
		} else {
			c.paraBoundary()
		}

	case a == atom.Li:
		c.blockBoundary()
		indent := strings.Repeat("  ", len(c.list)-1)
		c.write(indent + "- ")
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
	c.write("[image: " + collapseSpace(alt) + "]")
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
