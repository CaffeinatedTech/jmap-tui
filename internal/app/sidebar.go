package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// This file is the M8 multi-account sidebar (FR-C5): the flat row list
// every account's folder tree flows through, the key-anchored cursor over
// it, and the account-block reorder.

// sidebarRowKey is a sidebar row's identity: the bare account id for a
// header, the account id, a NUL, and the mailbox id for a folder. JMAP
// ids never contain a NUL, so the two key spaces cannot collide — and
// both survive an account reorder untouched (ids, not indexes).
func sidebarRowKey(acct string, mb mail.ID) string {
	if mb == "" {
		return acct
	}
	return acct + "\x00" + string(mb)
}

// accountInfo resolves an enrolled account's display info.
func (m *Model) accountInfo(id string) (sync.AccountInfo, bool) {
	for _, a := range m.accounts {
		if a.ID == id {
			return a, true
		}
	}
	return sync.AccountInfo{}, false
}

// orderedAccounts lists the enrolled accounts in display order (FR-C5):
// the sidebar's block sequence and the switcher both follow it.
// Enrollment order (m.accounts) stays the tint identity — see
// accountTint. An account enrolled after the order was computed keeps
// working: it appends instead of vanishing (same rule as a newly
// configured account, FR-C5).
func (m *Model) orderedAccounts() []sync.AccountInfo {
	out := make([]sync.AccountInfo, 0, len(m.accounts))
	seen := make(map[string]bool, len(m.accounts))
	for _, id := range m.acctOrder {
		if info, ok := m.accountInfo(id); ok {
			out = append(out, info)
			seen[id] = true
		}
	}
	for _, a := range m.accounts {
		if !seen[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

// accountTint is the account's enrollment ordinal — the tint the unified
// row bar uses (FR-A5). It never changes with the display order, so an
// account's colour stays its identity wherever the block sits (FR-C5).
func (m *Model) accountTint(id string) int {
	for i, a := range m.accounts {
		if a.ID == id {
			return i
		}
	}
	return 0
}

// sidebarRows assembles the folder column (FR-C5): one header per
// account in display order, then that account's mailbox tree. Built fresh
// from the stored snapshots — never persisted (NFR-4) — so live mailbox
// changes, account switches, and reorders are picked up on the next
// frame or keypress.
func (m *Model) sidebarRows() []ui.SidebarRow {
	rows := make([]ui.SidebarRow, 0, 16)
	for _, info := range m.orderedAccounts() {
		id := info.ID
		rows = append(rows, ui.SidebarRow{
			Kind:      ui.SidebarAccount,
			Key:       sidebarRowKey(id, ""),
			AccountID: id,
			Name:      info.Name,
			Tint:      m.accountTint(id),
			Active:    id == m.activeID,
		})
		snap := m.snaps[id]
		for _, node := range snap.Mailboxes {
			rows = append(rows, ui.SidebarRow{
				Kind:      ui.SidebarMailbox,
				Key:       sidebarRowKey(id, node.Mailbox.ID),
				AccountID: id,
				MailboxID: node.Mailbox.ID,
				Name:      node.Mailbox.Name,
				Depth:     node.Depth,
				Unread:    node.Mailbox.UnreadEmails,
				Active:    id == m.activeID && node.Mailbox.ID == snap.ActiveMailbox,
			})
		}
	}
	return rows
}

// sidebarIndexOf resolves a cursor key to a row index: exact match, else
// the key's account header (a destroyed folder lands on its section, not
// on an arbitrary row), else the first row. -1 when there is nothing to
// select.
func sidebarIndexOf(rows []ui.SidebarRow, key string) int {
	if len(rows) == 0 {
		return -1
	}
	if key == "" {
		return 0
	}
	for i, r := range rows {
		if r.Key == key {
			return i
		}
	}
	if acct, _, ok := strings.Cut(key, "\x00"); ok {
		for i, r := range rows {
			if r.Kind == ui.SidebarAccount && r.AccountID == acct {
				return i
			}
		}
	}
	return 0
}

// sidebarIndex is the cursor's current row index (see sidebarIndexOf).
func (m *Model) sidebarIndex() int {
	return sidebarIndexOf(m.sidebarRows(), m.sidebarKey)
}

// sidebarSelect parks the cursor on row i (clamped).
func (m *Model) sidebarSelect(i int) {
	rows := m.sidebarRows()
	if len(rows) == 0 {
		return
	}
	m.sidebarKey = rows[min(max(i, 0), len(rows)-1)].Key
}

// sidebarMove walks the cursor by delta rows across account headers and
// folders alike.
func (m *Model) sidebarMove(delta int) {
	m.sidebarSelect(m.sidebarIndex() + delta)
}

// moveAccountBlock shifts the whole account block under the cursor one
// place in the display order (FR-C5) and remembers the arrangement in
// prefs.toml (FR-J1 — the app never writes config.toml). The cursor rides
// along for free: its key belongs to the block. The first and last
// blocks have nowhere to go. Tints are untouched (accountTint).
func (m *Model) moveAccountBlock(delta int) {
	rows := m.sidebarRows()
	i := sidebarIndexOf(rows, m.sidebarKey)
	if i < 0 || i >= len(rows) {
		return
	}
	acct := rows[i].AccountID
	pos := -1
	for j, id := range m.acctOrder {
		if id == acct {
			pos = j
			break
		}
	}
	if pos < 0 {
		// Enrolled after the order was computed: give it a place, then
		// move it like any other block.
		m.acctOrder = append(m.acctOrder, acct)
		pos = len(m.acctOrder) - 1
	}
	next := pos + delta
	if next < 0 || next >= len(m.acctOrder) {
		return
	}
	m.acctOrder[pos], m.acctOrder[next] = m.acctOrder[next], m.acctOrder[pos]
	if m.opts.Prefs == nil {
		return
	}
	m.opts.Prefs.SetAccountOrder(m.acctOrder)
	if m.opts.PrefsPath == "" {
		return
	}
	if err := config.SavePrefs(m.opts.PrefsPath, m.opts.Prefs); err != nil {
		m.err = "account order not remembered: " + err.Error()
	}
}

// openSidebarRow acts on the cursor row (FR-C2, FR-C5). A header
// activates its account and keeps the cursor in the folder column, so the
// user can keep walking that tree; a folder opens on its owning account —
// switching first when that is not the active one — and hands focus to
// the list (reading starts there).
func (m *Model) openSidebarRow() (tea.Model, tea.Cmd) {
	rows := m.sidebarRows()
	i := sidebarIndexOf(rows, m.sidebarKey)
	if i < 0 || i >= len(rows) {
		return m, nil
	}
	row := rows[i]
	if row.Kind == ui.SidebarAccount {
		m.activateAccount(row.AccountID)
		return m, nil
	}
	m.focus = ui.PaneList
	var cmds []tea.Cmd
	switch {
	case row.AccountID != m.activeID:
		// A foreign folder (FR-C5): engines are warm, so activating the
		// owner is instant (FR-A4) and it leaves unified view on the way
		// (FR-A5: unified is a view).
		cmds = append(cmds, m.activateAccount(row.AccountID))
		cmds = append(cmds, m.openMailbox(row.MailboxID))
	case m.unified:
		// Opening a concrete mailbox leaves the unified view.
		_, cmd := m.leaveUnified()
		cmds = append(cmds, cmd)
		cmds = append(cmds, m.openMailbox(row.MailboxID))
	default:
		cmds = append(cmds, m.openMailbox(row.MailboxID))
	}
	return m, tea.Batch(cmds...)
}
