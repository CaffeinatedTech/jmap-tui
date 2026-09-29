package app

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// This file is the M6 multi-account surface: the switcher modal (FR-A4,
// FR-I7), the unified inbox (FR-A5), and the account views the footer and
// switcher render (FR-I5).

// switchState is the open switcher's selection (FR-A4).
type switchState struct {
	sel int
}

// accountViews assembles per-account render state from the stored
// snapshots in display order (FR-C5): sync status (FR-I5) plus the inbox
// unread count for the switcher rows.
func (m *Model) accountViews() []ui.AccountView {
	ordered := m.orderedAccounts()
	out := make([]ui.AccountView, 0, len(ordered))
	for _, a := range ordered {
		s := m.snaps[a.ID]
		v := ui.AccountView{
			ID:        a.ID,
			Name:      a.Name,
			Active:    a.ID == m.activeID,
			Mode:      s.Status.Mode,
			LastSync:  s.Status.LastSync,
			LastError: s.Status.LastError,
			Attempts:  s.Status.Attempts,
		}
		if v.Mode == "" {
			v.Mode = sync.ModeConnecting
		}
		if node := inboxNode(s.Mailboxes); node != nil {
			v.Unread = node.Mailbox.UnreadEmails
		}
		out = append(out, v)
	}
	return out
}

// accountName is the display name of an account id (the id itself when
// unenrolled — shouldn't happen, but the footer must never blank).
func (m *Model) accountName(id string) string {
	for _, a := range m.accounts {
		if a.ID == id {
			return a.Name
		}
	}
	return id
}

// inboxNode is the mailbox tree's inbox entry, if loaded.
func inboxNode(nodes []sync.MailboxNode) *sync.MailboxNode {
	for i := range nodes {
		if nodes[i].Mailbox.Role == mail.RoleInbox {
			return &nodes[i]
		}
	}
	return nil
}

// inboxID is the loaded inbox's id ("" before the tree arrives).
func inboxID(nodes []sync.MailboxNode) mail.ID {
	if n := inboxNode(nodes); n != nil {
		return n.Mailbox.ID
	}
	return ""
}

// openSwitcher shows the switcher modal (FR-A4). It needs at least two
// accounts to mean anything; rows follow the display order (FR-C5), same
// as the sidebar blocks.
func (m *Model) openSwitcher() {
	if len(m.accounts) < 2 {
		return
	}
	ordered := m.orderedAccounts()
	sel := 0
	for i, a := range ordered {
		if a.ID == m.activeID {
			sel = i
			break
		}
	}
	m.switcher = &switchState{sel: sel}
}

// switcherKey routes the keyboard while the switcher modal is open.
func (m *Model) switcherKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	sw := m.switcher
	if sw == nil {
		return m, nil
	}
	ordered := m.orderedAccounts()
	switch msg.Keystroke() {
	case "esc":
		m.switcher = nil
		return m, nil
	case "j", "down":
		if sw.sel < len(ordered)-1 {
			sw.sel++
		}
		return m, nil
	case "k", "up":
		if sw.sel > 0 {
			sw.sel--
		}
		return m, nil
	case "enter", "l":
		if sw.sel < 0 || sw.sel >= len(ordered) {
			return m, nil
		}
		return m.switchAccount(ordered[sw.sel].ID)
	}
	return m, nil
}

// switchAccount makes id the active account (FR-A4), as an Update return.
func (m *Model) switchAccount(id string) (tea.Model, tea.Cmd) {
	return m, m.activateAccount(id)
}

// activateAccount makes id the active account in place. Every engine is
// already warm, so the switch is instant: the stored snapshot renders
// immediately and each account keeps its own cursor, window, and search
// state. Switching out of unified view first restores every account's
// pre-unified mailbox. Returns nil when nothing changed.
func (m *Model) activateAccount(id string) tea.Cmd {
	m.switcher = nil
	if id == m.activeID || m.hub.Engine(id) == nil {
		return nil
	}

	var cmds []tea.Cmd
	if m.unified {
		_, cmd := m.leaveUnified()
		cmds = append(cmds, cmd)
	}

	// The query bar describes the account being left; close its search so
	// the header never shows one account's search over another's rows.
	if m.search != nil {
		m.search = nil
		if m.snaps[m.activeID].SearchActive {
			old := m.activeID
			cmds = append(cmds, m.opOn(old, "search-close", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
				eng.SearchClose()
				_ = eng.Prefetch(ctx)
				return eng.Snapshot(), nil
			}))
		}
	}

	m.activeID = id
	m.engine = m.hub.Engine(id)
	// The preview body belongs to the previous account's cursor; ids are
	// per-account, so the cached viewport must not leak across.
	m.vpBodyID = ""
	m.bodyReq = ""
	m.setBody("", false)
	m.sel = map[mail.ID]bool{}
	m.err = ""

	// Fresh from the engine, not the stored snapshot: the switch must
	// render the account's current cursor/window even if no broadcast has
	// landed yet (instant switch, FR-A4).
	if eng := m.hub.Engine(id); eng != nil {
		_, cmd := m.applySnapshot(id, eng.Snapshot())
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// toggleUnified flips the merged-inbox view (FR-I7).
func (m *Model) toggleUnified() (tea.Model, tea.Cmd) {
	if len(m.accounts) < 2 {
		return m, nil
	}
	if m.unified {
		return m.leaveUnified()
	}
	return m.enterUnified()
}

// enterUnified forces every account's window to its inbox (remembering
// where each one was, so leaving can restore it) and rebuilds the merged
// view. Any single-account search is closed first — unified owns search
// (FR-F3), and a lingering search would merge as if it were the inbox.
func (m *Model) enterUnified() (tea.Model, tea.Cmd) {
	if m.unified {
		return m, nil
	}
	m.search = nil
	m.unified = true
	m.uCursorID = ""
	m.cursorOwner = ""
	m.bodyReq = ""

	var cmds []tea.Cmd
	for _, a := range m.accounts {
		s := m.snaps[a.ID]
		if s.ViewKey == "" {
			continue // still first-loading; its inbox open follows
		}
		closeSearch := s.SearchActive
		inbox := inboxID(s.Mailboxes)
		openBox := mail.ID("")
		if inbox != "" && s.ActiveMailbox != inbox {
			openBox = inbox
			if s.ActiveMailbox != "" {
				m.prevBox[a.ID] = s.ActiveMailbox
			}
		}
		if !closeSearch && openBox == "" {
			continue
		}
		acct := a.ID
		cmds = append(cmds, m.opOn(acct, "enter-unified", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
			if closeSearch {
				eng.SearchClose()
			}
			if openBox != "" {
				if err := eng.OpenMailbox(ctx, openBox); err != nil {
					return sync.Snapshot{}, err
				}
			}
			return eng.Snapshot(), nil
		}))
	}
	_, cmd := m.applyUnified()
	cmds = append(cmds, cmd)
	m.saveUnifiedPref()
	return m, tea.Batch(cmds...)
}

// leaveUnified tears down the merged view: every account that was moved
// (forced inbox, closed search) gets one sequential op restoring it, and
// the active account's own snapshot repaints immediately (FR-A5 —
// unified is a view; leaving returns each account to independent state).
func (m *Model) leaveUnified() (tea.Model, tea.Cmd) {
	if !m.unified {
		return m, nil
	}
	m.unified = false
	m.bodyReq = ""
	m.uCursorID = ""
	m.cursorOwner = ""

	var cmds []tea.Cmd
	for _, a := range m.accounts {
		s := m.snaps[a.ID]
		closeSearch := s.SearchActive
		openBox := mail.ID("")
		if prev, ok := m.prevBox[a.ID]; ok && s.ActiveMailbox != prev {
			openBox = prev
		}
		if !closeSearch && openBox == "" {
			continue
		}
		acct := a.ID
		cmds = append(cmds, m.opOn(acct, "leave-unified", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
			if closeSearch {
				eng.SearchClose()
			}
			if openBox != "" {
				if err := eng.OpenMailbox(ctx, openBox); err != nil {
					return sync.Snapshot{}, err
				}
			}
			return eng.Snapshot(), nil
		}))
	}
	m.prevBox = map[string]mail.ID{}

	if eng := m.hub.Engine(m.activeID); eng != nil {
		_, cmd := m.applySnapshot(m.activeID, eng.Snapshot())
		cmds = append(cmds, cmd)
	}
	m.saveUnifiedPref()
	return m, tea.Batch(cmds...)
}

// saveUnifiedPref remembers the unified-view flag in prefs.toml (FR-A5,
// issue #2) so the next start reopens the view the reader left. It runs
// after enter/leave have repainted, which resets the status line — the
// same write-on-toggle discipline as the pane layout (FR-I10), and the
// same rule that a session without a prefs store stays memory-only
// (FR-J1). A failed write is a notice, never a blocked toggle.
func (m *Model) saveUnifiedPref() {
	if m.opts.Prefs == nil || m.opts.PrefsPath == "" {
		return
	}
	m.opts.Prefs.Unified = m.unified
	if err := config.SavePrefs(m.opts.PrefsPath, m.opts.Prefs); err != nil {
		m.err = "unified view not remembered: " + err.Error()
	}
}
