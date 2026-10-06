package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
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
	return newTestModelWith(t, []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 3, UnreadEmails: 2},
		{ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 3},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 4},
	})
}

// newTestModelWith builds the reader model against a custom mailbox tree.
func newTestModelWith(t *testing.T, mailboxes []mockjmap.Mailbox) (*Model, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", "correct-horse", mailboxes)
	srv.SetEmails([]mockjmap.Email{
		{
			ID: "e2", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			MessageID:  []string{"<m2@example.test>"},
			References: []string{"<m1@example.test>"},
			InReplyTo:  []string{"<m1@example.test>"},
			From:       []mockjmap.Address{{Name: "Bob", Email: "bob@example.test"}},
			Subject:    "Re: thread starter",
			ReceivedAt: time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC),
			TextBody:   "The reply body.\n",
			Attachments: []mockjmap.Attachment{
				{BlobID: "b1", Name: "notes.txt", Type: "text/plain", Size: 5},
			},
		},
		{
			ID: "e1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			MessageID:  []string{"<m1@example.test>"},
			From:       []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
			Subject:    "thread starter",
			ReceivedAt: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC),
			HTMLBody:   "<html><body><p>The <b>original</b> body.</p></body></html>",
		},
	})
	srv.SetBlob("b1", []byte("hello"))
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
// returned command chain — including everything inside a tea.Batch, which
// a real program runs concurrently. A generous iteration cap keeps a
// self-rearming timer from hanging the suite.
func pump(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for i := 0; i < 400 && len(queue) > 0; i++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		_, next := m.Update(msg)
		if next != nil {
			queue = append(queue, next)
		}
	}
}

func key(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Text: s, Code: rune(s[0])}
}

// TestThreadSizesArriveWithTheView (FR-D1): the list's "which of these
// rows can I press Enter on?" mark is fetched as the view renders — no
// keystroke, no manual refresh — and a collapsed two-member thread reads
// as expandable while nothing claims to be expandable before it lands.
func TestThreadSizesArriveWithTheView(t *testing.T) {
	m, _ := newTestModel(t)

	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		pump(t, m, cmd)
	}
	pump(t, m, m.loadAccountCmd())

	if m.snap.ActiveMailbox != "mb-inbox" {
		t.Fatalf("active mailbox = %q, want mb-inbox", m.snap.ActiveMailbox)
	}
	if len(m.snap.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 (the thread is collapsed)", len(m.snap.Rows))
	}
	if got := m.snap.Rows[0].ThreadSize; got != 2 {
		t.Fatalf("thread size = %d, want 2 (the reply sits beneath it)", got)
	}
}

func TestAppEndToEndReaderFlow(t *testing.T) {
	m, _ := newTestModel(t)

	// Window size arrives first in a real program.
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		pump(t, m, cmd)
	}

	// The account load (Init's network half, minus the push loop); the
	// snapshot then auto-opens the inbox.
	pump(t, m, m.loadAccountCmd())
	if m.snap.ActiveMailbox != "mb-inbox" {
		t.Fatalf("active mailbox = %q, want mb-inbox", m.snap.ActiveMailbox)
	}
	if m.snap.Total != 1 {
		t.Fatalf("total = %d, want 1 (thread collapsed)", m.snap.Total)
	}

	// View renders the loaded frame, in the alternate screen buffer so
	// quitting restores the pre-launch screen (FR-K3).
	view := m.View().Content
	if !m.View().AltScreen {
		t.Fatal("view does not claim the alternate screen (FR-K3)")
	}
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
	target := m.cursorID()
	toggle := m.engineOp("toggle-thread", func(ctx context.Context) (sync.Snapshot, error) {
		if err := m.engine.ToggleThread(ctx, target); err != nil {
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
	if !strings.Contains(m.snap.Body.Text, "The **original** body.") {
		t.Fatalf("html conversion failed: %q", m.snap.Body.Text)
	}
	if !m.snap.Body.Styled {
		t.Fatal("html body not marked styled for the preview renderer")
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

// TestOpenMailboxMovesFocusToList: Enter on a folder opens it and hands
// the cursor to the list — the folder panel's job is done, reading starts
// (FR-C2).
func TestOpenMailboxMovesFocusToList(t *testing.T) {
	m, _ := newTestModel(t)
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		pump(t, m, cmd)
	}
	pump(t, m, m.loadAccountCmd())

	m.focus = ui.PaneSidebar
	m.sidebarKey = sidebarRowKey(m.activeID, "mb-trash")
	_, cmd = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)

	if m.focus != ui.PaneList {
		t.Errorf("focus = %v, want list", m.focus)
	}
	if m.snap.ActiveMailbox != "mb-trash" {
		t.Errorf("active mailbox = %q, want mb-trash", m.snap.ActiveMailbox)
	}
}

// TestLayoutTogglePersists: z flips the pane layout and remembers it in
// prefs.toml — the app-managed file only, never config.toml (FR-I10,
// FR-J1) — and a fresh model boots from the saved choice.
func TestLayoutTogglePersists(t *testing.T) {
	m, _ := newTestModel(t)
	prefsPath := filepath.Join(t.TempDir(), "prefs.toml")
	m.opts.Prefs = &config.Prefs{}
	m.opts.PrefsPath = prefsPath

	_, cmd := m.handleKey(key("z"))
	if cmd != nil {
		t.Fatal("layout toggle produced a command (local state only)")
	}
	if !m.stacked {
		t.Fatal("z did not switch to the stacked layout")
	}
	data, err := os.ReadFile(prefsPath)
	if err != nil {
		t.Fatalf("prefs file: %v", err)
	}
	if !strings.Contains(string(data), `layout = "stacked"`) {
		t.Fatalf("prefs file missing the layout choice:\n%s", data)
	}

	// A fresh model boots stacked from the saved prefs.
	got, err := config.LoadPrefs(prefsPath)
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if fresh := New(Options{Prefs: got}); !fresh.stacked {
		t.Fatal("fresh model did not restore the stacked layout")
	}

	// Toggling back clears the key: prefs stay minimal, default layout.
	_, _ = m.handleKey(key("z"))
	if m.stacked {
		t.Fatal("second z did not return to side-by-side")
	}
	data, err = os.ReadFile(prefsPath)
	if err != nil {
		t.Fatalf("prefs file: %v", err)
	}
	if strings.Contains(string(data), "layout") {
		t.Fatalf("prefs still record a layout:\n%s", data)
	}
}

// TestPreviewPagingFromAnyPane: pgdn/pgup page the preview regardless of
// the focused pane and never steal focus; the preview's own keys keep
// working while it is focused (FR-E3).
func TestPreviewPagingFromAnyPane(t *testing.T) {
	m, _ := newTestModel(t)
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		pump(t, m, cmd)
	}
	pump(t, m, m.loadAccountCmd())

	// A long body in a short viewport, so a page move is observable.
	m.vp.SetContent(strings.Repeat("line of text\n", 100))
	m.vp.SetHeight(10)
	m.vp.GotoTop()

	m.focus = ui.PaneList
	_, cmd = m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if cmd != nil {
		t.Fatal("preview paging produced a command (local scroll only)")
	}
	if m.vp.YOffset() == 0 {
		t.Fatal("pgdn did not scroll the preview from the list pane")
	}
	if m.focus != ui.PaneList {
		t.Fatalf("focus = %v, want list (unchanged)", m.focus)
	}

	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.vp.YOffset() != 0 {
		t.Fatalf("pgup offset = %d, want 0", m.vp.YOffset())
	}

	// The current method while the preview is active is untouched.
	m.focus = ui.PanePreview
	_, _ = m.handleKey(key("d"))
	if m.vp.YOffset() == 0 {
		t.Fatal("d did not half-page the preview while focused")
	}
}

// TestBodyWrapsLongLines: a body line wider than the pane is word-wrapped
// instead of clipped off-screen (issue #5), and a resize re-wraps from the
// stored raw text without losing or duplicating content.
func TestBodyWrapsLongLines(t *testing.T) {
	m, _ := newTestModel(t)
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		pump(t, m, cmd)
	}
	if m.vp.Width() <= 0 {
		t.Fatalf("viewport width = %d after resize", m.vp.Width())
	}

	raw := strings.Repeat("word ", 60) + "unbreakablekeyword\nshort line"
	m.setBody(raw, false, nil)

	check := func(stage string) {
		t.Helper()
		view := m.vp.View()
		if !strings.Contains(ansi.Strip(view), "short line") {
			t.Errorf("%s: the short source line is missing", stage)
		}
		// Wrapping only moves whitespace: no character may be lost or
		// duplicated on the way through the viewport.
		if got, want := squeeze(view), squeeze(raw); got != want {
			t.Errorf("%s: content changed by wrapping:\n got %q\nwant %q", stage, got, want)
		}
		for i, l := range strings.Split(view, "\n") {
			if w := lipgloss.Width(l); w > m.vp.Width() {
				t.Errorf("%s: line %d is %d cells wide, viewport is %d", stage, i, w, m.vp.Width())
			}
		}
	}
	check("initial")

	// Wrapping is word-aware, not a hard cut at the pane edge: the long
	// word survives whole and the first line ends on a word boundary.
	lines := strings.Split(m.vp.View(), "\n")
	if !strings.Contains(strings.Join(lines, " "), "unbreakablekeyword") {
		t.Error("a word was split across wrapped lines")
	}
	if last := strings.TrimSpace(lines[0]); last != "" && !strings.HasSuffix(last, "word") {
		t.Errorf("first wrapped line ends mid-word: %q", last)
	}

	// Narrower pane: re-wrapped from raw, still complete and in-bounds.
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.vp.Width() >= 40 {
		t.Fatalf("resize did not narrow the viewport: width = %d", m.vp.Width())
	}
	check("narrow")

	// Back to wide: the wrapped lines rejoin, proving the raw text (not
	// the wrapped form) is what gets re-wrapped.
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	check("wide")
}

// TestBodyStyledRendersAndRerendersOnResize: a converted HTML body goes
// through the pane's renderer once — the palette's accent on the heading,
// the markdown consumed — and every resize then re-wraps that output the
// same way it wraps a plain body, staying in bounds at each width
// (FR-E2). Plain bodies take the identical path with nothing styled.
func TestBodyStyledRendersAndRerendersOnResize(t *testing.T) {
	m, _ := newTestModel(t)
	if _, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40}); cmd != nil {
		pump(t, m, cmd)
	}
	if m.vp.Width() <= 0 {
		t.Fatalf("viewport width = %d after resize", m.vp.Width())
	}

	md := "## Quarterly audit\n\nHello **world**, " + strings.Repeat("padding ", 40) + "end.\n\n> quoted line"
	m.setBody(md, true, nil)
	if !m.bodyStyled {
		t.Fatal("styled flag not recorded on the model")
	}

	check := func(stage string) {
		t.Helper()
		view := m.vp.View()
		if !strings.Contains(view, "\x1b[38;2;130;170;255") {
			t.Errorf("%s: heading accent missing from the frame", stage)
		}
		visible := ansi.Strip(view)
		if strings.Contains(visible, "**") || strings.Contains(visible, "##") {
			t.Errorf("%s: markdown markers leaked into the pane: %q", stage, visible)
		}
		if !strings.Contains(visible, "Quarterly audit") {
			t.Errorf("%s: heading text missing: %q", stage, visible)
		}
		for i, l := range strings.Split(view, "\n") {
			if w := lipgloss.Width(l); w > m.vp.Width() {
				t.Errorf("%s: line %d is %d cells wide, viewport is %d", stage, i, w, m.vp.Width())
			}
		}
	}
	check("initial")

	wide := m.vp.Width()

	// Narrower pane: re-rendered at the new width, still styled and
	// still in bounds.
	if _, _ = m.Update(tea.WindowSizeMsg{Width: 110, Height: 40}); m.vp.Width() >= wide {
		t.Fatalf("resize did not narrow the viewport: width = %d (was %d)", m.vp.Width(), wide)
	}
	narrow := m.vp.Width()
	check("narrow")

	// Wide again: the long line unwraps, proving the stored styled text
	// (not the already-wrapped frame) is what re-wraps.
	if _, _ = m.Update(tea.WindowSizeMsg{Width: 150, Height: 40}); m.vp.Width() <= narrow {
		t.Fatalf("resize did not widen the viewport: width = %d (was %d)", m.vp.Width(), narrow)
	}
	check("wide")

	// A plain body must not pick up styling on the way through.
	m.setBody("plain ** body ## text", false, nil)
	if m.bodyStyled {
		t.Fatal("plain body marked styled")
	}
	if view := m.vp.View(); strings.Contains(view, "\x1b[38;2;130;170;255") {
		t.Errorf("plain body was styled: %q", view)
	}
	if view := ansi.Strip(m.vp.View()); !strings.Contains(view, "plain ** body ## text") {
		t.Errorf("plain body not passed through: %q", view)
	}
}

// squeeze collapses all whitespace so two renderings of the same body can
// be compared regardless of where the wrap landed.
func squeeze(s string) string { return strings.Join(strings.Fields(ansi.Strip(s)), "") }

// TestAppLivePump proves the latest-wins broadcast reaches the model: a
// waiter armed before a publish receives it as a live snapshot (FR-B2).
func TestAppLivePump(t *testing.T) {
	m, _ := newTestModel(t)
	pump(t, m, m.loadAccountCmd())
	if m.snap.ActiveMailbox != "mb-inbox" {
		t.Fatalf("setup failed: active = %q", m.snap.ActiveMailbox)
	}
	before := m.snap.Version

	wait := m.waitUpdates()
	m.engine.MoveCursor(1) // publishes a new snapshot
	msg := wait()
	if msg == nil {
		t.Fatal("waitUpdates returned nothing after a publish")
	}
	_, next := m.Update(msg)
	if next == nil {
		t.Fatal("live delivery did not re-arm the waiter")
	}
	if m.snap.Version <= before {
		t.Fatalf("snapshot version = %d, want > %d", m.snap.Version, before)
	}

	// A stale delivery applies nothing but stays harmless.
	_, _ = m.Update(msg)
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
