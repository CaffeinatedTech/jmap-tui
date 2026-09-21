package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// renderHelp draws the help overlay (FR-I4): every binding for the focused
// pane plus globals, auto-generated from the keymap.
func renderHelp(w, h int, st State) string {
	th := st.Theme
	var b strings.Builder

	title := "Help — " + paneTitle(st.Focus)
	b.WriteString(th.Accent.Render(title))
	b.WriteString("\n\n")

	width := min(max(w-4, 20), 78)
	for _, bind := range st.HelpSec.Bindings {
		key := th.HelpKey.Render(pad(bind.Key, 10))
		desc := th.HelpDesc.Render(bind.Help)
		b.WriteString("  " + key + "  " + desc + "\n")
	}

	body := b.String()
	lines := strings.Split(body, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	block := lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
	// center-ish placement
	padLeft := max((w-lipgloss.Width(block))/2, 0)
	padTop := max((h-len(lines))/2, 0)
	var out strings.Builder
	for i := 0; i < padTop; i++ {
		out.WriteString("\n")
	}
	for _, line := range lines {
		out.WriteString(strings.Repeat(" ", padLeft) + line + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

func paneTitle(p Pane) string {
	switch p {
	case PaneSidebar:
		return "mailboxes"
	case PaneList:
		return "message list"
	case PanePreview:
		return "message"
	default:
		return "all panes"
	}
}
