package app

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// ctrlR is the terminal keystroke the mark-folder-read binding receives.
func ctrlR() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}
}

// TestCtrlRMarkFolderReadRoutesToOwner (FR-C7): ctrl+r on a folder in the
// sidebar sweeps that folder on its owning account — not the active one —
// and drops its unread badge to zero.
func TestCtrlRMarkFolderReadRoutesToOwner(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)
	if len(m.accounts) < 2 {
		t.Fatalf("accounts = %d, want 2", len(m.accounts))
	}

	m.sidebarKey = sidebarRowKey("personal", "mb-inbox")
	m.focus = ui.PaneSidebar

	pump(t, m, press(t, m, ctrlR()))

	if got := unreadOfMailbox(m.snaps["personal"].Mailboxes, "mb-inbox"); got != 0 {
		t.Errorf("personal inbox unread = %d, want 0", got)
	}
	if got := unreadOfMailbox(m.snaps["work"].Mailboxes, "mb-inbox"); got != 2 {
		t.Errorf("work inbox unread = %d, want 2 (untouched)", got)
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "read") {
		t.Errorf("toast = %+v, want a marked-read notice", m.toast)
	}
}

// TestCtrlRMarkFolderReadHeaderIsNoop: ctrl+r belongs to a folder row;
// on an account header it does nothing rather than guessing a scope.
func TestCtrlRMarkFolderReadHeaderIsNoop(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)
	m.sidebarKey = sidebarRowKey("personal", "") // header
	m.focus = ui.PaneSidebar

	before := m.snaps["personal"]
	pump(t, m, press(t, m, ctrlR()))

	if got := unreadOfMailbox(m.snaps["personal"].Mailboxes, "mb-inbox"); got != 2 {
		t.Errorf("header ctrl+r changed counts: inbox unread = %d, want 2", got)
	}
	if m.snaps["personal"].Version != before.Version || m.toast != nil {
		t.Errorf("header ctrl+r was not a no-op (version %d→%d, toast %+v)", before.Version, m.snaps["personal"].Version, m.toast)
	}
}

func unreadOfMailbox(nodes []sync.MailboxNode, id mail.ID) int {
	for _, n := range nodes {
		if n.Mailbox.ID == id {
			return n.Mailbox.UnreadEmails
		}
	}
	return -1
}
