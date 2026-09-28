package app

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
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

// --- issue #3: the top account is the default account ---

// TestMoveAccountBlockTopBecomesDefault: the account heading the sidebar
// order is the startup account — a reorder that changes the top rewrites
// default_account in config.toml (FR-C5, FR-J1's one app-written key),
// and moving it back flips the key again.
func TestMoveAccountBlockTopBecomesDefault(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`default_account = "work"

[accounts.work]
url = "https://work.example.com"
username = "work@example.com"

[accounts.personal]
url = "https://mail.example.com"
username = "me@example.com"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _, _ := newTwoAccountModel(t, func(o *Options) {
		o.ConfigPath = cfgPath
		o.DefaultAccount = "work"
	})
	loadAll(t, m)
	m.focus = ui.PaneSidebar
	if m.acctOrder[0] != "work" {
		t.Fatalf("setup: order = %v, want work first", m.acctOrder)
	}

	// Cursor inside personal's block: ctrl+up lifts it over work.
	m.sidebarKey = sidebarRowKey("personal", "mb-archive")
	_, cmd := m.handleKey(ctrlUp())
	if cmd != nil {
		t.Fatal("reorder produced a command (local state + file writes only)")
	}
	if m.acctOrder[0] != "personal" {
		t.Fatalf("order = %v, want personal first", m.acctOrder)
	}
	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DefaultAccount != "personal" {
		t.Errorf("default_account = %q, want personal (the new top)", got.DefaultAccount)
	}
	if m.defaultAccount != "personal" {
		t.Errorf("tracked default = %q, want personal", m.defaultAccount)
	}
	if m.err != "" {
		t.Errorf("unexpected error: %s", m.err)
	}

	// Ctrl+down moves it back and the default follows the top again.
	if _, cmd = m.handleKey(ctrlDown()); cmd != nil {
		t.Fatal("reorder produced a command (local state + file writes only)")
	}
	got, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after move back: %v", err)
	}
	if got.DefaultAccount != "work" {
		t.Errorf("default_account = %q, want work (back on top)", got.DefaultAccount)
	}
	if m.err != "" {
		t.Errorf("unexpected error: %s", m.err)
	}
}

// TestPinTopAccountDefaultOnlyWritesOnChange: an unchanged top leaves the
// file alone (no needless rewrite of a user-owned file), and a session
// with no config path — flags-only mode, tests — never even tries.
func TestPinTopAccountDefaultOnlyWritesOnChange(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("default_account = \"work\"\n\n[accounts.work]\nurl = \"https://work.example.com\"\nusername = \"work@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _, _ := newTwoAccountModel(t, func(o *Options) {
		o.ConfigPath = cfgPath
		o.DefaultAccount = "work"
	})
	before, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	m.acctOrder = []string{"work", "personal"}
	m.defaultAccount = "work"
	m.pinTopAccountDefault()
	after, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("config.toml rewritten although the top account never changed")
	}
	if m.err != "" {
		t.Errorf("unexpected error: %s", m.err)
	}

	// No config path: the tracked default must not move either.
	m.opts.ConfigPath = ""
	m.acctOrder = []string{"personal", "work"}
	m.defaultAccount = "work"
	m.pinTopAccountDefault()
	if m.defaultAccount != "work" {
		t.Errorf("tracked default = %q, want it untouched without a config path", m.defaultAccount)
	}
	if m.err != "" {
		t.Errorf("unexpected error: %s", m.err)
	}
}

// --- folding (FR-C6) ---

// foldFixtureModel wires a single account whose tree nests two folders
// (Work → Projects → Deep) and loads it headlessly, focused in the
// folder column.
func foldFixtureModel(t *testing.T) *Model {
	t.Helper()
	m, _ := newTestModelWith(t, []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 3, UnreadEmails: 2},
		{ID: "mb-work", Name: "Work", SortOrder: 1},
		{ID: "mb-proj", Name: "Projects", ParentID: "mb-work", SortOrder: 2, UnreadEmails: 1},
		{ID: "mb-deep", Name: "Deep", ParentID: "mb-proj", SortOrder: 3, UnreadEmails: 4},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 4},
	})
	pump(t, m, m.loadAccountCmd())
	m.focus = ui.PaneSidebar
	return m
}

// foldKey is a folder's row key on the fold fixture's account.
func foldKey(m *Model, mb mail.ID) string { return sidebarRowKey(m.activeID, mb) }

// pressFold presses h or l and asserts it stayed local state (FR-C6:
// no command, no network).
func pressFold(t *testing.T, m *Model, k string) {
	t.Helper()
	_, cmd := m.handleKey(key(k))
	if cmd != nil {
		t.Fatalf("%q produced a command (fold is local state only)", k)
	}
}

// TestFoldHidesSubtreeAndRollsUp: h on a folder folds its whole subtree
// away, the row's unread rolls up to the sum of what it hides, and the
// cursor stays on the row it folded; l unfolds it again and the count
// returns to the folder's own (FR-C6).
func TestFoldHidesSubtreeAndRollsUp(t *testing.T) {
	m := foldFixtureModel(t)
	m.sidebarKey = foldKey(m, "mb-work")

	if rows := m.sidebarRows(); !sidebarHasRow(rows, foldKey(m, "mb-proj")) {
		t.Fatal("setup: Projects not in the tree")
	}
	pressFold(t, m, "h")

	rows := m.sidebarRows()
	if sidebarHasRow(rows, foldKey(m, "mb-proj")) || sidebarHasRow(rows, foldKey(m, "mb-deep")) {
		t.Errorf("fold did not hide the subtree: %v", rowKeys(rows))
	}
	work := rows[sidebarIndexOf(rows, m.sidebarKey)]
	if work.Key != foldKey(m, "mb-work") {
		t.Fatalf("cursor = %q, want the folded Work row", work.Key)
	}
	if !work.Collapsed || !work.HasChildren {
		t.Errorf("Work row flags = collapsed %v / children %v, want both true", work.Collapsed, work.HasChildren)
	}
	if work.Unread != 5 {
		t.Errorf("folded Work unread = %d, want rolled-up 5 (1+4)", work.Unread)
	}
	// A sibling with no children keeps its own count.
	inbox := rows[sidebarIndexOf(rows, foldKey(m, "mb-inbox"))]
	if inbox.Unread != 2 {
		t.Errorf("Inbox unread = %d, want own count 2 (no rollup)", inbox.Unread)
	}

	pressFold(t, m, "l")
	rows = m.sidebarRows()
	if !sidebarHasRow(rows, foldKey(m, "mb-proj")) || !sidebarHasRow(rows, foldKey(m, "mb-deep")) {
		t.Errorf("l did not unfold the subtree: %v", rowKeys(rows))
	}
	work = rows[sidebarIndexOf(rows, foldKey(m, "mb-work"))]
	if work.Collapsed || work.Unread != 0 {
		t.Errorf("unfolded Work = collapsed %v unread %d, want open with own count 0", work.Collapsed, work.Unread)
	}
}

// TestFoldClimbChain: h folds when it can and otherwise climbs — leaf →
// parent → … → account header, folding each level on the way; a folded
// header has nowhere left to go (FR-C6).
func TestFoldClimbChain(t *testing.T) {
	m := foldFixtureModel(t)
	acct := m.activeID

	// Deep is a leaf: h climbs to Projects, its parent.
	m.sidebarKey = foldKey(m, "mb-deep")
	pressFold(t, m, "h")
	if m.sidebarKey != foldKey(m, "mb-proj") {
		t.Fatalf("h on a leaf = %q, want parent Projects", m.sidebarKey)
	}
	// Projects is expanded with children: h folds it (Deep hides).
	pressFold(t, m, "h")
	if rows := m.sidebarRows(); sidebarHasRow(rows, foldKey(m, "mb-deep")) {
		t.Error("h on an expanded folder did not fold it")
	}
	// Nothing left to fold: h climbs to Work.
	pressFold(t, m, "h")
	if m.sidebarKey != foldKey(m, "mb-work") {
		t.Fatalf("climb = %q, want Work", m.sidebarKey)
	}
	// Work is expanded: h folds it (Projects hides).
	pressFold(t, m, "h")
	if rows := m.sidebarRows(); sidebarHasRow(rows, foldKey(m, "mb-proj")) {
		t.Error("h on Work did not fold its subtree")
	}
	// Climb to the account header, then fold the whole account.
	pressFold(t, m, "h")
	if m.sidebarKey != sidebarRowKey(acct, "") {
		t.Fatalf("climb = %q, want the account header", m.sidebarKey)
	}
	pressFold(t, m, "h")
	rows := m.sidebarRows()
	for _, r := range rows {
		if r.Kind == ui.SidebarMailbox {
			t.Errorf("account fold left folder row %q visible", r.Key)
		}
	}
	if len(rows) != 1 || !rows[0].Collapsed {
		t.Fatalf("rows after account fold = %+v, want just the folded header", rows)
	}
	// The folded header is the end of the line: h has nowhere to go.
	pressFold(t, m, "h")
	if m.sidebarKey != sidebarRowKey(acct, "") {
		t.Errorf("h on a folded header moved the cursor to %q", m.sidebarKey)
	}
	// l unfolds the account again.
	pressFold(t, m, "l")
	if rows := m.sidebarRows(); !sidebarHasRow(rows, foldKey(m, "mb-inbox")) {
		t.Error("l on the header did not unfold the account")
	}
}

// TestExpandIsExpandOnly: l never opens anything and never moves the
// cursor — Enter is the sole key that opens a mailbox or activates an
// account (FR-C2, FR-C5 amended by FR-C6).
func TestExpandIsExpandOnly(t *testing.T) {
	m := foldFixtureModel(t)
	open := m.snap.ActiveMailbox
	m.sidebarKey = foldKey(m, "mb-inbox") // a leaf, nothing to expand

	pressFold(t, m, "l")
	if m.focus != ui.PaneSidebar {
		t.Errorf("focus = %v, want sidebar (l must not open or hand off)", m.focus)
	}
	if m.sidebarKey != foldKey(m, "mb-inbox") {
		t.Errorf("cursor = %q, want unmoved", m.sidebarKey)
	}
	if m.snap.ActiveMailbox != open {
		t.Errorf("mailbox changed to %q — l must never open (FR-C2)", m.snap.ActiveMailbox)
	}
	// An expanded parent is equally inert for l.
	m.sidebarKey = foldKey(m, "mb-work")
	pressFold(t, m, "l")
	if m.collapsed[foldKey(m, "mb-work")] {
		t.Error("l folded an open folder (expand-only means no-op)")
	}
}

// TestFoldAccountHidesOnlyItsBlock: folding one account's header hides
// that account's folders and nothing else; the next row takes the
// cursor's walk over (FR-C6, FR-C5 block integrity).
func TestFoldAccountHidesOnlyItsBlock(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)
	m.focus = ui.PaneSidebar
	m.sidebarKey = sidebarRowKey("work", "")

	pressFold(t, m, "h")
	rows := m.sidebarRows()
	for _, r := range rows {
		if r.AccountID == "work" && r.Kind != ui.SidebarAccount {
			t.Errorf("work folder %q survived the account fold", r.Key)
		}
	}
	if !sidebarHasRow(rows, sidebarRowKey("personal", "mb-archive")) {
		t.Error("personal's tree was folded too — folds are per account")
	}
	// j walks from the folded header into the next account's header.
	_, _ = m.handleKey(key("j"))
	if m.sidebarKey != sidebarRowKey("personal", "") {
		t.Errorf("j from the folded header = %q, want personal's header", m.sidebarKey)
	}
}

// TestFoldPersistsAcrossRestarts: the fold set round-trips prefs.toml
// (FR-C6, FR-J1) — folder and account folds restore independently, and a
// mailbox that left the tree is pruned instead of lingering (NFR-4: ids
// only, never names).
func TestFoldPersistsAcrossRestarts(t *testing.T) {
	m := foldFixtureModel(t)
	prefsPath := filepath.Join(t.TempDir(), "prefs.toml")
	m.opts.Prefs = &config.Prefs{}
	m.opts.PrefsPath = prefsPath

	m.sidebarKey = foldKey(m, "mb-work")
	pressFold(t, m, "h") // fold the folder
	m.sidebarKey = sidebarRowKey(m.activeID, "")
	pressFold(t, m, "h") // then fold the whole account

	got, err := config.LoadPrefs(prefsPath)
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if len(got.CollapsedAccounts) != 1 || got.CollapsedAccounts[0] != m.activeID {
		t.Errorf("CollapsedAccounts = %v, want [%s]", got.CollapsedAccounts, m.activeID)
	}
	if ids := got.CollapsedFolders[m.activeID]; len(ids) != 1 || ids[0] != "mb-work" {
		t.Errorf("CollapsedFolders[%s] = %v, want [mb-work]", m.activeID, ids)
	}

	// A mailbox id with no mailbox behind it prunes at the next save.
	m.collapsed[foldKey(m, "mb-ghost")] = true
	m.saveFoldState()
	if got, err = config.LoadPrefs(prefsPath); err != nil {
		t.Fatalf("LoadPrefs after prune: %v", err)
	}
	for _, ids := range got.CollapsedFolders {
		for _, id := range ids {
			if id == "mb-ghost" {
				t.Error("stale mailbox id not pruned at save time")
			}
		}
	}

	// Restart: the same prefs seed a fresh model.
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	m2 := New(Options{
		Accounts:  []AccountOpt{{ID: m.activeID, Name: "Work", Provider: m.opts.Provider, Connected: true}},
		Prefs:     got,
		PrefsPath: prefsPath,
		Keys:      km,
		Theme:     ui.NewTheme(ui.DarkTheme()),
	})
	m2.focus = ui.PaneSidebar
	pump(t, m2, m2.loadAccountCmd())

	rows := m2.sidebarRows()
	if len(rows) != 1 || !rows[0].Collapsed {
		t.Fatalf("restored rows = %+v, want only the folded account header", rows)
	}
	pressFold(t, m2, "l") // unfold the account — the folder fold is still there
	rows = m2.sidebarRows()
	if !sidebarHasRow(rows, foldKey(m2, "mb-inbox")) {
		t.Fatal("account fold did not restore")
	}
	work := rows[sidebarIndexOf(rows, foldKey(m2, "mb-work"))]
	if !work.Collapsed || sidebarHasRow(rows, foldKey(m2, "mb-proj")) {
		t.Errorf("folder fold did not restore: collapsed %v, Projects visible %v",
			work.Collapsed, sidebarHasRow(rows, foldKey(m2, "mb-proj")))
	}

	// Prefs naming an account this session does not enroll never seed
	// the fold set.
	stranger := New(Options{
		Accounts: []AccountOpt{{ID: "other", Name: "Other"}},
		Prefs:    &config.Prefs{CollapsedAccounts: []string{"ghost"}, CollapsedFolders: map[string][]string{"ghost": {"mb-x"}}},
		Keys:     km,
		Theme:    ui.NewTheme(ui.DarkTheme()),
	})
	if len(stranger.collapsed) != 0 {
		t.Errorf("unknown accounts seeded the fold set: %v", stranger.collapsed)
	}
}

// TestToggleSidebarHandsOffFocus: "[" hides the sidebar from anywhere —
// and focus never rides a hidden pane, so the hand-off to the list is
// part of the toggle itself (FR-C4, FR-C6: sidebar.close is unbound).
func TestToggleSidebarHandsOffFocus(t *testing.T) {
	m, _ := newTestModel(t)
	pump(t, m, m.loadAccountCmd())
	m.focus = ui.PaneSidebar

	_, _ = m.handleKey(key("["))
	if m.sidebarVisible {
		t.Fatal("sidebar still visible after [")
	}
	if m.focus != ui.PaneList {
		t.Errorf("focus = %v, want list (never ride a hidden pane)", m.focus)
	}
	_, _ = m.handleKey(key("["))
	if !m.sidebarVisible {
		t.Error("[ did not bring the sidebar back")
	}
}

// rowKeys lists visible row keys — test output helper.
func rowKeys(rows []ui.SidebarRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Key
	}
	return out
}

// TestFoldViaArrowKeys: the arrow aliases reach the same fold actions as
// h/l end-to-end — a real keypress decodes to "left"/"right" and folds
// or unfolds (FR-C6).
func TestFoldViaArrowKeys(t *testing.T) {
	m := foldFixtureModel(t)
	m.sidebarKey = foldKey(m, "mb-work")

	// right first: nothing folded, so expand is a no-op.
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyRight})
	if cmd != nil || m.collapsed[foldKey(m, "mb-work")] {
		t.Fatal("right did something before anything was folded")
	}
	// left folds.
	if _, cmd = m.handleKey(tea.KeyPressMsg{Code: tea.KeyLeft}); cmd != nil {
		t.Fatal("left fold produced a command")
	}
	if !m.collapsed[foldKey(m, "mb-work")] {
		t.Fatal("left did not fold Work")
	}
	// right unfolds.
	if _, cmd = m.handleKey(tea.KeyPressMsg{Code: tea.KeyRight}); cmd != nil {
		t.Fatal("right unfold produced a command")
	}
	if m.collapsed[foldKey(m, "mb-work")] {
		t.Fatal("right did not unfold Work")
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
