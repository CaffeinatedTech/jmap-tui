package app

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// bodyDebounce is the quiet period before a cursor move hydrates a body
// (FR-D4, FR-K4: "debounce search/scroll fetches"). Holding a movement key
// auto-repeats every few milliseconds; without this each row passed over
// would issue its own fetch even though only the resting row is ever
// displayed. A var so tests can shrink it (a real Tick blocks the
// headless pump).
var bodyDebounce = 120 * time.Millisecond

// bodyDebounceMsg fires once the cursor has rested on one row for the
// debounce window; stale generations (the cursor moved again) are dropped.
type bodyDebounceMsg struct {
	seq  int
	acct string
	id   mail.ID
}

// armBody schedules the hydration debounce for one cursor row. A repeated
// call for the row already pending is a no-op, so a stream of snapshot
// rebuilds (live updates) cannot keep postponing the fetch; a call for a
// different row supersedes the timer by bumping the generation, and the
// superseded tick is dropped when it fires.
func (m *Model) armBody(acct string, id mail.ID) tea.Cmd {
	key := m.rowKey(acct, id)
	if m.bodyPending == key {
		return nil
	}
	m.bodyPending = key
	m.bodySeq++
	seq := m.bodySeq
	return tea.Tick(bodyDebounce, func(time.Time) tea.Msg {
		return bodyDebounceMsg{seq: seq, acct: acct, id: id}
	})
}

// resetBodyPending invalidates any armed hydration debounce: a view or
// cursor change has made its target stale, so the tick must not issue.
func (m *Model) resetBodyPending() {
	m.bodyPending = ""
	m.bodySeq++
}

// bodyCached reports whether the owner engine already holds id's body, so
// the caller can load it immediately instead of spending a debounce window
// on a fetch that never touches the network.
func (m *Model) bodyCached(acct string, id mail.ID) bool {
	eng, ok := m.engineFor(acct)
	if !ok {
		return false
	}
	_, cached := eng.CachedBody(id)
	return cached
}
