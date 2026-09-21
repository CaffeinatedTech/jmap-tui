package app

import (
	"context"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// searchDebounce is the query-bar keystroke debounce (FR-F1, FR-K4):
// typing coalesces into one server query per quiet 300ms. A var so tests
// can shrink it (a real Tick blocks the headless pump).
var searchDebounce = 300 * time.Millisecond

// advDateLayout is the date-field format of the advanced modal.
const advDateLayout = "2006-01-02"

// searchState owns the query bar and the advanced modal (FR-F1, FR-F2).
// spec accumulates the fields of the open search; Text mirrors the input.
type searchState struct {
	input     textinput.Model
	spec      sync.SearchSpec
	baseScope mail.ID // scope the search opened with (tab returns here)
	adv       *advState
	seq       int    // debounce generation; a firing tick must match
	issued    string // text of the last issued query (debounce no-op guard)
}

// advField is one editable row of the advanced modal.
type advField struct {
	name  string
	input textinput.Model
}

// advState is the advanced-search modal (FR-F2): a fielded form over the
// RFC 8621 §4.4.1 condition. The attachment row is selectable but not
// typeable — space toggles it.
type advState struct {
	fields []advField
	sel    int // index into fields; len(fields) = attachment row
	attach bool
	err    string
}

// searchDebounceMsg fires when the query bar has been quiet for the
// debounce window; stale generations are dropped.
type searchDebounceMsg struct{ seq int }

// openSearch enters the search view (FR-F1): the query bar takes the
// header, scoped to the open mailbox (FR-F3; all mailboxes when none is
// open).
func (m *Model) openSearch() tea.Cmd {
	ti := textinput.New()
	ti.Placeholder = "type to search…"
	ti.Prompt = ""
	ti.SetWidth(32)
	ti.Focus()
	m.search = &searchState{input: ti, baseScope: m.snap.ActiveMailbox}
	m.search.spec.ScopeMailbox = m.snap.ActiveMailbox
	return ti.Focus()
}

// searchKey routes a keypress while the query bar is open. Everything is
// consumed: the bar, the advanced modal, and scope/tab own the keyboard.
func (m *Model) searchKey(msg tea.KeyPressMsg) tea.Cmd {
	if s := m.search; s.adv != nil {
		return m.advKey(msg)
	}
	key := msg.Keystroke()
	s := m.search
	switch key {
	case "esc":
		return m.closeSearch()
	case "enter":
		return m.issueSearch()
	case "tab":
		return m.toggleSearchScope()
	case "ctrl+s":
		m.openAdvSearch()
		return nil
	}
	ti, cmd := s.input.Update(msg)
	s.input = ti
	if txt := s.input.Value(); txt != s.spec.Text {
		s.spec.Text = txt
		s.seq++
		return tea.Batch(cmd, m.debounceSearch(s.seq))
	}
	return cmd
}

// debounceSearch arms the quiet-period timer for one input generation.
func (m *Model) debounceSearch(seq int) tea.Cmd {
	return tea.Tick(searchDebounce, func(time.Time) tea.Msg {
		return searchDebounceMsg{seq: seq}
	})
}

// toggleSearchScope flips current-mailbox ↔ all-mailboxes and re-issues
// immediately (FR-F3): one deliberate keystroke, not a typing stream. Tab
// always returns to the mailbox the search opened from.
func (m *Model) toggleSearchScope() tea.Cmd {
	s := m.search
	if s.spec.ScopeMailbox != "" {
		s.spec.ScopeMailbox = ""
	} else {
		s.spec.ScopeMailbox = s.baseScope
	}
	return m.issueSearch()
}

// issueSearch runs the current spec through the engine. The mailbox view
// is parked inside the engine until Esc (FR-F1).
func (m *Model) issueSearch() tea.Cmd {
	spec := m.search.spec
	m.search.issued = spec.Text
	return m.engineOp("search", func(ctx context.Context) (sync.Snapshot, error) {
		if err := m.engine.SearchOpen(ctx, spec); err != nil {
			return sync.Snapshot{}, err
		}
		return m.engine.Snapshot(), nil
	})
}

// closeSearch leaves the search view: the engine restores the parked
// mailbox window (position preserved, FR-F1) and the same op re-anchors
// it around the cursor id, folding in live changes that landed while the
// search was open (FR-B5). A failed re-anchor is not a failed close —
// Esc never surfaces an error; the next prefetch retries.
func (m *Model) closeSearch() tea.Cmd {
	m.search = nil
	return m.engineOp("search-close", func(ctx context.Context) (sync.Snapshot, error) {
		m.engine.SearchClose()
		_ = m.engine.Prefetch(ctx)
		return m.engine.Snapshot(), nil
	})
}

// --- advanced modal (FR-F2) ---

var advFieldNames = []string{"text", "from", "to", "subject", "after", "before", "keyword"}

// openAdvSearch builds the fielded form from the live spec.
func (m *Model) openAdvSearch() {
	fields := make([]advField, 0, len(advFieldNames))
	vals := []string{
		m.search.spec.Text, m.search.spec.From, m.search.spec.To,
		m.search.spec.Subject,
		advDateOut(m.search.spec.After), advDateOut(m.search.spec.Before),
		m.search.spec.HasKeyword,
	}
	for i, name := range advFieldNames {
		ti := textinput.New()
		ti.Prompt = ""
		ti.SetWidth(34)
		ti.SetValue(vals[i])
		fields = append(fields, advField{name: name, input: ti})
	}
	attach := m.search.spec.HasAttachment != nil && *m.search.spec.HasAttachment
	m.search.adv = &advState{fields: fields, attach: attach, sel: 0}
	m.advRefocus()
}

// advDateOut renders a spec date back into the field format ("" when
// unset); after dates display as themselves, before dates as the last
// included day.
func advDateOut(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(advDateLayout)
}

// advRefocus syncs textinput focus with the selected row.
func (m *Model) advRefocus() {
	av := m.search.adv
	for i := range av.fields {
		if i == av.sel {
			av.fields[i].input.Focus()
		} else {
			av.fields[i].input.Blur()
		}
	}
}

// advKey routes a keypress inside the advanced modal.
func (m *Model) advKey(msg tea.KeyPressMsg) tea.Cmd {
	av := m.search.adv
	key := msg.Keystroke()
	last := len(av.fields) // the attachment row
	switch key {
	case "esc":
		av.err = ""
		m.search.adv = nil
		return nil
	case "up":
		if av.sel > 0 {
			av.sel--
		}
		av.err = ""
		m.advRefocus()
		return nil
	case "down", "tab":
		if av.sel < last {
			av.sel++
		}
		av.err = ""
		m.advRefocus()
		return nil
	case "shift+tab":
		if av.sel > 0 {
			av.sel--
		}
		m.advRefocus()
		return nil
	case "space":
		if av.sel == last {
			av.attach = !av.attach
		}
		return nil
	case "enter":
		return m.advConfirm()
	}
	if av.sel < len(av.fields) {
		ti, cmd := av.fields[av.sel].input.Update(msg)
		av.fields[av.sel].input = ti
		av.err = ""
		return cmd
	}
	return nil
}

// advConfirm validates the form and runs the search.
func (m *Model) advConfirm() tea.Cmd {
	av := m.search.adv
	spec := m.search.spec
	spec.Text = av.fields[0].input.Value()
	spec.From = av.fields[1].input.Value()
	spec.To = av.fields[2].input.Value()
	spec.Subject = av.fields[3].input.Value()
	after, err := advDateIn(av.fields[4].input.Value())
	if err != "" {
		av.err = err
		return nil
	}
	spec.After = after
	before, err := advDateIn(av.fields[5].input.Value())
	if err != "" {
		av.err = err
		return nil
	}
	spec.Before = before
	spec.HasKeyword = av.fields[6].input.Value()
	if av.attach {
		yes := true
		spec.HasAttachment = &yes
	} else {
		spec.HasAttachment = nil
	}

	m.search.adv = nil
	m.search.spec = spec
	m.search.input.SetValue(spec.Text)
	return m.issueSearch()
}

// advDateIn parses a field date into the spec instant: the day at 00:00
// UTC. after is exclusive (RFC 8621), so "after D" matches D onward;
// before uses the same instant, so "before D" stops where D starts.
func advDateIn(v string) (time.Time, string) {
	if v == "" {
		return time.Time{}, ""
	}
	t, err := time.Parse(advDateLayout, v)
	if err != nil {
		return time.Time{}, "dates want " + advDateLayout
	}
	return t.UTC(), ""
}

// --- fullscreen (FR-E5) ---

// toggleFullscreen flips the full-screen message view: sidebar and list
// hide, the preview takes the frame.
func (m *Model) toggleFullscreen() {
	m.fullscreen = !m.fullscreen
	if m.fullscreen {
		m.focus = ui.PanePreview
	}
	m.resizeViewport()
}

// --- render state ---

// searchView assembles the query-bar line (FR-F1): live input view,
// advanced tokens, and the scope name.
func (m *Model) searchView() *ui.SearchView {
	s := m.search
	v := &ui.SearchView{Query: s.input.View()}
	if tok := m.advTokens(); len(tok) > 0 {
		v.Tokens = tok
	}
	v.Scope = "all mailboxes"
	if s.spec.ScopeMailbox != "" {
		if node := m.mailboxByID(s.spec.ScopeMailbox); node != nil {
			v.Scope = node.Name
		} else {
			v.Scope = "mailbox"
		}
	}
	return v
}

// advTokens renders the non-text advanced fields as muted tokens next to
// the query bar.
func (m *Model) advTokens() []string {
	s := m.search.spec
	var tok []string
	add := func(name, val string) {
		if val != "" {
			tok = append(tok, name+":"+val)
		}
	}
	add("from", s.From)
	add("to", s.To)
	add("subject", s.Subject)
	add("after", advDateOut(s.After))
	add("before", advDateOut(s.Before))
	add("keyword", s.HasKeyword)
	if s.HasAttachment != nil && *s.HasAttachment {
		tok = append(tok, "attachments")
	}
	return tok
}

// advSearchView assembles the advanced modal render state (FR-F2).
func (m *Model) advSearchView() *ui.AdvSearchView {
	av := m.search.adv
	out := &ui.AdvSearchView{Title: "Advanced search", Err: av.err}
	for i, f := range av.fields {
		out.Fields = append(out.Fields, ui.AdvField{
			Name:    f.name,
			View:    f.input.View(),
			Focused: i == av.sel,
		})
	}
	out.Attach = av.attach
	return out
}
