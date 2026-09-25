package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// keyCtrl builds a ctrl-modified keystroke (Keystroke "ctrl+<rune>").
func keyCtrl(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}

// keyEnter builds the enter keystroke (the shared key() helper only
// handles single runes).
func keyEnter() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyEnter}
}

// keyEsc builds the escape keystroke.
func keyEsc() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyEsc}
}

// triageTestModel is the standard reader model with the undo window shrunk
// so the headless pump never blocks on toast timers.
func triageTestModel(t *testing.T) *Model {
	t.Helper()
	old := toastTTL
	toastTTL = time.Millisecond
	t.Cleanup(func() { toastTTL = old })
	m, _ := newTestModel(t)
	pump(t, m, m.loadAccountCmd())
	return m
}

// rowIDs lists rendered row ids.
func rowIDs(m *Model) []mail.ID {
	var out []mail.ID
	for _, r := range m.snap.Rows {
		out = append(out, r.ID)
	}
	return out
}

// rowHasMailbox reports whether the rendered row for id shows the mailbox.
func rowHasMailbox(m *Model, id mail.ID, mb mail.ID) bool {
	for _, r := range m.snap.Rows {
		if r.ID == id {
			for _, cur := range r.Summary.MailboxIDs {
				if cur == mb {
					return true
				}
			}
		}
	}
	return false
}

func TestSelectThenBatchReadAndUndo(t *testing.T) {
	m := triageTestModel(t)

	// x selects the cursor row.
	_, _ = m.handleKey(key("x"))
	if len(m.sel) != 1 {
		t.Fatalf("selection = %v, want one row", m.sel)
	}

	// space marks the selection read in one batched action.
	_, cmd := m.handleKey(key(" "))
	if cmd == nil {
		t.Fatal("space produced no command")
	}
	pump(t, m, cmd)
	if len(m.sel) != 0 {
		t.Fatalf("selection survived the action: %v", m.sel)
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "Marked read") {
		t.Fatalf("toast = %+v", m.toast)
	}
	// The undo hint is rendered with the receipt.
	if m.toast.hint != "ctrl+z undo" || m.toast.undo == nil {
		t.Fatalf("undo affordance missing: %+v", m.toast)
	}

	// ctrl+z undoes while the toast lives: the receipt of the undo is a
	// fresh "Undone" toast (the reversal is itself reversible).
	_, cmd = m.handleKey(keyCtrl('z'))
	pump(t, m, cmd)
	if m.toast == nil || m.toast.text != "Undone" {
		t.Fatalf("toast after undo = %+v", m.toast)
	}
}

func TestMoveViaPicker(t *testing.T) {
	m := triageTestModel(t)

	_, _ = m.handleKey(key("m"))
	if m.picker == nil {
		t.Fatal("m did not open the picker")
	}
	// The active mailbox is excluded from move targets.
	for _, it := range m.picker.items {
		if it.ID == mail.ID("mb-inbox") {
			t.Fatal("picker offers the active mailbox for a move")
		}
	}

	// Type-to-filter, then choose Archive.
	_, _ = m.handleKey(key("a"))
	_, _ = m.handleKey(key("r"))
	if len(m.picker.items) == 0 || m.picker.items[0].Label != "Archive" {
		t.Fatalf("filter = %+v", m.picker.items)
	}
	_, cmd := m.handleKey(keyEnter())
	pump(t, m, cmd)

	if m.picker != nil {
		t.Fatal("picker stayed open after the choice")
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "Moved") {
		t.Fatalf("toast = %+v", m.toast)
	}
	// The confirmed summary (server truth) left the inbox.
	if rowHasMailbox(m, "e2", "mb-archive") {
		t.Fatal("moved message still shows the inbox membership")
	}
}

func TestArchiveUsesRoleMailbox(t *testing.T) {
	m := triageTestModel(t)

	_, cmd := m.handleKey(key("h"))
	pump(t, m, cmd)
	if m.picker != nil {
		t.Fatal("y opened the picker although an archive role exists")
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "Archived") {
		t.Fatalf("toast = %+v", m.toast)
	}
}

func TestArchivePromptRemembersChoice(t *testing.T) {
	// A server with no role-archive mailbox: y prompts via the picker,
	// remembers the choice in prefs, and archives (FR-G4, FR-J1).
	old := toastTTL
	toastTTL = time.Millisecond
	t.Cleanup(func() { toastTTL = old })

	prefsPath := filepath.Join(t.TempDir(), "prefs.toml")
	m, _ := newTestModelWith(t, []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0},
		{ID: "mb-hold", Name: "Hold", SortOrder: 1},
	})
	m.opts.Prefs = nil
	m.opts.PrefsPath = prefsPath
	m.opts.AccountID = "test"
	pump(t, m, m.loadAccountCmd())

	_, cmd := m.handleKey(key("h"))
	if cmd != nil || m.picker == nil {
		t.Fatal("y did not open the archive-destination picker")
	}
	// Choose the only non-active mailbox.
	_, _ = m.handleKey(key("j"))
	_, cmd = m.handleKey(keyEnter())
	pump(t, m, cmd)

	if m.toast == nil || !strings.Contains(m.toast.text, "Archived") {
		t.Fatalf("toast = %+v", m.toast)
	}
	// Server truth: the message now lives in the chosen mailbox (it has
	// left the inbox view, so ask the server).
	sums, err := m.opts.Provider.FetchSummaries(m.ctx, []mail.ID{"e2"})
	if err != nil || len(sums) != 1 {
		t.Fatalf("FetchSummaries: %v %v", err, sums)
	}
	found := false
	for _, mb := range sums[0].MailboxIDs {
		if mb == "mb-hold" {
			found = true
		}
	}
	if !found {
		t.Fatalf("server memberships = %v, want mb-hold", sums[0].MailboxIDs)
	}
	// The choice persisted to the app-managed prefs file.
	data, err := os.ReadFile(prefsPath)
	if err != nil {
		t.Fatalf("prefs file: %v", err)
	}
	if !strings.Contains(string(data), "mb-hold") {
		t.Fatalf("prefs file missing the choice: %s", data)
	}
	// A second archive goes straight to the remembered mailbox.
	_, cmd = m.handleKey(key("h"))
	pump(t, m, cmd)
	if m.picker != nil {
		t.Fatal("archive prompt reopened although prefs hold the choice")
	}
}

func TestDeleteToTrashThenPermanentDestroy(t *testing.T) {
	m := triageTestModel(t)

	// # in the inbox moves to trash (undoable).
	_, cmd := m.handleKey(key("#"))
	pump(t, m, cmd)
	if m.toast == nil || !strings.Contains(m.toast.text, "Deleted") {
		t.Fatalf("toast = %+v", m.toast)
	}

	// Inside Trash, # prepares the permanent destroy: the row hides now,
	// the server destroy waits for the undo window. The prepare is
	// synchronous; its commit Tick is NOT pumped here.
	pump(t, m, m.openMailbox("mb-trash"))
	if id := m.cursorID(); id == "" {
		t.Fatalf("trash view empty; rows=%v", rowIDs(m))
	}
	_, _ = m.handleKey(key("#"))
	if m.pendingDestroy == nil {
		t.Fatal("no pending destroy prepared")
	}
	if m.toast == nil || m.toast.hint != "ctrl+z cancel" {
		t.Fatalf("toast = %+v", m.toast)
	}
	if m.cursorID() != "" {
		t.Fatalf("prepared row still rendered: %q", m.cursorID())
	}

	// ctrl+z cancels: the row re-materialises.
	_, cmd = m.handleKey(keyCtrl('z'))
	pump(t, m, cmd)
	if m.pendingDestroy != nil {
		t.Fatal("cancel left the pending destroy armed")
	}
	if m.cursorID() == "" {
		t.Fatal("cancel did not restore the row")
	}

	// A second # commits after the (shrunk) undo window fires; this time
	// the timer is pumped through.
	_, cmd = m.handleKey(key("#"))
	pump(t, m, cmd)
	if m.pendingDestroy != nil {
		t.Fatal("destroy was not committed")
	}
	if id := m.cursorID(); id != "" {
		t.Fatalf("destroyed row still rendered: %q", id)
	}
}

func TestSaveAttachments(t *testing.T) {
	m := triageTestModel(t)
	dir := t.TempDir()
	saved := downloadsDirFn
	downloadsDirFn = func() string { return dir }
	t.Cleanup(func() { downloadsDirFn = saved })

	// The cursor message (e2) carries the attachment fixture.
	pump(t, m, m.loadBody(m.cursorID()))
	m.snap = m.engine.Snapshot()
	if m.snap.Body == nil || len(m.snap.Body.Attachments) == 0 {
		t.Fatal("fixture body with attachment did not load")
	}

	m.focus = ui.PanePreview
	_, cmd := m.handleKey(key("s"))
	if cmd == nil {
		t.Fatal("s did not open the save overlay")
	}
	pump(t, m, cmd) // filepicker Init readDir
	if m.fp == nil {
		t.Fatal("save overlay missing after open")
	}
	if m.fp.fp.CurrentDirectory != dir {
		t.Fatalf("picker root = %q, want %q", m.fp.fp.CurrentDirectory, dir)
	}

	// enter saves into the browsed directory.
	_, cmd = m.handleKey(keyEnter())
	pump(t, m, cmd)
	data, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil {
		t.Fatalf("saved file: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("saved content = %q", data)
	}
	if m.fp != nil {
		t.Fatal("save overlay stayed open after save")
	}

	// esc closes the overlay without saving.
	_, cmd = m.handleKey(key("s"))
	pump(t, m, cmd)
	_, _ = m.handleKey(keyEsc())
	if m.fp != nil {
		t.Fatal("esc did not close the save overlay")
	}
}

func TestSelectionClearsOnMailboxSwitch(t *testing.T) {
	m := triageTestModel(t)
	_, _ = m.handleKey(key("x"))
	if len(m.sel) != 1 {
		t.Fatalf("selection = %v", m.sel)
	}
	pump(t, m, m.openMailbox("mb-archive"))
	if len(m.sel) != 0 {
		t.Fatal("selection survived a mailbox switch")
	}
}

func TestUndoWithoutToastIsNoop(t *testing.T) {
	m := triageTestModel(t)
	_, cmd := m.handleKey(keyCtrl('z'))
	if cmd != nil {
		t.Fatal("undo with no toast produced a command")
	}
}

func TestKeywordDirectionFollowsState(t *testing.T) {
	m := triageTestModel(t)
	// e2 is unread: space marks read.
	_, cmd := m.handleKey(key(" "))
	pump(t, m, cmd)
	if m.toast == nil || !strings.Contains(m.toast.text, "Marked read") {
		t.Fatalf("first toast = %+v", m.toast)
	}
	// The confirmed state is read: space marks unread.
	_, cmd = m.handleKey(key(" "))
	pump(t, m, cmd)
	if m.toast == nil || !strings.Contains(m.toast.text, "Marked unread") {
		t.Fatalf("second toast = %+v", m.toast)
	}
}
