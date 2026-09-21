package app

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// newTestModel wires a model to a mockjmap server, headless: Cmds are
// executed by calling them, so the full Update loop runs without a
// terminal.
func newTestModel(t *testing.T) (*Model, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 3, UnreadEmails: 2},
	})
	srv.SetEmails([]mockjmap.Email{
		{
			ID: "e2", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From:       []mockjmap.Address{{Name: "Bob", Email: "bob@example.test"}},
			Subject:    "Re: thread starter",
			ReceivedAt: time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC),
			TextBody:   "The reply body.\n",
		},
		{
			ID: "e1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From:       []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
			Subject:    "thread starter",
			ReceivedAt: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC),
			HTMLBody:   "<html><body><p>The <b>original</b> body.</p></body></html>",
		},
	})
	t.Cleanup(srv.Close)

	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{Provider: c, Keys: km, Theme: ui.NewTheme(ui.DarkTheme())})
	m.width, m.height = 120, 40
	return m, srv
}

// pump executes a Cmd and feeds its message into the model, following the
// returned command chain.
func pump(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			return
		}
		var next tea.Cmd
		_, next = m.Update(msg)
		cmd = next
	}
}

func key(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Text: s, Code: rune(s[0])}
}

func TestAppEndToEndReaderFlow(t *testing.T) {
	m, _ := newTestModel(t)

	// Window size arrives first in a real program.
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		pump(t, m, cmd)
	}

	// Init loads mailboxes; the snapshot then auto-opens the inbox.
	pump(t, m, m.Init())
	if m.snap.ActiveMailbox != "mb-inbox" {
		t.Fatalf("active mailbox = %q, want mb-inbox", m.snap.ActiveMailbox)
	}
	if m.snap.Total != 1 {
		t.Fatalf("total = %d, want 1 (thread collapsed)", m.snap.Total)
	}

	// View renders the loaded frame.
	view := m.View().Content
	if !strings.Contains(stripANSI(view), "Inbox") || !strings.Contains(stripANSI(view), "thread starter") {
		t.Fatalf("view missing expected content:\n%s", stripANSI(view))
	}

	// Loading the body of the representative (text) works.
	id := m.cursorID()
	if cmd := m.loadBody(id); cmd != nil {
		pump(t, m, cmd)
	}
	if m.snap.Body == nil || m.vpBodyID != id {
		t.Fatalf("body not installed: %+v vp %q", m.snap.Body, m.vpBodyID)
	}
	if !strings.Contains(m.snap.Body.Text, "The reply body.") {
		t.Fatalf("text body failed: %q", m.snap.Body.Text)
	}

	// Expand the thread (FR-D2), move to the HTML member, load it: the
	// body goes through the FR-E2 converter.
	toggle := m.engineOp("toggle-thread", func(ctx context.Context) (sync.Snapshot, error) {
		if err := m.engine.ToggleThread(ctx); err != nil {
			return sync.Snapshot{}, err
		}
		return m.engine.Snapshot(), nil
	})
	pump(t, m, toggle)
	if len(m.snap.Rows) != 2 {
		t.Fatalf("expanded rows = %d, want 2", len(m.snap.Rows))
	}
	m.snap = m.engine.MoveCursor(1)
	htmlID := m.cursorID()
	if cmd := m.loadBody(htmlID); cmd != nil {
		pump(t, m, cmd)
	}
	if !strings.Contains(m.snap.Body.Text, "The original body.") {
		t.Fatalf("html conversion failed: %q", m.snap.Body.Text)
	}
	if m.vpBodyID != htmlID {
		t.Fatalf("viewport id = %q, want %q", m.vpBodyID, htmlID)
	}

	// Key routing through the keymap: help toggles.
	before := m.helpOpen
	_, _ = m.handleKey(key("?"))
	if m.helpOpen == before {
		t.Fatal("help did not toggle")
	}
	_, _ = m.handleKey(key("?"))

	// Quit stops the context (FR-K1).
	_, cmd = m.handleKey(key("q"))
	if cmd == nil {
		t.Fatal("quit produced no command")
	}
	select {
	case <-m.ctx.Done():
	default:
		t.Fatal("context not cancelled on quit")
	}
}

// stripANSI removes escape sequences for readable assertions.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
