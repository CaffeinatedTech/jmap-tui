package app

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// jumpUnread moves the cursor to the next/prev unread message (FR-D7).
// Per account the scan runs engine-side — it extends the window while it
// looks and reports ErrNoUnread, which the app shows as the non-fatal
// notice. The merged unified view has no engine (rows are app-side), so it
// scans what is loaded (KEYMAP_PLAN §6).
func (m *Model) jumpUnread(dir int) (tea.Model, tea.Cmd) {
	if len(m.snap.Rows) == 0 {
		return m, nil
	}
	if m.unified {
		if idx, ok := sync.FindUnread(m.snap.Rows, m.snap.Cursor+dir, dir); ok {
			return m.moveUnifiedCursorTo(idx)
		}
		m.err = "no more unread"
		return m, nil
	}
	return m, m.opOn(m.activeID, "unread-jump", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
		if err := eng.JumpUnread(ctx, dir); err != nil {
			return sync.Snapshot{}, err
		}
		return eng.Snapshot(), nil
	})
}

// moveUnifiedCursorTo lands the merged cursor on an absolute row index —
// the unified cursor is app-side (PLAN §4.3), like moveCursor.
func (m *Model) moveUnifiedCursorTo(idx int) (tea.Model, tea.Cmd) {
	if idx < 0 || idx >= len(m.snap.Rows) {
		return m, nil
	}
	if len(m.snap.Fresh) > 0 {
		m.clearFresh()
	}
	m.uCursorID = m.snap.Rows[idx].ID
	m.cursorOwner = m.snap.Rows[idx].Account
	return m.applyUnified()
}
