package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// ComposeZone is one focusable field of the composer (FR-H1 header/body
// focus zones). From is not a zone: it is picked with a modal because an
// account has few identities and many recipients.
type ComposeZone int

// Composer zones, in Tab order.
const (
	ZoneTo ComposeZone = iota
	ZoneCc
	ZoneBcc
	ZoneSubject
	ZoneBody
	ZoneAttach
	zoneCount
)

// Next cycles through the zones in Tab order.
func (z ComposeZone) Next() ComposeZone {
	return (z + 1) % zoneCount
}

// Prev cycles backwards through the zones in Shift+Tab order.
func (z ComposeZone) Prev() ComposeZone {
	return (z + zoneCount - 1) % zoneCount
}

// ZoneName is the label rendered in the field gutter.
func (z ComposeZone) ZoneName() string {
	switch z {
	case ZoneTo:
		return "To"
	case ZoneCc:
		return "Cc"
	case ZoneBcc:
		return "Bcc"
	case ZoneSubject:
		return "Subject"
	case ZoneBody:
		return ""
	case ZoneAttach:
		return "Files"
	}
	return ""
}

// ComposeAttachment is one attachment row's render state (FR-H3): label is
// always set, Progress carries the in-flight percentage ("" when idle) and
// Failed the reason an upload did not finish.
type ComposeAttachment struct {
	Label    string
	Progress string
	Failed   string
}

// DiscardConfirm is the yes/no box shown when closing a dirty composer
// (FR-H4).
type DiscardConfirm struct {
	Title string
	Hint  string
}

// ComposeView is the full-screen composer's render state. The app owns the
// bubbles models and hands over rendered strings, exactly as the advanced
// search modal does.
type ComposeView struct {
	// Title names the mode: "new message", "reply", "reply all",
	// "forward", "edit draft".
	Title string
	From  string

	To      string
	Cc      string
	Bcc     string
	Subject string

	// Body is the pre-rendered textarea; its geometry comes from
	// ComposeBodySize so the app and the renderer agree.
	Body string

	Focus ComposeZone

	// Status is the composer's own line: "saving…", "saved 12:04", or a
	// local error (autosave failure, missing recipient, upload failure).
	Status string

	Attachments []ComposeAttachment

	// Hint is the key-hint footer, already themed by the app.
	Hint string

	// Discard is non-nil while the discard confirmation is up; the box
	// takes over the frame and the keyboard (FR-H4).
	Discard *DiscardConfirm
}

// composeChromeRows is the composer's fixed row count around the body:
// title+From, To, Cc, Bcc, Subject, hairline, attachments, hint. Keeping
// it constant lets the app size the textarea without negotiating with the
// renderer.
const composeChromeRows = 8

// composeLabelW is the width of the field label gutter.
const composeLabelW = 9

// ComposeLabelWidth exposes the gutter width so the app can size the
// header fields to exactly the space the renderer leaves.
func ComposeLabelWidth() int { return composeLabelW }

// ComposeBodySize returns the textarea geometry for a full-screen
// composer at w×h, so the app can size the bubbles textarea to exactly the
// space the renderer leaves.
func ComposeBodySize(w, h int) (width, height int) {
	return max(w-2, 1), max(h-composeChromeRows, 1)
}

// renderCompose draws the full-screen composer: header, header fields,
// hairline, body, attachment strip, and key hints — with any layering box
// (discard confirm, picker) centred on top.
func renderCompose(w, h int, st State) string {
	c := st.Compose
	th := st.Theme
	if c == nil {
		return ""
	}

	field := func(zone ComposeZone, label, value string) string {
		name := zone.ZoneName()
		if name == "" {
			return ""
		}
		style := th.Muted
		if c.Focus == zone {
			style = th.Accent
		}
		gutter := style.Render(pad(label, composeLabelW))
		if c.Focus == zone {
			return gutter + th.RowSel.Render(truncate(value, max(w-composeLabelW-1, 1)))
		}
		if value == "" {
			return gutter + th.Muted.Render(truncate("…", max(w-composeLabelW-1, 1)))
		}
		return gutter + th.Header.Render(truncate(value, max(w-composeLabelW-1, 1)))
	}

	var b strings.Builder
	b.WriteString(th.Accent.Render(pad(c.Title, composeLabelW)) +
		th.Header.Render(truncate(c.From, max(w-composeLabelW-1, 1))))
	b.WriteString("\n")
	b.WriteString(field(ZoneTo, "To", c.To))
	b.WriteString("\n")
	b.WriteString(field(ZoneCc, "Cc", c.Cc))
	b.WriteString("\n")
	b.WriteString(field(ZoneBcc, "Bcc", c.Bcc))
	b.WriteString("\n")
	b.WriteString(field(ZoneSubject, "Subject", c.Subject))
	b.WriteString("\n")
	b.WriteString(th.Rule.Render(strings.Repeat("─", max(w-1, 1))))
	b.WriteString("\n")

	_, bodyH := ComposeBodySize(w, h)
	b.WriteString(padLines(c.Body, w, bodyH))
	b.WriteString("\n")

	b.WriteString(renderComposeAttachments(c, w, th))
	b.WriteString("\n")

	status := c.Status
	if status != "" {
		b.WriteString(th.Muted.Render(truncate(status, max(w-lipgloss.Width(c.Hint)-2, 1))) +
			strings.Repeat(" ", max(w-lipgloss.Width(c.Hint)-lipgloss.Width(status)-2, 0)) + c.Hint)
	} else {
		b.WriteString(c.Hint)
	}

	frame := padLines(strings.TrimRight(b.String(), "\n"), w, h)
	// The discard confirmation is a true modal: it floats over the
	// composer, which stays visible behind it (FR-H4). The attachment
	// filepicker and the identity picker replace the frame, exactly as
	// they do in the reader.
	if c.Discard != nil {
		return overlayCenter(frame, renderDiscardConfirm(c.Discard, th), w, h)
	}
	if st.FilePick != nil {
		return renderFilePick(w, h, st)
	}
	if st.Picker != nil {
		return renderPicker(w, h, st)
	}
	return frame
}

// renderDiscardConfirm draws the yes/no box (FR-H4).
func renderDiscardConfirm(d *DiscardConfirm, th Theme) string {
	boxW := 54
	var b strings.Builder
	b.WriteString("  " + th.Danger.Render(truncate(d.Title, boxW-4)))
	b.WriteString("\n")
	b.WriteString("  " + th.Rule.Render(strings.Repeat("─", boxW-4)))
	b.WriteString("\n")
	b.WriteString("  " + th.Muted.Render(truncate(d.Hint, boxW-4)))
	return strings.TrimRight(b.String(), "\n")
}

// renderComposeAttachments renders the attachment strip: one row, either
// the count, per-file upload progress (FR-H3), or nothing.
func renderComposeAttachments(c *ComposeView, w int, th Theme) string {
	if len(c.Attachments) == 0 {
		return th.Muted.Render(pad("no attachments", composeLabelW)) +
			th.Muted.Render(truncate("", max(w-composeLabelW-1, 1)))
	}
	parts := make([]string, 0, len(c.Attachments))
	for _, a := range c.Attachments {
		label := a.Label
		switch {
		case a.Failed != "":
			label = th.Danger.Render(label + " failed")
		case a.Progress != "":
			label = th.Accent.Render(label + " " + a.Progress)
		}
		parts = append(parts, label)
	}
	focus := th.Muted
	if c.Focus == ZoneAttach {
		focus = th.Accent
	}
	line := focus.Render(pad("Files", composeLabelW)) + strings.Join(parts, th.Muted.Render(" · "))
	return truncate(line, w)
}

// padLines right-pads a block to exactly height lines and truncates each
// line to width, so the composer's geometry never depends on how much the
// user has typed and never spills past the frame.
func padLines(s string, width, height int) string {
	lines := strings.Split(s, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, ln := range lines {
		lines[i] = truncate(ln, width)
	}
	return strings.Join(lines, "\n")
}

// overlayCenter floats box over frame: the covered band of the frame is
// replaced, everything above and below stays visible.
func overlayCenter(frame, box string, w, h int) string {
	lines := strings.Split(padLines(frame, w, h), "\n")
	boxLines := strings.Split(box, "\n")
	boxW := 0
	for _, ln := range boxLines {
		boxW = max(boxW, lipgloss.Width(ln))
	}
	for i, ln := range boxLines {
		boxLines[i] = ln + strings.Repeat(" ", max(boxW-lipgloss.Width(ln), 0))
	}
	padLeft := max((w-boxW)/2, 0)
	padTop := max((h-len(boxLines))/2, 0)
	for i := range boxLines {
		row := padTop + i
		if row < 0 || row >= len(lines) {
			continue
		}
		lines[row] = truncate(strings.Repeat(" ", padLeft)+boxLines[i], w)
	}
	return strings.Join(lines, "\n")
}
