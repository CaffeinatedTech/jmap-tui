package app

import (
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// M8 tests (FR-C5): the multi-account folder column — row flow, cross-
// account open, header activation, and the ctrl+arrows block reorder.

// ctrlUp/ctrlDown are the keystrokes a terminal delivers for the account
// reorder keys.
func ctrlUp() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl}
}

func ctrlDown() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl}
}

// TestSidebarRowsFlowAccounts: one flat list — Work's header and tree,
// then Personal's — with tints keyed to enrollment order (identity, not
// position; FR-A5 companion to FR-C5).
func TestSidebarRowsFlowAccounts(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)

	rows := m.sidebarRows()
	var headers []int
	for i, r := range rows {
		if r.Kind == ui.SidebarAccount {
			headers = append(headers, i)
		}
	}
	if len(headers) != 2 {
		t.Fatalf("headers = %v, want exactly two (one per account)", headers)
	}
	w, p := headers[0], headers[1]
	if rows[w].AccountID != "work" || rows[w].Tint != 0 || !rows[w].Active {
		t.Errorf("first header = %+v, want work/tint0/active", rows[w])
	}
	if rows[p].AccountID != "personal" || rows[p].Tint != 1 || rows[p].Active {
		t.Errorf("second header = %+v, want personal/tint1/inactive", rows[p])
	}
	// Blocks never interleave: everything before the personal header
	// belongs to work, everything from it on to personal.
	for i, r := range rows {
		want := "work"
		if i >= p {
			want = "personal"
		}
		if r.AccountID != want {
			t.Errorf("row %d (%q) belongs to %q, want %q — blocks must not interleave", i, r.Key, r.AccountID, want)
		}
	}
	if len(rows) == p+1 {
		t.Error("personal block has a header but no tree")
	}
	// The cursor key spaces are distinct: header vs folder.
	if rows[w].Key == rows[p].Key {
		t.Fatal("account headers share a key")
	}
}

// TestOpenForeignFolderSwitchesAccount: Enter on another account's
// folder activates that account, opens the folder, and hands focus to the
// list (FR-C2 + FR-C5).
func TestOpenForeignFolderSwitchesAccount(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)
	if m.activeID != "work" {
		t.Fatalf("setup: active = %q, want work", m.activeID)
	}

	// Park the cursor on Personal's Archive.
	archive := sidebarRowKey("personal", "mb-archive")
	found := false
	for _, r := range m.sidebarRows() {
		if r.Key == archive {
			found = true
		}
	}
	if !found {
		t.Fatal("setup: personal has no Archive row")
	}
	m.sidebarKey = archive
	m.focus = ui.PaneSidebar

	_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)

	if m.activeID != "personal" {
		t.Errorf("active account = %q, want personal", m.activeID)
	}
	if m.snap.ActiveMailbox != "mb-archive" {
		t.Errorf("open mailbox = %q, want mb-archive", m.snap.ActiveMailbox)
	}
	if m.focus != ui.PaneList {
		t.Errorf("focus = %v, want list (FR-C2)", m.focus)
	}
	// The cursor stayed on the same folder through the switch.
	if got := sidebarIndexOf(m.sidebarRows(), m.sidebarKey); got < 0 || m.sidebarRows()[got].Key != archive {
		t.Errorf("cursor lost across the switch: key %q index %d", m.sidebarKey, got)
	}
}

// TestEnterOnAccountHeaderActivates: a header is a row you can act on —
// Enter activates that account and keeps the cursor in the folder column,
// so the walk continues (FR-C5).
func TestEnterOnAccountHeaderActivates(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)

	m.focus = ui.PaneSidebar
	m.sidebarKey = sidebarRowKey("personal", "")
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)

	if m.activeID != "personal" {
		t.Errorf("active account = %q, want personal", m.activeID)
	}
	if m.focus != ui.PaneSidebar {
		t.Errorf("focus = %v, want sidebar (stay in the tree)", m.focus)
	}

	// Enter on the already-active header is a no-op.
	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.activeID != "personal" {
		t.Errorf("active account = %q after re-enter, want personal", m.activeID)
	}
}

// TestMoveAccountBlockPersistsOrder: ctrl+down swaps the cursor's whole
// account block with the next one, the cursor rides along, prefs.toml
// remembers the arrangement (FR-J1), and the tints never move — colour is
// identity (FR-A5).
func TestMoveAccountBlockPersistsOrder(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)
	prefsPath := filepath.Join(t.TempDir(), "prefs.toml")
	m.opts.Prefs = &config.Prefs{}
	m.opts.PrefsPath = prefsPath
	m.focus = ui.PaneSidebar

	// Cursor anywhere inside Work's block.
	m.sidebarKey = sidebarRowKey("work", "mb-archive")
	_, cmd := m.handleKey(ctrlDown())
	if cmd != nil {
		t.Fatal("reorder produced a command (local state + file write only)")
	}
	if m.acctOrder[0] != "personal" || m.acctOrder[1] != "work" {
		t.Fatalf("order = %v, want personal first", m.acctOrder)
	}
	// Cursor followed its block past personal's.
	idx := sidebarIndexOf(m.sidebarRows(), m.sidebarKey)
	if idx < 0 || m.sidebarRows()[idx].AccountID != "work" {
		t.Fatalf("cursor did not ride its block: key %q index %d", m.sidebarKey, idx)
	}
	// The arrangement persisted.
	got, err := config.LoadPrefs(prefsPath)
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if len(got.AccountOrder) != 2 || got.AccountOrder[0] != "personal" || got.AccountOrder[1] != "work" {
		t.Fatalf("persisted order = %v", got.AccountOrder)
	}
	// Tints follow enrollment, not position.
	if m.accountTint("work") != 0 || m.accountTint("personal") != 1 {
		t.Errorf("tints moved with the reorder: work=%d personal=%d",
			m.accountTint("work"), m.accountTint("personal"))
	}
	// The switcher rows follow the display order too (FR-C5).
	if v := m.accountViews(); v[0].ID != "personal" {
		t.Errorf("switcher first = %q, want personal", v[0].ID)
	}

	// Work is last: ctrl+down has nowhere to go.
	_, _ = m.handleKey(ctrlDown())
	if m.acctOrder[0] != "personal" {
		t.Fatalf("bottom bound moved the block: %v", m.acctOrder)
	}
	// Ctrl+up swaps it back.
	_, _ = m.handleKey(ctrlUp())
	if m.acctOrder[0] != "work" || m.acctOrder[1] != "personal" {
		t.Fatalf("ctrl+up did not restore the order: %v", m.acctOrder)
	}
}

// TestAccountOrderRestoredAtStartup: prefs decide the sidebar sequence
// from the first frame; with no prefs the enrollment order stands.
func TestAccountOrderRestoredAtStartup(t *testing.T) {
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	m := New(Options{
		Accounts: []AccountOpt{{ID: "a", Name: "Alpha"}, {ID: "b", Name: "Beta"}},
		Prefs:    &config.Prefs{AccountOrder: []string{"b", "a"}},
		Keys:     km,
		Theme:    ui.NewTheme(ui.DarkTheme()),
	})
	rows := m.sidebarRows()
	if len(rows) < 2 || rows[0].AccountID != "b" || rows[1].AccountID != "a" {
		t.Fatalf("rows do not follow the saved order: %+v", rows)
	}
	// Saved order only re-sequences; it never recolours.
	if rows[0].Tint != 1 || rows[1].Tint != 0 {
		t.Errorf("tints keyed to enrollment expected beta=1 alpha=0, got %d/%d", rows[0].Tint, rows[1].Tint)
	}

	plain := New(Options{
		Accounts: []AccountOpt{{ID: "a", Name: "Alpha"}, {ID: "b", Name: "Beta"}},
		Keys:     km,
		Theme:    ui.NewTheme(ui.DarkTheme()),
	})
	if got := plain.sidebarRows(); got[0].AccountID != "a" {
		t.Errorf("default order starts with %q, want enrollment order (a)", got[0].AccountID)
	}
}
