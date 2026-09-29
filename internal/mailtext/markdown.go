package mailtext

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// This file holds the markdown-only half of the walker: the emission
// helpers HTMLToMarkdown needs that the plain rendering does not.

// headingLevel returns the ATX level of a heading atom, or 0 when a is not
// a heading.
func headingLevel(a atom.Atom) int {
	switch a {
	case atom.H1:
		return 1
	case atom.H2:
		return 2
	case atom.H3:
		return 3
	case atom.H4:
		return 4
	case atom.H5:
		return 5
	case atom.H6:
		return 6
	}
	return 0
}

// quoteBlock prefixes every line of s with "> ", using a bare ">" for
// blank lines so the quote keeps its shape without trailing whitespace.
// The content arrives already escaped, so the markers we add here are the
// only unescaped ">" in the output.
func quoteBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ">"
			continue
		}
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// fenceFor returns a backtick fence long enough that the fenced content
// cannot close it early.
func fenceFor(s, base string) string {
	for strings.Contains(s, base) {
		base += "`"
	}
	return base
}

// escapeMD escapes the CommonMark metacharacters in a run of text-node
// content so sender-authored text cannot become structure: a heading, a
// list, an emphasis run, a table cell split, or raw HTML. Escaped
// characters render as themselves, so clean prose comes back unchanged.
//
// '&' is the exception: it round-trips as the entity &amp; rather than a
// backslash. The renderer runs html.UnescapeString over text nodes, which
// a backslash-escape cannot reach — it would decode &lt; back to '<' —
// while &amp; is decoded exactly once, restoring the character the HTML
// parser already decoded out of the original message.
//
// Block openers (#, -, +, =, and a leading "1.") only matter at the start
// of a line; text nodes never contain newlines after collapseSpace, so a
// segment start is the only place they can appear.
func escapeMD(s string) string {
	if s == "" {
		return s
	}
	if !mdNeedsEscape(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	i := 0
	switch s[0] {
	case '#', '-', '+', '=':
		b.WriteByte('\\')
		b.WriteByte(s[0])
		i = 1
	default:
		if s[0] >= '0' && s[0] <= '9' {
			j := 0
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			if j < len(s) && s[j] == '.' {
				b.WriteString(s[:j])
				b.WriteString("\\.")
				i = j + 1
			}
		}
	}
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case mdMeta(r):
			b.WriteByte('\\')
			b.WriteString(s[i : i+size])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// mdNeedsEscape reports whether s contains anything escapeMD would change.
// It lets the common case (plain prose) skip the copy entirely.
func mdNeedsEscape(s string) bool {
	if c := s[0]; c == '#' || c == '-' || c == '+' || c == '=' {
		return true
	}
	// '&' is not backslash-escaped (see escapeMD) but still needs the copy.
	if strings.ContainsRune(s, '&') {
		return true
	}
	if s[0] >= '0' && s[0] <= '9' {
		j := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j < len(s) && s[j] == '.' {
			return true
		}
	}
	for _, r := range s {
		if mdMeta(r) {
			return true
		}
	}
	return false
}

// mdMeta reports whether r must be backslash-escaped in markdown text.
// Every character here renders as itself once escaped. '&' is handled
// separately by escapeMD: it round-trips through an entity instead.
func mdMeta(r rune) bool {
	switch r {
	case '\\', '`', '*', '_', '[', ']', '<', '>', '|', '~':
		return true
	}
	return false
}

// tableMD renders a data table as a markdown pipe table. Rows are
// buffered, not streamed, because markdown needs the separator line in
// second position; cells arrive already escaped (their text nodes went
// through escapeMD), so a literal pipe in content stays literal.
func (c *converter) tableMD(n *html.Node) {
	c.paraBoundary()

	var rows [][]string
	for _, tr := range descendants(n, atom.Tr) {
		var cells []string
		for _, cell := range elementChildren(tr) {
			if cell.DataAtom != atom.Td && cell.DataAtom != atom.Th {
				continue
			}
			cells = append(cells, c.tableCell(cell))
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
		}
	}
	if len(rows) == 0 {
		c.paraBoundary()
		return
	}

	width := 0
	for _, r := range rows {
		if len(r) > width {
			width = len(r)
		}
	}
	sep := make([]string, width)
	for i := range sep {
		sep[i] = "---"
	}
	for i, r := range rows {
		for len(r) < width {
			r = append(r, "")
		}
		c.write("| " + strings.Join(r, " | ") + " |\n")
		if i == 0 {
			c.write("| " + strings.Join(sep, " | ") + " |\n")
		}
	}
	c.paraBoundary()
}

// tableCell renders one cell's children and flattens them to a single
// line: markdown table cells cannot span lines, and any block structure
// inside a cell reads better collapsed than as a grid break.
func (c *converter) tableCell(n *html.Node) string {
	inner := c.capture(func() {
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, false, 0)
		}
	})
	return strings.Join(strings.Fields(inner), " ")
}

// isDataTable reports whether n is worth rendering as a pipe table. Email
// is full of layout tables — spacers, sidebars, nested wrappers — and
// forcing those into a grid makes them worse, not better, so the checks
// are deliberately strict: consistent column counts, no colspan/rowspan
// (they only exist to fake layout), no nested tables, at least two
// columns, and either a header row or more than one row. Anything that
// fails falls through to block flow.
func isDataTable(n *html.Node) bool {
	if hasNestedTable(n) {
		return false
	}
	trs := descendants(n, atom.Tr)
	cols, used := -1, 0
	hasTH := false
	for _, tr := range trs {
		c := 0
		for _, cell := range elementChildren(tr) {
			if cell.DataAtom != atom.Td && cell.DataAtom != atom.Th {
				continue
			}
			if attr(cell, "colspan") != "" || attr(cell, "rowspan") != "" {
				return false
			}
			// A cell with nothing to read is a spacer, and spacer
			// cells are what make a layout table a layout table.
			if !hasTextContent(cell) {
				return false
			}
			c++
			if cell.DataAtom == atom.Th {
				hasTH = true
			}
		}
		if c == 0 {
			continue
		}
		used++
		if cols == -1 {
			cols = c
		} else if cols != c {
			return false
		}
	}
	if used == 0 || cols < 2 {
		return false
	}
	return hasTH || used > 1
}

// hasNestedTable reports whether any table sits below n's children.
func hasNestedTable(n *html.Node) bool {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode && ch.DataAtom == atom.Table {
			return true
		}
		if hasNestedTable(ch) {
			return true
		}
	}
	return false
}

// descendants collects every descendant element of n with the given atom,
// breadth-tracked in document order.
func descendants(n *html.Node, a atom.Atom) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		for ch := x.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type == html.ElementNode && ch.DataAtom == a {
				out = append(out, ch)
				continue
			}
			walk(ch)
		}
	}
	walk(n)
	return out
}

// elementChildren returns n's element children in document order.
func elementChildren(n *html.Node) []*html.Node {
	var out []*html.Node
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode {
			out = append(out, ch)
		}
	}
	return out
}

// hasTextContent reports whether n's subtree contains any character a
// reader could see: non-whitespace text, or an image with alt text.
func hasTextContent(n *html.Node) bool {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		switch ch.Type {
		case html.TextNode:
			if strings.TrimSpace(ch.Data) != "" {
				return true
			}
		case html.ElementNode:
			if ch.DataAtom == atom.Img && strings.TrimSpace(attr(ch, "alt")) != "" {
				return true
			}
			if hasTextContent(ch) {
				return true
			}
		}
	}
	return false
}
