package ui

import (
	"fmt"
	"image/color"
	"strings"
	"unicode/utf8"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"

	"github.com/CaffeinatedTech/jmap-tui/internal/mailtext"
)

// maxStyledBodyBytes is the guard on styling a body. Rendering costs
// about 1.6 ms per KB of markdown (BenchmarkStyledBody), so this caps
// the worst case at roughly a 100 ms frame — and only on the one frame
// that opens the body; resizes never render. Past it the structure is
// not worth the stall and the markdown source is shown instead. A normal
// email lands an order of magnitude below.
const maxStyledBodyBytes = 64 << 10

// RenderBody turns a body's markdown source into the ANSI string the
// preview viewport displays, painted with the theme's palette.
//
// The output is deliberately width-independent: glamour is asked not to
// wrap (WordWrap 0), so a body is rendered **once** on arrival and every
// later resize re-wraps it with ansi.Wrap exactly as a plain body does.
// That is not only the cheaper shape — glamour's own wrapping pads every
// line to the wrap width with a styled space per column, which measured
// ~10 ms/KB, four times the whole rest of the render — it is also the
// only shape that keeps resize off the render path.
//
// Failure — a renderer error, a pathological input — degrades to the
// sanitized source, which is readable plain text, never a blank pane.
//
// The renderer only ever adds SGR: the source arrives control-free from
// mailtext.HTMLToMarkdown, and the result goes through SanitizeStyled so
// the claim is verified rather than assumed.
func RenderBody(md string, p Palette) string {
	if md == "" {
		return ""
	}
	if len(md) > maxStyledBodyBytes {
		return Sanitize(md)
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithWordWrap(0),
		glamour.WithStyles(bodyStyle(p)),
		glamour.WithTableWrap(true),
		// Glamour collapses paragraph newlines to spaces by default,
		// which would rejoin the lines an email's <br> split apart.
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return Sanitize(md)
	}
	out, err := r.Render(md)
	if err != nil {
		return Sanitize(md)
	}
	// The gate runs first so everything after it is known-clean SGR:
	// trimTrailingPad only removes bytes and appends a reset.
	return trimTrailingPad(SanitizeStyled(strings.TrimRight(out, "\n")))
}

// bodyStyle is the preview pane's glamour stylesheet: one accent for
// structure (headings, links, code), the body colour for text, muted for
// quotes and footnotes. No margins — every column belongs to the pane —
// no borders, no backgrounds (FR-I2: minimal, one accent colour).
//
// Text deliberately carries no colour: glamour cascades the inline text
// style over its containing block, so a Text colour would repaint every
// heading and quote back to the body colour. The document colour is the
// base, and blocks override it.
func bodyStyle(p Palette) ansi.StyleConfig {
	accent, muted := hexColor(p.Accent), hexColor(p.Muted)
	body, rule := hexColor(p.BodyFg), hexColor(p.Rule)

	heading := ansi.StylePrimitive{
		Color: &accent,
		Bold:  boolPtr(true),
	}
	return ansi.StyleConfig{
		Document: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: &body}},

		// A heading is its own block: without the trailing break it
		// runs straight into the paragraph it titles.
		Heading: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{BlockSuffix: "\n"}},

		H1: ansi.StyleBlock{StylePrimitive: heading},
		H2: ansi.StyleBlock{StylePrimitive: heading},
		H3: ansi.StyleBlock{StylePrimitive: heading},
		H4: ansi.StyleBlock{StylePrimitive: heading},
		H5: ansi.StyleBlock{StylePrimitive: heading},
		H6: ansi.StyleBlock{StylePrimitive: heading},

		Strong:         ansi.StylePrimitive{Bold: boolPtr(true)},
		Emph:           ansi.StylePrimitive{Italic: boolPtr(true)},
		Strikethrough:  ansi.StylePrimitive{CrossedOut: boolPtr(true)},
		HorizontalRule: ansi.StylePrimitive{Color: &rule, Format: "\n---\n"},

		BlockQuote: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
			Color:  &muted,
			Italic: boolPtr(true),
		}},

		// The bullet is plain text; the colour comes from the parent
		// block so a quote's list stays quoted-coloured.
		Item:        ansi.StylePrimitive{Prefix: "- "},
		Enumeration: ansi.StylePrimitive{BlockSuffix: ". "},
		List:        ansi.StyleList{LevelIndent: 4},

		Link:     ansi.StylePrimitive{Color: &accent},
		LinkText: ansi.StylePrimitive{Color: &body},

		Code: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: &accent}},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: &muted}},
		},

		Table: ansi.StyleTable{
			StyleBlock:      ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: &body}},
			ColumnSeparator: strPtr("│"),
			CenterSeparator: strPtr("│"),
			RowSeparator:    strPtr("─"),
		},
	}
}

// hexColor renders c as the #rrggbb string glamour's stylesheet wants.
func hexColor(c color.Color) string {
	rgba := color.RGBAModel.Convert(c).(color.RGBA)
	return fmt.Sprintf("#%02x%02x%02x", rgba.R, rgba.G, rgba.B)
}

// trimTrailingPad drops the trailing spaces glamour leaves at the end of
// a line — table cells padded to their column width, an empty styled run
// after a hard break. It is invisible — no style in the pane sets a
// background — but the viewport carries every line of the body in memory
// on every frame, so nothing is kept that shows nothing. Content is
// untouched: only the tail of each line, past its last visible
// non-space, goes.
func trimTrailingPad(s string) string {
	if !strings.Contains(s, " ") {
		return s
	}
	lines := strings.Split(s, "\n")
	changed := false
	for i, ln := range lines {
		if t := trimLineRight(ln); t != ln {
			lines[i], changed = t, true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(lines, "\n")
}

// trimLineRight cuts everything after a line's last visible non-space
// rune, skipping escape sequences as it scans so a styled space is
// removed with the sequence that painted it. The kept part may then end
// mid-style, so a reset is appended when any styling survives.
func trimLineRight(ln string) string {
	cut := -1 // byte index just past the last rune worth keeping
	for i := 0; i < len(ln); {
		if ln[i] == 0x1b {
			i += mailtext.SeqLen(ln[i:])
			continue
		}
		r, size := utf8.DecodeRuneInString(ln[i:])
		if r != ' ' && r != '\t' {
			cut = i + size
		}
		i += size
	}
	if cut < 0 {
		return "" // nothing but padding
	}
	out := ln[:cut]
	if strings.ContainsRune(out, '\x1b') && !strings.HasSuffix(out, "\x1b[m") {
		out += "\x1b[m"
	}
	return out
}

func boolPtr(v bool) *bool    { return &v }
func strPtr(v string) *string { return &v }
