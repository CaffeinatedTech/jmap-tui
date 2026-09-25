package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

func keyShift(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModShift}
}

// TestKeymapV2Defaults pins the redesign's default bindings (KEYMAP_PLAN
// §5) at the resolver level.
func TestKeymapV2Defaults(t *testing.T) {
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	cases := []struct {
		pane ui.Pane
		key  string
		act  ui.Action
	}{
		{ui.PaneList, "space", ui.ActListPageDown},
		{ui.PaneList, "ctrl+d", ui.ActListHalfDown},
		{ui.PaneList, "ctrl+u", ui.ActListHalfUp},
		{ui.PaneList, "shift+j", ui.ActListNextUnread},
		{ui.PaneList, "shift+k", ui.ActListPrevUnread},
		{ui.PaneList, "d", ui.ActDelete},
		{ui.PaneList, "#", ui.ActDelete},
		{ui.PaneList, "e", ui.ActArchive},
		{ui.PaneList, "s", ui.ActSort},
		{ui.PaneList, "o", ui.ActSort},
		{ui.PaneList, "shift+s", ui.ActToggleSize},
		{ui.PaneList, "u", ui.ActToggleRead},
		{ui.PaneList, "enter", ui.ActToggleThread},
		{ui.PaneSidebar, "g", ui.ActSidebarTop},
		{ui.PaneSidebar, "shift+g", ui.ActSidebarBottom},
		{ui.PanePreview, "ctrl+f", ui.ActPreviewPageDown},
		{ui.PanePreview, "ctrl+b", ui.ActPreviewPageUp},
		{ui.PaneAny, "shift+a", ui.ActAccountSwitch},
		{ui.PaneAny, "esc", ui.ActSearchClear},
	}
	for _, tc := range cases {
		act, ok := km.Match(tc.pane, tc.key)
		if !ok || act != tc.act {
			t.Errorf("Match(%v, %q) = %v,%v want %v", tc.pane, tc.key, act, ok, tc.act)
		}
	}
	// Archive no longer hides behind h anywhere, and the list's h is
	// free (sidebar keeps h = collapse).
	if act, ok := km.Match(ui.PaneList, "h"); ok {
		t.Errorf("list h = %v, want unbound", act)
	}
}

// TestEscBackChain: esc leaves full-screen, then clears a selection
// (search-clear is covered by the search suite; modals intercept first).
func TestEscBackChain(t *testing.T) {
	m, _ := newTestModel(t)
	pump(t, m, m.loadAccountCmd())

	m.fullscreen = true
	m.focus = ui.PanePreview
	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.fullscreen {
		t.Error("esc did not leave full-screen view")
	}

	m.sel = map[mail.ID]bool{"e1": true}
	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if len(m.sel) != 0 {
		t.Error("esc did not clear the selection")
	}
}

// TestPreviewCtrlFPages: ctrl+f in the preview is a real page now, not
// the old 1-line scroll wearing a "page down" label.
func TestPreviewCtrlFPages(t *testing.T) {
	m, _ := newTestModel(t)
	pump(t, m, m.loadAccountCmd())

	m.focus = ui.PanePreview
	m.vp.SetContent(strings.Repeat("line of text\n", 100))
	m.vp.SetHeight(10)
	m.vp.GotoTop()
	_, _ = m.handleKey(keyCtrl('f'))
	if got := m.vp.YOffset(); got < 5 {
		t.Errorf("ctrl+f scrolled %d lines, want a page", got)
	}
	_, _ = m.handleKey(keyCtrl('b'))
	if m.vp.YOffset() != 0 {
		t.Errorf("ctrl+b offset = %d, want 0", m.vp.YOffset())
	}
}

// TestListHalfAndUnreadKeys: ctrl+d pages half a screen; J jumps the
// cursor to the next unread message (FR-D7).
func TestListHalfAndUnreadKeys(t *testing.T) {
	m, srv := newTestModel(t)
	// e2 read, e1 unread — the jump has a definite target.
	srv.SetEmails([]mockjmap.Email{
		{
			ID: "e2", ThreadID: "t2", MailboxIDs: []string{"mb-inbox"},
			Keywords:   map[string]bool{"$seen": true},
			From:       []mockjmap.Address{{Name: "Bob", Email: "bob@example.test"}},
			Subject:    "read one",
			ReceivedAt: time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
		},
		{
			ID: "e1", ThreadID: "t1b", MailboxIDs: []string{"mb-inbox"},
			From:       []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
			Subject:    "unread one",
			ReceivedAt: time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC),
		},
	})
	pump(t, m, m.loadAccountCmd())
	m.focus = ui.PaneList

	_, cmd := m.handleKey(keyCtrl('d'))
	pump(t, m, cmd)
	if m.snap.Cursor == 0 {
		t.Fatal("ctrl+d did not move the cursor")
	}

	m.snap = m.engine.MoveCursor(0)
	_, cmd = m.handleKey(keyShift('j'))
	pump(t, m, cmd)
	if m.snap.Cursor != 1 {
		t.Fatalf("J cursor = %d, want 1 (the unread row)", m.snap.Cursor)
	}

	// From the bottom, K walks back up; nothing unread above, so the
	// jump stops with the non-fatal notice.
	_, cmd = m.handleKey(keyShift('k'))
	pump(t, m, cmd)
	if m.snap.Cursor != 1 {
		t.Fatalf("K moved the cursor to %d, want it unchanged", m.snap.Cursor)
	}
	if m.err == "" {
		t.Error("expected a no-more-unread notice")
	}
}

// TestJumpUnreadPrefetchEdge: an unread beyond the loaded window is
// found because the engine force-extends while it scans (FR-D7) — one
// keypress, one cmd, bounded hops inside.
func TestJumpUnreadPrefetchEdge(t *testing.T) {
	m, srv := newTestModel(t)
	var emails []mockjmap.Email
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		emails = append(emails, mockjmap.Email{
			ID:         fmt.Sprintf("m%03d", i),
			ThreadID:   fmt.Sprintf("t%03d", i),
			MailboxIDs: []string{"mb-inbox"},
			From:       []mockjmap.Address{{Name: "Sender", Email: "s@example.test"}},
			Subject:    fmt.Sprintf("row %03d", i),
			ReceivedAt: base.Add(-time.Duration(i) * time.Minute),
			Keywords:   map[string]bool{"$seen": true},
			TextBody:   "body\n",
		})
	}
	// The oldest three are unread — beyond the first 50-row page.
	for i := 57; i < 60; i++ {
		emails[i].Keywords = map[string]bool{}
	}
	srv.SetEmails(emails)
	pump(t, m, m.loadAccountCmd())
	m.focus = ui.PaneList

	_, cmd := m.handleKey(keyShift('j'))
	pump(t, m, cmd)

	if m.snap.Cursor != 57 {
		t.Fatalf("cursor = %d, want 57 (first unread)", m.snap.Cursor)
	}
	if !strings.HasSuffix(string(m.snap.Rows[m.snap.Cursor].ID), "057") {
		t.Fatalf("cursor row = %s, want m057", m.snap.Rows[m.snap.Cursor].ID)
	}
}

// TestJumpUnreadAllRead: a fully-read mailbox exhausts the scan and says
// so with the non-fatal notice — no infinite prefetch loop.
func TestJumpUnreadAllRead(t *testing.T) {
	m, srv := newTestModel(t)
	var emails []mockjmap.Email
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		emails = append(emails, mockjmap.Email{
			ID:         fmt.Sprintf("m%03d", i),
			ThreadID:   fmt.Sprintf("t%03d", i),
			MailboxIDs: []string{"mb-inbox"},
			From:       []mockjmap.Address{{Name: "Sender", Email: "s@example.test"}},
			Subject:    fmt.Sprintf("row %03d", i),
			ReceivedAt: base.Add(-time.Duration(i) * time.Minute),
			Keywords:   map[string]bool{"$seen": true},
			TextBody:   "body\n",
		})
	}
	srv.SetEmails(emails)
	pump(t, m, m.loadAccountCmd())
	m.focus = ui.PaneList

	_, cmd := m.handleKey(keyShift('j'))
	pump(t, m, cmd)

	if m.err != "no more unread" {
		t.Fatalf("err = %q, want %q", m.err, "no more unread")
	}
	if m.snap.Cursor != 0 {
		t.Fatalf("cursor moved to %d, want 0", m.snap.Cursor)
	}
}

// TestSortFlow: s opens the picker, a choice re-queries server-side,
// remembers per account in prefs, and a fresh model boots with it.
func TestSortFlow(t *testing.T) {
	prefsPath := filepath.Join(t.TempDir(), "prefs.toml")
	m, _ := newTestModel(t)
	m.opts.Prefs = &config.Prefs{}
	m.opts.PrefsPath = prefsPath
	pump(t, m, m.loadAccountCmd())
	m.focus = ui.PaneList

	if m.snap.Rows[0].ID != "e2" {
		t.Fatalf("default order row0 = %s, want e2 (newest first)", m.snap.Rows[0].ID)
	}

	_, _ = m.handleKey(key("s"))
	if m.picker == nil || m.picker.mode != pickerSort {
		t.Fatalf("s did not open the sort picker: %+v", m.picker)
	}
	// "sender": newest(0) → oldest(1) → sender(2).
	_, _ = m.handleKey(key("j"))
	_, _ = m.handleKey(key("j"))
	_, cmd := m.handleKey(keyEnter())
	pump(t, m, cmd)

	if m.picker != nil {
		t.Fatal("picker stayed open after choose")
	}
	if m.snap.Rows[0].ID != "e1" {
		t.Fatalf("sender order row0 = %s, want e1 (alice < bob)", m.snap.Rows[0].ID)
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "Sorted by sender") {
		t.Fatalf("toast = %+v", m.toast)
	}
	data, err := os.ReadFile(prefsPath)
	if err != nil {
		t.Fatalf("prefs: %v", err)
	}
	if !strings.Contains(string(data), `sort = "sender"`) {
		t.Fatalf("prefs missing sort:\n%s", data)
	}

	// A fresh model restores the order from prefs: the mailbox opens
	// already sorted by sender.
	got, err := config.LoadPrefs(prefsPath)
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	fresh := New(Options{
		Provider: m.opts.Provider,
		Keys:     km,
		Theme:    ui.NewTheme(ui.DarkTheme()),
		Prefs:    got,
	})
	if fresh.sortByID[fresh.activeID] != "sender" {
		t.Fatalf("restored sort = %q, want sender", fresh.sortByID[fresh.activeID])
	}
	pump(t, fresh, fresh.loadAccountCmd())
	if fresh.snap.Rows[0].ID != "e1" {
		t.Fatalf("restored order row0 = %s, want e1", fresh.snap.Rows[0].ID)
	}

	// Unified refuses sort with a non-fatal notice (FR-D8).
	m.unified = true
	_, _ = m.handleKey(key("s"))
	if m.picker != nil {
		t.Fatal("sort picker opened over the unified view")
	}
	if !strings.Contains(m.err, "unified") {
		t.Fatalf("err = %q", m.err)
	}
}

// TestSidebarTreeJump: g/shift+g slam the folder tree to its ends.
func TestSidebarTreeJump(t *testing.T) {
	m, _ := newTestModel(t)
	pump(t, m, m.loadAccountCmd())
	m.focus = ui.PaneSidebar

	_, _ = m.handleKey(keyShift('g'))
	if want := len(m.snap.Mailboxes) - 1; m.sidebarSel != want {
		t.Errorf("shift+g sel = %d, want %d", m.sidebarSel, want)
	}
	_, _ = m.handleKey(key("g"))
	if m.sidebarSel != 0 {
		t.Errorf("g sel = %d, want 0", m.sidebarSel)
	}
}

// TestSizesOnShiftS: sizes moved to S.
func TestSizesOnShiftS(t *testing.T) {
	m, _ := newTestModel(t)
	pump(t, m, m.loadAccountCmd())
	m.focus = ui.PaneList

	before := m.showSize
	_, _ = m.handleKey(keyShift('s'))
	if m.showSize == before {
		t.Error("shift+s did not toggle sizes")
	}
}
