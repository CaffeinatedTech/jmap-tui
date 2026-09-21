package app

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
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
	typeInto(m, 't')
	pump(t, m, m.debounceSearch(m.search.seq))
	if !m.snap.SearchActive {
		t.Fatalf("SearchActive = false, ViewKey = %q", m.snap.ViewKey)
	}
	if !strings.Contains(m.snap.ViewKey, "s:") {
		t.Fatalf("ViewKey = %q, want a search view", m.snap.ViewKey)
	}
	if m.search.spec.Text != "t" {
		t.Fatalf("spec.Text = %q, want t", m.search.spec.Text)
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
