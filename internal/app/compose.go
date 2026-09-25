package app

import (
	"context"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// composeMode names why the composer opened; it drives the title, the
// prefill, and the threading headers (FR-H1, FR-H2).
type composeMode int

// Composer modes.
const (
	composeNew composeMode = iota
	composeReply
	composeReplyAll
	composeForward
	composeDraft // opening an existing draft from the Drafts list (FR-C3)
)

func (m composeMode) title() string {
	switch m {
	case composeReply:
		return "reply"
	case composeReplyAll:
		return "reply all"
	case composeForward:
		return "forward"
	case composeDraft:
		return "edit draft"
	}
	return "new message"
}

// Tuning knobs. Vars so tests can shrink them without waiting on wall time
// (the toastTTL/searchDebounce precedent).
var (
	// composeAutosaveDelay is the debounce before an autosave fires
	// (FR-H4: "debounced Email/set into role-drafts").
	composeAutosaveDelay = 2 * time.Second
	// uploadProgressTick is how often in-flight uploads repaint their
	// progress bar (FR-H3).
	uploadProgressTick = 100 * time.Millisecond
	// attachDirFn is where the attachment filepicker opens.
	attachDirFn = homeDir
	// runCursorBlink gates the cursor-blink timer bubbles' Focus returns;
	// it is only cosmetic, and tests turn it off so a pump never sleeps
	// on it (the toastTTL/downloadsDirFn precedent).
	runCursorBlink = true
)

// focusBlink returns a widget's Focus command, or nil when blinking is
// suppressed for the run.
func focusBlink(cmd tea.Cmd) tea.Cmd {
	if !runCursorBlink {
		return nil
	}
	return cmd
}

// composeAttachment is one attachment's lifecycle (FR-H3): picked from
// disk, uploaded to the session, then referenced by the draft.
type composeAttachment struct {
	att    mail.Attachment
	path   string
	name   string
	size   int64
	state  composeAttState
	sent   *atomic.Int64
	cancel context.CancelFunc
	err    string
}

type composeAttState int

// Attachment states.
const (
	attUploading composeAttState = iota
	attReady
	attFailed
)

// composeState is everything the composer holds. It is nil whenever the
// composer is closed, which is how the key router knows who owns the
// keyboard (the picker/filepick/search precedent).
type composeState struct {
	mode       composeMode
	identity   mail.Identity
	identities []mail.Identity

	to, cc, bcc, subject textinput.Model
	body                 textarea.Model
	focus                ui.ComposeZone

	// draftID is the server copy; it changes on every save because Email
	// content is immutable (RFC 8621 §4.1.2), so callers always adopt the
	// id the save returns.
	draftID mail.ID

	// Threading headers carried from the message being replied to (FR-H2).
	inReplyTo  []string
	references []string

	atts   []composeAttachment
	attSel int

	// dirty means the composer differs from what the server holds;
	// savedFP fingerprints the content the last successful save wrote, so
	// navigation keys never masquerade as edits.
	dirty   bool
	savedFP string
	saving  bool
	armed   bool // an autosave debounce is pending
	saveSeq int
	// closeAfterSave flushes on completion and then leaves: what the
	// discard confirmation's "keep in Drafts" promises (FR-H4).
	closeAfterSave bool
	status         string
	discard        bool // the discard confirmation is up (FR-H4)
}

// pendingSend holds a flushed draft through the undo window (FR-H5). The
// composer is kept — not destroyed — so cancelling restores it exactly.
type pendingSend struct {
	seq     int
	draft   mail.Draft
	compose *composeState
}

// --- messages ---

// composePrepMsg carries the fetched context for a reply/forward/draft.
type composePrepMsg struct {
	mode composeMode
	body mail.EmailBody
	err  error
}

// composeAutosaveMsg is the debounce tick that triggers a save.
type composeAutosaveMsg struct{ seq int }

// draftSavedMsg reports one autosave (or a pre-send flush). fp is the
// fingerprint of the content that was actually written, which may already
// be stale if the user kept typing.
type draftSavedMsg struct {
	seq  int
	fp   string
	id   mail.ID
	snap sync.Snapshot
	err  error
}

// sendArmedMsg means the draft is on the server and the undo window can
// start.
type sendArmedMsg struct {
	id   mail.ID
	snap sync.Snapshot
	err  error
}

// sendCommitMsg fires when the undo window elapses.
type sendCommitMsg struct{ seq int }

// sendDoneMsg is the submission result.
type sendDoneMsg struct {
	seq     int
	receipt mail.SendReceipt
	snap    sync.Snapshot
	err     error
}

// uploadDoneMsg is one finished attachment upload (FR-H3).
type uploadDoneMsg struct {
	index int
	att   mail.Attachment
	err   error
}

// uploadProgressMsg repaints in-flight upload progress.
type uploadProgressMsg struct{}

// --- opening ---

// openCompose starts a composer in the given mode. Reply, reply-all,
// forward, and draft-edit need the original message, so their content
// arrives as a follow-up composePrepMsg; the composer itself opens
// immediately so the UI never waits on the network (NFR-1).
func (m *Model) openCompose(mode composeMode) (tea.Model, tea.Cmd) {
	if m.compose != nil {
		return m, nil
	}
	if m.pendingSend != nil {
		m.err = "a send is pending — ctrl+z to cancel it first"
		return m, nil
	}
	c := &composeState{
		mode:       mode,
		identities: m.engine.Identities(),
		focus:      ui.ZoneTo,
	}
	if len(c.identities) > 0 {
		c.identity = c.identities[0]
	} else {
		c.status = "no sending identity on this account"
	}
	c.to = m.newHeaderInput("recipient@example.com")
	c.cc = m.newHeaderInput("")
	c.bcc = m.newHeaderInput("")
	c.subject = m.newHeaderInput("")
	c.body = textarea.New()
	c.body.SetPromptFunc(0, func(textarea.PromptInfo) string { return "" })
	m.compose = c
	m.resizeComposer()

	switch mode {
	case composeNew:
		c.focus = ui.ZoneTo
		return m, focusBlink(c.to.Focus())
	default:
		c.focus = ui.ZoneBody
		return m, tea.Batch(m.composePrepCmd(mode), focusBlink(c.body.Focus()))
	}
}

// newHeaderInput builds one header field, sized to the current width.
func (m *Model) newHeaderInput(placeholder string) textinput.Model {
	in := textinput.New()
	in.Placeholder = placeholder
	in.Prompt = ""
	in.SetWidth(max(m.width-ui.ComposeLabelWidth()-1, 10))
	return in
}

// composePrepCmd fetches the message the composer is derived from: its
// addressing, threading headers, and body (FR-H2).
func (m *Model) composePrepCmd(mode composeMode) tea.Cmd {
	id := m.cursorID()
	return func() tea.Msg {
		if id == "" {
			return composePrepMsg{mode: mode, err: fmt.Errorf("no message selected")}
		}
		body, err := m.engine.ReplyContext(m.ctx, id)
		if err != nil {
			return composePrepMsg{mode: mode, err: err}
		}
		return composePrepMsg{mode: mode, body: body}
	}
}

// handleComposePrep fills the composer once the original message arrives.
func (m *Model) handleComposePrep(msg composePrepMsg) (tea.Model, tea.Cmd) {
	c := m.compose
	if c == nil || c.mode != msg.mode {
		return m, nil
	}
	if msg.err != nil {
		c.status = msg.err.Error()
		return m, nil
	}
	b := msg.body
	switch msg.mode {
	case composeDraft:
		c.draftID = b.ID
		c.to.SetValue(formatAddressList(b.To))
		c.cc.SetValue(formatAddressList(b.Cc))
		c.bcc.SetValue(formatAddressList(b.Bcc))
		c.subject.SetValue(b.Subject)
		c.body.SetValue(b.Text)
		c.inReplyTo = append([]string(nil), b.InReplyTo...)
		c.references = append([]string(nil), b.References...)
		// The server already holds exactly this, so nothing is dirty.
		c.savedFP = composeFingerprint(c)
		c.dirty = false
		c.status = "loaded from Drafts"
		return m, nil
	default:
		m.fillReply(c, b, msg.mode)
		c.status = "unsaved"
		// A prefilled reply/forward is unsaved content (FR-H4).
		return m, m.markDirty()
	}
}

// fillReply prefill a reply/reply-all/forward from the original message:
// recipients, a "Re:"/"Fwd:" subject, threading headers, and a quoted body
// with an attribution line (FR-H2).
func (m *Model) fillReply(c *composeState, orig mail.EmailBody, mode composeMode) {
	self := map[string]bool{}
	for _, id := range c.identities {
		self[strings.ToLower(id.Email)] = true
	}

	switch mode {
	case composeForward:
		c.to.SetValue("")
		c.cc.SetValue("")
		c.bcc.SetValue("")
		c.subject.SetValue(prefixSubject("Fwd:", orig.Subject))
		c.body.SetValue(forwardBlock(orig) + quoteBlock(orig.Text))
		// A forward carries no threading headers: it starts a new branch.
		c.inReplyTo, c.references = nil, nil
		return
	}

	replyTo := firstAddress(orig.ReplyTo, orig.From)
	to := []mail.Address{}
	cc := []mail.Address{}
	if mode == composeReply {
		if replyTo.Email != "" {
			to = append(to, replyTo)
		}
	} else {
		// Reply-all: everyone on the original thread except ourselves,
		// To first, then anyone only on Cc.
		seen := map[string]bool{}
		add := func(a mail.Address, bucket *[]mail.Address) {
			k := strings.ToLower(a.Email)
			if a.Email == "" || self[k] || seen[k] {
				return
			}
			seen[k] = true
			*bucket = append(*bucket, a)
		}
		add(replyTo, &to)
		for _, a := range orig.To {
			add(a, &to)
		}
		if len(orig.From) > 0 && !self[strings.ToLower(orig.From[0].Email)] {
			add(orig.From[0], &cc)
		}
		for _, a := range orig.Cc {
			add(a, &cc)
		}
	}
	c.to.SetValue(formatAddressList(to))
	c.cc.SetValue(formatAddressList(cc))
	c.bcc.SetValue("")
	c.subject.SetValue(prefixSubject("Re:", orig.Subject))
	c.body.SetValue(attributionLine(orig) + quoteBlock(orig.Text))

	// Threading: inReplyTo names the replied-to Message-ID, references is
	// its chain extended by it (RFC 8621 §4.1.2.5 — the properties the
	// draft is created with, since content is immutable thereafter).
	c.inReplyTo = append([]string(nil), orig.MessageID...)
	c.references = append([]string(nil), orig.References...)
	for _, id := range orig.MessageID {
		if !containsString(c.references, id) {
			c.references = append(c.references, id)
		}
	}
}

// --- quoting (FR-H2) ---

// attributionLine is the Thunderbird-style "wrote:" header that opens a
// reply quote: date, name, address.
func attributionLine(orig mail.EmailBody) string {
	when := orig.ReceivedAt
	if when.IsZero() {
		when = time.Now()
	}
	from := firstAddress(orig.ReplyTo, orig.From)
	who := from.Email
	if from.Name != "" {
		who = fmt.Sprintf("%s <%s>", from.Name, from.Email)
	}
	if from.Email == "" {
		who = "someone"
	}
	return fmt.Sprintf("On %s, %s wrote:\n", when.Format("Mon, 02 Jan 2006 15:04 -0700"), who)
}

// forwardBlock is the RFC-style forwarded-message header block.
func forwardBlock(orig mail.EmailBody) string {
	when := orig.ReceivedAt
	if when.IsZero() {
		when = time.Now()
	}
	var b strings.Builder
	b.WriteString("---------- Forwarded message ----------\n")
	if a := firstAddress(orig.ReplyTo, orig.From); a.Email != "" {
		b.WriteString("From: " + addressHeader(a) + "\n")
	}
	b.WriteString("Date: " + when.Format("Mon, 02 Jan 2006 15:04 -0700") + "\n")
	if orig.Subject != "" {
		b.WriteString("Subject: " + orig.Subject + "\n")
	}
	if len(orig.To) > 0 {
		b.WriteString("To: " + formatAddressList(orig.To) + "\n")
	}
	if len(orig.Cc) > 0 {
		b.WriteString("Cc: " + formatAddressList(orig.Cc) + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// quoteBlock prefixes every line of text with "> ", keeping empty lines
// empty so the quote stays readable in a plain-text terminal.
func quoteBlock(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			out = append(out, ">")
			continue
		}
		out = append(out, "> "+ln)
	}
	return strings.Join(out, "\n") + "\n"
}

// prefixSubject adds marker unless it is already there, so replying to a
// reply does not become "Re: Re: …".
func prefixSubject(marker, subject string) string {
	s := strings.TrimSpace(subject)
	if s == "" {
		return marker + " "
	}
	// Re-adding the marker normalises its case: "re: topic" and "Re: Re:
	// topic" both come back as a single canonical "Re: …".
	if len(s) >= len(marker) && strings.EqualFold(s[:len(marker)], marker) {
		rest := strings.TrimSpace(s[len(marker):])
		if rest == "" {
			return marker + " "
		}
		return marker + " " + rest
	}
	return marker + " " + s
}

// firstAddress returns the first non-empty address of the candidates.
func firstAddress(lists ...[]mail.Address) mail.Address {
	for _, list := range lists {
		for _, a := range list {
			if a.Email != "" {
				return a
			}
		}
	}
	return mail.Address{}
}

// addressHeader renders one address as "Name <email>" or just "email".
func addressHeader(a mail.Address) string {
	if a.Name == "" {
		return a.Email
	}
	return fmt.Sprintf("%s <%s>", a.Name, a.Email)
}

// formatAddressList renders addresses for an editable field.
func formatAddressList(addrs []mail.Address) string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, addressHeader(a))
	}
	return strings.Join(out, ", ")
}

// parseAddressList reads a comma/newline separated field into addresses.
// "Name <email>", "email", and "Name email" all parse; anything without an
// "@" is dropped rather than sent malformed (FR-H1).
func parseAddressList(s string) []mail.Address {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ';' })
	out := make([]mail.Address, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if i := strings.Index(f, "<"); i > 0 && strings.HasSuffix(f, ">") {
			name := strings.TrimSpace(f[:i])
			email := strings.TrimSpace(f[i+1 : len(f)-1])
			if strings.Contains(email, "@") {
				out = append(out, mail.Address{Name: name, Email: email})
				continue
			}
			continue
		}
		if strings.Contains(f, "@") {
			out = append(out, mail.Address{Email: f})
		}
	}
	return out
}

// --- keys ---

// composeKey routes a keypress while the composer owns the keyboard. It
// returns without a Cmd when the key produced no work.
func (m *Model) composeKey(msg tea.KeyPressMsg) tea.Cmd {
	c := m.compose
	if c == nil {
		return nil
	}

	// The attachment filepicker layers over the composer.
	if m.attachPick != nil {
		return m.attachPickKey(msg)
	}
	// The discard confirmation takes everything (FR-H4).
	if c.discard {
		return m.discardKey(msg)
	}

	key := msg.Keystroke()
	if m.uploading() && key == "esc" {
		return m.cancelUploadsCmd()
	}

	switch key {
	case "tab":
		return m.focusZone(c.focus.Next())
	case "shift+tab":
		return m.focusZone(c.focus.Prev())
	case "ctrl+s":
		return m.startSend()
	case "ctrl+a":
		return m.openAttachPicker()
	case "ctrl+i":
		if len(c.identities) > 1 {
			m.openIdentityPicker()
		}
		return nil
	case "ctrl+x":
		if c.focus == ui.ZoneAttach {
			m.removeAttachment()
		}
		return nil
	case "esc":
		return m.tryCloseComposer()
	case "up", "down":
		if c.focus == ui.ZoneAttach && len(c.atts) > 0 {
			if key == "up" {
				c.attSel = max(c.attSel-1, 0)
			} else {
				c.attSel = min(c.attSel+1, len(c.atts)-1)
			}
			return nil
		}
	}

	// Enter in a header field advances to the next one, which is what
	// every other composer does; in the body it inserts a newline.
	if key == "enter" && c.focus != ui.ZoneBody && c.focus != ui.ZoneAttach {
		return m.focusZone(c.focus.Next())
	}

	// The widget's own command (cursor bookkeeping) and dirty tracking are
	// independent: bubbles returns a command on almost every keystroke, so
	// returning early here would skip markDirty and no autosave would ever
	// fire. Batch them instead.
	return tea.Batch(m.routeComposeInput(msg), m.markDirty())
}

// routeComposeInput sends the key to the focused widget and reports
// whether the widget consumed it.
func (m *Model) routeComposeInput(msg tea.KeyPressMsg) tea.Cmd {
	c := m.compose
	switch c.focus {
	case ui.ZoneTo:
		next, cmd := c.to.Update(msg)
		c.to = next
		return cmd
	case ui.ZoneCc:
		next, cmd := c.cc.Update(msg)
		c.cc = next
		return cmd
	case ui.ZoneBcc:
		next, cmd := c.bcc.Update(msg)
		c.bcc = next
		return cmd
	case ui.ZoneSubject:
		next, cmd := c.subject.Update(msg)
		c.subject = next
		return cmd
	case ui.ZoneBody:
		next, cmd := c.body.Update(msg)
		c.body = next
		return cmd
	}
	return nil
}

// focusZone moves composer focus and blurs the widget that lost it.
func (m *Model) focusZone(z ui.ComposeZone) tea.Cmd {
	c := m.compose
	old := c.focus
	switch old {
	case ui.ZoneTo:
		c.to.Blur()
	case ui.ZoneCc:
		c.cc.Blur()
	case ui.ZoneBcc:
		c.bcc.Blur()
	case ui.ZoneSubject:
		c.subject.Blur()
	case ui.ZoneBody:
		c.body.Blur()
	}
	c.focus = z
	var focus tea.Cmd
	switch z {
	case ui.ZoneTo:
		focus = focusBlink(c.to.Focus())
	case ui.ZoneCc:
		focus = focusBlink(c.cc.Focus())
	case ui.ZoneBcc:
		focus = focusBlink(c.bcc.Focus())
	case ui.ZoneSubject:
		focus = focusBlink(c.subject.Focus())
	case ui.ZoneBody:
		focus = focusBlink(c.body.Focus())
	}
	// A header field losing focus flushes immediately (FR-H4 "on blur");
	// the new widget still has to be focused or it would swallow typing.
	if old != z && c.dirty {
		return tea.Batch(focus, m.saveDraftCmd())
	}
	return focus
}

// markDirty records an edit and schedules the debounced autosave (FR-H4).
// It decides from the content fingerprint rather than from the keystroke:
// moving the cursor changes nothing on the server, so it must not arm a
// save (which would recreate the draft for no reason).
func (m *Model) markDirty() tea.Cmd {
	c := m.compose
	if c == nil || c.discard {
		return nil
	}
	if composeFingerprint(c) == c.savedFP {
		c.dirty = false
		return nil
	}
	c.dirty = true
	if c.armed || c.saving {
		return nil // a save or a debounce already covers this content
	}
	c.status = "editing…"
	return m.autosaveLater()
}

// composeFingerprint is everything a draft carries: change any of it and
// the server copy is stale.
func composeFingerprint(c *composeState) string {
	var b strings.Builder
	b.WriteString(string(c.identity.ID))
	for _, part := range []string{c.to.Value(), c.cc.Value(), c.bcc.Value(), c.subject.Value(), c.body.Value()} {
		b.WriteByte(0)
		b.WriteString(part)
	}
	for _, a := range c.atts {
		b.WriteByte(0)
		b.WriteString(a.name)
		b.WriteByte(0)
		b.WriteString(string(a.att.BlobID))
		b.WriteByte(0)
		b.WriteString(string(rune('0' + int(a.state))))
	}
	return b.String()
}

// autosaveLater arms the debounce tick for the current edit generation.
func (m *Model) autosaveLater() tea.Cmd {
	c := m.compose
	if c == nil {
		return nil
	}
	c.saveSeq = m.seqNext()
	c.armed = true
	seq := c.saveSeq
	return tea.Tick(composeAutosaveDelay, func(time.Time) tea.Msg {
		return composeAutosaveMsg{seq: seq}
	})
}

// --- saving (FR-H4) ---

// saveDraftCmd writes the composer's content to the server. The returned
// id replaces c.draftID: Email content is immutable, so the provider edits
// by recreating.
func (m *Model) saveDraftCmd() tea.Cmd {
	c := m.compose
	if c == nil || c.saving {
		return nil
	}
	d := m.composeDraft()
	fp := composeFingerprint(c)
	c.saving = true
	c.armed = false
	c.dirty = false
	c.status = "saving…"
	seq := c.saveSeq
	return func() tea.Msg {
		id, snap, err := m.engine.SaveDraft(m.ctx, d)
		return draftSavedMsg{seq: seq, fp: fp, id: id, snap: snap, err: err}
	}
}

// handleDraftSaved adopts the result of one save.
func (m *Model) handleDraftSaved(msg draftSavedMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	if msg.snap.Version > m.snap.Version {
		_, cmd = m.applySnapshot(msg.snap)
	}
	c := m.compose
	if c == nil {
		return m, cmd
	}
	if msg.err != nil {
		// Leave the composer open with the words still in it: a failed
		// "keep" must never look like a successful one (FR-I6).
		c.saving = false
		c.dirty = true
		c.closeAfterSave = false
		// Drop the confirmation too: it would hide the error behind the
		// box, and the user has to see why nothing was kept (FR-I6).
		c.discard = false
		c.status = "autosave failed: " + shortErr(msg.err)
		return m, tea.Batch(cmd, m.showToast("Draft not saved", shortErr(msg.err), nil, nil))
	}
	c.draftID = msg.id
	c.savedFP = msg.fp
	c.saving = false
	if composeFingerprint(c) != c.savedFP {
		// Keystrokes landed while the save was in flight; debounce again
		// rather than hammering the server with one save per keystroke.
		c.dirty = true
		c.status = "editing…"
		out := tea.Batch(cmd, m.autosaveLater())
		if c.closeAfterSave {
			return m, out
		}
		return m, out
	}
	c.dirty = false
	c.status = "saved"
	if c.closeAfterSave {
		return m, tea.Batch(cmd, m.closeComposer(false))
	}
	return m, cmd
}

// composeDraft builds the provider draft from the composer's current
// content.
func (m *Model) composeDraft() mail.Draft {
	c := m.compose
	d := mail.Draft{
		ID:         c.draftID,
		IdentityID: c.identity.ID,
		From:       []mail.Address{{Name: c.identity.Name, Email: c.identity.Email}},
		To:         parseAddressList(c.to.Value()),
		Cc:         parseAddressList(c.cc.Value()),
		Bcc:        parseAddressList(c.bcc.Value()),
		Subject:    strings.TrimSpace(c.subject.Value()),
		Text:       c.body.Value(),
		InReplyTo:  c.inReplyTo,
		References: c.references,
	}
	for _, a := range c.atts {
		if a.state == attReady && a.att.BlobID != "" {
			d.Attachments = append(d.Attachments, a.att)
		}
	}
	return d
}

// recipientCount reports whether a draft has anywhere to go.
func recipientCount(d mail.Draft) int { return len(d.To) + len(d.Cc) + len(d.Bcc) }

// --- sending (FR-H5) ---

// startSend flushes the draft, then arms the undo window. Nothing reaches
// the server until the window elapses, so ctrl+z cancels outright.
func (m *Model) startSend() tea.Cmd {
	c := m.compose
	if c == nil || c.sending() {
		return nil
	}
	d := m.composeDraft()
	if recipientCount(d) == 0 {
		c.status = "add at least one recipient"
		return nil
	}
	if len(c.identities) == 0 {
		c.status = "this account has no sending identity"
		return nil
	}
	c.status = "preparing…"
	needsFlush := c.dirty || c.draftID == ""
	return func() tea.Msg {
		id := c.draftID
		snap := m.engine.Snapshot()
		if needsFlush {
			var err error
			id, snap, err = m.engine.SaveDraft(m.ctx, d)
			if err != nil {
				return sendArmedMsg{err: err}
			}
		}
		return sendArmedMsg{id: id, snap: snap}
	}
}

// sending reports whether a submission is already underway.
func (c *composeState) sending() bool { return c.status == "preparing…" }

// handleSendArmed starts the undo countdown (or sends immediately when
// the delay is zero).
func (m *Model) handleSendArmed(msg sendArmedMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	if msg.snap.Version > m.snap.Version {
		_, cmd = m.applySnapshot(msg.snap)
	}
	c := m.compose
	if c == nil {
		return m, cmd
	}
	if msg.err != nil {
		c.status = "could not save draft: " + shortErr(msg.err)
		return m, cmd
	}
	c.draftID = msg.id

	ps := &pendingSend{seq: m.seqNext(), draft: m.composeDraft(), compose: c}
	ps.draft.ID = msg.id
	m.pendingSend = ps
	m.compose = nil

	if m.opts.UndoDelay <= 0 {
		return m, tea.Batch(cmd, m.commitSend(ps))
	}
	toast := fmt.Sprintf("Sending in %s", m.opts.UndoDelay.Round(time.Second))
	return m, tea.Batch(
		cmd,
		m.showToast(toast, "ctrl+z cancel", nil, nil),
		tea.Tick(m.opts.UndoDelay, func(time.Time) tea.Msg { return sendCommitMsg{seq: ps.seq} }),
	)
}

// commitSend performs the submission.
func (m *Model) commitSend(ps *pendingSend) tea.Cmd {
	return func() tea.Msg {
		receipt, snap, err := m.engine.Send(m.ctx, ps.draft)
		return sendDoneMsg{seq: ps.seq, receipt: receipt, snap: snap, err: err}
	}
}

// handleSendDone reports the outcome; a failure puts the composer back so
// nothing typed is lost.
func (m *Model) handleSendDone(msg sendDoneMsg) (tea.Model, tea.Cmd) {
	ps := m.pendingSend
	if ps == nil || ps.seq != msg.seq {
		return m, nil
	}
	m.pendingSend = nil
	m.toast = nil

	var cmd tea.Cmd
	if msg.snap.Version > m.snap.Version {
		_, cmd = m.applySnapshot(msg.snap)
	}
	if msg.err != nil {
		m.compose = ps.compose
		m.compose.status = "send failed: " + shortErr(msg.err)
		return m, tea.Batch(cmd, m.showToast("Send failed — draft kept", shortErr(msg.err), nil, nil))
	}
	return m, tea.Batch(cmd, m.showToast(sentText(msg.receipt), "", nil, nil))
}

// sentText renders the send receipt, including the server's own undo
// window when it reports one (FR-H5).
func sentText(r mail.SendReceipt) string {
	switch r.UndoStatus {
	case "pending":
		return "Sent · server undo window open"
	case "canceled":
		return "Sent · server reported the submission cancelled"
	}
	return "Sent"
}

// cancelPendingSend aborts a held submission and restores the composer
// with the flushed draft intact.
func (m *Model) cancelPendingSend() (tea.Model, tea.Cmd) {
	ps := m.pendingSend
	if ps == nil {
		return m, nil
	}
	m.pendingSend = nil
	m.compose = ps.compose
	m.compose.status = "send cancelled — draft kept"
	return m, m.showToast("Send cancelled", "draft kept on the server", nil, nil)
}

// --- closing & discard (FR-H4) ---

// tryCloseComposer closes when there is nothing to lose, and asks
// otherwise.
func (m *Model) tryCloseComposer() tea.Cmd {
	c := m.compose
	if c == nil {
		return nil
	}
	if !m.composerHasContent() {
		return m.closeComposer(false)
	}
	c.discard = true
	return nil
}

// composerHasContent reports whether the composer holds anything worth
// confirming about.
func (m *Model) composerHasContent() bool {
	c := m.compose
	if c == nil {
		return false
	}
	if strings.TrimSpace(c.to.Value()+c.cc.Value()+c.bcc.Value()+c.subject.Value()) != "" {
		return true
	}
	if strings.TrimSpace(c.body.Value()) != "" {
		return true
	}
	return len(c.atts) > 0
}

// discardKey handles the yes/no confirmation: y discards (destroying any
// server draft), n keeps the draft and leaves, esc returns to editing.
func (m *Model) discardKey(msg tea.KeyPressMsg) tea.Cmd {
	c := m.compose
	if c == nil {
		return nil
	}
	switch strings.ToLower(msg.Keystroke()) {
	case "y":
		return m.closeComposer(true)
	case "n":
		// "keep in Drafts" has to actually put it there: flush whatever
		// the debounce has not written yet, then leave (FR-H4).
		return m.keepAndClose()
	case "esc", "q":
		c.discard = false
		return nil
	}
	return nil
}

// keepAndClose writes the current content to the server and then leaves,
// honouring the confirmation's "keep in Drafts". A save already in flight
// is waited out rather than raced; if the save fails the composer stays
// open so nothing is lost.
func (m *Model) keepAndClose() tea.Cmd {
	c := m.compose
	if c == nil {
		return nil
	}
	if c.saving {
		c.closeAfterSave = true
		c.status = "saving…"
		return nil
	}
	if !c.dirty {
		return m.closeComposer(false)
	}
	c.closeAfterSave = true
	return m.saveDraftCmd()
}

// closeComposer leaves the composer, optionally destroying the server
// draft so it does not linger in Drafts.
func (m *Model) closeComposer(destroy bool) tea.Cmd {
	c := m.compose
	if c == nil {
		return nil
	}
	m.compose = nil
	m.attachPick = nil
	if !destroy || c.draftID == "" {
		return nil
	}
	id := c.draftID
	return func() tea.Msg {
		if _, err := m.engine.Triage(m.ctx, sync.TriageSpec{
			Kind: sync.TriageDestroy,
			IDs:  []mail.ID{id},
		}); err != nil {
			return errMsg{op: "discard draft", err: err}
		}
		return snapMsg{snap: m.engine.Snapshot()}
	}
}

// uploading reports whether any attachment upload is still running.
func (m *Model) uploading() bool {
	c := m.compose
	if c == nil {
		return false
	}
	for _, a := range c.atts {
		if a.state == attUploading {
			return true
		}
	}
	return false
}

// cancelUploadsCmd aborts every in-flight upload (FR-H3 cancel).
func (m *Model) cancelUploadsCmd() tea.Cmd {
	c := m.compose
	if c == nil {
		return nil
	}
	kept := c.atts[:0]
	for _, a := range c.atts {
		if a.state == attUploading {
			if a.cancel != nil {
				a.cancel()
			}
			continue
		}
		kept = append(kept, a)
	}
	c.atts = kept
	c.attSel = max(min(c.attSel, len(c.atts)-1), 0)
	c.status = "upload cancelled"
	return nil
}

// --- attachments (FR-H3) ---

// openAttachPicker opens the file chooser for attachments.
func (m *Model) openAttachPicker() tea.Cmd {
	if m.compose == nil || m.uploading() {
		return nil
	}
	fp := filepicker.New()
	fp.DirAllowed = true
	fp.FileAllowed = true
	fp.ShowPermissions = false
	fp.ShowSize = true
	fp.CurrentDirectory = attachDirFn()
	fp.SetHeight(14)
	fp.AutoHeight = false
	m.attachPick = &filepickState{fp: fp}
	return fp.Init()
}

// attachPickKey routes keys to the attachment filepicker: enter attaches,
// esc returns to the composer.
func (m *Model) attachPickKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Keystroke() {
	case "esc":
		m.attachPick = nil
		return nil
	default:
		next, cmd := m.attachPick.fp.Update(msg)
		m.attachPick.fp = next
		if path := m.attachPick.fp.FileSelected; path != "" {
			m.attachPick.fp.FileSelected = ""
			m.attachPick = nil
			return tea.Batch(cmd, m.uploadAttachmentCmd(path))
		}
		return cmd
	}
}

// uploadAttachmentCmd streams one file to the session uploadUrl
// (FR-H3). Progress is published through an atomic the repaint tick reads,
// so the Cmd itself returns only once — no partial messages, no races.
func (m *Model) uploadAttachmentCmd(path string) tea.Cmd {
	c := m.compose
	if c == nil {
		return nil
	}
	name := filepath.Base(path)
	f, err := os.Open(path)
	if err != nil {
		c.status = name + ": " + shortErr(err)
		return nil
	}
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		_ = f.Close()
		c.status = name + ": not a file"
		return nil
	}
	mediaType := mime.TypeByExtension(filepath.Ext(name))
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	ctx, cancel := context.WithCancel(m.ctx)
	idx := len(c.atts)
	sent := &atomic.Int64{}
	c.atts = append(c.atts, composeAttachment{
		path: path, name: name, size: fi.Size(),
		state: attUploading, sent: sent, cancel: cancel,
	})
	c.attSel = idx
	c.status = "uploading " + name

	return tea.Batch(m.uploadProgressCmd(), func() tea.Msg {
		defer func() { _ = f.Close() }()
		att, err := m.opts.Provider.UploadBlob(ctx, name, mediaType, fi.Size(),
			&countingReader{r: f, n: sent})
		cancel()
		return uploadDoneMsg{index: idx, att: att, err: err}
	})
}

// countingReader reports bytes read so upload progress can render.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

// uploadProgressCmd repaints while uploads run; it re-arms itself until
// none are left.
func (m *Model) uploadProgressCmd() tea.Cmd {
	return tea.Tick(uploadProgressTick, func(time.Time) tea.Msg {
		return uploadProgressMsg{}
	})
}

// handleUploadProgress refreshes the per-file percentages and keeps the
// tick alive while an upload runs.
func (m *Model) handleUploadProgress() (tea.Model, tea.Cmd) {
	c := m.compose
	if c == nil {
		return m, nil
	}
	active := false
	for i := range c.atts {
		a := &c.atts[i]
		if a.state == attUploading && a.sent != nil {
			a.sent.Load()
			if a.size > 0 && a.sent.Load() >= a.size {
				// Bytes are all read; the upload reply may still be in
				// flight, so keep waiting on uploadDoneMsg.
				a.err = ""
			}
			active = true
		}
	}
	if !active {
		return m, nil
	}
	return m, m.uploadProgressCmd()
}

// handleUploadDone records one finished upload (FR-H3).
func (m *Model) handleUploadDone(msg uploadDoneMsg) (tea.Model, tea.Cmd) {
	c := m.compose
	if c == nil || msg.index >= len(c.atts) {
		if msg.err == nil && msg.att.BlobID != "" {
			// Composer closed mid-upload: the blob is orphaned server-side
			// and harmless, but say nothing rather than crash.
			return m, nil
		}
		return m, nil
	}
	a := &c.atts[msg.index]
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	if msg.err != nil {
		a.state = attFailed
		a.err = shortErr(msg.err)
		c.status = a.name + ": " + a.err
		return m, nil
	}
	a.state = attReady
	a.att = msg.att
	a.att.Name = a.name
	c.status = "attached " + a.name
	if !m.uploading() {
		return m, m.markDirty()
	}
	return m, nil
}

// removeAttachment drops the selected attachment from the draft. Blobs are
// only referenced by the draft, so nothing server-side needs cleaning up
// until the next save.
func (m *Model) removeAttachment() {
	c := m.compose
	if c == nil || len(c.atts) == 0 {
		return
	}
	i := min(max(c.attSel, 0), len(c.atts)-1)
	a := c.atts[i]
	if a.state == attUploading && a.cancel != nil {
		a.cancel()
	}
	c.atts = append(c.atts[:i], c.atts[i+1:]...)
	c.attSel = max(min(c.attSel, len(c.atts)-1), 0)
	c.status = "removed " + a.name
	m.markDirty()
}

// --- identity picker (FR-H1) ---

// openIdentityPicker lists the account's sendable identities so From can
// be switched when there is more than one.
func (m *Model) openIdentityPicker() {
	c := m.compose
	if c == nil || len(c.identities) == 0 {
		return
	}
	items := make([]ui.PickerItem, 0, len(c.identities))
	for _, id := range c.identities {
		label := id.Email
		if id.Name != "" {
			label = fmt.Sprintf("%s <%s>", id.Name, id.Email)
		}
		items = append(items, ui.PickerItem{ID: id.ID, Label: label})
	}
	sel := 0
	for i, id := range c.identities {
		if id.ID == c.identity.ID {
			sel = i
		}
	}
	m.picker = &pickerState{mode: pickerIdentity, all: items, sel: sel}
	m.pickerRefilter()
}

// chooseIdentity applies the picked From identity (FR-H1).
func (m *Model) chooseIdentity(id mail.ID) {
	c := m.compose
	if c == nil {
		return
	}
	for _, ident := range c.identities {
		if ident.ID == id {
			c.identity = ident
			c.status = "sending as " + ident.Email
			m.markDirty()
			return
		}
	}
}

// --- rendering ---

// composeView assembles the composer's render state.
func (m *Model) composeView() *ui.ComposeView {
	c := m.compose
	if c == nil {
		return nil
	}
	_, bodyH := ui.ComposeBodySize(m.width, m.height)
	from := "—"
	if c.identity.Email != "" {
		from = addressHeader(mail.Address{Name: c.identity.Name, Email: c.identity.Email})
	}
	if len(c.identities) > 1 {
		from += "  (ctrl+i)"
	}

	atts := make([]ui.ComposeAttachment, 0, len(c.atts))
	for i, a := range c.atts {
		label := a.name
		if a.size > 0 {
			label = fmt.Sprintf("%s (%s)", a.name, humanBytes(a.size))
		}
		if i == c.attSel && c.focus == ui.ZoneAttach {
			label = "▸ " + label
		}
		att := ui.ComposeAttachment{Label: label}
		switch a.state {
		case attUploading:
			att.Progress = "uploading"
			if a.sent != nil && a.size > 0 {
				pct := min(int(a.sent.Load()*100/a.size), 99)
				att.Progress = fmt.Sprintf("uploading %d%%", pct)
			}
		case attFailed:
			att.Failed = a.err
		}
		atts = append(atts, att)
	}

	return &ui.ComposeView{
		Title:       c.mode.title(),
		From:        from,
		To:          c.to.Value(),
		Cc:          c.cc.Value(),
		Bcc:         c.bcc.Value(),
		Subject:     c.subject.Value(),
		Body:        padBody(c.body.View(), bodyH),
		Focus:       c.focus,
		Status:      c.status,
		Attachments: atts,
		Hint:        m.composeHint(),
		Discard:     m.discardView(),
	}
}

// discardView renders the confirmation box when it is up (FR-H4).
func (m *Model) discardView() *ui.DiscardConfirm {
	c := m.compose
	if c == nil || !c.discard {
		return nil
	}
	return &ui.DiscardConfirm{
		Title: "Discard this draft?",
		Hint:  "y discard · n keep in Drafts · esc keep editing",
	}
}

// composeHint is the composer's key-hint footer, adjusted for what is
// currently possible.
func (m *Model) composeHint() string {
	parts := []string{"ctrl+s send", "ctrl+a attach", "tab next", "esc close"}
	if m.uploading() {
		parts = append(parts, "esc cancel upload")
	}
	if len(m.compose.identities) > 1 {
		parts = append(parts, "ctrl+i identity")
	}
	th := m.opts.Theme
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		if i == 0 {
			out = append(out, th.Accent.Render(p))
			continue
		}
		out = append(out, th.Muted.Render(p))
	}
	return strings.Join(out, th.Muted.Render(" · "))
}

// resizeComposer fits the header fields and body to the current frame; it
// runs whenever the window changes or the composer opens.
func (m *Model) resizeComposer() {
	c := m.compose
	if c == nil {
		return
	}
	w := max(m.width-ui.ComposeLabelWidth()-1, 10)
	c.to.SetWidth(w)
	c.cc.SetWidth(w)
	c.bcc.SetWidth(w)
	c.subject.SetWidth(w)
	bw, bh := ui.ComposeBodySize(m.width, m.height)
	c.body.SetWidth(bw)
	c.body.SetHeight(bh)
}

// --- small helpers ---

// padBody trims the rendered textarea to the geometry the composer
// reserved for it.
func padBody(s string, height int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// shortErr renders an error for a status line.
func shortErr(err error) string {
	s := err.Error()
	if len(s) > 80 {
		return s[:77] + "…"
	}
	return s
}

// containsString reports membership.
func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// humanBytes renders a size the way the list's size column does.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGT"[exp])
}

// homeDir is the attachment picker's default directory.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// cursorDraft reports the cursor id when the open mailbox is Drafts and
// the row under the cursor really is a draft (FR-C3 edit-aware mode).
func (m *Model) cursorDraft() (mail.ID, bool) {
	if m.activeRole() != mail.RoleDrafts {
		return "", false
	}
	id := m.cursorID()
	if id == "" || m.snap.Cursor < 0 || m.snap.Cursor >= len(m.snap.Rows) {
		return "", false
	}
	if m.snap.Rows[m.snap.Cursor].Summary.Keywords.Has("$draft") {
		return id, true
	}
	return "", false
}
