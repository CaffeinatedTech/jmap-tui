package app

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// paste feeds a bracketed paste — what a terminal delivers for
// ctrl+shift+v or a middle click — through the model, as the program
// would, and returns any command. Callers drop it like typeInto's: a
// widget's cursor-blink command only ever waits inside the real
// program's goroutines, and every assertion below reads state the
// message already changed.
func paste(m *Model, content string) tea.Cmd {
	_, cmd := m.Update(tea.PasteMsg{Content: content})
	return cmd
}

// TestPasteReachesEveryTextField (issue #4): paste lands in whichever
// text field owns the keyboard, and nowhere else. Before the fix no
// Update case existed for tea.PasteMsg — and bubbles' ctrl+v reply is an
// unexported message type no case could ever match — so every field
// dropped both.
func TestPasteReachesEveryTextField(t *testing.T) {
	t.Run("composer header", func(t *testing.T) {
		m, _ := composeTestModel(t)
		_ = press(t, m, key("n"))
		if m.compose == nil || m.compose.focus != ui.ZoneTo {
			t.Fatalf("composer not open on To: %+v", m.compose)
		}
		paste(m, "alice@example.test")
		if got := m.compose.to.Value(); got != "alice@example.test" {
			t.Fatalf("To = %q, want the pasted address", got)
		}
		// The paste is an edit like any other: it arms the autosave
		// (the key path batches markDirty beside the widget's command).
		if !m.compose.dirty {
			t.Fatal("paste left the draft clean — autosave would never fire")
		}
	})

	t.Run("composer body keeps line breaks", func(t *testing.T) {
		m, _ := composeTestModel(t)
		_ = press(t, m, key("n"))
		_ = m.focusZone(ui.ZoneBody)
		paste(m, "line one\nline two")
		got := m.compose.body.Value()
		if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
			t.Fatalf("body = %q, want both pasted lines", got)
		}
	})

	t.Run("search bar", func(t *testing.T) {
		m := searchTestModel(t)
		_ = press(t, m, key("/"))
		if m.search == nil || !m.search.editing {
			t.Fatal("search bar did not open")
		}
		seq := m.search.seq
		paste(m, "invoice")
		if got := m.search.input.Value(); got != "invoice" {
			t.Fatalf("query = %q, want the pasted text", got)
		}
		// A pasted query searches like a typed one: the debounced issue
		// is scheduled, not left for the next keystroke.
		if m.search.seq == seq {
			t.Fatal("paste did not schedule the debounced search")
		}
	})

	t.Run("advanced search field", func(t *testing.T) {
		m := searchTestModel(t)
		_ = press(t, m, keyCtrl('s'))
		if m.search == nil || m.search.adv == nil {
			t.Fatal("ctrl+s did not open the advanced modal")
		}
		paste(m, "hopper")
		if got := m.search.adv.fields[0].input.Value(); got != "hopper" {
			t.Fatalf("field = %q, want the pasted text", got)
		}
	})

	t.Run("contact form", func(t *testing.T) {
		m, _ := contactsBoot(t)
		pump(t, m, press(t, m, key("c")))
		_ = press(t, m, key("n"))
		if m.contactForm == nil {
			t.Fatal("n did not open the form")
		}
		paste(m, "Grace")
		if got := m.contactForm.fields[0].Value(); got != "Grace" {
			t.Fatalf("field = %q, want the pasted text", got)
		}
	})

	t.Run("no field focused", func(t *testing.T) {
		m, _ := composeTestModel(t)
		if cmd := paste(m, "stray"); cmd != nil {
			t.Fatalf("paste with no text field returned a command: %v", cmd)
		}
		if m.compose != nil || m.search != nil || m.contactForm != nil {
			t.Fatal("stray paste opened something")
		}
	})

	t.Run("overlay keeps the keyboard", func(t *testing.T) {
		// The switcher needs two accounts to mean anything (openSwitcher
		// refuses with one).
		m, _, _ := newTwoAccountModel(t)
		loadAll(t, m)
		_ = press(t, m, key("n"))
		_ = m.focusZone(ui.ZoneBody)
		m.openSwitcher()
		if m.switcher == nil {
			t.Fatal("switcher did not open")
		}
		paste(m, "sneaky")
		if got := m.compose.body.Value(); strings.Contains(got, "sneaky") {
			t.Fatalf("paste bypassed the switcher into the body: %q", got)
		}
	})

	// Pasted control bytes never enter a value: bubbles' sanitizer drops
	// them the way it drops them from typed input, so paste cannot put an
	// escape into a field that a keystroke couldn't. The printable
	// remnants ("[2J") are inert text.
	t.Run("control bytes never land", func(t *testing.T) {
		m := searchTestModel(t)
		_ = press(t, m, key("/"))
		paste(m, "ab\x1b[2J\x07c")
		if got := m.search.input.Value(); got != "ab[2Jc" {
			t.Fatalf("query = %q, want control runes dropped", got)
		}
	})

	// ctrl+v arrives as a key and lands in the field, which answers with
	// bubbles' clipboard-read command. Running that command needs a system
	// clipboard (CI has none), so this pins the path, not the read: the
	// key produced the widget's command instead of dying in the model,
	// and the reply — unexported, unnamed by any case — takes the same
	// route into the field PasteMsg does.
	t.Run("ctrl+v reaches the field", func(t *testing.T) {
		m, _ := composeTestModel(t)
		_ = press(t, m, key("n"))
		if cmd := press(t, m, keyCtrl('v')); cmd == nil {
			t.Fatal("ctrl+v produced no command: the field never saw the key")
		}
		if got := m.compose.to.Value(); got != "" {
			t.Fatalf("To = %q: ctrl+v must not insert anything itself", got)
		}
	})
}
