package ui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// AccountView is one account's render state: the switcher rows (FR-A4),
// the footer's per-account status (FR-I5), and the badge names (FR-A5).
type AccountView struct {
	ID        string
	Name      string
	Active    bool
	Mode      sync.Mode
	LastSync  time.Time
	LastError string
	Attempts  int
	Unread    int // inbox unread; 0 when not yet known
}

// SwitchView is the account switcher modal (FR-A4, FR-I7); non-nil while
// open. It swallows the keyboard like the other overlays.
type SwitchView struct {
	Accounts []AccountView
	Sel      int
}

// modeLabel is the footer/switcher word for a sync mode (FR-I5).
func modeLabel(m sync.Mode) string {
	switch m {
	case sync.ModePush:
		return "live"
	case sync.ModePoll:
		return "polling"
	default:
		return "connecting…"
	}
}

// statusText renders one account's compact status for the switcher:
// an error wins, otherwise mode + last sync + unread.
func statusText(a AccountView) string {
	if a.LastError != "" {
		return a.LastError
	}
	parts := []string{modeLabel(a.Mode)}
	if !a.LastSync.IsZero() {
		parts = append(parts, "synced "+a.LastSync.Format("15:04:05"))
	}
	if a.Attempts > 1 {
		parts = append(parts, itoa(a.Attempts)+" retries")
	}
	if a.Unread > 0 {
		parts = append(parts, itoa(a.Unread)+" unread")
	}
	return strings.Join(parts, " · ")
}

// renderSwitch draws the account switcher modal (FR-A4): one row per
// account with its live status, the active account marked, and a key
// hint. Selecting a row switches instantly — every enrolled engine is
// already warm.
func renderSwitch(w, h int, st State) string {
	th := st.Theme
	sw := st.AccountSwitch

	boxW := 52
	// title + hint + rule + items, clamped to the frame.
	boxH := min(len(sw.Accounts), 8) + 3
	boxH = min(boxH, max(h-2, 5))

	var b strings.Builder
	b.WriteString(th.Accent.Render(truncate("switch account", boxW-2)))
	b.WriteString("\n")
	b.WriteString(th.Muted.Render(truncate("enter switch · j/k move · esc close", boxW-2)))
	b.WriteString("\n")
	b.WriteString(th.Rule.Render(strings.Repeat("─", boxW-2)))
	b.WriteString("\n")

	shown := boxH - 3
	start := 0
	if sw.Sel >= shown {
		start = sw.Sel - shown + 1
	}
	for i := start; i < len(sw.Accounts) && i-start < shown; i++ {
		a := sw.Accounts[i]
		marker := "  "
		if a.Active {
			marker = th.Accent.Render("▸ ")
		}
		name := truncate(a.Name, boxW-6)
		line := marker + name
		// Status sits right-aligned in muted/danger on the same row.
		stt := statusText(a)
		sttStyle := th.Muted
		if a.LastError != "" {
			sttStyle = th.Danger
		}
		budget := boxW - 2 - lipgloss.Width(line) - 1
		if budget > 4 {
			line += " " + sttStyle.Render(truncate(stt, budget))
		}
		if i == sw.Sel {
			// The wash pads plain text (pad measures runewidth), so the
			// selected row renders unstyled — same as the picker.
			b.WriteString(th.RowSel.Render(pad(stripStyles(line), boxW-2)))
		} else {
			b.WriteString(th.Row.Render(line))
		}
		b.WriteString("\n")
	}

	block := lipgloss.NewStyle().Width(boxW).Height(boxH).Render(strings.TrimRight(b.String(), "\n"))
	return centerBlock(w, h, block)
}

// stripStyles removes ANSI escapes so a styled line can be measured and
// padded for the selection wash (pad works on plain text only).
func stripStyles(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
