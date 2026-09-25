package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// truncate cuts s to display width n with an ellipsis (n-1 content cells
// + tail, so padded rows keep a gap before the next segment). ANSI-styled
// input is handled: escape sequences survive intact and only visible
// cells count against the budget — measuring with runewidth on styled
// strings over-truncates (it counts escape bytes).
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= n {
		return s
	}
	return ansi.Truncate(s, max(n-1, 1), "…")
}

// pad right-fills s with spaces to width n. Escape sequences are ignored
// for measurement (ANSI-styled callers pad mixed plain+styled lines), so
// the visible width lands on n either way.
func pad(s string, n int) string {
	d := n - ansi.StringWidth(s)
	if d <= 0 {
		return s
	}
	return s + strings.Repeat(" ", d)
}

// topRule draws a column's first line: a heavy accent rule under the
// focused pane, a hairline under the others. The glyph weight carries the
// focus signal on terminals where colour does not (FR-I1 companion).
func topRule(w int, focused bool, th Theme) string {
	if w <= 0 {
		return ""
	}
	if focused {
		return th.RuleActive.Render(strings.Repeat("━", w))
	}
	return th.Rule.Render(strings.Repeat("─", w))
}

// sidebarLabel draws the account label at the top of the folder column:
// accent-filled cells at both ends bracket the account name, so the line
// reads as chrome — it cannot be confused with a mailbox row, the
// selection wash, or the accent-coloured active mailbox (FR-A5 companion).
// The row is always emitted, even with an empty name, so the tree below
// never shifts between frames.
func sidebarLabel(w int, name string, th Theme) string {
	if w <= 0 {
		return ""
	}
	cell := lipgloss.NewStyle().Background(th.P.Accent).Render(" ")
	return cell + pad(th.SidebarLabel.Render(" "+name), w-2) + cell
}

// listTop picks the first visible row so a pane of avail rows keeps the
// cursor in view: centered when the list overflows the pane, clamped to
// both ends. Stateless — every frame derives it from the cursor alone
// (FR-D1: the selection can never run off the panel).
func listTop(rowCount, cursor, avail int) int {
	if avail <= 0 || rowCount <= avail {
		return 0
	}
	return max(0, min(cursor-avail/2, rowCount-avail))
}

// renderSidebar draws the mailbox tree (FR-C1): top rule, account label,
// then hierarchy indentation, unread counts, one accent on the active
// mailbox. The tree scrolls to keep the selected mailbox in view.
func renderSidebar(l Layout, h int, st State) string {
	th := st.Theme
	w := max(l.SidebarW-1, 0)
	var b strings.Builder
	used := 0
	if used < h {
		b.WriteString(topRule(w, st.Focus == PaneSidebar, th))
		b.WriteString("\n")
		used++
	}
	if used < h {
		b.WriteString(sidebarLabel(w, st.Account, th))
		b.WriteString("\n")
		used++
	}
	// Rule + label consumed two rows; the tree gets whatever is left.
	top := listTop(len(st.Snap.Mailboxes), st.SidebarSel, h-used)
	for i := top; i < len(st.Snap.Mailboxes); i++ {
		if used >= h {
			break
		}
		node := st.Snap.Mailboxes[i]
		indent := strings.Repeat("  ", node.Depth)
		name := node.Mailbox.Name
		plain := indent + name

		count := ""
		if node.Mailbox.UnreadEmails > 0 {
			count = fmtInt(node.Mailbox.UnreadEmails)
		}

		sel := i == st.SidebarSel
		if sel {
			// Selection wash covers the whole row; accent detail is
			// deliberately dropped under it (minimal, not rainbow).
			line := plain
			if count != "" {
				line = pad(line, l.SidebarW-1-len(count)) + count
			}
			style := th.RowSelDim
			if st.Focus == PaneSidebar {
				style = th.RowSel
			}
			b.WriteString(style.Render(pad(line, l.SidebarW-1)))
		} else {
			nameStyle := th.Row
			if node.Mailbox.ID == st.Snap.ActiveMailbox {
				nameStyle = th.Accent
			}
			fill := l.SidebarW - 1 - runewidth.StringWidth(plain) - runewidth.StringWidth(count)
			line := nameStyle.Render(plain)
			if fill > 0 {
				line += strings.Repeat(" ", fill)
			}
			if count != "" {
				line += th.Accent.Render(count)
			}
			b.WriteString(line)
		}
		b.WriteString("\n")
		used++
	}
	for used < h {
		b.WriteString(strings.Repeat(" ", max(l.SidebarW-1, 0)))
		b.WriteString("\n")
		used++
	}
	return strings.TrimRight(b.String(), "\n")
}

// flags renders the 4-cell flag column: unread dot, star, replied,
// attachment (FR-D1).
func flags(s mail.EmailSummary, th Theme) string {
	var unread, star, replied, att string
	if !s.Keywords.Has("$seen") {
		unread = th.Accent.Render("●")
	} else {
		unread = " "
	}
	if s.Keywords.Has("$flagged") {
		star = th.Accent.Render("★")
	} else {
		star = " "
	}
	if s.Keywords.Has("$answered") {
		replied = th.Muted.Render("↩")
	} else {
		replied = " "
	}
	if s.HasAttachment {
		att = th.Muted.Render("+")
	} else {
		att = " "
	}
	return unread + star + replied + att
}

// renderList draws the message list (FR-D1, FR-D3): flags, sender,
// subject with thread markers, optional size, date. Sent mailboxes show
// recipients in the sender column (FR-C3).
//
// Rows are composed from fixed-width segments measured on plain text —
// ANSI-styled strings never go through width math (escape bytes are not
// display width), so every row is exactly the pane width.
func renderList(l Layout, h int, st State) string {
	th := st.Theme
	w := l.ListW - 1 // rule column allowance
	sizeW := 0
	if st.ShowSize {
		sizeW = 6
	}
	dateW := 7
	// Unified rows (FR-A5) lead with a one-cell owner bar instead of the
	// old name badge: the tint says which account, the preview header
	// spells it out, and sender/subject keep their full widths.
	barW := 0
	if st.Unified {
		barW = 1
	}
	fromW := min(18, max(w/3, 8))

	role := activeMailboxRole(st)

	rows := st.Snap.Rows
	cursor := st.Snap.Cursor
	// Rows visible in the pane: rule + the two edge markers (drawn below)
	// are subtracted, so the window around the cursor is sized exactly —
	// the list scrolls with the cursor and never spills past the panel.
	avail := h - 1
	if st.Snap.LoadBackward {
		avail--
	}
	if st.Snap.LoadForward {
		avail--
	}
	top := 0
	if st.Snap.Total >= 0 {
		top = listTop(len(rows), cursor, avail)
	}
	end := min(top+max(avail, 0), len(rows))

	var b strings.Builder
	used := 0
	if used < h {
		b.WriteString(topRule(w, st.Focus == PaneList, th))
		b.WriteString("\n")
		used++
	}
	if st.Snap.Total < 0 {
		// First frame skeleton (FR-D6): never a freeze, never blank —
		// rendered under the top rule so the column chrome holds.
		b.WriteString(th.Muted.Render("loading mailbox…"))
		b.WriteString("\n")
		used++
	} else if st.Snap.LoadBackward && used < h {
		b.WriteString(th.Muted.Render("↑ more"))
		b.WriteString("\n")
		used++
	}
	for i := top; i < end; i++ {
		if st.Snap.Total < 0 || used >= h {
			break
		}
		r := rows[i]
		unread := !r.Summary.Keywords.Has("$seen")
		sel := i == cursor
		focused := st.Focus == PaneList

		// Row anatomy (FR-D1, FR-G3): owner bar (unified), selection
		// gutter, flag cells, sender, subject with thread markers,
		// optional size, date.
		subjectW := w - barW - 1 - (4 + 1) - fromW - 1 - dateW - sizeW
		if subjectW < 4 {
			subjectW = 4
		}

		from := truncate(DisplayName(fromColumn(r.Summary, role)), fromW)
		var prefix string
		switch {
		case r.ThreadHeader:
			prefix = "▾ "
		case r.ThreadMember:
			prefix = "  └ "
		}
		subject := prefix + truncate(r.Summary.Subject, subjectW-len(prefix))
		date := pad(RelativeDate(r.Summary.ReceivedAt, st.Now), dateW)

		mark := " "
		if st.Selected != nil && st.Selected[st.RowKey(r)] {
			mark = th.Accent.Render("×")
		}

		// Styles: selection wash spans every segment; a fresh (live-
		// arrived) row gets the wash without focus (the slide-in highlight,
		// PLAN §4.1 case 3); unread bolds sender and subject.
		base, dim := th.Row, th.RowSelDim
		fresh := r.Fresh
		if sel && focused {
			base, dim = th.RowSel, th.RowSel
			fresh = false
		} else if sel {
			base, dim = th.RowSelDim, th.RowSelDim
			fresh = false
		} else if fresh {
			base, dim = th.FreshRow, th.FreshRow
		}
		fromStyle, subjStyle := base, base
		if unread && !sel {
			fromStyle = base.Bold(true)
			subjStyle = subjStyle.Bold(true)
		}
		dateStyle := dim
		if !sel && !fresh {
			dateStyle = th.Muted
		}

		var line strings.Builder
		if barW > 0 {
			if tint, ok := st.tint(r); ok {
				line.WriteString(tint.Render(strings.Repeat(" ", barW)))
			} else {
				// Unknown owner: hold the column so rows stay aligned.
				line.WriteString(strings.Repeat(" ", barW))
			}
		}
		line.WriteString(mark)
		line.WriteString(flags(r.Summary, th))
		line.WriteString(" ")
		line.WriteString(fromStyle.Render(pad(from, fromW)))
		line.WriteString(" ")
		line.WriteString(subjStyle.Render(pad(subject, subjectW)))
		if sizeW > 0 {
			line.WriteString(dim.Render(pad(HumanSize(r.Summary.Size), sizeW)))
		}
		line.WriteString(dateStyle.Render(date))

		b.WriteString(line.String())
		b.WriteString("\n")
		used++
	}
	if st.Snap.Total >= 0 && st.Snap.LoadForward && used < h {
		b.WriteString(th.Muted.Render("↓ more"))
		b.WriteString("\n")
		used++
	}
	for used < h {
		b.WriteString("\n")
		used++
	}
	return strings.TrimRight(b.String(), "\n")
}

// fromColumn picks the address list to show: recipients for Sent (FR-C3).
func fromColumn(s mail.EmailSummary, role mail.Role) []mail.Address {
	if role == mail.RoleSent {
		return s.To
	}
	return s.From
}

func activeMailboxRole(st State) mail.Role {
	for _, mb := range st.Snap.Mailboxes {
		if mb.Mailbox.ID == st.Snap.ActiveMailbox {
			return mb.Mailbox.Role
		}
	}
	return ""
}

// previewHeader builds the fixed header lines of the preview pane (FR-E1).
// In the unified view the owning account leads (FR-A5): the row's colour
// bar is the glance, this is the text.
func previewHeader(st State) []string {
	var out []string
	if r, ok := cursorRow(st.Snap); ok {
		if st.Unified && r.Account != "" {
			name := st.AccountNames[r.Account]
			if name == "" {
				name = r.Account
			}
			out = append(out, "Account: "+name)
		}
		s := r.Summary
		add := func(label, value string) {
			if value != "" {
				out = append(out, label+": "+value)
			}
		}
		add("From", displayList(s.From))
		add("To", displayList(s.To))
		add("Date", s.ReceivedAt.Format("Mon, 02 Jan 2006 15:04"))
		out = append(out, "Subject: "+s.Subject)
	}
	return out
}

func displayList(addrs []mail.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Name != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", a.Name, a.Email))
		} else {
			parts = append(parts, a.Email)
		}
	}
	return strings.Join(parts, ", ")
}

func cursorRow(snap sync.Snapshot) (sync.Row, bool) {
	if snap.Cursor >= 0 && snap.Cursor < len(snap.Rows) {
		return snap.Rows[snap.Cursor], true
	}
	return sync.Row{}, false
}

// tint is the owner-bar style for a unified row (FR-A5); ok is false for
// an account outside the frame's index — the row renders a plain gap.
func (st State) tint(r sync.Row) (lipgloss.Style, bool) {
	i, ok := st.AccountIndex[r.Account]
	if !ok {
		return lipgloss.Style{}, false
	}
	return st.Theme.AccountTint(i), true
}

// RowKey is the selection key for a row: account-qualified in the unified
// view (JMAP ids are unique per account only), bare elsewhere (FR-G3,
// FR-A5). The app builds selection keys with the same rule.
func (st State) RowKey(r sync.Row) mail.ID {
	if st.Unified && r.Account != "" {
		return mail.ID(r.Account + "\x00" + string(r.ID))
	}
	return r.ID
}

// renderPreview draws headers, the body viewport, and the attachments
// strip (FR-E1).
func renderPreview(l Layout, h int, st State) string {
	th := st.Theme
	w := l.PreviewW - 1
	var b strings.Builder
	used := 0
	if used < h {
		b.WriteString(topRule(w, st.Focus == PanePreview, th))
		b.WriteString("\n")
		used++
	}

	if _, ok := cursorRow(st.Snap); ok {
		for _, line := range previewHeader(st) {
			if used >= h {
				break
			}
			b.WriteString(th.Header.Render(truncate(line, w)))
			b.WriteString("\n")
			used++
		}
	}
	if used < h {
		b.WriteString(th.Rule.Render(strings.Repeat("─", max(w, 1))))
		b.WriteString("\n")
		used++
	}

	// Body: the app pre-renders the viewport at exactly BodyH lines, so
	// pass it through untouched (no re-slicing, no newline surgery).
	if used < h {
		lines := strings.Split(strings.TrimRight(st.VpView, "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
		for _, line := range lines {
			if used >= h {
				break
			}
			b.WriteString(truncate(line, w))
			b.WriteString("\n")
			used++
		}
	}

	// attachments strip (FR-E1)
	if used < h {
		strip := attachmentStrip(st)
		if strip != "" {
			b.WriteString(th.Attachment.Render(truncate(strip, w)))
			b.WriteString("\n")
			used++
		}
	}
	for used < h {
		b.WriteString("\n")
		used++
	}
	return strings.TrimRight(b.String(), "\n")
}

func attachmentStrip(st State) string {
	if st.Snap.BodyLoading {
		return "loading message…"
	}
	if st.Snap.Body == nil {
		return ""
	}
	atts := st.Snap.Body.Attachments
	if len(atts) == 0 {
		return ""
	}
	names := make([]string, 0, len(atts))
	for _, a := range atts {
		names = append(names, fmt.Sprintf("%s (%s)", a.Name, HumanSize(a.Size)))
	}
	return fmt.Sprintf("%d attachment%s: %s", len(atts), plural(len(atts)), strings.Join(names, ", "))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
