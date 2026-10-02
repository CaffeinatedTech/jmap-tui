package app

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// markReadDoneMsg carries a finished mark-folder-read sweep (FR-C7).
type markReadDoneMsg struct {
	acct  string
	count int
	snap  sync.Snapshot
	err   error
}

// markFolderRead sweeps the folder under the sidebar cursor (FR-C7),
// routing to that row's owning account (FR-A5). An account header is a
// no-op: the action is per-folder by design.
func (m *Model) markFolderRead() tea.Cmd {
	rows := m.sidebarRows()
	i := sidebarIndexOf(rows, m.sidebarKey)
	if i < 0 || i >= len(rows) {
		return nil
	}
	row := rows[i]
	if row.Kind != ui.SidebarMailbox || row.MailboxID == "" {
		return nil
	}
	acct, id := row.AccountID, row.MailboxID
	eng, ok := m.engineFor(acct)
	return func() tea.Msg {
		if !ok {
			return markReadDoneMsg{acct: acct, err: fmt.Errorf("account %q is not connected", acct)}
		}
		n, err := eng.MarkMailboxRead(m.ctx, id)
		return markReadDoneMsg{acct: acct, count: n, snap: eng.Snapshot(), err: err}
	}
}

// handleMarkReadDone adopts the refreshed snapshot and reports the sweep.
func (m *Model) handleMarkReadDone(msg markReadDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = truncateErr("mark folder read", msg.err)
		return m, nil
	}
	var cmd tea.Cmd
	if msg.snap.Version > m.snaps[msg.acct].Version {
		_, cmd = m.applySnapshot(msg.acct, msg.snap)
	}
	if msg.count == 0 {
		return m, tea.Batch(cmd, m.showToast("No unread messages", "", nil, nil))
	}
	return m, tea.Batch(cmd, m.showToast("Marked "+countN(msg.count)+" read", "", nil, nil))
}
