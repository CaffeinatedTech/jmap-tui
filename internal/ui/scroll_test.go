package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// manyRowSnapshot is fixtureSnapshot with n numbered rows (distinct
// subjects, cursor wherever asked) and no load markers other than the
// fixture's, so row windows are predictable.
func manyRowSnapshot(n, cursor int) sync.Snapshot {
	snap := fixtureSnapshot()
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	snap.Rows = nil
	for i := 0; i < n; i++ {
		snap.Rows = append(snap.Rows, sync.Row{
			ID: mail.ID(fmt.Sprintf("e%03d", i)),
			Summary: mail.EmailSummary{
				ID:         mail.ID(fmt.Sprintf("e%03d", i)),
				From:       []mail.Address{{Name: "Sender", Email: "s@example.test"}},
				Subject:    fmt.Sprintf("row-%03d", i),
				ReceivedAt: now.Add(-time.Duration(i) * time.Minute),
				Keywords:   mail.Keywords{"$seen": {}},
			},
		})
	}
	snap.Cursor = cursor
	snap.Total = n
	return snap
}

// TestListScrollsToCursor: a list longer than the pane renders a window
// around the cursor — the selected row is always on screen, the frame
// never exceeds its geometry, and both ends clamp (FR-D1).
func TestListScrollsToCursor(t *testing.T) {
	const w, h = 120, 40
	// Side-by-side: contentH 38, rule - "↓ more" = 36 visible rows;
	// stacked: ListH 19 → 17 visible rows (fixture loads forward).
	cases := []struct {
		name       string
		stacked    bool
		cursor     int
		wantFirst  string
		wantLast   string
		wantAbsent string
	}{
		{name: "side-middle", cursor: 50, wantFirst: "row-032", wantLast: "row-067", wantAbsent: "row-031"},
		{name: "side-top", cursor: 0, wantFirst: "row-000", wantLast: "row-035", wantAbsent: "row-036"},
		{name: "side-end", cursor: 99, wantFirst: "row-064", wantLast: "row-099", wantAbsent: "row-063"},
		{name: "stacked-middle", stacked: true, cursor: 50, wantFirst: "row-042", wantLast: "row-058", wantAbsent: "row-041"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := baseState()
			st.Stacked = tc.stacked
			st.Focus = PaneList
			st.Snap = manyRowSnapshot(100, tc.cursor)

			frame := Render(w, h, st)
			lines := strings.Split(frame, "\n")
			if len(lines) != h {
				t.Errorf("frame has %d lines, want %d", len(lines), h)
			}
			maxW := 0
			for _, ln := range lines {
				maxW = max(maxW, ansi.StringWidth(ln))
			}
			if maxW > w {
				t.Errorf("widest line = %d, want ≤ %d", maxW, w)
			}
			text := ansi.Strip(frame)
			for _, want := range []string{tc.wantFirst, tc.wantLast} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in window", want)
				}
			}
			if strings.Contains(text, tc.wantAbsent) {
				t.Errorf("%q rendered outside the window", tc.wantAbsent)
			}
			// The cursor row itself is always visible.
			if !strings.Contains(text, fmt.Sprintf("row-%03d", tc.cursor)) {
				t.Errorf("cursor row %d not on screen", tc.cursor)
			}
		})
	}
}

// TestSidebarScrollsToSelection: a folder tree taller than the panel keeps
// the selected mailbox in view (same rule as the list).
func TestSidebarScrollsToSelection(t *testing.T) {
	st := baseState()
	st.Focus = PaneSidebar
	st.SidebarSel = 50
	st.Snap = manyRowSnapshot(1, 0)
	st.Snap.Mailboxes = nil
	for i := 0; i < 60; i++ {
		st.Snap.Mailboxes = append(st.Snap.Mailboxes, sync.MailboxNode{
			Mailbox: mail.Mailbox{ID: mail.ID(fmt.Sprintf("mb%03d", i)), Name: fmt.Sprintf("Box %03d", i)},
		})
	}

	text := ansi.Strip(Render(120, 40, st))
	if !strings.Contains(text, "Box 050") {
		t.Error("selected mailbox not on screen")
	}
	if !strings.Contains(text, "Box 059") {
		t.Error("tree did not scroll to the bottom of the panel")
	}
	if strings.Contains(text, "Box 023") {
		t.Error("rows above the window rendered")
	}
}

// TestPreviewClipsLongBody: an overlong body clips at the panel edge —
// the preview never spills past its allotted rows (the viewport itself
// is sized to BodyH by the app and scrolled with j/k or pgup/pgdn).
func TestPreviewClipsLongBody(t *testing.T) {
	st := baseState()
	st.Focus = PanePreview
	const w, h = 120, 40

	// Raw overlong content is clipped at the panel edge: the frame keeps
	// its geometry even if the viewport ever hands over too much.
	st.VpView = strings.Repeat("body line of text\n", 100)
	frame := Render(w, h, st)
	if lines := strings.Split(frame, "\n"); len(lines) != h {
		t.Errorf("frame has %d lines, want %d", len(lines), h)
	}

	// The real app hands vp.View() at exactly BodyH lines, so the
	// attachment strip lands inside the pane budget.
	st.VpView = strings.TrimRight(strings.Repeat("body line of text\n", ComputeLayout(w, h, st).BodyH), "\n")
	frame = Render(w, h, st)
	text := ansi.Strip(frame)
	if !strings.Contains(text, "1 attachment") {
		t.Error("attachment strip fell off the pane")
	}
	if !strings.Contains(text, "body line of text") {
		t.Error("body missing from the pane")
	}
	if lines := strings.Split(frame, "\n"); len(lines) != h {
		t.Errorf("frame has %d lines, want %d", len(lines), h)
	}
}
