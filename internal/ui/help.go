package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// colGap is the visible separator between help columns (" │ ").
const helpColGap = " │ "

// renderHelp draws the help panel (FR-I4): every command, grouped by
// purpose into labelled sections and laid out across as many columns as
// the frame allows, filling the whole screen. Keys come from the resolved
// keymap, so [keys] remaps show; the section order is fixed so the panel
// does not jump around between themes or remaps.
func renderHelp(w, h int, st State) string {
	th := st.Theme
	groups := st.HelpGroups

	head := th.Accent.Render("Keys")
	hint := "esc · ? · q to close"
	if st.Version != "" {
		hint += "   jmap-tui " + st.Version
	}
	var headLine string
	if gap := w - lipgloss.Width(head) - lipgloss.Width(hint); gap >= 2 {
		headLine = head + strings.Repeat(" ", gap) + th.Muted.Render(hint)
	} else {
		headLine = head + "  " + th.Muted.Render(hint)
	}

	if len(groups) == 0 {
		return strings.TrimRight(headLine, "\n")
	}

	// The key column is shared by every row so the descriptions line up.
	keyW := 0
	for _, g := range groups {
		for _, r := range g.Rows {
			keyW = max(keyW, lipgloss.Width(r.Keys))
		}
	}
	keyW = min(keyW, 12)

	// Use as many columns as fit the frame's width — all commands visible
	// with no wrapping — capped at four. If even one column is too wide,
	// fall back to a single truncated column. A short frame clips
	// vertically; it never wraps.
	avail := h - 1 // the header line
	layout := distribute(groups, 1)
	colW := columnWidths(layout, keyW)
	cols := 1
	for c := max(1, min(4, len(groups))); c >= 2; c-- {
		l := distribute(groups, c)
		cw := columnWidths(l, keyW)
		if layoutWidth(cw) <= w {
			cols, layout, colW = c, l, cw
			break
		}
	}

	// A single column wider than the frame: cut the descriptions rather
	// than let the terminal wrap them.
	helpBudget := 0
	if cols == 1 && colW[0] > w {
		helpBudget = max(w-2-keyW-2, 4)
		colW[0] = w
	}

	// Render each column to its own lines.
	colsLines := make([][]string, len(layout))
	for i, col := range layout {
		var lines []string
		for gi, g := range col {
			if gi > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, th.Accent.Render(g.Title))
			for _, r := range g.Rows {
				help := r.Help
				if helpBudget > 0 {
					help = truncate(help, helpBudget)
				}
				key := th.HelpKey.Render(pad(r.Keys, keyW))
				desc := th.HelpDesc.Render(help)
				lines = append(lines, "  "+key+"  "+desc)
			}
		}
		colsLines[i] = lines
	}

	bodyH := 0
	for _, l := range colsLines {
		bodyH = max(bodyH, len(l))
	}
	padTop := max((avail-bodyH)/2, 0)

	// Compose columns side by side, each padded to its own width, with a
	// hairline rule between them. Rows run to the tallest column.
	var out strings.Builder
	out.WriteString(headLine)
	out.WriteString("\n")
	for i := 0; i < padTop; i++ {
		out.WriteString("\n")
	}
	sep := th.Rule.Render(helpColGap)
	for row := 0; row < bodyH && row < avail-padTop; row++ {
		for i, l := range colsLines {
			if i > 0 {
				out.WriteString(sep)
			}
			var line string
			if row < len(l) {
				line = l[row]
			}
			out.WriteString(line)
			out.WriteString(strings.Repeat(" ", max(colW[i]-lipgloss.Width(line), 0)))
		}
		out.WriteString("\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// distribute splits groups into n contiguous, balanced columns — the
// order is preserved and no section is split. It minimises the tallest
// column (a linear partition), so groups do not pile into the last one.
func distribute(groups []HelpGroup, n int) [][]HelpGroup {
	if n < 1 {
		n = 1
	}
	if n > len(groups) {
		n = len(groups)
	}
	if n <= 1 {
		return [][]HelpGroup{groups}
	}
	const inf = 1 << 30

	cost := func(a, b int) int {
		h := b - a - 1 // blank lines between groups
		for i := a; i < b; i++ {
			h += len(groups[i].Rows) + 1
		}
		return h
	}

	dp := make([][]int, n+1)
	from := make([][]int, n+1)
	for c := range dp {
		dp[c] = make([]int, len(groups)+1)
		from[c] = make([]int, len(groups)+1)
	}
	for i := 1; i <= len(groups); i++ {
		dp[1][i] = cost(0, i)
	}
	for c := 2; c <= n; c++ {
		for i := c; i <= len(groups); i++ {
			dp[c][i] = inf
			for j := c - 1; j < i; j++ {
				if v := max(dp[c-1][j], cost(j, i)); v < dp[c][i] {
					dp[c][i] = v
					from[c][i] = j
				}
			}
		}
	}

	out := make([][]HelpGroup, n)
	end := len(groups)
	for c := n; c >= 1; c-- {
		start := 0
		if c > 1 {
			start = from[c][end]
		}
		out[c-1] = groups[start:end]
		end = start
	}
	return out
}

// columnWidths is the natural width of each column: its widest label.
func columnWidths(layout [][]HelpGroup, keyW int) []int {
	out := make([]int, len(layout))
	for i, col := range layout {
		for _, g := range col {
			out[i] = max(out[i], lipgloss.Width(g.Title))
			for _, r := range g.Rows {
				out[i] = max(out[i], 2+keyW+2+lipgloss.Width(r.Help))
			}
		}
	}
	return out
}

// layoutWidth is the full width the columns occupy, separators included.
func layoutWidth(colW []int) int {
	total := 0
	for _, w := range colW {
		total += w
	}
	return total + max(len(colW)-1, 0)*lipgloss.Width(helpColGap)
}

// paneTitle names a pane for keymap error messages (FR-I3).
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
