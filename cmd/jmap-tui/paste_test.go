package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Paste into the wizard (issue #4): the Update switch had no case for
// tea.PasteMsg — a terminal's bracketed paste, what ctrl+shift+v sends —
// and bubbles' ctrl+v command answers with an unexported message type no
// case can name, so both fell through to `return m, nil` and a password
// had to be typed. Paste now travels the same path as typing.

// TestWizardPasteReachesFocusedField: a pasted secret lands in the field
// that has focus, stays masked on screen, and carries no control bytes.
func TestWizardPasteReachesFocusedField(t *testing.T) {
	m, _ := newTestWizard(t, nil,
		testFetch(mailboxItems(), nil),
		func(string, string, string, bool) (string, error) { return "OS keyring", nil })
	send(m, tea.WindowSizeMsg{Width: 100, Height: 40})

	// Two tabs from the server URL land on the password field.
	send(m, keyTab(false))
	send(m, keyTab(false))
	if m.focus != 2 {
		t.Fatalf("focus = %d, want 2 (password)", m.focus)
	}

	send(m, tea.PasteMsg{Content: "p@ss-w0rd!"})
	if got := m.inputs[2].Value(); got != "p@ss-w0rd!" {
		t.Fatalf("password = %q, want the pasted text", got)
	}
	// Masked: the secret renders as bullets, never in clear.
	if frame := m.render(); strings.Contains(frame, "p@ss-w0rd!") {
		t.Fatalf("password rendered in clear:\n%s", frame)
	}

	// Control bytes never enter a value: bubbles' sanitizer drops them the
	// way it drops them from typed input, so a paste cannot put an escape
	// into a field that a keystroke couldn't. The printable remnants
	// ("[2J") are inert text.
	send(m, tea.PasteMsg{Content: "ab\x1b[2J\x07c"})
	if got := m.inputs[2].Value(); got != "p@ss-w0rd!ab[2Jc" {
		t.Fatalf("value = %q, want control runes dropped", got)
	}
}

// TestWizardPasteIgnoredOffTheForm: outside the form step there is no
// focused field, so a paste must not stuff a hidden one.
func TestWizardPasteIgnoredOffTheForm(t *testing.T) {
	m, _ := newTestWizard(t, nil,
		testFetch(mailboxItems(), nil),
		func(string, string, string, bool) (string, error) { return "OS keyring", nil })
	m.step = wizMailbox

	send(m, tea.PasteMsg{Content: "nope"})
	for i, in := range m.inputs {
		if in.Value() != "" {
			t.Fatalf("field %d = %q on a non-form step", i, in.Value())
		}
	}
}

// TestWizardCtrlVReachesTheField: ctrl+v arrives as a key and lands in
// the field, which answers with bubbles' own clipboard-read command.
// Running that command needs a system clipboard (CI has none), so this
// pins the path, not the read: the key produced the widget's command
// instead of dying in the model, and the reply — unexported, unnamed by
// any case — takes the same route into the field that PasteMsg does
// above.
func TestWizardCtrlVReachesTheField(t *testing.T) {
	m, _ := newTestWizard(t, nil,
		testFetch(mailboxItems(), nil),
		func(string, string, string, bool) (string, error) { return "OS keyring", nil })

	cmd := send(m, tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+v produced no command: the field never saw the key")
	}
	if got := m.inputs[0].Value(); got != "" {
		t.Fatalf("value = %q: ctrl+v must not insert anything itself", got)
	}
}
