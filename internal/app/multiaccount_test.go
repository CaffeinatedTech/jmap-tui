package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// M6 tests (FR-A4/A5, FR-F3): two accounts, the switcher, the unified
// inbox, and the routing rule that actions never cross accounts.
//
// Both servers deliberately use the SAME message ids ("e1"/"e2"): JMAP
// ids are only unique per account, so selection, body tracking, and
// action routing must qualify them with the owner everywhere (FR-A5).

// twoAccountFixture is one mock server with two inbox messages: a newest
// at 10:00/09:00 and an older one at 08:00/07:00 (relative to base), all
// unread.
func twoAccountFixture(user string, newest, oldest time.Time) []mockjmap.Email {
	return []mockjmap.Email{
		{
			ID: "e2", ThreadID: "t2", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Newest " + user, Email: "n-" + user + "@example.test"}},
			Subject: "newest " + user, ReceivedAt: newest,
			TextBody: "newest " + user + " body\n",
		},
		{
			ID: "e1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Older " + user, Email: "o-" + user + "@example.test"}},
			Subject: "older " + user, ReceivedAt: oldest,
			TextBody: "oldest " + user + " body\n",
		},
	}
}

// newTwoAccountModel wires the model to two independent mock servers —
// "work" (active) and "personal" — whose inbox dates interleave
// (work e2 10:00, personal e2 09:00, work e1 08:00, personal e1 07:00).
func newTwoAccountModel(t *testing.T) (*Model, *mockjmap.Server, *mockjmap.Server) {
	t.Helper()
	mailboxes := []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 2, UnreadEmails: 2},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 1},
		{ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 2},
	}
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

	srvWork := mockjmap.New("work@example.com", "pw", mailboxes)
	srvWork.SetEmails(twoAccountFixture("work", base.Add(10*time.Hour), base.Add(8*time.Hour)))
	t.Cleanup(srvWork.Close)
	srvPersonal := mockjmap.New("personal@example.com", "pw", mailboxes)
	srvPersonal.SetEmails(twoAccountFixture("personal", base.Add(9*time.Hour), base.Add(7*time.Hour)))
	t.Cleanup(srvPersonal.Close)

	connect := func(srv *mockjmap.Server, user string) *jmapclient.Client {
		c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: user, Password: "pw"})
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		return c
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{
		Accounts: []AccountOpt{
			{ID: "work", Name: "Work", Provider: connect(srvWork, "work@example.com"), Connected: true},
			{ID: "personal", Name: "Personal", Provider: connect(srvPersonal, "personal@example.com"), Connected: true},
		},
		Keys:  km,
		Theme: ui.NewTheme(ui.DarkTheme()),
	})
	m.width, m.height = 120, 40
	return m, srvWork, srvPersonal
}

// loadAll runs the FR-B1 load for every enrolled account headlessly: the
// first mailbox snapshot then auto-opens each account's inbox (the warm
// window instant switching needs, FR-A4).
func loadAll(t *testing.T, m *Model) {
	t.Helper()
	for _, a := range m.accounts {
		if m.hub.Provider(a.ID) == nil {
			continue // credential-less accounts never load (failure isolation)
		}
		acct := a.ID
		pump(t, m, m.opOn(acct, "load", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
			_ = eng.LoadIdentities(ctx)
			if err := eng.LoadMailboxes(ctx); err != nil {
				return sync.Snapshot{}, err
			}
			return eng.Snapshot(), nil
		}))
	}
}

// accountOfTags lists the owner tag of each rendered row.
func accountOfTags(m *Model) []string {
	out := make([]string, 0, len(m.snap.Rows))
	for _, r := range m.snap.Rows {
		out = append(out, r.Account)
	}
	return out
}

// TestKeymapAccounts: the documented keys resolve (README `S`/`i`).
func TestKeymapAccounts(t *testing.T) {
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	if act, ok := km.Match(ui.PaneAny, "shift+a"); !ok || act != ui.ActAccountSwitch {
		t.Fatalf("shift+a = %v,%v, want account.switch", act, ok)
	}
	if act, ok := km.Match(ui.PaneAny, "i"); !ok || act != ui.ActUnified {
		t.Fatalf("i = %v,%v, want unified.toggle", act, ok)
	}
	if err := km.Validate(); err != nil {
		t.Fatalf("keymap conflict: %v", err)
	}
}

// TestAccountSwitchKeepsIndependentState: each account keeps its own
// cursor and window; switching renders the stored engine state instantly
// (FR-A4).
func TestAccountSwitchKeepsIndependentState(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)

	if m.activeID != "work" {
		t.Fatalf("active = %q, want work", m.activeID)
	}
	// Move the work cursor to row 1 (older message).
	_, _ = m.moveCursor(1)
	if m.cursorID() != "e1" {
		t.Fatalf("work cursor = %q, want e1", m.cursorID())
	}

	// Switch (what the switcher's Enter does).
	_, cmd := m.switchAccount("personal")
	pump(t, m, cmd)
	if m.activeID != "personal" {
		t.Fatalf("active = %q, want personal", m.activeID)
	}
	if m.snap.Rows[m.snap.Cursor].Summary.Subject != "newest personal" {
		t.Fatalf("personal cursor row = %+v, want newest personal", m.snap.Rows[m.snap.Cursor])
	}

	// Switch back: work's cursor survived (independent sync state).
	_, cmd = m.switchAccount("work")
	pump(t, m, cmd)
	if m.cursorID() != "e1" {
		t.Fatalf("work cursor after round trip = %q, want e1", m.cursorID())
	}
}

// TestSwitcherModal: S opens the switcher on the active account and Enter
// switches (FR-A4, FR-I7).
func TestSwitcherModal(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)

	m.openSwitcher()
	if m.switcher == nil {
		t.Fatal("switcher did not open")
	}
	if m.switcher.sel != 0 {
		t.Fatalf("sel = %d, want 0 (work)", m.switcher.sel)
	}
	st := m.uiState()
	if st.AccountSwitch == nil || len(st.AccountSwitch.Accounts) != 2 {
		t.Fatalf("switcher view = %+v, want 2 accounts", st.AccountSwitch)
	}
	if !st.AccountSwitch.Accounts[0].Active || st.AccountSwitch.Accounts[1].Active {
		t.Fatalf("active flags wrong: %+v", st.AccountSwitch.Accounts)
	}

	// j moves to personal, enter switches.
	_, _ = m.switcherKey(key("j"))
	_, cmd := m.switcherKey(keyEnter())
	pump(t, m, cmd)
	if m.switcher != nil {
		t.Fatal("switcher stayed open after enter")
	}
	if m.activeID != "personal" {
		t.Fatalf("active = %q, want personal", m.activeID)
	}
}

// TestUnifiedInterleavesByReceivedAt: the merged inbox interleaves by
// receivedAt; the frame's rows carry the owner colour bar and the preview
// names the owner (FR-A5).
func TestUnifiedInterleavesByReceivedAt(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)

	_, cmd := m.handleKey(key("i"))
	pump(t, m, cmd)
	if !m.unified {
		t.Fatal("unified view did not open")
	}
	got := accountOfTags(m)
	want := []string{"work", "personal", "work", "personal"}
	if len(got) != len(want) {
		t.Fatalf("owners = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d owner = %q, want %q (rows: %+v)", i, got[i], want[i], m.snap.Rows)
		}
	}
	// Dates descend across the merge.
	for i := 1; i < len(m.snap.Rows); i++ {
		if m.snap.Rows[i].Summary.ReceivedAt.After(m.snap.Rows[i-1].Summary.ReceivedAt) {
			t.Fatalf("row %d is newer than row %d: not interleaved", i, i-1)
		}
	}

	// The frame shows the unified header; ownership is a colour bar on
	// each row and a spelled-out Account line in the preview (FR-A5).
	raw := m.View().Content
	view := stripANSI(raw)
	if !strings.Contains(view, "unified inbox") {
		t.Fatalf("view missing unified header:\n%s", view)
	}
	if !strings.Contains(view, "Account: Work") {
		t.Fatalf("preview missing the owner line:\n%s", view)
	}
	if !strings.Contains(raw, "48;2;") {
		t.Fatal("row owner bars (background tints) not rendered")
	}
}

// TestUnifiedBodyLoadsForEveryRow: the merged cursor walks every row and
// each one shows its own body. The unified cursor is app-side (PLAN
// §4.3) — no engine cursor tracks it — so a body load must not be gated
// on one (issue #1: each account's first row read, every row below it
// stuck on "loading message…").
func TestUnifiedBodyLoadsForEveryRow(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)

	_, cmd := m.handleKey(key("i"))
	pump(t, m, cmd)
	if !m.unified {
		t.Fatal("unified view did not open")
	}

	// Merged by receivedAt: work e2 10:00, personal e2 09:00,
	// work e1 08:00, personal e1 07:00.
	want := []string{"newest work body", "newest personal body", "oldest work body", "oldest personal body"}
	if len(m.snap.Rows) != len(want) {
		t.Fatalf("merged rows = %d, want %d: %+v", len(m.snap.Rows), len(want), m.snap.Rows)
	}
	for i, text := range want {
		if i > 0 {
			_, cmd = m.handleKey(key("j"))
			// The frame that creates the request must say so — no blank
			// preview while the fetch is set up.
			if !m.snap.BodyLoading {
				t.Errorf("row %d: issuing frame does not claim to be loading", i)
			}
			pump(t, m, cmd)
		}
		acct, row, ok := m.cursorRef()
		if !ok {
			t.Fatalf("row %d: no cursor row", i)
		}
		if got := m.rowKey(acct, row.ID); m.vpBodyID != got {
			t.Errorf("row %d: viewport key = %q, want %q", i, m.vpBodyID, got)
		}
		if m.snap.BodyLoading {
			t.Errorf("row %d: preview still claims to be loading", i)
		}
		if m.snap.Body == nil || m.snap.Body.ID != row.ID {
			t.Errorf("row %d: merged snapshot carries no body: %+v", i, m.snap.Body)
		}
		if !strings.Contains(m.vp.View(), text) {
			t.Errorf("row %d: viewport = %q, want %q", i, m.vp.View(), text)
		}
		if view := stripANSI(m.View().Content); strings.Contains(view, "loading message") {
			t.Errorf("row %d: frame still says loading:\n%s", i, view)
		}
	}
}

// TestUnifiedDropsStaleBodyReply: a body that lands for a row the cursor
// already left must not install under the new one, and must not retire
// the request the preview is actually waiting on.
func TestUnifiedDropsStaleBodyReply(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)

	_, cmd := m.handleKey(key("i"))
	pump(t, m, cmd)
	firstKey := m.cursorKey()
	if m.vpBodyID != firstKey {
		t.Fatalf("row 0 body not installed: %q, want %q", m.vpBodyID, firstKey)
	}

	// Moving to row 1 issues its load synchronously (applyUnified runs on
	// the UI thread), so bodyReq waits on row 1 while row 0 still shows.
	_, cmd = m.handleKey(key("j"))
	wantReq := m.cursorKey()
	if wantReq == firstKey {
		t.Fatal("cursor did not move")
	}
	if m.bodyReq != wantReq {
		t.Fatalf("in-flight request = %q, want %q", m.bodyReq, wantReq)
	}

	// A late reply for row 2 — a row the cursor is no longer on — arrives.
	staleKey := m.rowKey("work", "e1")
	if _, staleCmd := m.Update(bodyMsg{key: staleKey, body: &sync.BodyView{ID: "e1", Text: "stale body"}}); staleCmd != nil {
		t.Fatalf("stale reply returned a command: %v", staleCmd)
	}
	if m.bodyReq != wantReq {
		t.Fatalf("stale reply retired the current request: %q, want %q", m.bodyReq, wantReq)
	}
	if m.vpBodyID != firstKey {
		t.Fatalf("stale reply installed under the cursor: %q, want %q", m.vpBodyID, firstKey)
	}
	if strings.Contains(m.vp.View(), "stale body") {
		t.Fatalf("stale body reached the viewport: %q", m.vp.View())
	}

	// The pending request still installs row 1 when it lands.
	pump(t, m, cmd)
	if m.vpBodyID != wantReq {
		t.Fatalf("row 1 not installed: %q, want %q", m.vpBodyID, wantReq)
	}
	if !strings.Contains(m.vp.View(), "newest personal body") {
		t.Fatalf("row 1 viewport = %q, want newest personal body", m.vp.View())
	}
	if m.snap.BodyLoading {
		t.Fatal("row 1 preview still claims to be loading")
	}
}

// TestUnifiedEnterExitRestoresMailboxes: entering forces every account to
// its inbox (the merge needs it); leaving restores where each account was
// (unified is a view, FR-A5).
func TestUnifiedEnterExitRestoresMailboxes(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)

	// personal browses Archive before unified opens.
	cmd := m.openMailboxOn("personal", "mb-archive")
	pump(t, m, cmd)
	if m.snaps["personal"].ActiveMailbox != "mb-archive" {
		t.Fatalf("personal active = %q, want mb-archive", m.snaps["personal"].ActiveMailbox)
	}

	_, cmd = m.handleKey(key("i"))
	pump(t, m, cmd)
	inboxOf := func(id string) mail.ID {
		return inboxID(m.snaps[id].Mailboxes)
	}
	if m.snaps["personal"].ActiveMailbox != inboxOf("personal") {
		t.Fatalf("personal in unified = %q, want inbox", m.snaps["personal"].ActiveMailbox)
	}
	if m.snaps["work"].ActiveMailbox != inboxOf("work") {
		t.Fatalf("work in unified = %q, want inbox", m.snaps["work"].ActiveMailbox)
	}

	_, cmd = m.handleKey(key("i"))
	pump(t, m, cmd)
	if m.unified {
		t.Fatal("unified view did not close")
	}
	if m.snaps["personal"].ActiveMailbox != "mb-archive" {
		t.Fatalf("personal after leave = %q, want mb-archive restored", m.snaps["personal"].ActiveMailbox)
	}
}

// TestUnifiedActionsRouteToOwner: a triage action on a unified row runs
// on the row's owning account and touches nothing else — the M6 gate's
// "actions never cross accounts". Both servers use the same message ids,
// so only owner-qualified routing can keep this correct.
func TestUnifiedActionsRouteToOwner(t *testing.T) {
	m, _, srvPersonal := newTwoAccountModel(t)
	loadAll(t, m)

	_, cmd := m.handleKey(key("i"))
	pump(t, m, cmd)

	// Cursor starts on the newest row: work's e2. Toggle it read — the
	// work server must see it, personal must not.
	if owner, _, _ := m.cursorRef(); owner != "work" {
		t.Fatalf("cursor owner = %q, want work", owner)
	}
	_, cmd = m.handleKey(key("u"))
	pump(t, m, cmd)

	if !m.snaps["work"].Rows[0].Summary.Keywords.Has("$seen") {
		t.Fatalf("work row not marked: %+v", m.snaps["work"].Rows[0].Summary)
	}
	for _, r := range m.snaps["personal"].Rows {
		if r.Summary.Keywords.Has("$seen") {
			t.Fatalf("personal row %q was touched by a work action", r.ID)
		}
	}
	// The personal server itself is untouched.
	if got := srvPersonal.CountIn("mb-inbox"); got != 2 {
		t.Fatalf("personal inbox count = %d, want 2", got)
	}

	// Move the cursor onto personal's row and toggle: only personal moves.
	_, _ = m.moveCursor(1)
	if owner, _, _ := m.cursorRef(); owner != "personal" {
		t.Fatalf("cursor owner = %q, want personal", owner)
	}
	_, cmd = m.handleKey(key("u"))
	pump(t, m, cmd)
	if !m.snaps["personal"].Rows[0].Summary.Keywords.Has("$seen") {
		t.Fatalf("personal row not marked: %+v", m.snaps["personal"].Rows[0].Summary)
	}
	if !m.snaps["work"].Rows[0].Summary.Keywords.Has("$seen") {
		t.Fatal("work lost its earlier read marker")
	}
	if m.snaps["work"].Rows[1].Summary.Keywords.Has("$seen") {
		t.Fatal("work older row was touched by a personal action")
	}
}

// TestUnifiedToggleThreadExpandsPressedRow: the unified cursor is app-side
// and no engine cursor ever moves there (PLAN §4.3), so Enter must expand
// the row it rendered. A cursor-relative toggle expands whatever the owner
// engine last had selected — the chevron landing on a message nobody
// pressed, often a different subject.
func TestUnifiedToggleThreadExpandsPressedRow(t *testing.T) {
	mailboxes := []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 3, UnreadEmails: 3},
	}
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	addr := func(name string) []mockjmap.Address {
		return []mockjmap.Address{{Name: name, Email: name + "@example.test"}}
	}

	// work: a standalone at 10:00, plus a two-member thread at 08:00/07:00.
	srvWork := mockjmap.New("work@example.com", "pw", mailboxes)
	srvWork.SetEmails([]mockjmap.Email{
		{
			ID: "w3", ThreadID: "t3", MailboxIDs: []string{"mb-inbox"},
			From: addr("solo"), Subject: "standalone work",
			ReceivedAt: base.Add(10 * time.Hour), TextBody: "solo\n",
		},
		{
			ID: "w2", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From: addr("bob"), Subject: "Re: thread",
			ReceivedAt: base.Add(8 * time.Hour), TextBody: "reply\n",
		},
		{
			ID: "w1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From: addr("alice"), Subject: "thread",
			ReceivedAt: base.Add(7 * time.Hour), TextBody: "original\n",
		},
	})
	t.Cleanup(srvWork.Close)

	// personal: two standalones that interleave around it.
	srvPersonal := mockjmap.New("personal@example.com", "pw", mailboxes)
	srvPersonal.SetEmails([]mockjmap.Email{
		{
			ID: "p2", ThreadID: "pt2", MailboxIDs: []string{"mb-inbox"},
			From: addr("carol"), Subject: "newer personal",
			ReceivedAt: base.Add(9 * time.Hour), TextBody: "hi\n",
		},
		{
			ID: "p1", ThreadID: "pt1", MailboxIDs: []string{"mb-inbox"},
			From: addr("dave"), Subject: "older personal",
			ReceivedAt: base.Add(5 * time.Hour), TextBody: "hi\n",
		},
	})
	t.Cleanup(srvPersonal.Close)

	connect := func(srv *mockjmap.Server, user string) *jmapclient.Client {
		c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: user, Password: "pw"})
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		return c
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{
		Accounts: []AccountOpt{
			{ID: "work", Name: "Work", Provider: connect(srvWork, "work@example.com"), Connected: true},
			{ID: "personal", Name: "Personal", Provider: connect(srvPersonal, "personal@example.com"), Connected: true},
		},
		Keys:  km,
		Theme: ui.NewTheme(ui.DarkTheme()),
	})
	m.width, m.height = 120, 40
	loadAll(t, m)

	_, cmd := m.handleKey(key("i"))
	pump(t, m, cmd)

	// Merged by date: w3 10:00, p2 09:00, w2 08:00, p1 05:00.
	rows := m.snap.Rows
	if len(rows) != 4 {
		t.Fatalf("merged rows = %d: %+v", len(rows), rows)
	}

	// Cursor onto w2 (the thread) — engine cursors stay put at row 0.
	_, _ = m.moveCursor(2)
	if id := m.cursorID(); id != "w2" {
		t.Fatalf("unified cursor = %q, want w2", id)
	}
	_, cmd = m.handleKey(keyEnter())
	pump(t, m, cmd)

	rows = m.snap.Rows
	if len(rows) != 5 {
		t.Fatalf("rows after Enter = %d: %+v", len(rows), rows)
	}
	if !rows[2].ThreadHeader || rows[2].ID != "w2" {
		t.Fatalf("chevron not on the pressed row: %+v", rows[2])
	}
	if !rows[3].ThreadMember || rows[3].ID != "w1" {
		t.Fatalf("thread's second member not rendered under the header: %+v", rows[3])
	}
	if rows[0].ThreadHeader {
		t.Fatalf("chevron on the unpressed standalone row: %+v", rows[0])
	}
}

// TestUnifiedSelectionQualifiesIds: with identical ids across accounts,
// x on one row must not select the other account's row (FR-G3 + FR-A5).
func TestUnifiedSelectionQualifiesIds(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)
	_, cmd := m.handleKey(key("i"))
	pump(t, m, cmd)

	_, _ = m.handleKey(key("x")) // selects work's e2
	if len(m.sel) != 1 {
		t.Fatalf("sel = %v, want exactly one qualified key", m.sel)
	}
	var key0 mail.ID
	for k := range m.sel {
		key0 = k
	}
	if !strings.HasPrefix(string(key0), "work\x00") {
		t.Fatalf("selection key = %q, want work-qualified", key0)
	}
	// The rendered view agrees: exactly one marker in the frame.
	view := stripANSI(m.View().Content)
	if c := strings.Count(view, "×"); c != 1 {
		t.Fatalf("selection markers in view = %d, want 1\n%s", c, view)
	}
}

// TestUnifiedSearchMergesAccounts (FR-F3): one query fans out to every
// account and the results merge, each row tagged with its owner.
func TestUnifiedSearchMergesAccounts(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	loadAll(t, m)
	_, cmd := m.handleKey(key("i"))
	pump(t, m, cmd)

	_, cmd = m.handleKey(key("/"))
	pump(t, m, cmd)
	if m.search == nil {
		t.Fatal("search bar did not open")
	}
	m.search.spec.Text = "newest"
	cmd = m.searchKey(keyEnter())
	pump(t, m, cmd)

	if len(m.snap.Rows) != 2 {
		t.Fatalf("merged results = %d rows, want 2 (one per account): %+v", len(m.snap.Rows), m.snap.Rows)
	}
	owners := accountOfTags(m)
	if owners[0] != "work" || owners[1] != "personal" {
		t.Fatalf("result owners = %v, want [work personal] (by date)", owners)
	}

	// Esc closes the search on both accounts and restores the merge of
	// inbox windows.
	cmd = m.searchKey(keyEsc())
	pump(t, m, cmd)
	if m.search != nil {
		t.Fatal("search stayed open after esc")
	}
	if len(m.snap.Rows) != 4 {
		t.Fatalf("rows after close = %d, want 4 inbox rows", len(m.snap.Rows))
	}
}

// TestFailureIsolationInStatus: a pre-flight failure surfaces on that
// account's status (FR-I5) while the healthy account keeps working (M6
// failure isolation).
func TestFailureIsolationInStatus(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	// Enroll a third account with no provider and a recorded failure.
	bad := AccountOpt{ID: "broken", Name: "Broken", Err: errors.New("connect: auth rejected")}
	m.opts.Accounts = append(m.opts.Accounts, bad)
	m.hub.Enroll(bad.ID, bad.Name, nil, false, sync.Config{})
	m.hub.NoteError(bad.ID, bad.Err)
	m.accounts = m.hub.Accounts()
	m.snaps[bad.ID] = sync.Snapshot{Total: -1}

	// The status waiter delivers the failure snapshot.
	wait := m.waitUpdatesFor("broken")
	msg := wait()
	if msg == nil {
		t.Fatal("no status snapshot for the failed account")
	}
	_, _ = m.Update(msg)

	views := m.accountViews()
	var found bool
	for _, v := range views {
		if v.ID == "broken" {
			found = true
			if !strings.Contains(v.LastError, "auth rejected") {
				t.Fatalf("broken status = %q, want auth rejection", v.LastError)
			}
		}
	}
	if !found {
		t.Fatal("broken account missing from status views")
	}

	// The healthy accounts are unaffected: they still load and render.
	loadAll(t, m)
	if m.snaps["work"].ActiveMailbox == "" || m.snaps["personal"].ActiveMailbox == "" {
		t.Fatalf("healthy accounts did not load: work=%q personal=%q",
			m.snaps["work"].ActiveMailbox, m.snaps["personal"].ActiveMailbox)
	}
}

// TestMovePickerQueuesOwners: in unified view a move prompts per owning
// account — each prompt lists that account's own tree — and dispatches
// both groups to their engines (FR-A5).
func TestMovePickerQueuesOwners(t *testing.T) {
	m, srvWork, srvPersonal := newTwoAccountModel(t)
	loadAll(t, m)
	_, cmd := m.handleKey(key("i"))
	pump(t, m, cmd)

	// Select both newest rows (work e2 + personal e2).
	_, _ = m.handleKey(key("x"))
	_, _ = m.moveCursor(1)
	_, _ = m.handleKey(key("x"))
	if len(m.pickPending) != 0 {
		t.Fatal("pending batch exists before the picker opened")
	}

	_, _ = m.handleKey(key("m")) // move → queue two prompts
	if m.picker == nil {
		t.Fatal("move picker did not open")
	}
	if m.picker.acct != "work" {
		t.Fatalf("first prompt owner = %q, want work", m.picker.acct)
	}
	if len(m.picker.queue) != 1 || m.picker.queue[0] != "personal" {
		t.Fatalf("queue = %v, want [personal]", m.picker.queue)
	}

	// First pick: work's Archive. Second prompt: personal's tree.
	cmd = m.pickerChoose("mb-archive")
	pump(t, m, cmd)
	if m.picker == nil {
		t.Fatal("second prompt (personal) did not open")
	}
	if m.picker.acct != "personal" {
		t.Fatalf("second prompt owner = %q, want personal", m.picker.acct)
	}
	cmd = m.pickerChoose("mb-trash")
	pump(t, m, cmd)
	if m.picker != nil {
		t.Fatal("picker stayed open after the last pick")
	}

	// Each server got its own destination — and only its own message
	// (identical ids everywhere, so only owner routing keeps this right).
	if got := srvWork.CountIn("mb-archive"); got != 1 {
		t.Fatalf("work archive count = %d, want 1 (e2)", got)
	}
	if got := srvPersonal.CountIn("mb-trash"); got != 1 {
		t.Fatalf("personal trash count = %d, want 1 (e2)", got)
	}
	if got := srvPersonal.CountIn("mb-inbox"); got != 1 {
		t.Fatalf("personal inbox count = %d, want 1 (e1 only)", got)
	}
	for _, r := range m.snap.Rows {
		if r.ID == "e2" {
			t.Fatalf("e2 still in the merged inbox: %s (%s)", r.Summary.Subject, r.Account)
		}
	}
}

// TestDefaultIdentity (FR-A1): the composer's From starts on the
// account's configured default_identity; unset falls back to the first
// identity.
func TestDefaultIdentity(t *testing.T) {
	mailboxes := []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0},
	}
	srv := mockjmap.New("tester@example.com", "pw", mailboxes)
	srv.SetIdentities([]mockjmap.Identity{
		{ID: "id-1", Name: "First", Email: "first@example.test"},
		{ID: "id-2", Name: "Second", Email: "second@example.test"},
	})
	t.Cleanup(srv.Close)
	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "pw"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{
		Accounts: []AccountOpt{{
			ID: "a", Name: "A", Provider: c, Connected: true,
			DefaultIdentity: "second@example.test",
		}},
		Keys:  km,
		Theme: ui.NewTheme(ui.DarkTheme()),
	})
	loadAll(t, m)

	_, _ = m.openCompose(composeNew)
	if m.compose == nil {
		t.Fatal("composer did not open")
	}
	if got := m.compose.identity.Email; got != "second@example.test" {
		t.Fatalf("identity = %q, want second@example.test (configured default)", got)
	}
}

// TestInitialMailboxHonorsWizardChoice: the first snapshot opens the
// wizard-chosen initial mailbox (FR-I8), falls back to the inbox when the
// id no longer exists, and never changes what other accounts open.
func TestInitialMailboxHonorsWizardChoice(t *testing.T) {
	mailboxes := []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 2, UnreadEmails: 2},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 1},
		{ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 2},
	}
	srv := mockjmap.New("tester@example.com", "pw", mailboxes)
	srv.SetEmails(twoAccountFixture("tester", time.Now(), time.Now().Add(-time.Hour)))
	t.Cleanup(srv.Close)
	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "pw"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	// work: wizard-chosen Archive; personal: orphaned id → inbox.
	m := New(Options{
		Accounts: []AccountOpt{
			{ID: "work", Name: "Work", Provider: c, Connected: true, InitialMailbox: "mb-archive"},
			{ID: "personal", Name: "Personal", Provider: c, Connected: true, InitialMailbox: "mb-gone"},
		},
		AccountID: "work",
		Keys:      km,
		Theme:     ui.NewTheme(ui.DarkTheme()),
	})
	m.width, m.height = 120, 40
	loadAll(t, m)

	if got := m.snaps["work"].ActiveMailbox; got != "mb-archive" {
		t.Errorf("work opened %q, want mb-archive (the wizard's choice)", got)
	}
	if got := m.snaps["personal"].ActiveMailbox; got != "mb-inbox" {
		t.Errorf("personal opened %q, want mb-inbox (orphaned id falls back)", got)
	}
	if m.snap.ActiveMailbox != "mb-archive" {
		t.Errorf("active view = %q, want mb-archive", m.snap.ActiveMailbox)
	}
}

// TestAccountManageKey: ctrl+a is the documented escape hatch to the
// account wizard (FR-I8) — it resolves in the keymap, marks the request,
// and quits the session so the command layer can run the wizard and
// relaunch.
func TestAccountManageKey(t *testing.T) {
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	if act, ok := km.Match(ui.PaneAny, "ctrl+a"); !ok || act != ui.ActAccountManage {
		t.Fatalf("ctrl+a = %v,%v, want account.manage", act, ok)
	}
	if err := km.Validate(); err != nil {
		t.Fatalf("keymap conflict: %v", err)
	}

	m, _ := newTestModel(t)
	if m.ManageRequested() {
		t.Fatal("flag set before the key was pressed")
	}
	_, cmd := m.handleKey(keyCtrl('a'))
	if cmd == nil {
		t.Fatal("ctrl+a produced no quit command")
	}
	if !m.ManageRequested() {
		t.Fatal("ManageRequested not set — the wizard would never run")
	}
}

// TestUIStateAccountOrdinal: the owner-bar tint ordinal follows enrollment
// order — the same order as the switcher (FR-A5).
func TestUIStateAccountOrdinal(t *testing.T) {
	m, _, _ := newTwoAccountModel(t)
	st := m.uiState()
	if st.AccountIndex["work"] != 0 || st.AccountIndex["personal"] != 1 {
		t.Fatalf("AccountIndex = %v, want work:0 personal:1", st.AccountIndex)
	}
	if st.AccountNames["work"] != "Work" {
		t.Errorf("AccountNames = %v", st.AccountNames)
	}
}
