package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	pickerSort     // choose the list order (FR-D8)
	pickerContactQuick
)

// pickerState is the modal mailbox chooser (FR-G2, FR-G4). In unified
// view one prompt is shown per owning account, in queue order — each
// prompt lists that account's own mailbox tree (FR-A5).
type pickerState struct {
	mode   pickerMode
	filter string
	all    []ui.PickerItem
	items  []ui.PickerItem
	sel    int

	// acct is whose tree this prompt shows; queue holds the accounts
	// still waiting for a pick. The identity picker (compose) sets
	// neither — its items are prebuilt.
	acct  string
	queue []string
}

// filepickState is the attachment-save overlay (FR-E4). acct pins the
// owning account: downloads go through that account's session (FR-A5).
type filepickState struct {
	fp   filepicker.Model
	atts []mail.Attachment
	acct string
}

// toastState is the active action receipt with its undo affordance (FR-G5).
type toastState struct {
	id   int
	text string
	hint string
	// undo holds one reversal spec per owning account — a batched action
	// in unified view spans accounts, and ctrl+z reverses all of them
	// (FR-A5: each reversal runs on its own engine).
	undo    []undoPart
	destroy []mail.ID // prepared delayed destroy (cancel path)
	until   time.Time
}

// undoPart is one account's reversal spec inside a toast.
type undoPart struct {
	acct string
	spec sync.TriageSpec
}

// pendingDestroy tracks a prepared (delayed) destroy awaiting its commit.
type pendingDestroy struct {
	seq int
	ids []mail.ID
}

// triageDoneMsg carries a finished triage action back to the model: one
// result per owning account (a unified batch spans accounts, FR-A5).
type triageDoneMsg struct {
	verb    string
	results []triageResult
	err     error // batch-level failure (account not enrolled)
}

// triageResult is one account's outcome inside a triage batch.
type triageResult struct {
	acct    string
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

// triageGroup is one account's slice of a batched action.
type triageGroup struct {
	acct string
	spec sync.TriageSpec
}

// triageCmd runs a single-account action on the active engine — the
// destroy path and other intrinsically single-view actions (FR-G2).
func (m *Model) triageCmd(spec sync.TriageSpec, verb string) tea.Cmd {
	return m.triageBatch([]triageGroup{{acct: m.activeID, spec: spec}}, verb)
}

// triageBatch runs one logical action on each owning account (FR-A5) and
// returns a single triageDoneMsg with per-account results. Grouping
// happens before this call, so no op ever crosses an account boundary
// (M6 gate: actions never cross accounts).
func (m *Model) triageBatch(groups []triageGroup, verb string) tea.Cmd {
	return func() tea.Msg {
		msg := triageDoneMsg{verb: verb}
		for _, g := range groups {
			eng, ok := m.engineFor(g.acct)
			if !ok {
				msg.results = append(msg.results, triageResult{
					acct: g.acct,
					err:  fmt.Errorf("account %q is not connected", g.acct),
				})
				continue
			}
			rcpt, err := eng.Triage(m.ctx, g.spec)
			msg.results = append(msg.results, triageResult{acct: g.acct, receipt: rcpt, err: err})
		}
		return msg
	}
}

// actionTarget is one row a triage action applies to: its owning account,
// the message id, and the rendered summary (direction decisions read its
// keywords).
type actionTarget struct {
	acct string
	id   mail.ID
	sum  mail.EmailSummary
}

// actionTargets resolves the target set for a triage action: the
// multi-select set in list order (FR-G3), else the cursor row. Each
// target carries its owner — in unified view rows interleave accounts
// (FR-A5).
func (m *Model) actionTargets() []actionTarget {
	var out []actionTarget
	for _, r := range m.snap.Rows {
		acct := r.Account
		if acct == "" {
			acct = m.activeID
		}
		if m.sel[m.rowKey(acct, r.ID)] {
			out = append(out, actionTarget{acct: acct, id: r.ID, sum: r.Summary})
		}
	}
	if len(out) > 0 {
		return out
	}
	if acct, r, ok := m.cursorRef(); ok {
		return []actionTarget{{acct: acct, id: r.ID, sum: r.Summary}}
	}
	return nil
}

// groupTargets splits targets by owning account, preserving list order —
// the execution shape triageBatch expects (FR-A5).
func groupTargets(ts []actionTarget) []triageGroup {
	var groups []triageGroup
	byAcct := map[string]int{}
	for _, t := range ts {
		if i, ok := byAcct[t.acct]; ok {
			groups[i].spec.IDs = append(groups[i].spec.IDs, t.id)
			continue
		}
		byAcct[t.acct] = len(groups)
		groups = append(groups, triageGroup{acct: t.acct, spec: sync.TriageSpec{IDs: []mail.ID{t.id}}})
	}
	return groups
}

// clearSelection empties the multi-select set (actions consume it; mailbox
// switches reset it).
func (m *Model) clearSelection() {
	if len(m.sel) > 0 {
		m.sel = map[mail.ID]bool{}
	}
}

// keywordCmd builds the batched read/unread or star/unstar action: any id
// missing the keyword flips the whole set toward it (batch triage
// semantics — one action, one direction, FR-G1/G3), grouped per owner.
func (m *Model) keywordCmd(kind sync.TriageKind) tea.Cmd {
	targets := m.actionTargets()
	if len(targets) == 0 {
		return nil
	}
	keyword := "$seen"
	toward := sync.TriageRead
	if kind == sync.TriageStar || kind == sync.TriageUnstar {
		keyword = "$flagged"
		toward = sync.TriageStar
	}
	anyMissing := false
	for _, t := range targets {
		if !t.sum.Keywords.Has(keyword) {
			anyMissing = true
			break
		}
	}
	var applied sync.TriageKind
	var verb string
	if anyMissing {
		applied = toward
		verb = "Marked read"
		if toward == sync.TriageStar {
			verb = "Starred"
		}
	} else {
		applied = sync.TriageUnread
		verb = "Marked unread"
		if toward == sync.TriageStar {
			applied = sync.TriageUnstar
			verb = "Unstarred"
		}
	}
	groups := groupTargets(targets)
	for i := range groups {
		groups[i].spec.Kind = applied
	}
	return m.triageBatch(groups, fmt.Sprintf("%s %s", verb, countN(len(targets))))
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
// the role-trash mailbox — resolved on each row's owning account, never
// the active account's tree (FR-A5).
func (m *Model) deleteAction() (tea.Model, tea.Cmd) {
	targets := m.actionTargets()
	if len(targets) == 0 {
		return m, nil
	}
	groups := groupTargets(targets)

	// Inside Trash (a single-account view — unified merges inboxes) the
	// destroy path applies.
	inTrash := true
	for _, g := range groups {
		if m.ownerRole(g.acct) != mail.RoleTrash {
			inTrash = false
			break
		}
	}
	if inTrash {
		return m, m.prepareDestroy(groups[0].spec.IDs)
	}

	total := 0
	names := []string{}
	var move []triageGroup
	var missing []string
	for _, g := range groups {
		dest, node := m.mailboxByRoleOn(g.acct, mail.RoleTrash)
		if dest == "" {
			missing = append(missing, m.accountName(g.acct))
			continue
		}
		g.spec.Kind = sync.TriageMove
		g.spec.Mailbox = dest
		move = append(move, g)
		total += len(g.spec.IDs)
		names = append(names, node.Name)
	}
	if len(missing) > 0 {
		if len(groups) == 1 {
			m.err = "no trash mailbox on this server"
		} else {
			m.err = "no trash mailbox on " + strings.Join(missing, ", ")
		}
	}
	if len(move) == 0 {
		return m, nil
	}
	return m, m.triageBatch(move,
		fmt.Sprintf("Deleted %s to %s", countN(total), firstOr(names, "Trash")))
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
// that remembers in app-managed prefs (FR-J1). Destinations resolve on
// each row's owning account (FR-A5); owners that have neither resolve to
// a picker prompt queued per account.
func (m *Model) archiveAction() (tea.Model, tea.Cmd) {
	targets := m.actionTargets()
	if len(targets) == 0 {
		return m, nil
	}
	groups := groupTargets(targets)

	names := []string{}
	var needPick []string
	for i := range groups {
		g := &groups[i]
		g.spec.Kind = sync.TriageMove
		if dest, node := m.mailboxByRoleOn(g.acct, mail.RoleArchive); dest != "" {
			g.spec.Mailbox = dest
			names = append(names, node.Name)
			continue
		}
		if dest := m.opts.Prefs.ArchiveMailbox(g.acct); dest != "" {
			if node := m.mailboxByIDOn(g.acct, mail.ID(dest)); node != nil {
				g.spec.Mailbox = mail.ID(dest)
				names = append(names, node.Name)
				continue
			}
		}
		needPick = append(needPick, g.acct)
	}

	if len(needPick) == 0 {
		return m, m.triageBatch(groups,
			fmt.Sprintf("Archived %d to %s", countGroupIDs(groups), firstOr(names, "Archive")))
	}
	// Owners without a role or remembered destination each get a prompt
	// over their own mailbox tree; the whole batch dispatches when the
	// last pick lands (one toast, one undo — FR-G5).
	m.openPickerQueued(pickerArchive, groups, needPick)
	return m, nil
}

// countGroupIDs totals the ids across a grouped batch.
func countGroupIDs(groups []triageGroup) int {
	n := 0
	for _, g := range groups {
		n += len(g.spec.IDs)
	}
	return n
}

// firstOr returns the first element or a fallback.
func firstOr(xs []string, fallback string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return fallback
}

// undoAction reverses the toast's action while its window is open (FR-G5):
// stored reversal specs re-run as Triage on their owning accounts; a
// prepared destroy cancels.
func (m *Model) undoAction() (tea.Model, tea.Cmd) {
	// A held submission outranks any action receipt: ctrl+z during the
	// undo window cancels the send outright (FR-H5).
	if m.pendingSend != nil {
		return m.cancelPendingSend()
	}
	// A held contact destroy is likewise cancelled before its window
	// closes — the /set was never sent (FR-L2).
	if m.undoContactDelete() {
		return m, nil
	}
	t := m.toast
	if t == nil {
		return m, nil
	}
	switch {
	case t.destroy != nil:
		ids := t.destroy
		acct := m.activeID
		m.toast = nil
		m.pendingDestroy = nil
		return m, m.opOn(acct, "cancel-destroy", func(_ context.Context, eng *sync.Engine) (sync.Snapshot, error) {
			return eng.CancelDestroy(m.ctx, ids), nil
		})
	case len(t.undo) > 0:
		parts := t.undo
		m.toast = nil
		groups := make([]triageGroup, 0, len(parts))
		for _, p := range parts {
			groups = append(groups, triageGroup(p))
		}
		return m, m.triageBatch(groups, "Undone")
	}
	m.toast = nil
	return m, nil
}

// showToast installs the active toast and returns its expiry Cmd.
func (m *Model) showToast(text, hint string, undo []undoPart, destroy []mail.ID) tea.Cmd {
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

// handleTriageDone applies a finished triage action: snapshot adoption
// per owning account, selection reset, and the undo toast (FR-G5). Any
// per-account failure surfaces as the error line and suppresses the
// toast — the reversal of a half-applied batch would be a lie.
func (m *Model) handleTriageDone(msg triageDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = truncateErr("triage", msg.err)
		return m, nil
	}
	var cmds []tea.Cmd
	var undo []undoPart
	var errs []string
	applied := 0
	for _, r := range msg.results {
		if r.err != nil {
			errs = append(errs, truncateErr("triage", r.err))
			continue
		}
		if r.receipt.Snap.Version > m.snaps[r.acct].Version {
			_, cmd := m.applySnapshot(r.acct, r.receipt.Snap)
			cmds = append(cmds, cmd)
		}
		if len(r.receipt.Failed) > 0 {
			errs = append(errs, r.receipt.Err)
		}
		applied += len(r.receipt.Applied)
		if r.receipt.Undo != nil {
			undo = append(undo, undoPart{acct: r.acct, spec: *r.receipt.Undo})
		}
	}
	m.clearSelection()

	if len(errs) > 0 {
		m.err = errs[0]
		return m, tea.Batch(cmds...)
	}
	if applied == 0 {
		return m, tea.Batch(cmds...)
	}
	hint := ""
	if len(undo) > 0 {
		hint = "ctrl+z undo"
	}
	return m, tea.Batch(append(cmds, m.showToast(msg.verb, hint, undo, nil))...)
}

// --- mailbox picker (FR-G2, FR-G4) ---

// openPicker opens the chooser for the cursor/selection's owners: one
// prompt per owning account (in unified view that is one per account,
// FR-A5), dispatching the grouped batch when the last pick lands.
func (m *Model) openPicker(mode pickerMode) {
	targets := m.actionTargets()
	if len(targets) == 0 {
		return
	}
	groups := groupTargets(targets)
	kind := sync.TriageMove
	if mode == pickerCopy {
		kind = sync.TriageCopy
	}
	queue := make([]string, 0, len(groups))
	for i := range groups {
		groups[i].spec.Kind = kind
		queue = append(queue, groups[i].acct)
	}
	m.openPickerQueued(mode, groups, queue)
}

// openPickerQueued opens the prompt for queue[0], remembering pending
// grouped work; picks fill each account's Mailbox and the batch dispatches
// when the queue empties.
func (m *Model) openPickerQueued(mode pickerMode, pending []triageGroup, queue []string) {
	if len(queue) == 0 {
		return
	}
	m.pickPending = pending
	m.pickLabel = ""
	m.picker = &pickerState{
		mode:  mode,
		acct:  queue[0],
		queue: append([]string(nil), queue[1:]...),
	}
	m.picker.all = m.pickerItemsFor(mode, queue[0])
	m.pickerRefilter()
}

// pickerItemsFor renders one account's mailbox tree into picker rows; a
// move excludes that account's own open mailbox (moving onto itself is a
// no-op) — never the active account's, which may be a different one.
func (m *Model) pickerItemsFor(mode pickerMode, acct string) []ui.PickerItem {
	s := m.snapFor(acct)
	exclude := mail.ID("")
	if mode == pickerMove {
		exclude = s.ActiveMailbox
	}
	items := make([]ui.PickerItem, 0, len(s.Mailboxes))
	for _, node := range s.Mailboxes {
		if node.Mailbox.ID == exclude {
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
	case pickerSort:
		title = "Sort by"
	case pickerContactQuick:
		title = "Add contact"
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
		m.pickPending = nil
		m.pickLabel = ""
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

// pickerChoose records the picked mailbox for the prompting account and
// either advances the queue (FR-A5: one prompt per owner) or dispatches
// the accumulated batch as a single undoable action.
func (m *Model) pickerChoose(id mail.ID) tea.Cmd {
	p := m.picker
	if p == nil {
		return nil
	}
	label := ""
	for _, it := range p.all {
		if it.ID == id {
			label = it.Label
		}
	}
	m.picker = nil
	if p.mode == pickerIdentity {
		return m.chooseIdentity(id)
	}
	if p.mode == pickerSort {
		return m.chooseSort(string(id))
	}
	if p.mode == pickerContactQuick {
		m.picker = nil
		return m.insertComposeAddress(label)
	}
	if m.pickLabel == "" {
		m.pickLabel = label
	}
	for i := range m.pickPending {
		if m.pickPending[i].acct == p.acct {
			m.pickPending[i].spec.Mailbox = id
		}
	}
	if len(p.queue) > 0 {
		m.picker = &pickerState{
			mode:  p.mode,
			acct:  p.queue[0],
			queue: append([]string(nil), p.queue[1:]...),
		}
		m.picker.all = m.pickerItemsFor(p.mode, p.queue[0])
		m.pickerRefilter()
		return nil
	}

	groups := m.pickPending
	m.pickPending = nil
	name := m.pickLabel
	m.pickLabel = ""
	if len(groups) == 0 {
		return nil
	}
	total := countGroupIDs(groups)
	switch p.mode {
	case pickerMove:
		return m.triageBatch(groups, fmt.Sprintf("Moved %d to %s", total, name))
	case pickerCopy:
		return m.triageBatch(groups, fmt.Sprintf("Copied %d to %s", total, name))
	case pickerArchive:
		if m.opts.Prefs == nil {
			m.opts.Prefs = &config.Prefs{}
		}
		// Remember per owning account — archive destinations are a
		// per-account preference (FR-J1).
		for _, g := range groups {
			m.opts.Prefs.SetArchiveMailbox(g.acct, string(g.spec.Mailbox))
		}
		if err := savePrefs(m.opts, groups[0].acct); err != nil {
			m.err = truncateErr("prefs", err)
		}
		return m.triageBatch(groups, fmt.Sprintf("Archived %d to %s", total, name))
	}
	return nil
}

// savePrefs persists app-managed preferences (FR-J1: prefs.toml only, never
// config.toml). Sessions without an account id keep memory-only prefs.
func savePrefs(opts Options, acct string) error {
	if opts.PrefsPath == "" || acct == "" || opts.Prefs == nil {
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
	m.fp = &filepickState{
		fp:   fp,
		atts: append([]mail.Attachment(nil), m.snap.Body.Attachments...),
		acct: m.ownerAccount(),
	}
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
// dir (FR-E4): bytes stream from the owning account's session download
// URL and land on disk only here — the one sanctioned mail-data disk
// write (NFR-4).
func (m *Model) saveAttachmentsCmd(dir string) tea.Cmd {
	atts := m.fp.atts
	acct := m.fp.acct
	if acct == "" {
		acct = m.activeID
	}
	m.fp = nil
	return func() tea.Msg {
		prov := m.hub.Provider(acct)
		if prov == nil {
			return saveResultMsg{err: fmt.Errorf("account %q is not connected", acct)}
		}
		var names []string
		for _, a := range atts {
			rc, err := prov.DownloadBlob(m.ctx, a.BlobID, a.Name, a.Type)
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
			if err := writeNewFile(path, data); err != nil {
				return saveResultMsg{err: err}
			}
			names = append(names, a.Name)
		}
		return saveResultMsg{text: fmt.Sprintf("Saved %s to %s", strings.Join(names, ", "), dir)}
	}
}

// uniqueTries bounds the collision loop in uniquePath. The loop advances
// on every iteration and gives up with an error, so no filename — however
// hostile — can keep it spinning.
const uniqueTries = 1000

// uniquePath resolves dir/name without ever overwriting and without ever
// leaving dir. The name comes from the JMAP server and ultimately the
// sender, so invalid names are rejected outright: "",
// ".", "..", path separators (either kind — the download-URL side folds
// Windows separators too), NUL, and anything past one filesystem's
// 255-byte component limit. Collisions get a numeric suffix (name-1.ext,
// name-2.ext, …), and every lookup uses os.Lstat with a full
// errors.Is(err, fs.ErrNotExist) check: a stat failure that is not
// "does not exist" (EINVAL from a NUL byte, ENAMETOOLONG from an
// oversized component) returns that error instead of looping forever on a
// predicate that can never become true.
func uniquePath(dir, name string) (string, error) {
	if err := validAttachmentName(name); err != nil {
		return "", err
	}
	name = filepath.Base(name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i <= uniqueTries; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		p := filepath.Join(dir, cand)
		switch _, err := os.Lstat(p); {
		case err == nil:
			continue // occupied — try the next suffix
		case errors.Is(err, fs.ErrNotExist):
			return p, nil
		default:
			return "", fmt.Errorf("save attachment %q: %w", name, err)
		}
	}
	return "", fmt.Errorf("save attachment %q: no free file name in %s after %d tries", name, dir, uniqueTries)
}

// validAttachmentName rejects a server-supplied filename that could
// escape the chosen directory or wedge the save. The upload
// side applies the same discipline to local names (compose.go); this is
// its mirror for names travelling the other way.
func validAttachmentName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return errors.New("attachment has no usable file name")
	case strings.ContainsAny(name, "/\\\x00"):
		return fmt.Errorf("attachment name %q contains a path separator or NUL", name)
	case len(name) > 255:
		return fmt.Errorf("attachment name is too long (%d bytes, limit 255)", len(name))
	}
	return nil
}

// writeNewFile creates path without ever overwriting or following a
// symlink: O_EXCL fails if anything already sits at the
// destination — a raced collision or a pre-planted link — and the mode
// stays 0600. os.WriteFile would have written straight through the link.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("save attachment: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("save attachment: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("save attachment: %w", err)
	}
	return nil
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

// snapFor returns the real (non-synthetic) snapshot of an account — the
// tree every owner-scoped decision reads. In unified view the view
// snapshot's tree belongs to the active account only (FR-A5).
func (m *Model) snapFor(acct string) sync.Snapshot {
	if m.unified {
		if s, ok := m.snaps[acct]; ok {
			return s
		}
		return m.snap
	}
	if acct == m.activeID {
		return m.snap
	}
	if s, ok := m.snaps[acct]; ok {
		return s
	}
	return m.snap
}

// ownerRole is the role of the mailbox an account currently has open.
func (m *Model) ownerRole(acct string) mail.Role {
	s := m.snapFor(acct)
	for _, node := range s.Mailboxes {
		if node.Mailbox.ID == s.ActiveMailbox {
			return node.Mailbox.Role
		}
	}
	return ""
}

// mailboxByRoleOn finds a role mailbox on one account's tree (FR-A5).
func (m *Model) mailboxByRoleOn(acct string, role mail.Role) (mail.ID, mail.Mailbox) {
	for _, node := range m.snapFor(acct).Mailboxes {
		if node.Mailbox.Role == role {
			return node.Mailbox.ID, node.Mailbox
		}
	}
	return "", mail.Mailbox{}
}

// mailboxByIDOn finds a mailbox by id on one account's tree.
func (m *Model) mailboxByIDOn(acct string, id mail.ID) *mail.Mailbox {
	nodes := m.snapFor(acct).Mailboxes
	for i, node := range nodes {
		if node.Mailbox.ID == id {
			return &nodes[i].Mailbox
		}
	}
	return nil
}

// activeRole returns the open mailbox's role (active account).
func (m *Model) activeRole() mail.Role {
	for _, node := range m.snap.Mailboxes {
		if node.Mailbox.ID == m.snap.ActiveMailbox {
			return node.Mailbox.Role
		}
	}
	return ""
}

// mailboxByID finds a mailbox by id (nil when absent).
func (m *Model) mailboxByID(id mail.ID) *mail.Mailbox {
	return m.mailboxByIDOn(m.activeID, id)
}
