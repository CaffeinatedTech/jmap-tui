package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// composeTestModel is the reader model with the Drafts and Sent roles
// compose needs, timers shrunk, and a short undo window.
func composeTestModel(t *testing.T) (*Model, *mockjmap.Server) {
	t.Helper()
	oldAuto, oldTick, oldBlink := composeAutosaveDelay, uploadProgressTick, runCursorBlink
	composeAutosaveDelay = time.Millisecond
	uploadProgressTick = time.Millisecond
	runCursorBlink = false
	t.Cleanup(func() {
		composeAutosaveDelay, uploadProgressTick = oldAuto, oldTick
		runCursorBlink = oldBlink
	})

	m, srv := newTestModelWith(t, []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 2, UnreadEmails: 1},
		{ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 3},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 4},
		{ID: "mb-drafts", Name: "Drafts", Role: "drafts", SortOrder: 5},
		{ID: "mb-sent", Name: "Sent", Role: "sent", SortOrder: 6},
	})
	pump(t, m, m.loadAccountCmd())
	if m.snap.ActiveMailbox != "mb-inbox" {
		t.Fatalf("setup: active mailbox = %q", m.snap.ActiveMailbox)
	}
	m.opts.UndoDelay = time.Millisecond
	return m, srv
}

// TestComposeOpensWithNewMessage: `n` opens the composer with the To field
// focused (FR-H1).
func TestComposeOpensWithNewMessage(t *testing.T) {
	m, _ := composeTestModel(t)
	_, cmd := m.handleKey(key("n"))
	if m.compose == nil {
		t.Fatal("n did not open the composer")
	}
	if m.compose.focus != ui.ZoneTo {
		t.Errorf("focus = %v, want To", m.compose.focus)
	}
	if m.compose.mode != composeNew {
		t.Errorf("mode = %v, want new", m.compose.mode)
	}
	if len(m.compose.identities) == 0 {
		t.Error("identities not loaded for From (FR-H1)")
	}
	pump(t, m, cmd)
	if got := stripANSI(m.View().Content); !strings.Contains(got, "new message") {
		t.Errorf("render missing title:\n%s", got)
	}
}

// TestReplyPrefillsAndThreads covers FR-H2 end to end at the app layer:
// recipients, a Re: subject, the attribution quote, and the threading
// headers the draft will be created with.
func TestReplyPrefillsAndThreads(t *testing.T) {
	m, srv := composeTestModel(t)
	_, cmd := m.handleKey(key("r"))
	pump(t, m, cmd)

	c := m.compose
	if c == nil {
		t.Fatal("r did not open the composer")
	}
	if c.mode != composeReply {
		t.Errorf("mode = %v, want reply", c.mode)
	}
	if !strings.Contains(c.to.Value(), "bob@example.test") {
		t.Errorf("To = %q, want the original sender", c.to.Value())
	}
	if c.subject.Value() != "Re: thread starter" {
		t.Errorf("Subject = %q", c.subject.Value())
	}
	body := c.body.Value()
	if !strings.Contains(body, "wrote:") {
		t.Errorf("no attribution line:\n%s", body)
	}
	if !strings.Contains(body, "> The reply body.") {
		t.Errorf("body not quoted:\n%s", body)
	}
	if len(c.inReplyTo) != 1 || c.inReplyTo[0] != "<m2@example.test>" {
		t.Errorf("inReplyTo = %v", c.inReplyTo)
	}
	if len(c.references) != 2 {
		t.Errorf("references = %v, want the chain plus the replied-to id", c.references)
	}
	// The prefill is unsaved content, so it autosaved during the pump.
	if c.draftID == "" {
		t.Error("reply prefill never autosaved (FR-H4)")
	}
	if srv.CountIn("mb-drafts") != 1 {
		t.Errorf("server drafts = %d, want 1", srv.CountIn("mb-drafts"))
	}
}

// TestReplyAllKeepsEveryoneButSelf: reply-all addresses the thread minus
// our own identities (FR-H2).
func TestReplyAllKeepsEveryoneButSelf(t *testing.T) {
	m, _ := composeTestModel(t)
	_, cmd := m.handleKey(key("a"))
	pump(t, m, cmd)
	c := m.compose
	if c == nil {
		t.Fatal("a did not open the composer")
	}
	all := c.to.Value() + "," + c.cc.Value()
	if !strings.Contains(all, "bob@example.test") {
		t.Errorf("reply-all dropped the sender: to=%q cc=%q", c.to.Value(), c.cc.Value())
	}
	if strings.Contains(all, "tester@example.com") {
		t.Errorf("reply-all addressed ourselves: %q", all)
	}
}

// TestForwardQuotesWithoutThreading: a forward carries the header block
// and quoted body but no in-reply-to (FR-H2).
func TestForwardQuotesWithoutThreading(t *testing.T) {
	m, _ := composeTestModel(t)
	_, cmd := m.handleKey(key("f"))
	pump(t, m, cmd)
	c := m.compose
	if c == nil {
		t.Fatal("f did not open the composer")
	}
	if c.mode != composeForward {
		t.Errorf("mode = %v", c.mode)
	}
	if c.subject.Value() != "Fwd: Re: thread starter" {
		t.Errorf("Subject = %q", c.subject.Value())
	}
	body := c.body.Value()
	if !strings.Contains(body, "---------- Forwarded message ----------") {
		t.Errorf("missing forwarded block:\n%s", body)
	}
	if !strings.Contains(body, "> The reply body.") {
		t.Errorf("forwarded body not quoted:\n%s", body)
	}
	if len(c.inReplyTo) != 0 || len(c.references) != 0 {
		t.Errorf("forward must not thread: %v %v", c.inReplyTo, c.references)
	}
	if c.to.Value() != "" {
		t.Errorf("forward prefilled To: %q", c.to.Value())
	}
}

// TestAutosaveTracksServerId: because Email content is immutable, each
// save returns a new id and the composer must adopt it (FR-H4).
func TestAutosaveTracksServerId(t *testing.T) {
	m, srv := composeTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)

	c := m.compose
	c.to.SetValue("alice@example.test")
	c.subject.SetValue("first")
	c.body.SetValue("one\n")
	cmd = m.markDirty()
	pump(t, m, cmd)
	first := c.draftID
	if first == "" || srv.CountIn("mb-drafts") != 1 {
		t.Fatalf("first save: id=%q drafts=%d", first, srv.CountIn("mb-drafts"))
	}

	c.subject.SetValue("second")
	c.body.SetValue("two\n")
	cmd = m.markDirty()
	pump(t, m, cmd)
	second := c.draftID
	if second == "" || second == first {
		t.Errorf("second save id = %q (first %q); a content edit must recreate", second, first)
	}
	if srv.CountIn("mb-drafts") != 1 {
		t.Errorf("drafts after edit = %d, want 1", srv.CountIn("mb-drafts"))
	}
}

// TestSendUndoWindowCancels is the FR-H5 client-side hold: the submission
// never reaches the server while the window is open, and ctrl+z restores
// the composer with the flushed draft intact.
func TestSendUndoWindowCancels(t *testing.T) {
	m, srv := composeTestModel(t)
	_, cmd := m.handleKey(key("r"))
	pump(t, m, cmd)
	if m.compose == nil || m.compose.draftID == "" {
		t.Fatal("setup: reply never autosaved")
	}
	savedCompose := m.compose

	// Run the flush, then the arming — but not the countdown tick.
	_, cmd = m.handleKey(keyCtrl('s'))
	if cmd == nil {
		t.Fatal("ctrl+s produced no command")
	}
	armed := cmd()
	if _, ok := armed.(sendArmedMsg); !ok {
		t.Fatalf("flush returned %T, want sendArmedMsg", armed)
	}
	_, countdown := m.Update(armed)
	if m.pendingSend == nil {
		t.Fatal("undo window never armed")
	}
	if m.compose != nil {
		t.Error("composer should be parked during the hold")
	}
	if srv.CountIn("mb-sent") != 0 {
		t.Fatal("something was sent before the undo window elapsed")
	}

	// ctrl+z during the window cancels outright (FR-H5).
	_, cmd = m.handleKey(keyCtrl('z'))
	pump(t, m, cmd)
	if m.pendingSend != nil {
		t.Fatal("ctrl+z did not cancel the held send")
	}
	if m.compose == nil || m.compose.draftID != savedCompose.draftID {
		t.Fatalf("composer not restored: %+v", m.compose)
	}
	if srv.CountIn("mb-sent") != 0 {
		t.Error("a cancelled send still reached the server")
	}
	if srv.CountIn("mb-drafts") != 1 {
		t.Errorf("drafts after cancel = %d, want 1", srv.CountIn("mb-drafts"))
	}

	// A countdown that was already armed must not fire after the cancel.
	pump(t, m, countdown)
	if srv.CountIn("mb-sent") != 0 {
		t.Error("stale countdown committed a cancelled send")
	}
}

// TestSendCommitsAfterUndoWindow: the full FR-H5/FR-H6 happy path —
// hold, commit, message lands in Sent, draft leaves Drafts.
func TestSendCommitsAfterUndoWindow(t *testing.T) {
	m, srv := composeTestModel(t)
	_, cmd := m.handleKey(key("r"))
	pump(t, m, cmd)
	if m.compose == nil {
		t.Fatal("setup failed")
	}

	_, cmd = m.handleKey(keyCtrl('s'))
	pump(t, m, cmd)

	if m.pendingSend != nil || m.compose != nil {
		t.Fatalf("send did not finish: pending=%v compose=%v", m.pendingSend != nil, m.compose != nil)
	}
	if srv.CountIn("mb-sent") != 1 {
		t.Errorf("sent copies = %d, want 1 (FR-H6)", srv.CountIn("mb-sent"))
	}
	if srv.CountIn("mb-drafts") != 0 {
		t.Errorf("draft left in Drafts: %d", srv.CountIn("mb-drafts"))
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "Sent") {
		t.Errorf("toast = %+v, want a Sent receipt", m.toast)
	}
	if m.engine.Snapshot().ActiveMailbox == "mb-inbox" {
		// The inbox view must still render — send must not disturb it.
		if len(m.engine.Snapshot().Rows) == 0 {
			t.Error("inbox view emptied by a send")
		}
	}
}

// TestSendRejectedWithoutRecipients: an empty To is a local error, not a
// round-trip (FR-H1).
func TestSendRejectedWithoutRecipients(t *testing.T) {
	m, srv := composeTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	_, cmd = m.handleKey(keyCtrl('s'))
	if cmd != nil {
		t.Error("send without recipients should not produce a command")
	}
	if m.compose == nil {
		t.Fatal("composer closed on a validation error")
	}
	if !strings.Contains(m.compose.status, "recipient") {
		t.Errorf("status = %q", m.compose.status)
	}
	if srv.CountIn("mb-sent") != 0 {
		t.Error("sent without recipients")
	}
}

// TestDiscardConfirmationCoversDirtyComposer: Esc asks before throwing
// words away, `y` destroys the server draft, `n` keeps it (FR-H4).
func TestDiscardConfirmationCoversDirtyComposer(t *testing.T) {
	m, srv := composeTestModel(t)
	_, cmd := m.handleKey(key("r"))
	pump(t, m, cmd)
	if m.compose.draftID == "" {
		t.Fatal("setup: no draft to discard")
	}

	_, cmd = m.handleKey(keyEsc())
	if cmd != nil {
		pump(t, m, cmd)
	}
	if m.compose == nil || !m.compose.discard {
		t.Fatal("esc did not ask for confirmation")
	}
	if srv.CountIn("mb-drafts") != 1 {
		t.Error("confirmation must not destroy anything by itself")
	}

	// esc inside the confirmation returns to editing.
	_, _ = m.handleKey(keyEsc())
	if m.compose == nil || m.compose.discard {
		t.Fatal("esc should have dismissed the confirmation")
	}

	// Re-arm, then keep (`n`): the draft stays on the server (FR-H4).
	_, _ = m.handleKey(keyEsc())
	if m.compose == nil {
		t.Fatal("composer lost")
	}
	m.compose.discard = true
	_, cmd = m.handleKey(key("n"))
	pump(t, m, cmd)
	if m.compose != nil {
		t.Error("n did not close the composer")
	}
	if srv.CountIn("mb-drafts") != 1 {
		t.Errorf("keeping must leave the draft: %d", srv.CountIn("mb-drafts"))
	}

	// Reopen, discard (`y`): the draft is destroyed.
	if err := m.engine.OpenMailbox(m.ctx, "mb-drafts"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	m.snap = m.engine.Snapshot()
	m.viewKey = m.snap.ViewKey
	_, cmd = m.handleKey(keyEnter())
	pump(t, m, cmd)
	if m.compose == nil {
		t.Fatal("draft did not open for editing (FR-C3)")
	}
	draftID := m.compose.draftID
	_, _ = m.handleKey(keyEsc())
	m.compose.discard = true
	_, cmd = m.handleKey(key("y"))
	pump(t, m, cmd)
	if m.compose != nil {
		t.Error("y did not close the composer")
	}
	if srv.CountIn("mb-drafts") != 0 {
		t.Errorf("y did not destroy draft %s: %d left", draftID, srv.CountIn("mb-drafts"))
	}
}

// TestEnterOpensDraftInComposer is FR-C3: in the Drafts mailbox Enter
// edits the draft instead of expanding a thread.
func TestEnterOpensDraftInComposer(t *testing.T) {
	m, srv := composeTestModel(t)
	ids := m.engine.Identities()
	if len(ids) == 0 {
		t.Fatal("no identities")
	}
	d := mail.Draft{
		IdentityID: ids[0].ID,
		From:       []mail.Address{{Name: "Tester", Email: "tester@example.com"}},
		To:         []mail.Address{{Email: "alice@example.test"}},
		Subject:    "saved somewhere",
		Text:       "draft body\n",
	}
	if _, _, err := m.engine.SaveDraft(m.ctx, d); err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	if err := m.engine.OpenMailbox(m.ctx, "mb-drafts"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	m.snap = m.engine.Snapshot()
	m.viewKey = m.snap.ViewKey
	if len(m.snap.Rows) == 0 {
		t.Fatal("draft not in the Drafts view")
	}

	_, cmd := m.handleKey(keyEnter())
	pump(t, m, cmd)
	if m.compose == nil {
		t.Fatal("enter did not open the composer")
	}
	if m.compose.mode != composeDraft {
		t.Errorf("mode = %v, want edit draft", m.compose.mode)
	}
	if m.compose.subject.Value() != "saved somewhere" {
		t.Errorf("subject = %q", m.compose.subject.Value())
	}
	if !strings.Contains(m.compose.body.Value(), "draft body") {
		t.Errorf("body = %q", m.compose.body.Value())
	}
	if srv.CountIn("mb-drafts") != 1 {
		t.Errorf("drafts = %d, want 1", srv.CountIn("mb-drafts"))
	}
}

// TestTabCyclesComposerZones: the header/body focus zones of FR-H1.
func TestTabCyclesComposerZones(t *testing.T) {
	m, _ := composeTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)

	want := []int{
		int(ui.ZoneCc), int(ui.ZoneBcc), int(ui.ZoneSubject), int(ui.ZoneBody),
		int(ui.ZoneAttach), int(ui.ZoneTo),
	}
	for _, w := range want {
		_, cmd = m.handleKey(keyTab())
		pump(t, m, cmd)
		if int(m.compose.focus) != w {
			t.Fatalf("focus = %v, want zone %d", m.compose.focus, w)
		}
	}
}

// --- pure helpers ---

func TestQuoteBlockPrefixesEveryLine(t *testing.T) {
	got := quoteBlock("one\n\ntwo\n")
	want := "> one\n>\n> two\n"
	if got != want {
		t.Errorf("quoteBlock = %q, want %q", got, want)
	}
	if quoteBlock("") != "" {
		t.Error("empty body must stay empty")
	}
}

func TestPrefixSubjectIsIdempotent(t *testing.T) {
	cases := []struct{ marker, in, want string }{
		{"Re:", "topic", "Re: topic"},
		{"Re:", "Re: topic", "Re: topic"},
		{"Re:", "re: topic", "Re: topic"},
		{"Fwd:", "Fwd: topic", "Fwd: topic"},
		{"Re:", "Fwd: topic", "Re: Fwd: topic"},
		{"Re:", "", "Re: "},
	}
	for _, c := range cases {
		if got := prefixSubject(c.marker, c.in); got != c.want {
			t.Errorf("prefixSubject(%q,%q) = %q, want %q", c.marker, c.in, got, c.want)
		}
	}
}

func TestParseAddressListAcceptsRealForms(t *testing.T) {
	got := parseAddressList("alice@example.test, Bob <bob@example.test>; \"Carol, C\" <carol@example.test>, not-an-address")
	if len(got) != 3 {
		t.Fatalf("parsed %d addresses: %+v", len(got), got)
	}
	if got[0].Email != "alice@example.test" || got[0].Name != "" {
		t.Errorf("bare: %+v", got[0])
	}
	if got[1].Email != "bob@example.test" || got[1].Name != "Bob" {
		t.Errorf("named: %+v", got[1])
	}
	if got[2].Name != `"Carol` {
		// The comma inside a quoted name is not something this parser
		// promises to handle; it must not produce a bogus address.
		if got[2].Email != "carol@example.test" {
			t.Errorf("third: %+v", got[2])
		}
	}
}

func TestFormatAndParseRoundTrip(t *testing.T) {
	in := []mail.Address{
		{Name: "Alice Root", Email: "alice@example.test"},
		{Email: "bob@example.test"},
	}
	out := parseAddressList(formatAddressList(in))
	if len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Errorf("round trip: %+v → %q → %+v", in, formatAddressList(in), out)
	}
}

// keyTab builds the tab keystroke.
func keyTab() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyTab} }

// --- attachments (FR-H3) ---

// writeAttachFile drops a file in a temp dir and points the attachment
// filepicker at it.
func writeAttachFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := attachDirFn
	attachDirFn = func() string { return dir }
	t.Cleanup(func() { attachDirFn = old })
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("attachment bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestUploadAttachmentCompletes drives one file from picker to blob: the
// placeholder appears immediately as "uploading", progress repaints, and
// the finished blob joins the draft (FR-H3).
func TestUploadAttachmentCompletes(t *testing.T) {
	m, _ := composeTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	path := writeAttachFile(t)

	cmd = m.uploadAttachmentCmd(path)
	if len(m.compose.atts) != 1 || m.compose.atts[0].state != attUploading {
		t.Fatalf("attachment not staged: %+v", m.compose.atts)
	}
	view := m.composeView()
	if len(view.Attachments) != 1 || !strings.Contains(view.Attachments[0].Progress, "uploading") {
		t.Fatalf("progress not rendered: %+v", view.Attachments)
	}
	if !m.uploading() {
		t.Error("uploading() false while a transfer is staged")
	}

	pump(t, m, cmd)
	a := m.compose.atts[0]
	if a.state != attReady {
		t.Fatalf("state = %v err = %q", a.state, a.err)
	}
	if a.att.BlobID == "" {
		t.Fatal("upload produced no blob id")
	}
	if a.att.Name != "notes.txt" {
		t.Errorf("name = %q", a.att.Name)
	}
	// The ready blob rides along with the next save.
	m.compose.to.SetValue("alice@example.test")
	m.compose.subject.SetValue("with attachment")
	cmd = m.markDirty()
	pump(t, m, cmd)
	sums, err := m.opts.Provider.FetchSummaries(m.ctx, []mail.ID{m.compose.draftID})
	if err != nil || len(sums) != 1 {
		t.Fatalf("FetchSummaries: %v (%d)", err, len(sums))
	}
	if !sums[0].HasAttachment {
		t.Error("draft saved without its attachment")
	}
}

// TestAttachPickerListsAndSelects drives the chooser itself end to end —
// the one path the other tests bypass. The filepicker's directory read is
// an unexported bubbles message, so unless the model forwards it the
// listing stays empty ("Bummer. No Files Found.") and enter never attaches
// (FR-H3).
func TestAttachPickerListsAndSelects(t *testing.T) {
	m, _ := composeTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	path := writeAttachFile(t)

	cmd = m.composeKey(keyCtrl('a'))
	if m.attachPick == nil {
		t.Fatal("ctrl+a did not open the attach picker")
	}
	pump(t, m, cmd) // filepicker Init readDir
	view := m.attachPick.fp.View()
	if !strings.Contains(view, "notes.txt") {
		t.Fatalf("listing never loaded: %q", view)
	}
	if strings.Contains(view, "Bummer") {
		t.Fatalf("empty-directory placeholder shown: %q", view)
	}

	cmd = m.composeKey(keyEnter())
	if m.attachPick != nil {
		t.Fatal("picker stayed open after enter")
	}
	if len(m.compose.atts) != 1 || m.compose.atts[0].name != filepath.Base(path) {
		t.Fatalf("attachment not staged: %+v", m.compose.atts)
	}
	pump(t, m, cmd)
	a := m.compose.atts[0]
	if a.state != attReady || a.att.BlobID == "" {
		t.Fatalf("upload did not complete: state=%v err=%q", a.state, a.err)
	}
}

// TestEscCancelsInFlightUpload: while an upload runs, esc cancels the
// transfer instead of closing the composer (FR-H3 cancel).
func TestEscCancelsInFlightUpload(t *testing.T) {
	m, _ := composeTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	path := writeAttachFile(t)

	// Stage the upload without running it, so it is genuinely in flight.
	_ = m.uploadAttachmentCmd(path)
	if !m.uploading() {
		t.Fatal("upload not staged")
	}
	cmd = m.composeKey(keyEsc())
	if cmd != nil {
		t.Fatal("cancel must not close the composer")
	}
	if m.compose == nil {
		t.Fatal("composer closed on cancel")
	}
	if len(m.compose.atts) != 0 {
		t.Errorf("cancelled attachment still listed: %+v", m.compose.atts)
	}
	if !strings.Contains(m.compose.status, "cancelled") {
		t.Errorf("status = %q", m.compose.status)
	}
}

// TestFailedUploadIsReported: a rejected upload names the file and stays
// actionable instead of failing silently (FR-I6).
func TestFailedUploadIsReported(t *testing.T) {
	m, _ := composeTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	m.compose.atts = append(m.compose.atts, composeAttachment{
		name: "broken.bin", size: 10, state: attUploading, sent: &atomic.Int64{},
	})
	_, _ = m.handleUploadDone(uploadDoneMsg{index: 0, err: errors.New("HTTP 507 insufficient storage")})
	a := m.compose.atts[0]
	if a.state != attFailed || a.err == "" {
		t.Fatalf("state = %v err = %q", a.state, a.err)
	}
	view := m.composeView()
	if !strings.Contains(view.Attachments[0].Failed, "insufficient storage") {
		t.Errorf("failure reason missing from the view: %+v", view.Attachments[0])
	}
	if !strings.Contains(stripANSI(m.View().Content), "failed") {
		t.Errorf("attachment strip does not show the failure:\n%s", stripANSI(m.View().Content))
	}
}

// TestEscKeepSavesDraft reproduces the reported flow: type a message,
// press Esc, choose "n keep in Drafts" — the words must actually reach the
// server, even if the autosave debounce never had a chance to fire (FR-H4).
func TestEscKeepSavesDraft(t *testing.T) {
	m, srv := composeTestModel(t)
	// A debounce long enough that nothing saves on its own: only the
	// keep-path may write the draft.
	old := composeAutosaveDelay
	composeAutosaveDelay = time.Hour
	t.Cleanup(func() { composeAutosaveDelay = old })

	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)

	// Type the whole message without leaving the field, so neither the
	// debounce nor a blur-flush can fire: only the keep-path may write.
	composeType(t, m, ui.ZoneTo, "alice@example.test")
	m.compose.subject.SetValue("half-written thoughts")
	m.compose.body.SetValue("just getting started\n")
	_ = m.markDirty() // arms a debounce this test never runs

	if srv.CountIn("mb-drafts") != 0 {
		t.Fatalf("setup: autosave fired anyway (%d)", srv.CountIn("mb-drafts"))
	}
	if m.compose.draftID != "" || !m.compose.dirty {
		t.Fatalf("setup: draftID=%q dirty=%v", m.compose.draftID, m.compose.dirty)
	}

	_, _ = m.handleKey(keyEsc())
	if m.compose == nil || !m.compose.discard {
		t.Fatal("esc did not ask for confirmation")
	}
	_, cmd = m.handleKey(key("n"))
	pump(t, m, cmd)

	if m.compose != nil {
		t.Fatalf("composer still open after keep (status %q)", m.compose.status)
	}
	if srv.CountIn("mb-drafts") != 1 {
		t.Fatalf("drafts on the server = %d, want 1", srv.CountIn("mb-drafts"))
	}

	// …and it is visible where the user looks for it.
	if err := m.engine.OpenMailbox(m.ctx, "mb-drafts"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	m.snap = m.engine.Snapshot()
	m.viewKey = m.snap.ViewKey
	if len(m.snap.Rows) == 0 {
		t.Fatal("Drafts view is empty")
	}
	found := false
	for _, row := range m.snap.Rows {
		if row.Summary.Subject == "half-written thoughts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("draft not in the Drafts view: %+v", m.snap.Rows)
	}
}

// TestTypingMarksDraftDirty pins the root cause of the lost-draft report:
// bubbles' textinput returns a command on almost every keystroke (cursor
// blink), so dirty tracking must not be conditional on the widget's
// command coming back empty.
func TestTypingMarksDraftDirty(t *testing.T) {
	m, srv := composeTestModel(t)
	old := composeAutosaveDelay
	composeAutosaveDelay = time.Hour
	t.Cleanup(func() { composeAutosaveDelay = old })

	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	composeType(t, m, ui.ZoneTo, "alice@example.test")
	if !m.compose.dirty {
		t.Fatal("typing did not mark the draft dirty — autosave would never fire")
	}
	if srv.CountIn("mb-drafts") != 0 {
		t.Fatalf("a debounce fired in a %s window", old)
	}
}

// TestNavigationDoesNotArmAutosave: moving the cursor changes nothing on
// the server, so it must not schedule a save (which would recreate the
// draft on every arrow key).
func TestNavigationDoesNotArmAutosave(t *testing.T) {
	m, srv := composeTestModel(t)
	old := composeAutosaveDelay
	composeAutosaveDelay = time.Millisecond
	t.Cleanup(func() { composeAutosaveDelay = old })

	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	m.compose.subject.SetValue("stable")
	_ = m.markDirty()
	pump(t, m, m.saveDraftCmd())
	if srv.CountIn("mb-drafts") != 1 {
		t.Fatalf("setup: drafts = %d", srv.CountIn("mb-drafts"))
	}
	id := m.compose.draftID

	// Left/right/up/down over the saved content.
	for _, k := range []string{"left", "right", "up", "down", "left"} {
		pump(t, m, m.composeKey(key(k)))
	}
	if m.compose.dirty {
		t.Error("cursor movement marked the draft dirty")
	}
	time.Sleep(20 * time.Millisecond) // any stray debounce would land here
	if got := srv.CountIn("mb-drafts"); got != 1 {
		t.Errorf("drafts = %d after navigation, want still 1", got)
	}
	if m.compose.draftID != id {
		t.Errorf("draft was recreated by navigation: %s → %s", id, m.compose.draftID)
	}
}

// TestAutosaveBeforeSubjectOrBody reproduces the reported failure: a fresh
// composer saves the moment the recipient field blurs (FR-H4 "on blur"),
// before the user has touched the subject or the body, and that create must
// be accepted rather than answered with "autosave failed: … invalidProperties:
// Missing body value for partId".
func TestAutosaveBeforeSubjectOrBody(t *testing.T) {
	m, srv := composeTestModel(t)

	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	composeType(t, m, ui.ZoneTo, "alice@example.test")

	// Tab out of To: the blur flush writes the draft immediately, with the
	// subject and body still empty.
	_, cmd = m.handleKey(keyTab())
	pump(t, m, cmd)

	if m.compose == nil {
		t.Fatal("composer closed on its own")
	}
	if strings.HasPrefix(m.compose.status, "autosave failed") {
		t.Fatalf("autosave of a body-less draft failed: %q", m.compose.status)
	}
	if m.compose.draftID == "" {
		t.Fatal("no draft id was adopted")
	}
	if got := srv.CountIn("mb-drafts"); got != 1 {
		t.Fatalf("drafts on the server = %d, want 1", got)
	}
}

// TestDraftEditKeepsAttachmentAcrossSaves is the regression for draft
// editing losing attachments or dying on the second save. Both halves
// are exercised: opening a draft must load its attachments (FR-H4 — an
// edit writes a *new* Email carrying the same content), and every save
// must adopt the server's freshly derived blob ids — draft blobs are
// message-scoped, so carrying the replaced message's id answers
// blobNotFound.
func TestDraftEditKeepsAttachmentAcrossSaves(t *testing.T) {
	m, _ := composeTestModel(t)

	// Create a draft with an attachment the way the composer does.
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	path := writeAttachFile(t)
	pump(t, m, m.uploadAttachmentCmd(path))
	if m.compose.atts[0].state != attReady {
		t.Fatalf("upload: %v %q", m.compose.atts[0].state, m.compose.atts[0].err)
	}
	m.compose.to.SetValue("alice@example.test")
	m.compose.subject.SetValue("carry")
	m.compose.body.SetValue("body one\n")
	pump(t, m, m.saveDraftCmd())
	if m.compose.draftID == "" || strings.HasPrefix(m.compose.status, "autosave failed") {
		t.Fatalf("first save: id=%q status=%q", m.compose.draftID, m.compose.status)
	}
	first := m.compose.draftID

	// Close and reopen through the draft-edit path (FR-C3): the prep
	// message carries the server's view, attachments included.
	m.compose = nil
	body, err := m.opts.Provider.FetchBody(m.ctx, first)
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	_, _ = m.openCompose(composeDraft)
	_, _ = m.handleComposePrep(composePrepMsg{mode: composeDraft, body: body})
	if len(m.compose.atts) != 1 || m.compose.atts[0].state != attReady {
		t.Fatalf("attachments not loaded on draft edit: %+v", m.compose.atts)
	}

	// Save twice: the first recreate retires `first`, the second must
	// still resolve the attachment's blob (fails with blobNotFound if
	// the composer keeps the replaced message's ids).
	for i, subject := range []string{"carry 2", "carry 3"} {
		m.compose.subject.SetValue(subject)
		pump(t, m, m.saveDraftCmd())
		if m.compose.status != "" && strings.HasPrefix(m.compose.status, "autosave failed") {
			t.Fatalf("save %d failed: %q", i+2, m.compose.status)
		}
		if m.compose.draftID == "" {
			t.Fatalf("save %d produced no draft id", i+2)
		}
	}

	got, err := m.opts.Provider.FetchBody(m.ctx, m.compose.draftID)
	if err != nil {
		t.Fatalf("FetchBody final: %v", err)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("final draft attachments = %d, want 1 (edit must not drop it)", len(got.Attachments))
	}
	// The live derived id must resolve for download, not just metadata.
	rc, err := m.opts.Provider.DownloadBlob(m.ctx, mail.ID(got.Attachments[0].BlobID),
		got.Attachments[0].Name, got.Attachments[0].Type)
	if err != nil {
		t.Fatalf("DownloadBlob of live derived id: %v", err)
	}
	_ = rc.Close()
}
