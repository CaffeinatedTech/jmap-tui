package app

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// searchTestModel is the reader model with the search debounce shrunk so
// the headless pump never blocks on real timers.
func searchTestModel(t *testing.T) *Model {
	t.Helper()
	old := searchDebounce
	searchDebounce = time.Millisecond
	t.Cleanup(func() { searchDebounce = old })
	m, _ := newTestModel(t)
	pump(t, m, m.loadAccountCmd())
	return m
}

// typeInto feeds a rune keystroke into the model's current key routing.
// The returned command (input bookkeeping + debounce tick) is typically
// ignored; tests pump the debounce tick directly for determinism.
func typeInto(m *Model, r rune) tea.Cmd {
	_, cmd := m.handleKey(tea.KeyPressMsg{Text: string(r), Code: r})
	return cmd
}

func TestSearchBarOpenTypeDebounceClose(t *testing.T) {
	m := searchTestModel(t)

	// "/" opens the query bar.
	_, _ = m.handleKey(key("/"))
	if m.search == nil {
		t.Fatal("/ did not open the search bar")
	}
	if m.snap.ViewKey != "m:mb-inbox" {
		t.Fatalf("pre-search ViewKey = %q, want m:mb-inbox", m.snap.ViewKey)
	}

	// Typing arms the debounce; firing the current generation issues.
	// A full token takes the fast path — rows are present immediately.
	for _, r := range "thread" {
		typeInto(m, r)
	}
	pump(t, m, m.debounceSearch(m.search.seq))
	if !m.snap.SearchActive {
		t.Fatalf("SearchActive = false, ViewKey = %q", m.snap.ViewKey)
	}
	if !strings.Contains(m.snap.ViewKey, "s:") {
		t.Fatalf("ViewKey = %q, want a search view", m.snap.ViewKey)
	}
	if m.search.spec.Text != "thread" {
		t.Fatalf("spec.Text = %q, want thread", m.search.spec.Text)
	}
	if len(m.snap.Rows) == 0 {
		t.Fatal("fast-path search returned no rows")
	}

	// Esc restores the mailbox view with the cursor position preserved.
	before := m.snap.Rows[m.snap.Cursor].ID
	pump(t, m, m.closeSearch())
	if m.search != nil {
		t.Fatal("esc did not clear the search state")
	}
	if m.snap.SearchActive {
		t.Fatal("SearchActive survived esc")
	}
	if m.snap.ViewKey != "m:mb-inbox" {
		t.Fatalf("ViewKey = %q after esc, want m:mb-inbox", m.snap.ViewKey)
	}
	if got := m.snap.Rows[m.snap.Cursor].ID; got != before {
		t.Fatalf("cursor %s after esc, want %s", got, before)
	}
}

func TestSearchDebounceCoalescesTyping(t *testing.T) {
	m := searchTestModel(t)
	_, _ = m.handleKey(key("/"))

	// Three rapid keystrokes: input state is synchronous, and only the
	// last generation's tick issues one query.
	typeInto(m, 'a')
	typeInto(m, 'b')
	typeInto(m, 'c')
	if m.search.spec.Text != "abc" {
		t.Fatalf("text = %q, want abc", m.search.spec.Text)
	}
	pump(t, m, m.debounceSearch(m.search.seq))
	if m.search.issued != "abc" {
		t.Fatalf("issued = %q, want abc (last generation wins)", m.search.issued)
	}
	if !m.snap.SearchActive {
		t.Fatal("coalesced search never issued")
	}
	// A stale generation is dropped.
	typeInto(m, 'd')
	pump(t, m, m.debounceSearch(m.search.seq-1))
	if m.search.issued != "abc" {
		t.Fatalf("stale tick issued: %q", m.search.issued)
	}
}

func TestSearchScopeToggle(t *testing.T) {
	m := searchTestModel(t)
	_, _ = m.handleKey(key("/"))

	// Tab flips the scope to all mailboxes and issues immediately.
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	pump(t, m, cmd)
	if m.search.spec.ScopeMailbox != "" {
		t.Fatalf("scope = %q, want empty (all mailboxes)", m.search.spec.ScopeMailbox)
	}
	st := m.uiState()
	if st.Search == nil || st.Search.Scope != "all mailboxes" {
		t.Fatalf("search scope render = %+v, want all mailboxes", st.Search)
	}
	// Tab again returns to the mailbox the search opened from (FR-F3).
	_, cmd = m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	pump(t, m, cmd)
	if m.search.spec.ScopeMailbox != mail.ID("mb-inbox") {
		t.Fatalf("scope = %q, want mb-inbox", m.search.spec.ScopeMailbox)
	}
}

func TestSearchAdvancedModal(t *testing.T) {
	m := searchTestModel(t)
	_, _ = m.handleKey(key("/"))

	// ctrl+s opens the fielded form on the text field.
	_, _ = m.handleKey(keyCtrl('s'))
	if m.search == nil || m.search.adv == nil {
		t.Fatal("ctrl+s did not open the advanced modal")
	}
	if m.search.adv.sel != 0 {
		t.Fatalf("modal opens on field %d, want 0", m.search.adv.sel)
	}

	// Down to "from", type, then enter runs the search.
	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	typeInto(m, 'x')
	if got := m.search.adv.fields[1].input.Value(); got != "x" {
		t.Fatalf("from field = %q, want x", got)
	}
	_, cmd := m.handleKey(keyEnter())
	pump(t, m, cmd)
	if m.search.adv != nil {
		t.Fatal("enter did not close the modal")
	}
	if m.search.spec.From != "x" {
		t.Fatalf("spec.From = %q, want x", m.search.spec.From)
	}
	if !m.snap.SearchActive {
		t.Fatal("advanced search did not issue a query")
	}
	// The advanced field renders as a token next to the bar.
	if toks := m.advTokens(); len(toks) == 0 || toks[0] != "from:x" {
		t.Fatalf("tokens = %v, want [from:x]", toks)
	}

	// Esc (bar open, no modal) closes the whole search view.
	pump(t, m, m.closeSearch())
	if m.search != nil {
		t.Fatal("esc did not close the search view")
	}
}

func TestSearchAdvancedDateValidation(t *testing.T) {
	m := searchTestModel(t)
	_, _ = m.handleKey(key("/"))
	_, _ = m.handleKey(keyCtrl('s'))

	// Navigate to the "after" field (index 4) and enter an invalid date.
	for i := 0; i < 4; i++ {
		_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	typeInto(m, 'q')
	_, cmd := m.handleKey(keyEnter())
	if cmd != nil {
		t.Fatal("invalid date produced a command")
	}
	if m.search.adv == nil || m.search.adv.err == "" {
		t.Fatal("invalid date left no error in the modal")
	}

	// Clear the field (ctrl+u), enter a valid date: the modal confirms.
	_, _ = m.handleKey(keyCtrl('u'))
	for _, r := range "2026-01-02" {
		typeInto(m, r)
	}
	_, cmd = m.handleKey(keyEnter())
	pump(t, m, cmd)
	if m.search.adv != nil {
		t.Fatalf("valid date did not confirm: %v", m.search.adv.err)
	}
	if m.search.spec.After.IsZero() {
		t.Fatal("spec.After not set by the modal")
	}
}

func TestFullscreenToggle(t *testing.T) {
	m := searchTestModel(t)

	_, _ = m.handleKey(key("v"))
	if !m.fullscreen {
		t.Fatal("v did not enter fullscreen")
	}
	if m.focus != ui.PanePreview {
		t.Fatalf("focus = %v, want preview", m.focus)
	}
	st := m.uiState()
	l := ui.ComputeLayout(m.width, m.height, st)
	if l.SidebarW != 0 || l.ListW != 0 || l.PreviewW == 0 {
		t.Fatalf("fullscreen layout = %+v, want preview only", l)
	}
	_, _ = m.handleKey(key("v"))
	if m.fullscreen {
		t.Fatal("v did not leave fullscreen")
	}
}

func TestCtrlSOpensAdvancedModalDirectly(t *testing.T) {
	m := searchTestModel(t)

	// ctrl+s from a closed search view opens the bar AND the fielded
	// form in one step (FR-F2 regression: it used to only open the bar).
	_, _ = m.handleKey(keyCtrl('s'))
	if m.search == nil {
		t.Fatal("ctrl+s did not open the search bar")
	}
	if m.search.adv == nil {
		t.Fatal("ctrl+s did not open the advanced modal")
	}

	// "/" while the modal is open types into the selected field — the
	// modal owns the keyboard until esc or enter.
	typeInto(m, 'q')
	if got := m.search.adv.fields[0].input.Value(); got != "q" {
		t.Fatalf("modal text field = %q, want q", got)
	}
}

func TestSearchFuzzyScanIndicator(t *testing.T) {
	m, srv := newTestModelWith(t, []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 3, UnreadEmails: 2},
	})
	srv.SetSyntheticMailbox(mockjmap.SyntheticMailbox{MailboxID: "mb-syn", Prefix: "syn", Count: 1200})
	pump(t, m, m.loadAccountCmd())

	_, _ = m.handleKey(key("/"))
	// Tab to the all-mailbox scope so the scan covers the synthetic box.
	_, scopeCmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	pump(t, m, scopeCmd)

	// A partial word over a 1,200-message scope: zero server results, so
	// the engine's LIKE scan takes over and streams matches in (FR-F1
	// fuzzy fallback). The scan needs several chunks — catch it active.
	for _, r := range "synth" {
		typeInto(m, r)
	}
	pump(t, m, m.debounceSearch(m.search.seq))
	if !m.snap.SearchActive {
		t.Fatalf("SearchActive = false, ViewKey = %q", m.snap.ViewKey)
	}

	// Wait for the scan to publish (async goroutine → live broadcast).
	// Each wait is bounded: once the scan finishes, no further publishes
	// come and the waiter must not block the test.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.snap.Scan != nil && len(m.snap.Rows) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
		msgCh := make(chan tea.Msg, 1)
		go func() { msgCh <- m.waitUpdates()() }()
		select {
		case msg := <-msgCh:
			if msg != nil {
				m.Update(msg)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if m.snap.Scan == nil {
		t.Fatal("zero-result search never started a scan")
	}
	if len(m.snap.Rows) == 0 {
		t.Fatal("scan produced no rows")
	}

	// The header indicator maps the engine's live scan progress.
	m.snap.Scan = &sync.ScanProgress{Active: true, Scanned: 500, Total: 1200}
	st := m.uiState()
	if st.Search == nil || !st.Search.Scanning {
		t.Fatalf("header scan indicator missing: %+v", st.Search)
	}
	if st.Search.Scanned != 500 || st.Search.ScanTotal != 1200 {
		t.Fatalf("scan progress = %d/%d, want 500/1200", st.Search.Scanned, st.Search.ScanTotal)
	}
	m.snap.Scan = nil

	// Esc cancels the scan and restores the mailbox view.
	pump(t, m, m.closeSearch())
	if m.snap.SearchActive || m.snap.Scan != nil {
		t.Fatal("scan survived esc")
	}
}

// searchTestModelWithExtraRow is the standard search model plus one
// standalone (non-threaded) message, so subject searches yield two
// independent rows.
func searchTestModelWithExtraRow(t *testing.T) (*Model, *mockjmap.Server) {
	t.Helper()
	m, srv := newTestModel(t)
	old := searchDebounce
	searchDebounce = time.Millisecond
	t.Cleanup(func() { searchDebounce = old })
	pump(t, m, m.loadAccountCmd())
	srv.CreateEmails([]mockjmap.Email{{
		ID: "e9", ThreadID: "t9", MailboxIDs: []string{"mb-inbox"},
		From:       []mockjmap.Address{{Name: "Zoe Solo", Email: "zoe@example.test"}},
		Subject:    "second thread note",
		ReceivedAt: time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC),
		TextBody:   "A standalone message.\n",
	}})
	return m, srv
}

func TestSearchEnterConfirmsAndFocussList(t *testing.T) {
	m, _ := searchTestModelWithExtraRow(t)

	_, _ = m.handleKey(key("/"))
	for _, r := range "thread" {
		typeInto(m, r)
	}
	// Enter confirms: search issued, keyboard returns to the list.
	_, cmd := m.handleKey(keyEnter())
	if cmd == nil {
		t.Fatal("enter issued no search")
	}
	pump(t, m, cmd)
	if m.search.editing {
		t.Fatal("enter left the cursor in the search bar")
	}
	if !m.snap.SearchActive || len(m.snap.Rows) < 2 {
		t.Fatalf("search results missing: active=%v rows=%d", m.snap.SearchActive, len(m.snap.Rows))
	}

	// j/k navigate the results now (the bar no longer owns the keys).
	before := m.snap.Cursor
	_, _ = m.handleKey(key("j"))
	if m.snap.Cursor != before+1 {
		t.Fatalf("cursor %d after j, want %d — list navigation blocked", m.snap.Cursor, before+1)
	}

	// A letter that is no binding does not leak into the search.
	_, _ = m.handleKey(key("z"))
	if m.search.spec.Text != "thread" {
		t.Fatalf("query mutated while browsing results: %q", m.search.spec.Text)
	}

	// "/" re-focuses the bar for editing.
	_, cmd = m.handleKey(key("/"))
	if !m.search.editing {
		t.Fatal("/ did not re-focus the search bar")
	}
	if cmd == nil {
		t.Fatal("refocus produced no command (input cursor)")
	}

	// Esc clears the search from the bar.
	pump(t, m, m.closeSearch())
	if m.search != nil || m.snap.SearchActive {
		t.Fatal("esc did not clear the search")
	}
}

func TestSearchEnterDoesNotRestartScan(t *testing.T) {
	m, srv := newTestModelWith(t, []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 3, UnreadEmails: 2},
	})
	srv.SetSyntheticMailbox(mockjmap.SyntheticMailbox{MailboxID: "mb-syn", Prefix: "syn", Count: 1200})
	pump(t, m, m.loadAccountCmd())

	_, _ = m.handleKey(key("/"))
	// Tab to the all-mailbox scope so the scan has real work to do.
	_, scopeCmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	pump(t, m, scopeCmd)
	for _, r := range "synth" {
		typeInto(m, r)
	}
	// Fire the debounce, then let the scan stream its first batches.
	pump(t, m, m.debounceSearch(m.search.seq))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.snap.Scan != nil && m.snap.Scan.Scanned > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
		msgCh := make(chan tea.Msg, 1)
		go func() { msgCh <- m.waitUpdates()() }()
		select {
		case msg := <-msgCh:
			if msg != nil {
				m.Update(msg)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if m.snap.Scan == nil {
		t.Fatal("scan never started")
	}
	if !m.snap.Scan.Active {
		t.Log("note: scan already finished (fast mock); restart check is weaker")
	}

	// Enter with an unchanged spec: no re-issue, the scan keeps its
	// progress (an identical re-issue would cancel and restart it).
	scanned := m.snap.Scan.Scanned
	_, cmd := m.handleKey(keyEnter())
	if cmd != nil {
		pump(t, m, cmd)
	}
	if m.snap.Scan == nil {
		t.Fatal("enter reset the scan state")
	}
	if m.snap.Scan.Scanned < scanned {
		t.Fatalf("scan restarted by enter: scanned %d → %d", scanned, m.snap.Scan.Scanned)
	}
	if m.search.editing {
		t.Fatal("enter left the cursor in the search bar")
	}
}

func TestSearchEscClearsFromResults(t *testing.T) {
	m, _ := searchTestModelWithExtraRow(t)

	// Confirm a search, then clear it from the results list — the bar is
	// not focused here, so esc must flow through the global binding.
	_, _ = m.handleKey(key("/"))
	for _, r := range "thread" {
		typeInto(m, r)
	}
	_, cmd := m.handleKey(keyEnter())
	pump(t, m, cmd)
	if m.search.editing || !m.snap.SearchActive {
		t.Fatalf("setup: editing=%v active=%v", m.search.editing, m.snap.SearchActive)
	}

	// Esc with focus in the list clears the search — via the binding,
	// not a manual close (the returned command is what esc produced).
	_, escCmd := m.handleKey(keyEsc())
	if escCmd == nil {
		t.Fatal("esc while browsing results produced no command")
	}
	pump(t, m, escCmd)
	if m.search != nil || m.snap.SearchActive {
		t.Fatal("esc did not clear the search while browsing results")
	}
	if m.snap.ViewKey != "m:mb-inbox" {
		t.Fatalf("view after esc = %q, want the mailbox view", m.snap.ViewKey)
	}

	// Esc with no search open is a no-op.
	_, _ = m.handleKey(keyEsc())
	if m.search != nil {
		t.Fatal("esc invented a search state")
	}
}
