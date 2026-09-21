package ui

import (
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// truncate cuts s to display width n with an ellipsis.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= n {
		return s
	}
	return runewidth.Truncate(s, max(n-1, 0), "…")
}

// pad right-fills s with spaces to width n.
func pad(s string, n int) string {
	d := n - runewidth.StringWidth(s)
	if d <= 0 {
		return s
	}
	return s + strings.Repeat(" ", d)
}

// renderSidebar draws the mailbox tree (FR-C1): hierarchy indentation,
// unread counts, one accent on the active mailbox.
func renderSidebar(l Layout, h int, st State) string {
	th := st.Theme
	var b strings.Builder
	used := 0
	for i, node := range st.Snap.Mailboxes {
		if used >= h {
			break
		}
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
	fromW := min(18, max(w/3, 8))

	role := activeMailboxRole(st)

	if st.Snap.Total < 0 {
		// First frame skeleton (FR-D6): never a freeze, never blank.
		return th.Muted.Render("loading mailbox…")
	}

	rows := st.Snap.Rows
	cursor := st.Snap.Cursor

	var b strings.Builder
	used := 0
	if st.Snap.LoadBackward && used < h {
		b.WriteString(th.Muted.Render("↑ more"))
		b.WriteString("\n")
		used++
	}
	for i, r := range rows {
		if used >= h {
			break
		}
		unread := !r.Summary.Keywords.Has("$seen")
		sel := i == cursor
		focused := st.Focus == PaneList

		subjectW := w - (4 + 1) - fromW - 1 - dateW - sizeW
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

		// Styles: selection wash spans every segment; unread bolds the
		// sender and subject; date stays muted.
		base, dim := th.Row, th.RowSelDim
		if sel && focused {
			base, dim = th.RowSel, th.RowSel
		} else if sel {
			base, dim = th.RowSelDim, th.RowSelDim
		}
		fromStyle, subjStyle := base, base
		if unread && !sel {
			fromStyle = base.Bold(true)
			subjStyle = subjStyle.Bold(true)
		}
		dateStyle := dim
		if !sel {
			dateStyle = th.Muted
		}

		var line strings.Builder
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
	if st.Snap.LoadForward && used < h {
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
func previewHeader(snap sync.Snapshot) []string {
	var out []string
	if r, ok := cursorRow(snap); ok {
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

// renderPreview draws headers, the body viewport, and the attachments
// strip (FR-E1).
func renderPreview(l Layout, h int, st State) string {
	th := st.Theme
	w := l.PreviewW - 1
	var b strings.Builder
	used := 0

	if _, ok := cursorRow(st.Snap); ok {
		for _, line := range previewHeader(st.Snap) {
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
