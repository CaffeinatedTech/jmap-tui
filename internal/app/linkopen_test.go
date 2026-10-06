package app

import (
	"errors"
	"testing"

	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// linkModel is a minimal reader parked on a message whose body carries the
// given footnote hrefs (FR-E2).
func linkModel(links []string) *Model {
	km, _ := ui.NewKeyMap(nil)
	return &Model{
		opts:      Options{Keys: km},
		snap:      sync.Snapshot{Rows: []sync.Row{{ID: "e1"}}, Cursor: 0},
		activeID:  "work",
		vpBodyID:  "e1",
		bodyLinks: links,
	}
}

// TestOpenLinkPickerListsFootnotes: ctrl+o (runAction) lists the body's
// links numbered like the footnotes, and choosing one hands exactly that
// URL to the opener.
func TestOpenLinkPickerListsFootnotes(t *testing.T) {
	var opened string
	m := linkModel([]string{"https://a.test", "mailto:b@test"})
	m.bodyStyled = true // HTML body: rows are numbered like the footnotes
	m.opts.OpenURL = func(raw string) error { opened = raw; return nil }

	if _, _ = m.runAction(ui.ActOpenLink); m.picker == nil || m.picker.mode != pickerLink {
		t.Fatalf("picker = %+v, want pickerLink", m.picker)
	}
	if len(m.picker.items) != 2 {
		t.Fatalf("items = %d, want 2", len(m.picker.items))
	}
	if got := m.picker.items[0].Label; got != "[1] https://a.test" {
		t.Fatalf("label = %q, want %q", got, "[1] https://a.test")
	}

	cmd, _ := m.pickerKey("enter")
	if cmd == nil {
		t.Fatal("enter produced no command")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("open command produced no message")
	}
	if opened != "https://a.test" {
		t.Fatalf("opened = %q, want https://a.test", opened)
	}
	if m.err != "" {
		t.Fatalf("err = %q, want empty", m.err)
	}
}

// TestOpenLinkPickerPlainBodyLabels: a plain-text body has no footnotes,
// so its link rows carry the bare URL, not a "[1]" prefix.
func TestOpenLinkPickerPlainBodyLabels(t *testing.T) {
	m := linkModel([]string{"https://a.test/reset?token=x"})
	m.openLinkPicker()
	if m.picker == nil || len(m.picker.items) != 1 {
		t.Fatalf("picker = %+v", m.picker)
	}
	if got := m.picker.items[0].Label; got != "https://a.test/reset?token=x" {
		t.Fatalf("label = %q, want the bare URL", got)
	}
}

// TestOpenLinkPickerRefusals: no links, and a body that is not the one on
// screen, are reported on the status line instead of opening a modal.
func TestOpenLinkPickerRefusals(t *testing.T) {
	m := linkModel(nil)
	m.openLinkPicker()
	if m.picker != nil || m.err != "no links in this message" {
		t.Fatalf("empty links: picker=%v err=%q", m.picker, m.err)
	}

	m = linkModel([]string{"https://a.test"})
	m.vpBodyID = "" // cursor's body has not hydrated yet
	m.openLinkPicker()
	if m.picker != nil || m.err != "message still loading" {
		t.Fatalf("loading: picker=%v err=%q", m.picker, m.err)
	}
}

// TestOpenURLSchemeAllowList: only http/https/mailto reach the opener, so
// a sender-chosen URL can never ask the desktop to run a local handler.
func TestOpenURLSchemeAllowList(t *testing.T) {
	m := linkModel(nil)
	called := false
	m.opts.OpenURL = func(string) error { called = true; return nil }

	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "/relative/path", "ftp://host/x"} {
		m.err = ""
		if cmd := m.openURL(bad); cmd != nil {
			t.Errorf("%q: expected nil command", bad)
		}
		if m.err == "" {
			t.Errorf("%q: expected a refusal on the status line", bad)
		}
	}
	if called {
		t.Fatal("opener called for a refused scheme")
	}

	// An allowed scheme reaches the opener; its failure travels back as a
	// linkOpenMsg so Update can surface it.
	m.err = ""
	wantErr := errors.New("no browser found")
	m.opts.OpenURL = func(string) error { return wantErr }
	msg := m.openURL("https://ok.test")()
	if lm, ok := msg.(linkOpenMsg); !ok || lm.err != wantErr {
		t.Fatalf("msg = %#v, want linkOpenMsg carrying the opener error", msg)
	}
}
