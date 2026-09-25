package ui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// State is everything Render needs. The app builds it each frame from its
// model; golden tests build it from fixtures.
type State struct {
	Theme Theme
	Snap  sync.Snapshot

	Focus          Pane
	SidebarVisible bool
	ShowSize       bool
	HelpOpen       bool
	SidebarSel     int // sidebar cursor row index

	// VpView is the pre-rendered preview viewport (sized by the app via
	// ComputeLayout).
	VpView string

	// HelpSec is the generated binding list for the overlay (FR-I4).
	HelpSec HelpSection

	// Err flashes a non-fatal error line under the header.
	Err string

	// Selected marks multi-selected row ids (FR-G3): rendered with a
	// leading marker gutter in the list.
	Selected map[mail.ID]bool

	// Toast is the transient action-receipt line above the footer
	// (FR-G5); ToastHint, when set, renders the undo-key affordance.
	Toast     string
	ToastHint string

	// Picker is the mailbox picker modal (move/copy/archive destination).
	Picker *PickerView

	// FilePick is the attachment-save overlay (FR-E4): the app pre-renders
	// the bubbles filepicker and hands over the string.
	FilePick *FilePickView

	// Search is the query-bar line state (FR-F1); non-nil while a search
	// view is open — the header becomes the search line.
	Search *SearchView

	// AdvSearch is the advanced-search modal (FR-F2); non-nil while open.
	AdvSearch *AdvSearchView

	// Compose is the full-screen composer (FR-H1); non-nil while open. It
	// owns the frame, so it takes precedence over the other overlays.
	Compose *ComposeView

	// AccountSwitch is the account switcher modal (FR-A4, FR-I7);
	// non-nil while open. It takes the frame like the picker.
	AccountSwitch *SwitchView

	// Accounts lists configured accounts for multi-account chrome: the
	// switcher rows, the footer's per-account status (FR-I5), and badge
	// names. Empty keeps single-account rendering unchanged.
	Accounts []AccountView

	// Account is the active account's display name — the footer chip
	// shown when more than one account is configured.
	Account string

	// Unified marks the merged-inbox view (FR-A5): rows carry their
	// owning account (Row.Account) and render an account badge.
	Unified bool

	// AccountNames maps account id → display name for row badges (FR-A5).
	AccountNames map[string]string

	// Fullscreen hides the sidebar and list so the preview takes the full
	// frame (FR-E5).
	Fullscreen bool

	// Now anchors relative dates; injected for deterministic goldens.
	Now time.Time
}

// Pane widths per FR-I1: sidebar 24–30, the list takes 55% of what's left
// (clamped), the preview takes the remainder.
const (
	sidebarWidth = 26
	listMin      = 36
	listMax      = 60
)

// Layout is the resolved pane geometry for one frame.
type Layout struct {
	SidebarW int // 0 when the sidebar is not shown
	ListW    int // 0 when the list is not shown
	PreviewW int // 0 when the preview is not shown

	BodyH int // preview viewport height (0 when preview hidden)
}

// shownPanes decides which panes render for the given width and focus
// (FR-I1): three panes ≥ 100 cols, two panes 60–99 (preview swaps in for
// the list when focused), single focused pane below 60. Fullscreen mode
// (FR-E5) shows the preview alone at any width.
func shownPanes(w int, st State) (sidebar, list, preview bool) {
	if st.Fullscreen {
		return false, false, true
	}
	if w < 60 {
		return st.Focus == PaneSidebar && st.SidebarVisible,
			st.Focus == PaneList,
			st.Focus == PanePreview
	}
	if w < 100 {
		sidebar = st.SidebarVisible
		list = st.Focus != PanePreview
		preview = st.Focus == PanePreview
		return sidebar, list, preview
	}
	return st.SidebarVisible, true, true
}

// ComputeLayout resolves pane sizes for the frame. The app uses it to size
// the preview viewport; Render uses it to compose.
func ComputeLayout(w, h int, st State) Layout {
	var l Layout
	sidebar, list, preview := shownPanes(w, st)
	contentH := frameContentHeight(h, st)
	if st.HelpOpen {
		return Layout{}
	}
	if sidebar {
		l.SidebarW = sidebarWidth
	}
	rest := w - l.SidebarW
	listW := max(min(rest*55/100, listMax), listMin)
	if list && preview {
		l.ListW = listW
		if pw := rest - listW; pw > 0 {
			l.PreviewW = pw
		} else {
			l.ListW = rest
		}
	} else if list {
		l.ListW = rest
	} else if preview {
		l.PreviewW = rest
	}
	if l.PreviewW > 0 {
		// header block + hairline + attachments strip + error line budget
		l.BodyH = contentH - previewChrome(st)
		if l.BodyH < 1 {
			l.BodyH = 1
		}
	}
	return l
}

// previewChrome counts the preview's fixed lines for the given state.
func previewChrome(st State) int {
	n := len(previewHeader(st.Snap)) + 1 + 1 // header block + rule + strip
	if st.Err != "" {
		n++
	}
	return n
}

// Render composes one full frame, width w and height h.
func Render(w, h int, st State) string {
	if st.HelpOpen {
		return renderHelp(w, h, st)
	}
	if st.AccountSwitch != nil {
		return renderSwitch(w, h, st)
	}
	if st.Picker != nil {
		return renderPicker(w, h, st)
	}
	if st.FilePick != nil {
		return renderFilePick(w, h, st)
	}
	if st.AdvSearch != nil {
		return renderAdvSearch(w, h, st)
	}
	if st.Compose != nil {
		return renderCompose(w, h, st)
	}
	var b strings.Builder
	b.WriteString(renderHeader(w, st))
	b.WriteString("\n")

	l := ComputeLayout(w, h, st)
	sidebar, list, preview := shownPanes(w, st)

	var panes []string
	if l.SidebarW > 0 && sidebar {
		panes = append(panes, renderSidebar(l, contentHeight(h, st), st))
	}
	if l.ListW > 0 && list {
		panes = append(panes, renderList(l, contentHeight(h, st), st))
	}
	if l.PreviewW > 0 && preview {
		panes = append(panes, renderPreview(l, contentHeight(h, st), st))
	}
	b.WriteString(joinPanes(panes, st.Theme))
	b.WriteString("\n")
	if st.Toast != "" {
		b.WriteString(renderToast(w, st))
		b.WriteString("\n")
	}
	b.WriteString(renderFooter(w, st))
	return b.String()
}

func contentHeight(h int, st State) int { return frameContentHeight(h, st) }

// frameContentHeight is the pane-area height: header + footer, minus the
// toast line while one is visible (FR-G5).
func frameContentHeight(h int, st State) int {
	n := h - 2
	if st.Toast != "" {
		n--
	}
	return n
}

// renderFooter draws the status line (FR-I5): connection state, last-sync
// time, sync errors, and the active mailbox's counts. With several
// accounts it leads with the account identity chip (or "unified") and
// names every other account's error — the active account's own error
// keeps its right-aligned never-truncate treatment.
func renderFooter(w int, st State) string {
	th := st.Theme
	stt := st.Snap.Status
	multi := len(st.Accounts) > 1

	var mode string
	var modeStyle lipgloss.Style
	switch stt.Mode {
	case sync.ModePush:
		mode, modeStyle = "live", th.Success
	case sync.ModePoll:
		mode, modeStyle = "polling", th.Muted
	default:
		mode, modeStyle = "connecting…", th.Muted
	}
	var parts []string
	if multi {
		chip := st.Account
		if st.Unified {
			chip = "unified"
		}
		if chip != "" {
			parts = append(parts, th.Header.Render(chip))
		}
	}
	parts = append(parts, modeStyle.Render(mode))

	if !stt.LastSync.IsZero() {
		parts = append(parts, th.Muted.Render("synced "+stt.LastSync.Format("15:04:05")))
	}
	if stt.Attempts > 1 {
		parts = append(parts, th.Muted.Render(fmtInt(stt.Attempts)+" retries"))
	}
	if name := activeMailboxName(st); name != "" {
		counts := activeMailboxCounts(st)
		parts = append(parts, th.Muted.Render(name+" "+counts))
	}
	if st.Snap.NewAbove {
		parts = append(parts, th.Accent.Render("↑ new mail"))
	}
	line := strings.Join(parts, "  ")

	// Errors form the one segment that must never truncate away. With
	// several accounts each is named (unified names all; a normal view
	// names the others and keeps the active account's error nameless),
	// so a failing neighbour is always attributable (FR-I5).
	var errSegs []string
	switch {
	case multi:
		for _, a := range st.Accounts {
			if a.LastError == "" {
				continue
			}
			if a.Active && !st.Unified {
				errSegs = append(errSegs, a.LastError)
			} else {
				errSegs = append(errSegs, a.Name+": "+a.LastError)
			}
		}
	case stt.LastError != "":
		errSegs = append(errSegs, stt.LastError)
	}
	if len(errSegs) > 0 {
		budget := max(w-lipgloss.Width(line)-2, 0)
		errLine := th.Danger.Render(truncate(strings.Join(errSegs, " · "), budget))
		pad := w - lipgloss.Width(line) - lipgloss.Width(errLine)
		if pad > 0 {
			return line + strings.Repeat(" ", pad) + errLine
		}
		return errLine
	}
	return truncate(line, w)
}

// renderToast draws the action-receipt line (FR-G5): right-aligned above
// the footer, with the undo affordance in the accent colour.
func renderToast(w int, st State) string {
	th := st.Theme
	line := st.Toast
	if st.ToastHint != "" {
		line += "  " + th.Accent.Render(st.ToastHint)
	}
	used := lipgloss.Width(line)
	if used >= w {
		return truncate(line, w)
	}
	return strings.Repeat(" ", w-used) + line
}

// activeMailboxCounts renders "N unread · M total" for the open mailbox.
func activeMailboxCounts(st State) string {
	for _, mb := range st.Snap.Mailboxes {
		if mb.Mailbox.ID == st.Snap.ActiveMailbox {
			if mb.Mailbox.UnreadEmails > 0 {
				return fmtInt(mb.Mailbox.UnreadEmails) + " unread · " + fmtInt(mb.Mailbox.TotalEmails)
			}
			return fmtInt(mb.Mailbox.TotalEmails) + " total"
		}
	}
	return ""
}

// joinPanes composes panes side by side with a hairline rule between them.
// Panes are equal-height blocks; shorter panes keep blank rows.
func joinPanes(panes []string, th Theme) string {
	switch len(panes) {
	case 0:
		return ""
	case 1:
		return panes[0]
	}
	split := make([][]string, len(panes))
	widths := make([]int, len(panes))
	maxH := 0
	for i, p := range panes {
		split[i] = strings.Split(p, "\n")
		for _, ln := range split[i] {
			widths[i] = max(widths[i], lipgloss.Width(ln))
		}
		maxH = max(maxH, len(split[i]))
	}
	var out strings.Builder
	padStyle := lipgloss.NewStyle()
	for row := 0; row < maxH; row++ {
		for i := range split {
			ln := ""
			if row < len(split[i]) {
				ln = split[i][row]
			}
			out.WriteString(padStyle.Width(widths[i]).Render(ln))
			if i < len(split)-1 {
				out.WriteString(th.Rule.Render("│"))
			}
		}
		if row < maxH-1 {
			out.WriteString("\n")
		}
	}
	return out.String()
}

// renderHeader draws the breadcrumb line: title, active mailbox, totals.
// While a search view is open it becomes the query bar (FR-F1): the query
// tokens, the scope, and the result count.
func renderHeader(w int, st State) string {
	th := st.Theme
	var parts []string
	if st.Search != nil {
		parts = append(parts, th.Accent.Render("search"))
		q := st.Search.Query
		if q == "" && len(st.Search.Tokens) == 0 {
			q = "…"
		}
		parts = append(parts, th.Header.Render(q))
		for _, tok := range st.Search.Tokens {
			parts = append(parts, th.Muted.Render(tok))
		}
		parts = append(parts, th.Muted.Render("in: "+st.Search.Scope))
		if st.Search.Scanning {
			progress := "scanning…"
			if st.Search.ScanTotal >= 0 {
				progress = fmt.Sprintf("scanning %s/%s", fmtInt(st.Search.Scanned), fmtInt(st.Search.ScanTotal))
			}
			parts = append(parts, th.Muted.Render(progress))
		}
	} else {
		parts = append(parts, th.Accent.Render("jmap-tui"))
		if st.Unified {
			parts = append(parts, th.Header.Render("unified inbox"))
		} else if name := activeMailboxName(st); name != "" {
			parts = append(parts, th.Header.Render(name))
		}
	}
	if st.Snap.Total >= 0 {
		parts = append(parts, th.Muted.Render(renderCount(st)))
	}
	line := strings.Join(parts, "  ")
	if st.Err != "" {
		pad := w - lipgloss.Width(line) - lipgloss.Width(st.Err) - 2
		if pad > 0 {
			line += strings.Repeat(" ", pad) + th.Danger.Render(st.Err)
		} else {
			line = th.Danger.Render(st.Err)
		}
	} else if st.Search != nil {
		// The query bar is the one header that grows with user input —
		// keep it on one line at any width. Overflow sheds the tail
		// first (count, then scope, then tokens), ellipsizing the query
		// last.
		line = truncate(line, w)
	}
	return line
}

func renderCount(st State) string {
	unread := 0
	for _, mb := range st.Snap.Mailboxes {
		if mb.Mailbox.ID == st.Snap.ActiveMailbox {
			unread = mb.Mailbox.UnreadEmails
		}
	}
	if unread > 0 {
		return strings.TrimSpace(strings.Join([]string{itoa(st.Snap.Total), "messages", "·", itoa(unread), "unread"}, " "))
	}
	return strings.TrimSpace(strings.Join([]string{itoa(st.Snap.Total), "messages"}, " "))
}

func itoa(n int) string {
	if n < 0 {
		return ""
	}
	return fmtInt(n)
}

func fmtInt(n int) string {
	// small local itoa to keep imports tidy
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func activeMailboxName(st State) string {
	for _, mb := range st.Snap.Mailboxes {
		if mb.Mailbox.ID == st.Snap.ActiveMailbox {
			return mb.Mailbox.Name
		}
	}
	return ""
}
