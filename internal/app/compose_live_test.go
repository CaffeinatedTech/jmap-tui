package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// liveCreds returns the env-gated Stalwart test credentials (AGENTS.md:
// unset env ⇒ skip; never hardcode, never echo).
func liveCreds(t *testing.T) (url, user, pass string) {
	t.Helper()
	url = os.Getenv("JMAP_TUI_TEST_URL")
	user = os.Getenv("JMAP_TUI_TEST_USER")
	pass = os.Getenv("JMAP_TUI_TEST_PASSWORD")
	if url == "" || user == "" || pass == "" {
		t.Skip("live Stalwart creds not set (JMAP_TUI_TEST_URL / _USER / _PASSWORD)")
	}
	return url, user, pass
}

// TestLiveM5Gate is the M5 acceptance gate (REQUIREMENTS §7): compose →
// attach → send → the message appears in Sent; undo cancels a send; and a
// draft survives a restart because it lives on the server.
//
// It drives the real app model headlessly against live Stalwart. One
// message is sent (self-addressed, per AGENTS.md), and every artifact it
// creates is destroyed in cleanup.
func TestLiveM5Gate(t *testing.T) {
	url, user, pass := liveCreds(t)

	oldAuto, oldBlink := composeAutosaveDelay, runCursorBlink
	// Typing arms a debounce per keystroke, but the gate discards those
	// commands and saves once after the upload — one draft, not one per
	// typing pause (FR-K4 rate courtesy against the live server).
	composeAutosaveDelay = 250 * time.Millisecond
	runCursorBlink = false
	t.Cleanup(func() {
		composeAutosaveDelay, runCursorBlink = oldAuto, oldBlink
	})

	client := jmapclient.New(jmapclient.Options{ServerURL: url, Username: user, Password: pass, Auth: liveAuth()})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{
		Provider:  client,
		Keys:      km,
		Theme:     ui.NewTheme(ui.DarkTheme()),
		UndoDelay: 2 * time.Second, // long enough to cancel, short enough to wait out
	})
	m.width, m.height = 120, 40
	t.Cleanup(m.Cancel)

	// The account load, minus the push loop (the gate does not need it).
	pump(t, m, m.loadAccountCmd())
	if m.snap.ActiveMailbox == "" {
		t.Fatal("no mailbox opened")
	}

	subject := "M5 gate " + time.Now().UTC().Format("20060102-150405")
	var sentID mail.ID
	var draftID mail.ID
	t.Cleanup(func() {
		// Leave nothing behind: the sent copy first, then any draft.
		for _, id := range []mail.ID{sentID, draftID} {
			if id == "" {
				continue
			}
			if _, err := m.opts.Provider.Mutate(m.ctx, mail.Mutation{Destroy: []mail.ID{id}}); err != nil {
				t.Logf("cleanup: destroy %s: %v", id, err)
			}
		}
	})

	// --- compose ---
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	if m.compose == nil {
		t.Fatal("n did not open the composer")
	}
	composeType(t, m, ui.ZoneTo, user)
	composeType(t, m, ui.ZoneSubject, subject)
	composeType(t, m, ui.ZoneBody, "M5 acceptance gate message — safe to ignore, auto-cleaned.\n")

	// --- attach (FR-H3) ---
	dir := t.TempDir()
	path := filepath.Join(dir, "gate.txt")
	if err := os.WriteFile(path, []byte("gate attachment payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	pump(t, m, m.uploadAttachmentCmd(path))
	if len(m.compose.atts) != 1 || m.compose.atts[0].state != attReady {
		t.Fatalf("attachment not uploaded: %+v", m.compose.atts)
	}

	// --- draft survives a restart (the gate's third clause) ---
	// Flush unconditionally: an earlier blur autosave may have adopted a
	// draft id while the body/attachment save is still debounce-pending
	// (f64830b made the empty-body autosave succeed), and the assert
	// below reads the server, not the composer.
	pump(t, m, m.saveDraftCmd())
	if m.compose.status != "" && strings.HasPrefix(m.compose.status, "autosave failed") {
		t.Fatalf("autosave failed: %s", m.compose.status)
	}
	if m.compose.draftID == "" {
		t.Fatalf("autosave produced no draft id (status %q)", m.compose.status)
	}
	t.Logf("draft %s saved (status %q, subject %q)", m.compose.draftID, m.compose.status, m.compose.subject.Value())
	draftID = m.compose.draftID
	assertDraftSurvivesRestart(t, draftID, subject)

	// --- undo cancels a send (FR-H5) ---
	// Drive the arming by hand so the countdown does not fire yet: the
	// window has to be open for ctrl+z to have anything to cancel.
	_, cmd = m.handleKey(keyCtrl('s'))
	if cmd == nil {
		t.Fatalf("ctrl+s produced no command (status %q)", m.compose.status)
	}
	armed, ok := cmd().(sendArmedMsg)
	if !ok {
		t.Fatalf("flush returned %T, want sendArmedMsg", armed)
	}
	if armed.err != nil {
		t.Fatalf("flush failed: %v", armed.err)
	}
	var countdown tea.Cmd
	_, countdown = m.Update(armed)
	if countdown == nil {
		t.Fatal("arming produced no countdown")
	}
	if m.pendingSend == nil {
		t.Fatal("undo window never armed")
	}
	if m.compose != nil {
		t.Fatal("composer should be parked during the hold")
	}
	_, cmd = m.handleKey(keyCtrl('z'))
	pump(t, m, cmd)
	if m.pendingSend != nil {
		t.Fatal("ctrl+z did not cancel the held send")
	}
	if m.compose == nil {
		t.Fatal("composer not restored after cancel")
	}
	if got := sentMatches(t, client, subject); len(got) != 0 {
		t.Fatalf("a cancelled send still landed in Sent: %v", got)
	}
	// A countdown already armed must not fire after the cancel.
	pump(t, m, countdown)
	if got := sentMatches(t, client, subject); len(got) != 0 {
		t.Fatalf("stale countdown sent anyway: %v", got)
	}

	// --- send for real ---
	_, cmd = m.handleKey(keyCtrl('s'))
	pump(t, m, cmd)
	if m.pendingSend != nil {
		t.Fatal("send never committed")
	}
	got := sentMatches(t, client, subject)
	if len(got) == 0 {
		// The move to Sent is confirmed by the submission; give push a
		// beat before declaring the gate failed.
		time.Sleep(3 * time.Second)
		got = sentMatches(t, client, subject)
	}
	if len(got) != 1 {
		t.Fatalf("Sent copies for %q = %d, want 1 (FR-H6)", subject, len(got))
	}
	sentID = got[0]
	draftID = "" // it became the sent message
	if m.compose != nil {
		t.Error("composer still open after a successful send")
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "Sent") {
		t.Errorf("toast = %+v, want a Sent receipt", m.toast)
	}
	if n := countInMailbox(t, client, subject, mail.RoleDrafts); n != 0 {
		t.Errorf("draft left in Drafts after send: %d", n)
	}

	// The attachment rode along: the sent message still has it.
	sums, err := m.opts.Provider.FetchSummaries(m.ctx, []mail.ID{sentID})
	if err != nil || len(sums) != 1 {
		t.Fatalf("FetchSummaries: %v (%d)", err, len(sums))
	}
	if !sums[0].HasAttachment {
		t.Error("sent message lost its attachment")
	}
}

// typeInto focuses a composer zone and types text one key at a time, so
// the gate exercises the real widget key path rather than SetValue.
func composeType(t *testing.T, m *Model, zone ui.ComposeZone, text string) {
	t.Helper()
	for i := 0; m.compose.focus != zone && i < 8; i++ {
		// Tab can carry a blur-flush (FR-H4); dropping it would leave the
		// composer stuck in "saving". Keystrokes are the ones we skip —
		// their command is only the autosave debounce.
		_, cmd := m.handleKey(keyTab())
		pump(t, m, cmd)
	}
	if m.compose.focus != zone {
		t.Fatalf("focus = %v, want %v", m.compose.focus, zone)
	}
	for _, r := range text {
		// The returned command is the autosave debounce; the gate saves
		// explicitly so it does not create a draft per keystroke pause.
		_ = m.composeKey(key(string(r)))
	}
}

// assertDraftSurvivesRestart reads the draft back through a brand-new
// client — the gate's "draft survives restart" clause: nothing about it
// lived in the old process.
func assertDraftSurvivesRestart(t *testing.T, id mail.ID, subject string) {
	t.Helper()
	fresh := jmapclient.New(jmapclient.Options{
		ServerURL: os.Getenv("JMAP_TUI_TEST_URL"),
		Username:  os.Getenv("JMAP_TUI_TEST_USER"),
		Password:  os.Getenv("JMAP_TUI_TEST_PASSWORD"),
		Auth:      liveAuth(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := fresh.Connect(ctx); err != nil {
		t.Fatalf("restart Connect: %v", err)
	}
	body, err := fresh.FetchBody(ctx, id)
	if err != nil {
		t.Fatalf("restart FetchBody: %v", err)
	}
	if body.Subject != subject {
		t.Errorf("restarted subject = %q, want %q", body.Subject, subject)
	}
	if !strings.Contains(body.Text, "M5 acceptance gate message") {
		t.Errorf("restarted body = %q", body.Text)
	}
	if len(body.Attachments) != 1 {
		t.Fatalf("restarted attachments = %d, want 1", len(body.Attachments))
	}
	if body.Attachments[0].BlobID == "" {
		t.Error("restarted attachment has no blob id")
	}
}

// sentMatches returns ids of messages in Sent whose subject is exactly
// subject.
func sentMatches(t *testing.T, c *jmapclient.Client, subject string) []mail.ID {
	t.Helper()
	return idsInMailbox(t, c, subject, mail.RoleSent)
}

// countInMailbox counts messages with subject in the named role mailbox.
func countInMailbox(t *testing.T, c *jmapclient.Client, subject string, role mail.Role) int {
	t.Helper()
	return len(idsInMailbox(t, c, subject, role))
}

// idsInMailbox resolves a role mailbox and lists ids whose subject is
// exactly subject.
func idsInMailbox(t *testing.T, c *jmapclient.Client, subject string, role mail.Role) []mail.ID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	var box mail.ID
	for _, mb := range mbs.Mailboxes {
		if mb.Role == role {
			box = mb.ID
		}
	}
	if box == "" {
		return nil
	}
	_, sums, err := c.OpenQuery(ctx, mail.QuerySpec{
		MailboxID:       box,
		CollapseThreads: false,
		Search:          &mail.SearchFilter{Subject: subject},
		Limit:           50,
	})
	if err != nil {
		t.Fatalf("query %s: %v", role, err)
	}
	out := make([]mail.ID, 0, len(sums))
	for _, s := range sums {
		out = append(out, s.ID)
	}
	return out
}

// TestLiveM5DraftKeepGate is the reported flow against live Stalwart:
// type a message, press Esc, choose "n keep in Drafts" — the draft must
// be findable in the Drafts mailbox afterwards, from a client that has
// never seen the composer. The autosave debounce is disabled so only the
// keep-path can possibly write it.
func TestLiveM5DraftKeepGate(t *testing.T) {
	url, user, pass := liveCreds(t)

	oldAuto, oldBlink := composeAutosaveDelay, runCursorBlink
	composeAutosaveDelay = time.Hour
	runCursorBlink = false
	t.Cleanup(func() { composeAutosaveDelay, runCursorBlink = oldAuto, oldBlink })

	client := jmapclient.New(jmapclient.Options{ServerURL: url, Username: user, Password: pass, Auth: liveAuth()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{Provider: client, Keys: km, Theme: ui.NewTheme(ui.DarkTheme())})
	m.width, m.height = 120, 40
	t.Cleanup(m.Cancel)
	pump(t, m, m.loadAccountCmd())
	if m.snap.ActiveMailbox == "" {
		t.Fatal("no mailbox opened")
	}

	subject := "M5 keep gate " + time.Now().UTC().Format("20060102-150405")
	var draftID mail.ID
	t.Cleanup(func() {
		// Fresh context: the test's own defer-cancel runs before t.Cleanup.
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if draftID == "" {
			return
		}
		if _, err := client.Mutate(cctx, mail.Mutation{Destroy: []mail.ID{draftID}}); err != nil {
			t.Logf("cleanup: destroy %s: %v", draftID, err)
		}
	})

	// Compose without leaving the first field, so neither the debounce nor
	// a blur-flush can write anything on its own.
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	if m.compose == nil {
		t.Fatal("n did not open the composer")
	}
	composeType(t, m, ui.ZoneTo, user)
	m.compose.subject.SetValue(subject)
	m.compose.body.SetValue("keep me — safe to ignore, auto-cleaned.\n")
	_ = m.markDirty() // arms a debounce this test never runs
	if !m.compose.dirty {
		t.Fatal("typing did not mark the draft dirty")
	}

	_, _ = m.handleKey(keyEsc())
	if m.compose == nil || !m.compose.discard {
		t.Fatal("esc did not ask for confirmation")
	}
	_, cmd = m.handleKey(key("n"))
	if cmd == nil {
		t.Fatalf("keep produced no command (dirty=%v saving=%v status=%q)",
			m.compose.dirty, m.compose.saving, m.compose.status)
	}
	pump(t, m, cmd)
	if m.compose != nil {
		t.Fatalf("composer still open after keep (status %q)", m.compose.status)
	}
	draftID = findDraftBySubject(t, client, subject)
	t.Logf("kept draft %s", draftID)

	// It is where the user looks for it: the Drafts view.
	if err := m.engine.OpenMailbox(m.ctx, mailboxesByRole(t, client)[mail.RoleDrafts]); err != nil {
		t.Fatalf("OpenMailbox drafts: %v", err)
	}
	m.snap = m.engine.Snapshot()
	m.viewKey = m.snap.ViewKey
	found := false
	for _, row := range m.snap.Rows {
		if row.Summary.Subject == subject {
			found = true
		}
	}
	if !found {
		t.Fatalf("kept draft not visible in the Drafts view (%d rows)", len(m.snap.Rows))
	}
}

// composeIDFromServer finds the kept draft by subject through an
// independent client — the check that does not trust the composer.
// findDraftBySubject locates a kept draft the way a user would: list the
// Drafts mailbox and compare subjects. Deliberately avoids the server's
// content filter — Stalwart's index settles asynchronously after a write
// (PLAN §7, M4 observation), so a subject search immediately after a save
// can legitimately return nothing.
func findDraftBySubject(t *testing.T, c *jmapclient.Client, subject string) mail.ID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	var box mail.ID
	for _, mb := range mbs.Mailboxes {
		if mb.Role == mail.RoleDrafts {
			box = mb.ID
		}
	}
	if box == "" {
		t.Fatal("no Drafts mailbox")
	}
	_, sums, err := c.OpenQuery(ctx, mail.QuerySpec{MailboxID: box, CollapseThreads: false, Limit: 200})
	if err != nil {
		t.Fatalf("Drafts query: %v", err)
	}
	for _, s := range sums {
		if s.Subject == subject {
			return s.ID
		}
	}
	for _, s := range sums {
		t.Logf("draft on server: id=%s subject=%q", s.ID, s.Subject)
	}
	t.Fatalf("no draft with subject %q in Drafts (%d drafts listed)", subject, len(sums))
	return ""
}

// mailboxesByRole resolves each role mailbox id through the provider.
func mailboxesByRole(t *testing.T, c *jmapclient.Client) map[mail.Role]mail.ID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	out := map[mail.Role]mail.ID{}
	for _, mb := range mbs.Mailboxes {
		if mb.Role != "" {
			out[mb.Role] = mb.ID
		}
	}
	return out
}

// TestLiveDraftEditKeepsAttachment drives the draft-edit flow against
// live Stalwart: create a draft with an attachment, reopen it through
// FR-C3's edit path, and save it twice more. Draft blobs are
// message-scoped (Stalwart derives fresh ids at write and frees them
// with their message), so the composer must load attachments on open —
// or the edit silently drops them — and adopt the fresh ids after every
// save — or the second save answers blobNotFound. Artifact destroyed.
func TestLiveDraftEditKeepsAttachment(t *testing.T) {
	url, user, pass := liveCreds(t)

	oldAuto, oldBlink := composeAutosaveDelay, runCursorBlink
	composeAutosaveDelay = 250 * time.Millisecond
	runCursorBlink = false
	t.Cleanup(func() {
		composeAutosaveDelay, runCursorBlink = oldAuto, oldBlink
	})

	client := jmapclient.New(jmapclient.Options{ServerURL: url, Username: user, Password: pass, Auth: liveAuth()})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{
		Provider:  client,
		Keys:      km,
		Theme:     ui.NewTheme(ui.DarkTheme()),
		UndoDelay: 2 * time.Second,
	})
	m.width, m.height = 120, 40
	t.Cleanup(m.Cancel)

	pump(t, m, m.loadAccountCmd())
	if m.snap.ActiveMailbox == "" {
		t.Fatal("no mailbox opened")
	}

	// --- create a draft with an attachment ---
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	if m.compose == nil {
		t.Fatal("n did not open the composer")
	}
	composeType(t, m, ui.ZoneTo, user)
	composeType(t, m, ui.ZoneSubject, "draft edit carry")
	composeType(t, m, ui.ZoneBody, "body before edit — safe to ignore, auto-cleaned.\n")

	dir := t.TempDir()
	path := filepath.Join(dir, "carry.txt")
	if err := os.WriteFile(path, []byte("carry payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	pump(t, m, m.uploadAttachmentCmd(path))
	if len(m.compose.atts) != 1 || m.compose.atts[0].state != attReady {
		t.Fatalf("attachment not uploaded: %+v", m.compose.atts)
	}
	pump(t, m, m.saveDraftCmd())
	if m.compose.draftID == "" || strings.HasPrefix(m.compose.status, "autosave failed") {
		t.Fatalf("first save: id=%q status=%q", m.compose.draftID, m.compose.status)
	}
	first := m.compose.draftID
	t.Cleanup(func() {
		for _, id := range []mail.ID{m.compose.draftID, first} {
			if id == "" {
				continue
			}
			if _, err := m.opts.Provider.Mutate(m.ctx, mail.Mutation{Destroy: []mail.ID{id}}); err != nil {
				t.Logf("cleanup: destroy %s: %v", id, err)
			}
		}
	})

	// --- reopen through the draft-edit path (FR-C3) ---
	m.compose = nil
	body, err := m.opts.Provider.FetchBody(ctx, first)
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	_, _ = m.openCompose(composeDraft)
	_, _ = m.handleComposePrep(composePrepMsg{mode: composeDraft, body: body})
	if len(m.compose.atts) != 1 || m.compose.atts[0].state != attReady {
		t.Fatalf("attachments not loaded on draft edit: %+v", m.compose.atts)
	}

	// --- two more saves; each must carry the live blob ids ---
	for i, subject := range []string{"draft edit carry (2)", "draft edit carry (3)"} {
		m.compose.subject.SetValue(subject)
		pump(t, m, m.saveDraftCmd())
		if m.compose.status != "" && strings.HasPrefix(m.compose.status, "autosave failed") {
			t.Fatalf("save %d failed: %q", i+2, m.compose.status)
		}
		if m.compose.draftID == "" {
			t.Fatalf("save %d produced no draft id", i+2)
		}
	}

	final, err := m.opts.Provider.FetchBody(ctx, m.compose.draftID)
	if err != nil {
		t.Fatalf("FetchBody final: %v", err)
	}
	if len(final.Attachments) != 1 {
		t.Fatalf("final draft attachments = %d, want 1 (edit must not drop it)", len(final.Attachments))
	}
	rc, err := m.opts.Provider.DownloadBlob(ctx, mail.ID(final.Attachments[0].BlobID),
		final.Attachments[0].Name, final.Attachments[0].Type)
	if err != nil {
		t.Fatalf("DownloadBlob of live derived id: %v", err)
	}
	_ = rc.Close()
}
