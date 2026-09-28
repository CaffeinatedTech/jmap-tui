package app

import (
	"sort"
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
// frame or keypress. The fold set (FR-C6) is applied as a view over that
// tree: a folded account contributes only its header, a folded folder
// hides its whole subtree (the snapshot is a pre-order walk, so depth
// carries the extent), and a folded folder's unread rolls up to the sum
// of what it hides.
func (m *Model) sidebarRows() []ui.SidebarRow {
	rows := make([]ui.SidebarRow, 0, 16)
	for _, info := range m.orderedAccounts() {
		id := info.ID
		snap := m.snaps[id]
		acctKey := sidebarRowKey(id, "")
		_, acctFolded := m.collapsed[acctKey]
		rows = append(rows, ui.SidebarRow{
			Kind:        ui.SidebarAccount,
			Key:         acctKey,
			AccountID:   id,
			Name:        info.Name,
			Tint:        m.accountTint(id),
			Active:      id == m.activeID,
			HasChildren: len(snap.Mailboxes) > 0,
			Collapsed:   acctFolded,
		})
		if acctFolded {
			continue
		}
		rollup := subtreeUnread(snap.Mailboxes)
		hideBelow := -1
		for i, node := range snap.Mailboxes {
			if hideBelow >= 0 && node.Depth > hideBelow {
				continue
			}
			hideBelow = -1
			key := sidebarRowKey(id, node.Mailbox.ID)
			_, folded := m.collapsed[key]
			unread := node.Mailbox.UnreadEmails
			if folded {
				unread = rollup[i]
			}
			rows = append(rows, ui.SidebarRow{
				Kind:        ui.SidebarMailbox,
				Key:         key,
				AccountID:   id,
				MailboxID:   node.Mailbox.ID,
				Name:        node.Mailbox.Name,
				Depth:       node.Depth,
				Unread:      unread,
				Active:      id == m.activeID && node.Mailbox.ID == snap.ActiveMailbox,
				HasChildren: i+1 < len(snap.Mailboxes) && snap.Mailboxes[i+1].Depth > node.Depth,
				Collapsed:   folded,
			})
			if folded {
				hideBelow = node.Depth
			}
		}
	}
	return rows
}

// subtreeUnread rolls up unread counts over a pre-order mailbox list
// (FR-C6): result[i] is that mailbox's own unread plus every descendant's
// — what a folded folder shows instead of its own count.
func subtreeUnread(nodes []sync.MailboxNode) []int {
	sums := make([]int, len(nodes))
	for i := len(nodes) - 1; i >= 0; i-- {
		sums[i] = nodes[i].Mailbox.UnreadEmails
		if i+1 < len(nodes) && nodes[i+1].Depth > nodes[i].Depth {
			sums[i] += sums[i+1]
		}
	}
	return sums
}

// loadCollapsed seeds the fold set from prefs (FR-C6): remembered
// accounts and mailboxes, keyed by row key, for accounts enrolled this
// session. Mailbox ids are validated against the tree when it loads
// (saveFoldState prunes what has since been deleted).
func loadCollapsed(p *config.Prefs, accounts []sync.AccountInfo) map[string]bool {
	out := map[string]bool{}
	if p == nil {
		return out
	}
	enrolled := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		enrolled[a.ID] = true
	}
	for _, id := range p.CollapsedAccounts {
		if enrolled[id] {
			out[sidebarRowKey(id, "")] = true
		}
	}
	for acct, mbs := range p.CollapsedFolders {
		if !enrolled[acct] {
			continue
		}
		for _, mb := range mbs {
			out[sidebarRowKey(acct, mail.ID(mb))] = true
		}
	}
	return out
}

// saveFoldState serializes the fold set into prefs and writes prefs.toml
// (FR-C6, FR-J1: fold state lives in prefs, never config.toml). Entries
// for accounts not enrolled are dropped, and a folded mailbox id whose
// mailbox has left a loaded tree is pruned — a tree not yet loaded keeps
// its entries. Slices are built fresh so the document never aliases the
// model's map.
func (m *Model) saveFoldState() {
	if m.opts.Prefs == nil || m.opts.PrefsPath == "" {
		return
	}
	enrolled := make(map[string]bool, len(m.accounts))
	for _, a := range m.accounts {
		enrolled[a.ID] = true
	}
	var accts []string
	folders := map[string][]string{}
	for key := range m.collapsed {
		acct, mb, isFolder := strings.Cut(key, "\x00")
		if !enrolled[acct] {
			continue
		}
		if !isFolder {
			accts = append(accts, acct)
			continue
		}
		if snap := m.snaps[acct]; len(snap.Mailboxes) > 0 && indexOfMailbox(snap.Mailboxes, mail.ID(mb)) < 0 {
			continue
		}
		folders[acct] = append(folders[acct], mb)
	}
	sort.Strings(accts)
	for _, ids := range folders {
		sort.Strings(ids)
	}
	if len(folders) == 0 {
		folders = nil
	}
	m.opts.Prefs.CollapsedAccounts = accts
	m.opts.Prefs.CollapsedFolders = folders
	if err := config.SavePrefs(m.opts.PrefsPath, m.opts.Prefs); err != nil {
		m.err = "fold state not remembered: " + err.Error()
	}
}

// setFolded flips one row's fold state (FR-C6) and remembers the set.
func (m *Model) setFolded(key string, folded bool) {
	if m.collapsed == nil {
		m.collapsed = map[string]bool{}
	}
	if folded {
		if m.collapsed[key] {
			return
		}
		m.collapsed[key] = true
	} else {
		if !m.collapsed[key] {
			return
		}
		delete(m.collapsed, key)
	}
	m.saveFoldState()
}

// sidebarCollapse folds the cursor's row shut (FR-C6). A row with
// nothing to fold — a leaf folder, an already-folded folder, a folded
// account header — makes the cursor climb to its parent instead
// (ranger's h): the folder's ParentID row when it exists, else the
// account header; a folded header has nowhere to go. The cursor stays on
// the row it folded (the row itself remains visible).
func (m *Model) sidebarCollapse() {
	rows := m.sidebarRows()
	i := sidebarIndexOf(rows, m.sidebarKey)
	if i < 0 || i >= len(rows) {
		return
	}
	row := rows[i]
	if row.Kind == ui.SidebarAccount {
		if row.HasChildren && !row.Collapsed {
			m.setFolded(row.Key, true)
		}
		return
	}
	if row.HasChildren && !row.Collapsed {
		m.setFolded(row.Key, true)
		return
	}
	// Nothing to fold: climb to the parent row that actually exists —
	// an orphan's ParentID (or a missing parent) lands on the header,
	// never on a dangling key.
	want := sidebarRowKey(row.AccountID, "")
	if node := lookupMailbox(m.snaps[row.AccountID], row.MailboxID); node != nil && node.Mailbox.ParentID != "" {
		if key := sidebarRowKey(row.AccountID, node.Mailbox.ParentID); sidebarHasRow(rows, key) {
			want = key
		}
	}
	m.sidebarKey = want
}

// sidebarExpand unfolds the row under the cursor (FR-C6) — that folder's
// subtree or the whole account tree. It only ever expands: opening a
// mailbox or activating an account is Enter's job alone (FR-C2, FR-C5),
// so a row with nothing folded leaves the cursor put.
func (m *Model) sidebarExpand() {
	rows := m.sidebarRows()
	i := sidebarIndexOf(rows, m.sidebarKey)
	if i < 0 || i >= len(rows) {
		return
	}
	m.setFolded(rows[i].Key, false)
}

// lookupMailbox finds a mailbox node in a snapshot's tree (nil when
// absent).
func lookupMailbox(snap sync.Snapshot, id mail.ID) *sync.MailboxNode {
	for i := range snap.Mailboxes {
		if snap.Mailboxes[i].Mailbox.ID == id {
			return &snap.Mailboxes[i]
		}
	}
	return nil
}

// sidebarHasRow reports whether the visible row list contains the key.
func sidebarHasRow(rows []ui.SidebarRow, key string) bool {
	for _, r := range rows {
		if r.Key == key {
			return true
		}
	}
	return false
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
// prefs.toml (FR-J1). The cursor rides along for free: its key belongs to
// the block. The first and last blocks have nowhere to go. Tints are
// untouched (accountTint). Whatever lands on top becomes the startup
// account: default_account in config.toml follows the top of the order
// (pinTopAccountDefault, issue #3).
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
	if m.opts.Prefs != nil {
		m.opts.Prefs.SetAccountOrder(m.acctOrder)
		if m.opts.PrefsPath != "" {
			if err := config.SavePrefs(m.opts.PrefsPath, m.opts.Prefs); err != nil {
				m.err = "account order not remembered: " + err.Error()
			}
		}
	}
	m.pinTopAccountDefault()
}

// pinTopAccountDefault keeps config.toml's default_account on whatever
// account heads the sidebar order (FR-C5/FR-J1, issue #3): the top block
// is what opens first, so a reorder that changes the top rewrites that
// one key — surgically, the rest of the file untouched. A session with no
// config path (flags-only, tests) or an unchanged top never writes; a
// failed write is a status notice, never a crash, and leaves the tracked
// default alone so the next move retries.
func (m *Model) pinTopAccountDefault() {
	if m.opts.ConfigPath == "" || len(m.acctOrder) == 0 {
		return
	}
	top := m.acctOrder[0]
	if top == m.defaultAccount {
		return
	}
	if err := config.SetDefaultAccount(m.opts.ConfigPath, top); err != nil {
		m.err = "default account not updated: " + err.Error()
		return
	}
	m.defaultAccount = top
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
