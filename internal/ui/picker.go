package ui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// PickerView is the mailbox-picker modal's render state (FR-G2, FR-G4):
// move, copy, and archive-destination choice share it.
type PickerView struct {
	Title  string
	Filter string // type-to-filter substring ("" = show all)
	Items  []PickerItem
	Sel    int // index into Items (already filtered)
}

// PickerItem is one selectable mailbox row in the picker.
type PickerItem struct {
	ID    mail.ID
	Label string
	Depth int
}

// FilePickView is the attachment-save overlay's render state (FR-E4): the
// app owns the bubbles filepicker model and hands over its rendered view.
type FilePickView struct {
	Title string
	Path  string // directory currently browsed
	View  string // pre-rendered filepicker body
}

// renderPicker draws the modal mailbox chooser: title, type-to-filter
// line, and the mailbox list with a selection wash. Filtered-out items are
// hidden; the selection index refers to the filtered list.
func renderPicker(w, h int, st State) string {
	th := st.Theme
	p := st.Picker

	boxW := 34
	boxH := min(len(p.Items), 14) + 4 // title + filter + rule + items
	boxH = min(boxH, max(h-2, 5))

	var body strings.Builder
	body.WriteString(th.Accent.Render(truncate(p.Title, boxW-2)))
	body.WriteString("\n")
	if p.Filter != "" {
		body.WriteString(th.Muted.Render(truncate("find: "+p.Filter, boxW-2)))
	} else {
		body.WriteString(th.Muted.Render(truncate("type to filter · enter to choose", boxW-2)))
	}
	body.WriteString("\n")
	body.WriteString(th.Rule.Render(strings.Repeat("─", boxW-2)))
	body.WriteString("\n")

	shown := boxH - 3
	// Keep the selection visible in a scrolled list.
	start := 0
	if p.Sel >= shown {
		start = p.Sel - shown + 1
	}
	for i := start; i < len(p.Items) && i-start < shown; i++ {
		it := p.Items[i]
		indent := strings.Repeat("  ", it.Depth)
		line := truncate(indent+it.Label, boxW-4)
		if i == p.Sel {
			body.WriteString(th.RowSel.Render(pad(line, boxW-2)))
		} else {
			body.WriteString(th.Row.Render(line))
		}
		body.WriteString("\n")
	}

	block := lipgloss.NewStyle().Width(boxW).Height(boxH).Render(strings.TrimRight(body.String(), "\n"))
	return centerBlock(w, h, block)
}

// renderFilePick draws the attachment-save overlay: title, browsed path,
// and the pre-rendered filepicker body (FR-E4).
func renderFilePick(w, h int, st State) string {
	th := st.Theme
	fp := st.FilePick

	boxW := 56
	lines := strings.Split(strings.TrimRight(fp.View, "\n"), "\n")
	boxH := min(len(lines)+4, max(h-2, 6))

	var body strings.Builder
	body.WriteString(th.Accent.Render(truncate(fp.Title, boxW-2)))
	body.WriteString("\n")
	body.WriteString(th.Muted.Render(truncate(fp.Path, boxW-2)))
	body.WriteString("\n")
	body.WriteString(th.Rule.Render(strings.Repeat("─", boxW-2)))
	body.WriteString("\n")
	for i, ln := range lines {
		if i >= boxH-3 {
			break
		}
		body.WriteString(truncate(ln, boxW-2))
		body.WriteString("\n")
	}

	block := lipgloss.NewStyle().Width(boxW).Height(boxH).Render(strings.TrimRight(body.String(), "\n"))
	return centerBlock(w, h, block)
}

// centerBlock places a fixed-size block in the middle of the frame.
func centerBlock(w, h int, block string) string {
	lines := strings.Split(block, "\n")
	padLeft := max((w-lipgloss.Width(block))/2, 0)
	padTop := max((h-len(lines))/2, 0)
	var out strings.Builder
	for i := 0; i < padTop; i++ {
		out.WriteString("\n")
	}
	for _, line := range lines {
		out.WriteString(strings.Repeat(" ", padLeft))
		out.WriteString(line)
		out.WriteString("\n")
	}
	return strings.TrimRight(out.String(), "\n")
}
