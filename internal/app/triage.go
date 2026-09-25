package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/filepicker"
	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// toastTTL is the undo window for destructive actions (FR-G5). A var so
// tests can shrink it (a real Tick blocks the headless pump).
var toastTTL = 5 * time.Second

// pickerMode says what a mailbox-picker choice will do.
type pickerMode int

// Picker modes.
const (
	pickerMove pickerMode = iota
	pickerCopy
	pickerArchive
	pickerIdentity // choose the From identity (FR-H1)
)

// pickerState is the modal mailbox chooser (FR-G2, FR-G4).
type pickerState struct {
	mode   pickerMode
	filter string
	all    []ui.PickerItem
	items  []ui.PickerItem
	sel    int
}

// filepickState is the attachment-save overlay (FR-E4).
type filepickState struct {
	fp   filepicker.Model
	atts []mail.Attachment
}

// toastState is the active action receipt with its undo affordance (FR-G5).
type toastState struct {
	id      int
	text    string
	hint    string
	undo    *sync.TriageSpec // reversal action, nil when irreversible
	destroy []mail.ID        // prepared delayed destroy (cancel path)
	until   time.Time
}

// pendingDestroy tracks a prepared (delayed) destroy awaiting its commit.
type pendingDestroy struct {
	seq int
	ids []mail.ID
}

// triageDoneMsg carries a finished triage action back to the model.
type triageDoneMsg struct {
	verb    string
	receipt sync.Receipt
	err     error
}

// toastExpireMsg retires the toast with the matching id.
type toastExpireMsg struct{ id int }

// destroyCommitMsg fires when a delayed destroy's undo window closes.
type destroyCommitMsg struct{ seq int }

// saveResultMsg reports the attachment-save outcome (FR-E4).
type saveResultMsg struct {
	text string
	err  error
}

// triageCmd wraps one Triage action as a Cmd: optimistic state lands via
// the engine's broadcast; the receipt arrives here (FR-G3, FR-G5).
func (m *Model) triageCmd(spec sync.TriageSpec, verb string) tea.Cmd {
	return func() tea.Msg {
		rcpt, err := m.engine.Triage(m.ctx, spec)
		if err != nil {
			return triageDoneMsg{verb: verb, err: err}
		}
		return triageDoneMsg{verb: verb, receipt: rcpt}
	}
}

// actionIDs resolves the target set for a triage action: the multi-select
// set in list order (FR-G3), else the cursor row.
func (m *Model) actionIDs() []mail.ID {
	var ids []mail.ID
	for _, r := range m.snap.Rows {
		if m.sel[r.ID] {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) > 0 {
		return ids
	}
	if id := m.cursorID(); id != "" {
		return []mail.ID{id}
	}
	return nil
}

// clearSelection empties the multi-select set (actions consume it; mailbox
// switches reset it).
func (m *Model) clearSelection() {
	if len(m.sel) > 0 {
		m.sel = map[mail.ID]bool{}
	}
}

// summaryFor returns the rendered summary for id (keywords included).
func (m *Model) summaryFor(id mail.ID) (mail.EmailSummary, bool) {
	for _, r := range m.snap.Rows {
		if r.ID == id {
			return r.Summary, true
		}
	}
	return mail.EmailSummary{}, false
}

// keywordCmd builds the batched read/unread or star/unstar action: any id
// missing the keyword flips the whole set toward it (batch triage
// semantics — one action, one direction, FR-G1/G3).
func (m *Model) keywordCmd(kind sync.TriageKind) tea.Cmd {
	ids := m.actionIDs()
	if len(ids) == 0 {
		return nil
	}
	keyword := "$seen"
	toward := sync.TriageRead
	if kind == sync.TriageStar || kind == sync.TriageUnstar {
		keyword = "$flagged"
		toward = sync.TriageStar
	}
	anyMissing := false
	for _, id := range ids {
		if s, ok := m.summaryFor(id); ok && !s.Keywords.Has(keyword) {
			anyMissing = true
			break
		}
	}
	spec := sync.TriageSpec{IDs: ids}
	var verb string
	if anyMissing {
		spec.Kind = toward
		verb = "Marked read"
		if toward == sync.TriageStar {
			verb = "Starred"
		}
	} else {
		spec.Kind = sync.TriageUnread
		verb = "Marked unread"
		if toward == sync.TriageStar {
			spec.Kind = sync.TriageUnstar
			verb = "Unstarred"
		}
	}
	return m.triageCmd(spec, fmt.Sprintf("%s %s", verb, countN(len(ids))))
}

// countN renders "1 message" / "3 messages".
func countN(n int) string {
	if n == 1 {
		return "1 message"
	}
	return fmt.Sprintf("%d messages", n)
}

// deleteAction routes delete (FR-G2): inside Trash it is the delayed
// permanent destroy (FR-G5 undo-as-cancel); anywhere else it is a move to
// the role-trash mailbox.
func (m *Model) deleteAction() (tea.Model, tea.Cmd) {
	ids := m.actionIDs()
	if len(ids) == 0 {
		return m, nil
	}
	if m.activeRole() == mail.RoleTrash {
		return m, m.prepareDestroy(ids)
	}
	dest, node := m.mailboxByRole(mail.RoleTrash)
	if dest == "" {
		m.err = "no trash mailbox on this server"
		return m, nil
	}
	return m, m.triageCmd(sync.TriageSpec{Kind: sync.TriageMove, IDs: ids, Mailbox: dest},
		fmt.Sprintf("Deleted %s to %s", countN(len(ids)), node.Name))
}

// prepareDestroy hides rows now and schedules the server destroy after the
// undo window (FR-G2 permanent delete, FR-G5 cancel).
func (m *Model) prepareDestroy(ids []mail.ID) tea.Cmd {
	snap := m.engine.PrepareDestroy(ids)
	if snap.Version > m.snap.Version {
		m.snap = snap
	}
	m.pendingDestroy = &pendingDestroy{seq: m.seqNext(), ids: ids}
	seq := m.pendingDestroy.seq
	return tea.Batch(
		m.showToast(fmt.Sprintf("Deleting %s from Trash", countN(len(ids))), "ctrl+z cancel", nil, ids),
		tea.Tick(toastTTL, func(time.Time) tea.Msg { return destroyCommitMsg{seq: seq} }),
	)
}

// archiveAction archives (FR-G4): the role-archive mailbox wins; otherwise
// the remembered per-account choice; otherwise a one-time picker prompt
// that remembers in app-managed prefs (FR-J1).
func (m *Model) archiveAction() (tea.Model, tea.Cmd) {
	ids := m.actionIDs()
	if len(ids) == 0 {
		return m, nil
	}
	if dest, node := m.mailboxByRole(mail.RoleArchive); dest != "" {
		return m, m.triageCmd(sync.TriageSpec{Kind: sync.TriageMove, IDs: ids, Mailbox: dest},
			fmt.Sprintf("Archived %d to %s", len(ids), node.Name))
	}
	if dest := m.opts.Prefs.ArchiveMailbox(m.opts.AccountID); dest != "" {
		if node := m.mailboxByID(mail.ID(dest)); node != nil {
			return m, m.triageCmd(sync.TriageSpec{Kind: sync.TriageMove, IDs: ids, Mailbox: mail.ID(dest)},
				fmt.Sprintf("Archived %d to %s", len(ids), node.Name))
		}
	}
	m.openPicker(pickerArchive)
	return m, nil
}

// undoAction reverses the toast's action while its window is open (FR-G5):
// a stored reversal spec re-runs as Triage; a prepared destroy cancels.
func (m *Model) undoAction() (tea.Model, tea.Cmd) {
	// A held submission outranks any action receipt: ctrl+z during the
	// undo window cancels the send outright (FR-H5).
	if m.pendingSend != nil {
		return m.cancelPendingSend()
	}
	t := m.toast
	if t == nil {
		return m, nil
	}
	switch {
	case t.destroy != nil:
		ids := t.destroy
		m.toast = nil
		m.pendingDestroy = nil
		return m, func() tea.Msg {
			snap := m.engine.CancelDestroy(m.ctx, ids)
			return snapMsg{snap: snap}
		}
	case t.undo != nil:
		spec := *t.undo
		m.toast = nil
		return m, m.triageCmd(spec, "Undone")
	}
	m.toast = nil
	return m, nil
}

// showToast installs the active toast and returns its expiry Cmd.
func (m *Model) showToast(text, hint string, undo *sync.TriageSpec, destroy []mail.ID) tea.Cmd {
	m.toast = &toastState{
		id:      m.seqNext(),
		text:    text,
		hint:    hint,
		undo:    undo,
		destroy: destroy,
		until:   time.Now().Add(toastTTL),
	}
	return nil
}

// handleTriageDone applies a finished triage action: snapshot adoption,
// selection reset, and the undo toast (FR-G5).
func (m *Model) handleTriageDone(msg triageDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = truncateErr("triage", msg.err)
		return m, nil
	}
	var cmd tea.Cmd
	if msg.receipt.Snap.Version > m.snap.Version {
		_, cmd = m.applySnapshot(msg.receipt.Snap)
	}
	m.clearSelection()

	if len(msg.receipt.Failed) > 0 {
		m.err = msg.receipt.Err
		return m, cmd
	}
	if len(msg.receipt.Applied) == 0 {
		return m, cmd
	}
	hint := ""
	if msg.receipt.Undo != nil {
		hint = "ctrl+z undo"
	}
	return m, tea.Batch(cmd, m.showToast(msg.verb, hint, msg.receipt.Undo, nil))
}

// --- mailbox picker (FR-G2, FR-G4) ---

// openPicker opens the chooser in the given mode, excluding the active
// mailbox for moves (moving onto itself is a no-op).
func (m *Model) openPicker(mode pickerMode) {
	p := &pickerState{mode: mode}
	switch mode {
	case pickerMove:
		p.all = m.pickerItems(false)
	case pickerCopy:
		p.all = m.pickerItems(true)
	case pickerArchive:
		p.all = m.pickerItems(true)
	}
	m.picker = p
	m.pickerRefilter()
}

// pickerItems renders the mailbox tree into picker rows.
func (m *Model) pickerItems(includeActive bool) []ui.PickerItem {
	items := make([]ui.PickerItem, 0, len(m.snap.Mailboxes))
	for _, node := range m.snap.Mailboxes {
		if !includeActive && node.Mailbox.ID == m.snap.ActiveMailbox {
			continue
		}
		items = append(items, ui.PickerItem{ID: node.Mailbox.ID, Label: node.Mailbox.Name, Depth: node.Depth})
	}
	return items
}

// pickerRefilter applies the type-to-filter substring and clamps the
// selection into the filtered list.
func (m *Model) pickerRefilter() {
	p := m.picker
	f := strings.ToLower(p.filter)
	p.items = p.items[:0]
	for _, it := range p.all {
		if f == "" || strings.Contains(strings.ToLower(it.Label), f) {
			p.items = append(p.items, it)
		}
	}
	p.sel = min(max(p.sel, 0), max(len(p.items)-1, 0))
}

// pickerView renders the modal's state for ui.Render.
func (m *Model) pickerView() *ui.PickerView {
	p := m.picker
	title := "Move to mailbox"
	switch p.mode {
	case pickerCopy:
		title = "Copy to mailbox"
	case pickerArchive:
		title = "Choose archive destination"
	case pickerIdentity:
		title = "Send as"
	}
	return &ui.PickerView{Title: title, Filter: p.filter, Items: p.items, Sel: p.sel}
}

// pickerKey handles a keypress while the picker modal is open: true when
// the key was consumed.
func (m *Model) pickerKey(key string) (tea.Cmd, bool) {
	p := m.picker
	switch key {
	case "esc":
		m.picker = nil
		return nil, true
	case "j", "down":
		if p.sel < len(p.items)-1 {
			p.sel++
		}
		return nil, true
	case "k", "up":
		if p.sel > 0 {
			p.sel--
		}
		return nil, true
	case "enter":
		if p.sel < len(p.items) {
			return m.pickerChoose(p.items[p.sel].ID), true
		}
		return nil, true
	case "backspace":
		if p.filter != "" {
			r := []rune(p.filter)
			p.filter = string(r[:len(r)-1])
			m.pickerRefilter()
		}
		return nil, true
	default:
		if len(key) == 1 && key[0] >= 32 {
			p.filter += key
			p.sel = 0
			m.pickerRefilter()
			return nil, true
		}
		return nil, false
	}
}

// pickerChoose dispatches the picked mailbox per mode.
func (m *Model) pickerChoose(id mail.ID) tea.Cmd {
	p := m.picker
	name := ""
	for _, it := range p.all {
		if it.ID == id {
			name = it.Label
		}
	}
	m.picker = nil
	if p.mode == pickerIdentity {
		m.chooseIdentity(id)
		return nil
	}
	ids := m.actionIDs()
	if len(ids) == 0 {
		return nil
	}
	switch p.mode {
	case pickerMove:
		return m.triageCmd(sync.TriageSpec{Kind: sync.TriageMove, IDs: ids, Mailbox: id},
			fmt.Sprintf("Moved %d to %s", len(ids), name))
	case pickerCopy:
		return m.triageCmd(sync.TriageSpec{Kind: sync.TriageCopy, IDs: ids, Mailbox: id},
			fmt.Sprintf("Copied %d to %s", len(ids), name))
	case pickerArchive:
		if m.opts.Prefs == nil {
			m.opts.Prefs = &config.Prefs{}
		}
		m.opts.Prefs.SetArchiveMailbox(m.opts.AccountID, string(id))
		if err := savePrefs(m.opts); err != nil {
			m.err = truncateErr("prefs", err)
		}
		return m.triageCmd(sync.TriageSpec{Kind: sync.TriageMove, IDs: ids, Mailbox: id},
			fmt.Sprintf("Archived %d to %s", len(ids), name))
	}
	return nil
}

// savePrefs persists app-managed preferences (FR-J1: prefs.toml only, never
// config.toml). Sessions without an account id keep memory-only prefs.
func savePrefs(opts Options) error {
	if opts.PrefsPath == "" || opts.AccountID == "" || opts.Prefs == nil {
		return nil
	}
	return config.SavePrefs(opts.PrefsPath, opts.Prefs)
}

// --- attachment save (FR-E4) ---

// openFilePicker opens the directory chooser for attachment saves,
// rooted at the downloads directory.
func (m *Model) openFilePicker() (tea.Model, tea.Cmd) {
	if m.snap.Body == nil || len(m.snap.Body.Attachments) == 0 {
		m.err = "no attachments on this message"
		return m, nil
	}
	fp := filepicker.New()
	fp.DirAllowed = true
	fp.FileAllowed = false
	fp.ShowPermissions = false
	fp.ShowSize = false
	fp.CurrentDirectory = downloadsDirFn()
	fp.SetHeight(12)
	fp.AutoHeight = false
	m.fp = &filepickState{fp: fp, atts: append([]mail.Attachment(nil), m.snap.Body.Attachments...)}
	return m, fp.Init()
}

// filePickKey handles a keypress while the save overlay is open: true when
// consumed. enter saves into the browsed directory; esc cancels — the
// filepicker receives everything else.
func (m *Model) filePickKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch msg.Keystroke() {
	case "esc":
		m.fp = nil
		return nil, true
	case "enter":
		dir := m.fp.fp.CurrentDirectory
		return m.saveAttachmentsCmd(dir), true
	default:
		next, cmd := m.fp.fp.Update(msg)
		m.fp.fp = next
		return cmd, true
	}
}

// saveAttachmentsCmd downloads every attachment of the open message into
// dir (FR-E4): bytes stream from the session download URL and land on disk
// only here — the one sanctioned mail-data disk write (NFR-4).
func (m *Model) saveAttachmentsCmd(dir string) tea.Cmd {
	atts := m.fp.atts
	m.fp = nil
	return func() tea.Msg {
		var names []string
		for _, a := range atts {
			rc, err := m.opts.Provider.DownloadBlob(m.ctx, a.BlobID, a.Name, a.Type)
			if err != nil {
				return saveResultMsg{err: err}
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return saveResultMsg{err: err}
			}
			path, err := uniquePath(dir, a.Name)
			if err != nil {
				return saveResultMsg{err: err}
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return saveResultMsg{err: err}
			}
			names = append(names, a.Name)
		}
		return saveResultMsg{text: fmt.Sprintf("Saved %s to %s", strings.Join(names, ", "), dir)}
	}
}

// uniquePath resolves dir/name without ever overwriting: collisions get a
// numeric suffix (name-1.ext, name-2.ext, …).
func uniquePath(dir, name string) (string, error) {
	base := filepath.Join(dir, name)
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base, nil
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand, nil
		}
	}
}

// downloadsDirFn is the attachment-save default location (FR-E4); a var so
// tests can redirect it to a temp directory.
var downloadsDirFn = downloadsDir

// downloadsDir is the attachment-save default (FR-E4): ~/Downloads when it
// exists, the home directory otherwise.
func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	dl := filepath.Join(home, "Downloads")
	if fi, err := os.Stat(dl); err == nil && fi.IsDir() {
		return dl
	}
	return home
}

// --- small helpers ---

// seqNext allocates a monotonic id for toasts and destroy timers.
func (m *Model) seqNext() int {
	m.seq++
	return m.seq
}

// activeRole returns the open mailbox's role.
func (m *Model) activeRole() mail.Role {
	for _, node := range m.snap.Mailboxes {
		if node.Mailbox.ID == m.snap.ActiveMailbox {
			return node.Mailbox.Role
		}
	}
	return ""
}

// mailboxByRole finds a role mailbox, returning its id and sidebar node.
func (m *Model) mailboxByRole(role mail.Role) (mail.ID, mail.Mailbox) {
	for _, node := range m.snap.Mailboxes {
		if node.Mailbox.Role == role {
			return node.Mailbox.ID, node.Mailbox
		}
	}
	return "", mail.Mailbox{}
}

// mailboxByID finds a mailbox by id (nil when absent).
func (m *Model) mailboxByID(id mail.ID) *mail.Mailbox {
	for i, node := range m.snap.Mailboxes {
		if node.Mailbox.ID == id {
			return &m.snap.Mailboxes[i].Mailbox
		}
	}
	return nil
}
