// Package app wires the sync engine to the UI: a Bubble Tea model that
// routes keys, schedules engine operations as Cmds, and renders ui.State.
// Nothing in Update blocks: every network hop runs in a Cmd goroutine and
// lands as a message (NFR-1).
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/mailtext"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// Options configure the app model.
type Options struct {
	// Keys/Theme as before.
	Keys  *ui.KeyMap
	Theme ui.Theme // resolved dark/light at startup (FR-I2)

	// Provider/AccountID describe the single account of pre-M6 call
	// sites; when Accounts is set it wins (M6, FR-A1).
	Provider  mail.Provider
	AccountID string

	// Accounts is the enrolled set (M6): one entry per configured
	// account, in config order. ID keys app-managed preferences
	// (prefs.toml, FR-J1); empty disables preference writes.
	Accounts []AccountOpt

	// Prefs is the app-managed preference store (FR-J1) and PrefsPath
	// where it persists; either may be empty (session-only memory).
	Prefs     *config.Prefs
	PrefsPath string

	// UndoDelay is how long Send holds the submission before it reaches
	// the server (FR-H5); 5s when zero, negative submits immediately.
	UndoDelay time.Duration

	// Version is the build version shown as the help overlay's last
	// line (FR-I11); empty hides the line.
	Version string
}

// AccountOpt is one account to enroll (M6): its identity for the
// switcher, the provider driving it, and whether the TUI pre-flight
// Connect succeeded (false ⇒ the Hub retries with backoff while every
// other account keeps running — failure isolation, PLAN §4.3).
type AccountOpt struct {
	ID        string
	Name      string
	Provider  mail.Provider
	Connected bool
	// DefaultIdentity optionally pins the composer's From for this
	// account (FR-A1): matched by email, then by id.
	DefaultIdentity string
	// InitialMailbox is the mailbox this account opens on first load,
	// chosen in the wizard (FR-I8); empty (or an id no longer in the
	// tree) falls back to the inbox.
	InitialMailbox mail.ID
	// Err is the pre-flight failure (bad credentials, unreachable
	// server). Non-nil with a nil Provider means the account cannot
	// recover without a config fix; the Hub surfaces it on the status
	// line and the other accounts keep running (failure isolation).
	Err error
}

// Model is the Bubble Tea model for the reader.
type Model struct {
	opts Options
	hub  *sync.Hub
	// accounts is the enrollment order (tint identity, FR-A5); acctOrder
	// is the display order of the sidebar blocks and switcher (FR-C5,
	// prefs.toml). activeID is the account the list/preview/bindings
	// default to (FR-A4); engine is activeID's engine, cached for the hot
	// path.
	accounts    []sync.AccountInfo
	acctOrder   []string
	activeID    string
	engine      *sync.Engine
	snaps       map[string]sync.Snapshot // last snapshot per account
	unified     bool                     // merged-inbox view (FR-A5)
	uCursorID   mail.ID                  // unified cursor id; "" = top
	cursorOwner string                   // account owning the unified cursor row
	bodyReq     mail.ID                  // in-flight unified body load (row key)
	prevBox     map[string]mail.ID       // unified enter/exit mailbox restore
	switcher    *switchState             // non-nil while the switcher is open

	snap   sync.Snapshot
	width  int
	height int

	focus          ui.Pane
	sidebarVisible bool
	// sidebarKey is the sidebar cursor's identity (FR-C5): a bare account
	// id for a header, "acct\x00mailbox" for a folder. The row index is
	// derived from it per frame, so reorders and tree changes re-anchor
	// instead of stranding the selection.
	sidebarKey string
	// collapsed is the fold set (FR-C6): row keys (sidebarRowKey) whose
	// subtree is hidden — a bare account id folds that account's whole
	// tree, "acct\x00mailbox" folds that folder's subtree. Seeded from
	// prefs at startup, mutated only by the fold actions, persisted back
	// (ids only, never mail data — NFR-4).
	collapsed  map[string]bool
	showSize   bool
	helpOpen   bool
	fullscreen bool // full-screen message view (FR-E5)
	stacked    bool // list above preview (FR-I10), remembered in prefs
	// sortByID remembers each account's current list order for the
	// picker's preselect (FR-D8); prefs persist it across runs.
	sortByID map[string]string
	viewKey  string // last seen snapshot view (mailbox/search switch)

	vp       viewport.Model
	vpBodyID mail.ID

	lastCtrlC  time.Time
	err        string
	freshArmed bool

	// Search state (M4): query bar + advanced modal; nil when closed
	// (FR-F1, FR-F2).
	search *searchState

	// manageAccounts is set when the user asked for the account wizard
	// (ctrl+a): the command layer quits this session, runs the wizard, and
	// relaunches with whatever it saved (FR-I8).
	manageAccounts bool

	// Triage state (M3): multi-select set, modal overlays, toast, and the
	// prepared (delayed) destroy (FR-G2..G5). pickPending/pickLabel hold
	// a queued multi-account picker batch (M6, FR-A5).
	sel            map[mail.ID]bool
	picker         *pickerState
	fp             *filepickState
	toast          *toastState
	pendingDestroy *pendingDestroy
	seq            int
	pickPending    []triageGroup
	pickLabel      string

	// Compose state (M5): nil when the composer is closed (FR-H1..H5).
	compose     *composeState
	attachPick  *filepickState
	pendingSend *pendingSend

	// Contacts state (M9, FR-L): the full-screen view and the form modal
	// are nil when closed (the overlay pattern); contactSnaps holds every
	// account's latest contact store; pendingContactDelete is a held
	// destroy inside its undo window (FR-L2).
	contacts             *contactsState
	contactForm          *contactFormState
	contactSnaps         map[string]sync.ContactSnapshot
	pendingContactDelete *pendingContactDelete

	ctx    context.Context
	cancel context.CancelFunc
}

// New returns the root model. With no Accounts enrolled it falls back to
// the single Provider/AccountID pair (pre-M6 call sites).
func New(opts Options) *Model {
	ctx, cancel := context.WithCancel(context.Background())
	if opts.UndoDelay == 0 {
		opts.UndoDelay = 5 * time.Second
	}
	accs := opts.Accounts
	if len(accs) == 0 && opts.Provider != nil {
		id := opts.AccountID
		if id == "" {
			id = "default"
		}
		// A pre-M6 provider is always handed over connected.
		accs = []AccountOpt{{ID: id, Name: id, Provider: opts.Provider, Connected: true}}
	}
	hub := sync.NewHub()
	sortByID := map[string]string{}
	for _, a := range accs {
		var scfg sync.Config
		if opts.Prefs != nil {
			// The account's remembered list order (FR-D8) rides the
			// engine config; unknown ids fall back to newest first.
			if o := opts.Prefs.Sort(a.ID); o != "" {
				sortByID[a.ID] = o
				scfg.Sort = sortCrits(o)
			}
		}
		hub.Enroll(a.ID, a.Name, a.Provider, a.Connected, scfg)
		if a.Err != nil {
			// Show the pre-flight failure immediately; Hub.Start keeps
			// retrying whatever provider exists (failure isolation).
			hub.NoteError(a.ID, a.Err)
		}
	}
	infos := hub.Accounts()
	active := opts.AccountID
	if hub.Engine(active) == nil && len(infos) > 0 {
		active = infos[0].ID
	}
	// Display order (FR-C5): enrollment order overlaid with the user's
	// saved arrangement; tints stay keyed to enrollment (accountTint).
	base := make([]string, 0, len(infos))
	for _, a := range infos {
		base = append(base, a.ID)
	}
	var saved []string
	if opts.Prefs != nil {
		saved = opts.Prefs.AccountOrder
	}
	m := &Model{
		opts:           opts,
		hub:            hub,
		accounts:       infos,
		acctOrder:      config.MergeAccountOrder(base, saved),
		collapsed:      loadCollapsed(opts.Prefs, infos),
		activeID:       active,
		engine:         hub.Engine(active),
		snaps:          map[string]sync.Snapshot{},
		contactSnaps:   map[string]sync.ContactSnapshot{},
		prevBox:        map[string]mail.ID{},
		focus:          ui.PaneList,
		sidebarVisible: true,
		vp:             viewport.New(),
		ctx:            ctx,
		cancel:         cancel,
		stacked:        opts.Prefs.Stacked(),
		sortByID:       sortByID,
	}
	for _, a := range infos {
		// Every account starts as an unloaded view: the unified merge
		// excludes Total<0 rows and the switcher shows the skeleton until
		// the account's first snapshot lands.
		m.snaps[a.ID] = sync.Snapshot{Total: -1}
	}
	if opts.AccountID == "" && len(accs) == 1 {
		m.opts.AccountID = accs[0].ID
	}
	return m
}

// snapMsg carries a fresh engine snapshot from one account. live marks
// snapshots delivered by the sync loop's broadcast (as opposed to an
// operation's return), which re-arm the waiter. An empty acct means the
// active account (single-account paths).
type snapMsg struct {
	acct string
	snap sync.Snapshot
	live bool
}

// errMsg carries a failed engine operation (acct empty ⇒ active).
type errMsg struct {
	acct string
	op   string
	err  error
}

// resolve fills the account of a message that omitted it.
func (m *Model) resolve(acct string) string {
	if acct == "" {
		return m.activeID
	}
	return acct
}

// opOn wraps a blocking call on a specific account's engine as a Cmd. The
// target engine is captured at creation on the UI thread, so a mid-flight
// account switch can never redirect the op — actions never cross accounts
// (M6 gate). Accounts without a working provider (enrollment or credential
// failure) fail gracefully instead of touching a nil engine.
func (m *Model) opOn(acct, op string, f func(context.Context, *sync.Engine) (sync.Snapshot, error)) tea.Cmd {
	eng, ok := m.engineFor(acct)
	return func() tea.Msg {
		if !ok {
			return errMsg{acct: acct, op: op, err: fmt.Errorf("account %q is not connected", acct)}
		}
		snap, err := f(m.ctx, eng)
		if err != nil {
			return errMsg{acct: acct, op: op, err: err}
		}
		return snapMsg{acct: acct, snap: snap}
	}
}

// engineFor returns the account's engine when it can actually talk to a
// server; unenrolled or credential-less accounts route to a graceful
// error (failure isolation, PLAN §4.3).
func (m *Model) engineFor(acct string) (*sync.Engine, bool) {
	eng := m.hub.Engine(acct)
	if eng == nil || m.hub.Provider(acct) == nil {
		return nil, false
	}
	return eng, true
}

// engineOp wraps a blocking call on the active account's engine (FR-B2
// plumbing). Multi-account routing goes through opOn, which captures the
// owner engine up front.
func (m *Model) engineOp(op string, f func(ctx context.Context) (sync.Snapshot, error)) tea.Cmd {
	acct := m.activeID
	return m.opOn(acct, op, func(ctx context.Context, _ *sync.Engine) (sync.Snapshot, error) {
		return f(ctx)
	})
}

// waitUpdates consumes one account's latest-wins broadcast; each delivery
// re-arms itself so exactly one waiter per account is outstanding at a
// time (FR-B2: live changes repaint with no user action).
func (m *Model) waitUpdates() tea.Cmd { return m.waitUpdatesFor(m.activeID) }

// waitUpdatesFor is waitUpdates for one account; Init arms one per
// enrolled account so every engine repaints (status, switcher, unified).
func (m *Model) waitUpdatesFor(acct string) tea.Cmd {
	eng := m.hub.Engine(acct)
	if eng == nil {
		return nil
	}
	return func() tea.Msg {
		snap, ok := <-eng.Updates()
		if !ok {
			return nil
		}
		return snapMsg{acct: acct, snap: snap, live: true}
	}
}

// freshFade is the timer that clears the new-mail highlight (the slide-in
// fades, PLAN §4.1 case 3). Clearing is local and hits every account —
// each engine republishes, so the waiters fold the result back in.
func (m *Model) freshFade() tea.Cmd {
	return tea.Tick(sync.FreshTTL, func(time.Time) tea.Msg {
		m.clearFresh()
		return nil
	})
}

// clearFresh drops the new-mail highlight on every account that has one.
func (m *Model) clearFresh() {
	for _, a := range m.accounts {
		eng := m.hub.Engine(a.ID)
		if eng == nil {
			continue
		}
		if len(m.snaps[a.ID].Fresh) > 0 {
			eng.ClearFresh()
		}
	}
}

// Init starts every account's sync loop (connect-with-retry through the
// Hub, PLAN §4.3) and arms one live-update waiter per account; each
// account's first snapshot then opens its own inbox (FR-B1) — that warm
// window is what makes switching instant (FR-A4).
func (m *Model) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, a := range m.accounts {
		if c := m.waitUpdatesFor(a.ID); c != nil {
			cmds = append(cmds, c)
		}
		// The contacts broadcast idles until the store warms (FR-L1) —
		// one waiter per account, armed from the start like the mail one.
		if c := m.waitContactsFor(a.ID); c != nil {
			cmds = append(cmds, c)
		}
	}
	m.hub.StartAll(m.ctx)
	return tea.Batch(cmds...)
}

// loadAccountCmd fetches identities and the mailbox tree for the active
// account (FR-B1). Production loading is driven by Hub.StartAll at Init;
// tests drive it directly to keep the Update loop synchronous.
func (m *Model) loadAccountCmd() tea.Cmd {
	return m.opOn(m.activeID, "load-account", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
		// Identity absence or failure never blocks reading (FR-A6):
		// compose needs identities in M5, the reader does not.
		_ = eng.LoadIdentities(ctx)
		if err := eng.LoadMailboxes(ctx); err != nil {
			return sync.Snapshot{}, err
		}
		return eng.Snapshot(), nil
	})
}

// Update processes one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeViewport()
		return m, nil

	case snapMsg:
		// Snapshots are immutable per version: stale deliveries (a
		// broadcast racing an op's direct return) apply nothing but still
		// re-arm the waiter for live deliveries.
		acct := m.resolve(msg.acct)
		var cmd tea.Cmd
		if msg.snap.Version > m.snaps[acct].Version {
			_, cmd = m.applySnapshot(acct, msg.snap)
		}
		if msg.live {
			return m, tea.Batch(cmd, m.waitUpdatesFor(acct))
		}
		return m, cmd

	case errMsg:
		acct := m.resolve(msg.acct)
		if msg.op == "load-body" {
			// A failed body load must re-issue on the next rebuild, not
			// wedge the unified preview behind its request key.
			m.bodyReq = ""
		}
		if errors.Is(msg.err, sync.ErrNoUnread) {
			// The jump's own notice, verbatim (FR-D7).
			m.err = sync.ErrNoUnread.Error()
		} else if acct != m.activeID && len(m.accounts) > 1 {
			m.err = truncateErr(m.accountName(acct)+": "+msg.op, msg.err)
		} else {
			m.err = truncateErr(msg.op, msg.err)
		}
		return m, nil

	case searchDebounceMsg:
		// Stale generations (newer keystrokes arrived) are dropped; a
		// no-op equals the text already issued.
		if m.search == nil || m.search.adv != nil || msg.seq != m.search.seq {
			return m, nil
		}
		if m.search.spec.Text == m.search.issued {
			return m, nil
		}
		return m, m.issueSearch()

	case triageDoneMsg:
		return m.handleTriageDone(msg)

	case toastExpireMsg:
		if m.toast != nil && m.toast.id == msg.id {
			m.toast = nil
		}
		return m, nil

	case destroyCommitMsg:
		if m.pendingDestroy == nil || m.pendingDestroy.seq != msg.seq {
			return m, nil
		}
		ids := m.pendingDestroy.ids
		m.pendingDestroy = nil
		m.toast = nil
		return m, m.triageCmd(sync.TriageSpec{Kind: sync.TriageDestroy, IDs: ids},
			"Destroyed")

	case saveResultMsg:
		if msg.err != nil {
			m.err = truncateErr("save attachment", msg.err)
			return m, nil
		}
		return m, m.showToast(msg.text, "", nil, nil)

	// --- compose (M5, FR-H1..H6) ---
	case composePrepMsg:
		return m.handleComposePrep(msg)
	case composeAutosaveMsg:
		if m.compose == nil || m.compose.discard || msg.seq != m.compose.saveSeq {
			return m, nil
		}
		m.compose.armed = false
		if !m.compose.dirty {
			return m, nil
		}
		return m, m.saveDraftCmd()
	case draftSavedMsg:
		return m.handleDraftSaved(msg)
	case sendArmedMsg:
		return m.handleSendArmed(msg)
	case sendCommitMsg:
		if m.pendingSend == nil || m.pendingSend.seq != msg.seq {
			return m, nil // cancelled during the undo window
		}
		return m, m.commitSend(m.pendingSend)
	case sendDoneMsg:
		return m.handleSendDone(msg)
	case uploadProgressMsg:
		return m.handleUploadProgress()
	case uploadDoneMsg:
		return m.handleUploadDone(msg)

	// --- contacts (M9, FR-L) ---
	case contactMsg:
		acct := m.resolve(msg.acct)
		cmd := m.applyContactSnap(acct, msg.snap)
		if msg.live {
			return m, tea.Batch(cmd, m.waitContactsFor(acct))
		}
		return m, cmd
	case contactOpMsg:
		return m.handleContactOp(msg)
	case contactDeleteCommitMsg:
		return m.contactDeleteCommit(msg.seq)

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	// The filepicker's directory reads arrive as unexported bubbles
	// message types, so no case above can ever match them: hand every
	// unhandled message to the open picker, or its listing never loads
	// (FR-E4 save, FR-H3 attach). Keys never reach here — they return
	// above — and a picker ignores messages carrying another picker's id.
	var cmd tea.Cmd
	if m.fp != nil {
		next, c := m.fp.fp.Update(msg)
		m.fp.fp = next
		cmd = c
	}
	if m.attachPick != nil {
		next, c := m.attachPick.fp.Update(msg)
		m.attachPick.fp = next
		cmd = tea.Batch(cmd, c)
	}
	return m, cmd
}

// truncateErr renders an operation failure for the status line: bounded
// to 120 bytes, rune-safe and control-free via mailtext.Truncate
// (the old byte slice split multi-byte runes).
func truncateErr(op string, err error) string {
	return op + ": " + mailtext.Truncate(err.Error(), 120)
}

// applySnapshot stores one account's snapshot and schedules follow-up
// work: the first mailbox snapshot opens that account's starting mailbox
// (the wizard's initial_mailbox when it still exists, else the inbox —
// every account gets a warm window; instant switch and the unified merge
// both need it, FR-A4/A5/FR-I8), then either the active view's pipeline
// or the unified rebuild.
func (m *Model) applySnapshot(acct string, snap sync.Snapshot) (tea.Model, tea.Cmd) {
	m.snaps[acct] = snap

	// First load: open the starting mailbox (FR-B1 initial window).
	// ViewKey "" means no view is open yet — mailbox or search.
	if snap.ViewKey == "" && len(snap.Mailboxes) > 0 {
		if node := firstMailboxNode(m.opts.Accounts, acct, snap.Mailboxes); node != nil {
			if acct == m.activeID {
				m.sidebarKey = sidebarRowKey(acct, node.Mailbox.ID)
			}
			return m, m.openMailboxOn(acct, node.Mailbox.ID)
		}
		return m, nil
	}

	if m.unified {
		return m.applyUnified()
	}
	if acct != m.activeID {
		return m, nil // stored for the switcher, footer, and unified merge
	}
	return m.applyView(snap)
}

// indexOfMailbox is the sidebar row index of a mailbox id (-1 when
// absent).
func indexOfMailbox(nodes []sync.MailboxNode, id mail.ID) int {
	for i, node := range nodes {
		if node.Mailbox.ID == id {
			return i
		}
	}
	return -1
}

// firstMailboxNode picks the mailbox an account opens on its first
// snapshot: the wizard-chosen initial mailbox when it is still in the
// tree (FR-I8), else the inbox — a re-created mailbox orphans the id and
// must not strand the account on an empty view.
func firstMailboxNode(accounts []AccountOpt, acct string, nodes []sync.MailboxNode) *sync.MailboxNode {
	var want mail.ID
	for _, a := range accounts {
		if a.ID == acct {
			want = a.InitialMailbox
			break
		}
	}
	if want != "" {
		for i := range nodes {
			if nodes[i].Mailbox.ID == want {
				return &nodes[i]
			}
		}
	}
	for i := range nodes {
		if nodes[i].Mailbox.Role == mail.RoleInbox {
			return &nodes[i]
		}
	}
	return nil
}

// applyView runs the render pipeline over the active account's snapshot:
// selection reset, fresh-row fade, sidebar sync, lazy body load, and edge
// prefetch (FR-B2, FR-D3, FR-D4).
func (m *Model) applyView(snap sync.Snapshot) (tea.Model, tea.Cmd) {
	m.snap = snap
	m.err = ""

	var cmds []tea.Cmd

	// View switches (mailbox open, search open/close/re-issue) reset the
	// multi-select (FR-G3): the selection is a view over the open view's
	// rows.
	if snap.ViewKey != m.viewKey {
		m.sel = map[mail.ID]bool{}
	}
	m.viewKey = snap.ViewKey

	// Fresh-row fade (PLAN §4.1 case 3): one timer per highlight batch.
	if len(snap.Fresh) > 0 {
		if !m.freshArmed {
			m.freshArmed = true
			cmds = append(cmds, m.freshFade())
		}
	} else {
		m.freshArmed = false
	}

	// Keep the sidebar cursor on the open mailbox (FR-C5). Only a folder
	// that exists re-anchors: an empty ActiveMailbox (all-mailbox search)
	// or a vanished id leaves the cursor where the user put it.
	if snap.ActiveMailbox != "" && indexOfMailbox(snap.Mailboxes, snap.ActiveMailbox) >= 0 {
		m.sidebarKey = sidebarRowKey(m.activeID, snap.ActiveMailbox)
	}

	// Body for the cursor message (FR-D4 lazy load).
	if id := m.cursorID(); id != "" && id != m.vpBodyID && snap.Body == nil && !snap.BodyLoading {
		cmds = append(cmds, m.loadBody(id))
	}
	// Fresh body arrived: install and reset scroll (FR-E3).
	if snap.Body != nil && snap.Body.ID != m.vpBodyID {
		m.vp.SetContent(snap.Body.Text)
		m.vp.GotoTop()
		m.vpBodyID = snap.Body.ID
	}
	// Cursor changed under an already-cached body: show it immediately.
	if id := m.cursorID(); id != "" && id != m.vpBodyID && snap.Body != nil && snap.Body.ID == id {
		m.vp.SetContent(snap.Body.Text)
		m.vp.GotoTop()
		m.vpBodyID = id
	}

	m.resizeViewport()

	// Edge prefetch (FR-D3): the engine coalesces via outstanding-request
	// bookkeeping, so firing eagerly is safe.
	if snap.LoadForward || snap.LoadBackward {
		cmds = append(cmds, m.opOn(m.activeID, "prefetch", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
			if err := eng.Prefetch(ctx); err != nil {
				return sync.Snapshot{}, err
			}
			return eng.Snapshot(), nil
		}))
	}
	cmds = append(cmds, m.threadSizesCmds(snap.Rows)...)
	return m, tea.Batch(cmds...)
}

// threadSizesCmds asks each account with an unsized row in view for its
// thread member counts (FR-D1) — the list marks which rows expand, and
// that mark is fetched lazily, once per row set. Rows already sized and
// flat fuzzy-scan rows (ThreadSize -1) are skipped, so a warm view never
// issues a command at all.
func (m *Model) threadSizesCmds(rows []sync.Row) []tea.Cmd {
	var cmds []tea.Cmd
	seen := map[string]bool{}
	for _, r := range rows {
		if r.ThreadSize != 0 {
			continue
		}
		acct := r.Account
		if acct == "" {
			acct = m.activeID
		}
		if seen[acct] {
			continue
		}
		seen[acct] = true
		if _, ok := m.engineFor(acct); !ok {
			continue // credential-less accounts never load (FR-A3)
		}
		cmds = append(cmds, m.opOn(acct, "thread-sizes", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
			eng.RefreshThreadSizes(ctx)
			return eng.Snapshot(), nil
		}))
	}
	return cmds
}

// applyUnified rebuilds the merged view (FR-A5) from every account's
// latest snapshot and runs the render pipeline over it. The sidebar lists
// every account's tree (FR-C5); the cursor's body comes from the row's
// owning account.
func (m *Model) applyUnified() (tea.Model, tea.Cmd) {
	snap := m.mergeUnified()
	m.snap = snap
	m.err = ""

	var cmds []tea.Cmd

	if snap.ViewKey != m.viewKey {
		m.sel = map[mail.ID]bool{}
	}
	m.viewKey = snap.ViewKey

	if len(snap.Fresh) > 0 {
		if !m.freshArmed {
			m.freshArmed = true
			cmds = append(cmds, m.freshFade())
		}
	} else {
		m.freshArmed = false
	}

	// The sidebar follows the active account's open mailbox, same as the
	// single-account path (the folder column itself lists every account,
	// FR-C5 — the merge changes the list, not the tree).
	act := m.snaps[m.activeID]
	if act.ActiveMailbox != "" && indexOfMailbox(act.Mailboxes, act.ActiveMailbox) >= 0 {
		m.sidebarKey = sidebarRowKey(m.activeID, act.ActiveMailbox)
	}

	// Body of the cursor row loads from its owning account (FR-A5) and
	// renders under the account-qualified key. bodyReq tracks the
	// in-flight load: the owner's BodyLoading flag describes its own
	// cursor, not ours, so the app does not trust it here.
	if acct, row, ok := m.cursorRef(); ok {
		key := m.rowKey(acct, row.ID)
		owner := m.snaps[acct]
		switch {
		case key == m.vpBodyID:
			// already showing it
		case owner.Body != nil && owner.Body.ID == row.ID:
			m.vp.SetContent(owner.Body.Text)
			m.vp.GotoTop()
			m.vpBodyID = key
			m.bodyReq = ""
		case m.bodyReq != key:
			cmds = append(cmds, m.loadBodyOn(acct, row.ID))
		}
	}

	m.resizeViewport()

	// Edge prefetch fans out to every account: each engine coalesces its
	// own outstanding request and no-ops when its window has room, so the
	// merge keeps extending as the cursor nears either edge (FR-D3).
	if snap.LoadForward || snap.LoadBackward {
		for _, a := range m.accounts {
			acct := a.ID
			cmds = append(cmds, m.opOn(acct, "prefetch", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
				if err := eng.Prefetch(ctx); err != nil {
					return sync.Snapshot{}, err
				}
				return eng.Snapshot(), nil
			}))
		}
	}
	// Thread chevrons (FR-D1) fan out the same way, but only to the
	// accounts whose rows are still unsized in the merge.
	cmds = append(cmds, m.threadSizesCmds(snap.Rows)...)
	return m, tea.Batch(cmds...)
}

// mergeUnified interleaves every enrolled account's open window by
// receivedAt descending (FR-A5). Thread blocks (header + members) stay
// contiguous — they merge as one unit keyed by the header's date — and
// every row is stamped with its owning account. The cursor tracks its id
// across rebuilds (PLAN §4.1 invariant: ids, not indexes).
func (m *Model) mergeUnified() sync.Snapshot {
	type ublock struct {
		acct string
		rows []sync.Row
		recv time.Time
	}
	type queue struct {
		acct    string
		blocks  []ublock
		total   int
		hasTot  bool
		loadFwd bool
		loadBwd bool
	}

	queues := make([]queue, 0, len(m.accounts))
	for _, a := range m.accounts {
		s := m.snaps[a.ID]
		q := queue{acct: a.ID, total: s.Total, hasTot: s.Total >= 0, loadFwd: s.LoadForward, loadBwd: s.LoadBackward}
		for i := 0; i < len(s.Rows); {
			b := ublock{acct: a.ID, recv: s.Rows[i].Summary.ReceivedAt}
			row := s.Rows[i]
			row.Account = a.ID
			b.rows = append(b.rows, row)
			i++
			if row.ThreadHeader {
				for i < len(s.Rows) && s.Rows[i].ThreadMember {
					mrow := s.Rows[i]
					mrow.Account = a.ID
					b.rows = append(b.rows, mrow)
					i++
				}
			}
			q.blocks = append(q.blocks, b)
		}
		queues = append(queues, q)
	}

	var merged []sync.Row
	idx := make([]int, len(queues))
	for {
		best := -1
		var bestRecv time.Time
		for i, q := range queues {
			if idx[i] >= len(q.blocks) {
				continue
			}
			recv := q.blocks[idx[i]].recv
			// Strictly newer wins; ties keep account order (stable).
			if best < 0 || recv.After(bestRecv) {
				best, bestRecv = i, recv
			}
		}
		if best < 0 {
			break
		}
		merged = append(merged, queues[best].blocks[idx[best]].rows...)
		idx[best]++
	}

	out := sync.Snapshot{
		Version:       m.mergeVersion(),
		Rows:          merged,
		Cursor:        0,
		Total:         -1,
		ViewKey:       "u",
		ActiveMailbox: "",
	}
	total, any := 0, false
	out.LoadForward, out.LoadBackward = false, false
	for _, q := range queues {
		if q.hasTot {
			total += q.total
			any = true
		}
		out.LoadForward = out.LoadForward || q.loadFwd
		out.LoadBackward = out.LoadBackward || q.loadBwd
	}
	if any {
		out.Total = total
	}

	// The active account drives the footer's status and counts; its
	// mailbox tree rides along for them (the sidebar builds its own
	// per-account rows, FR-C5).
	if act, ok := m.snaps[m.activeID]; ok {
		out.Mailboxes = act.Mailboxes
		out.Status = act.Status
		out.NewAbove = act.NewAbove
	}

	var fresh []mail.ID
	var scan *sync.ScanProgress
	for _, a := range m.accounts {
		s := m.snaps[a.ID]
		fresh = append(fresh, s.Fresh...)
		if s.NewAbove {
			out.NewAbove = true
		}
		if s.SearchActive {
			out.SearchActive = true
			out.ViewKey = "u:" + s.ViewKey
		}
		if s.Scan != nil && s.Scan.Active {
			if scan == nil {
				scan = &sync.ScanProgress{}
			}
			scan.Active = true
			scan.Scanned += s.Scan.Scanned
			scan.Total += s.Scan.Total
		}
	}
	out.Fresh = fresh
	out.Scan = scan

	// Cursor by id, falling back to the previous index when the row is
	// gone (destroyed under the cursor, PLAN §4.1); a fresh unified view
	// starts at the top.
	found := false
	for i, r := range merged {
		if r.ID == m.uCursorID && (r.Account == m.cursorOwner || m.cursorOwner == "") {
			out.Cursor = i
			found = true
			break
		}
	}
	if !found {
		if m.uCursorID == "" {
			out.Cursor = 0
		} else {
			out.Cursor = min(max(m.snap.Cursor, 0), max(len(merged)-1, 0))
		}
	}
	if len(merged) > 0 {
		m.uCursorID = merged[out.Cursor].ID
		m.cursorOwner = merged[out.Cursor].Account
	}

	// The cursor row's body, when its owner already has it.
	if len(merged) > 0 {
		r := merged[out.Cursor]
		if owner, ok := m.snaps[r.Account]; ok && owner.Body != nil && owner.Body.ID == r.ID {
			out.Body = owner.Body
		}
		key := m.rowKey(r.Account, r.ID)
		out.BodyLoading = m.bodyReq == key
	}
	return out
}

// mergeVersion is a monotonic stamp for the synthetic unified snapshot —
// the engines' versions are per-account and incomparable here.
func (m *Model) mergeVersion() uint64 {
	var v uint64
	for _, a := range m.accounts {
		if s := m.snaps[a.ID]; s.Version > v {
			v = s.Version
		}
	}
	// Version 0 would stall against the zero-value previous snapshot; the
	// synthetic view has no version war of its own (application is
	// unconditional in the unified path).
	if v == 0 {
		return 1
	}
	return v
}

// cursorRef resolves the cursor row and the account that owns it: the
// row's own account in unified view, the active account otherwise.
func (m *Model) cursorRef() (string, sync.Row, bool) {
	if m.snap.Cursor < 0 || m.snap.Cursor >= len(m.snap.Rows) {
		return "", sync.Row{}, false
	}
	r := m.snap.Rows[m.snap.Cursor]
	acct := r.Account
	if acct == "" {
		acct = m.activeID
	}
	return acct, r, true
}

// rowKey qualifies a message id with its account in unified view — JMAP
// ids are unique per account only, so selection and body tracking must
// not collide across accounts (FR-A5). The ui layer applies the same
// rule (ui.State.RowKey).
func (m *Model) rowKey(acct string, id mail.ID) mail.ID {
	if m.unified && acct != "" {
		return mail.ID(acct + "\x00" + string(id))
	}
	return id
}

func (m *Model) cursorID() mail.ID {
	if _, r, ok := m.cursorRef(); ok {
		return r.ID
	}
	return ""
}

// cursorKey is the selection/body key of the cursor row (rowKey of the
// cursor's owner + id).
func (m *Model) cursorKey() mail.ID {
	if acct, r, ok := m.cursorRef(); ok {
		return m.rowKey(acct, r.ID)
	}
	return ""
}

// ownerAccount is the account every cursor action targets: the row's
// owner in unified view, the active account otherwise (FR-A5 — actions
// never cross accounts).
func (m *Model) ownerAccount() string {
	if acct, _, ok := m.cursorRef(); ok && m.unified && acct != "" {
		return acct
	}
	return m.activeID
}

// openMailbox opens a mailbox on the active account (FR-C1).
func (m *Model) openMailbox(id mail.ID) tea.Cmd { return m.openMailboxOn(m.activeID, id) }

// openMailboxOn opens a mailbox on one account; only the active account's
// open resets the preview body.
func (m *Model) openMailboxOn(acct string, id mail.ID) tea.Cmd {
	return m.opOn(acct, "open-mailbox", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
		if err := eng.OpenMailbox(ctx, id); err != nil {
			return sync.Snapshot{}, err
		}
		if acct == m.activeID {
			m.vpBodyID = ""
			m.vp.SetContent("")
		}
		return eng.Snapshot(), nil
	})
}

// loadBody loads the cursor message's body on the active account (FR-D4).
func (m *Model) loadBody(id mail.ID) tea.Cmd { return m.loadBodyOn(m.activeID, id) }

// loadBodyOn loads a message body from one account. In unified view the
// in-flight request is tracked by qualified key so the rebuild does not
// re-issue it every snapshot.
func (m *Model) loadBodyOn(acct string, id mail.ID) tea.Cmd {
	if m.unified {
		m.bodyReq = m.rowKey(acct, id)
	}
	return m.opOn(acct, "load-body", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
		if err := eng.LoadBody(ctx, id); err != nil {
			return sync.Snapshot{}, err
		}
		return eng.Snapshot(), nil
	})
}

// handleKey routes a keypress through the keymap for the focused pane.
func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Keystroke()

	if key == "ctrl+c" {
		if time.Since(m.lastCtrlC) < 2*time.Second {
			m.cancel()
			return m, tea.Quit
		}
		m.lastCtrlC = time.Now()
		m.err = "press ctrl-c again to exit"
		return m, nil
	}

	if m.helpOpen {
		if act, ok := m.opts.Keys.Match(ui.PaneAny, key); ok && (act == ui.ActHelp || act == ui.ActQuit) {
			m.helpOpen = false
			return m, nil
		}
		if key == "esc" {
			m.helpOpen = false
			return m, nil
		}
		return m, nil
	}

	// The switcher is a modal over everything but help (FR-A4, FR-I7).
	if m.switcher != nil {
		return m.switcherKey(msg)
	}

	// Modal overlays swallow keys while open (move/copy/archive picker,
	// attachment save).
	if m.picker != nil {
		cmd, _ := m.pickerKey(key)
		return m, cmd
	}
	if m.fp != nil {
		cmd, _ := m.filePickKey(msg)
		return m, cmd
	}
	// The contact form modal (FR-L2) outranks the composer and the
	// screen alike: it can open over either.
	if m.contactForm != nil {
		return m, m.contactFormKey(msg)
	}
	// The composer owns the keyboard while it is open (FR-H1); its own
	// overlays (attach picker, discard confirm) are handled inside.
	if m.compose != nil {
		return m, m.composeKey(msg)
	}
	// The search bar and the advanced modal over it own the keyboard
	// only while the bar is focused; once a search is confirmed, normal
	// pane keys apply (FR-F1).
	if m.search != nil && (m.search.editing || m.search.adv != nil) {
		return m, m.searchKey(msg)
	}
	// The contacts screen (FR-L1) owns the keyboard below every modal —
	// esc, motion and its action keys never reach the mail keymap.
	if m.contacts != nil {
		return m.contactsKey(msg)
	}

	focus := m.focus
	if act, ok := m.opts.Keys.Match(focus, key); ok {
		return m.runAction(act)
	}
	if act, ok := m.opts.Keys.Match(ui.PaneAny, key); ok {
		return m.runAction(act)
	}
	m.err = ""
	return m, nil
}

// runAction executes a resolved action. List cursor moves are local engine
// state (no Cmd); everything touching the network is scheduled.
func (m *Model) runAction(act ui.Action) (tea.Model, tea.Cmd) {
	switch act {
	case ui.ActQuit:
		m.cancel()
		return m, tea.Quit

	case ui.ActHelp:
		m.helpOpen = true
		return m, nil

	// --- accounts (M6, FR-A4/A5, FR-I7) ---
	case ui.ActAccountSwitch:
		m.openSwitcher()
		return m, nil
	case ui.ActAccountManage:
		// FR-I8: leave the TUI, run the wizard, come back — the command
		// layer owns the relaunch, so the engines shut down first.
		m.manageAccounts = true
		m.cancel()
		return m, tea.Quit
	case ui.ActUnified:
		return m.toggleUnified()
	case ui.ActContacts:
		return m.toggleContactsScreen()
	case ui.ActContactNew:
		return m.openContactNew()

	case ui.ActCyclePane:
		m.focus = m.nextPane(1)
		return m, nil
	case ui.ActCyclePaneRev:
		m.focus = m.nextPane(-1)
		return m, nil
	case ui.ActToggleSidebar:
		m.sidebarVisible = !m.sidebarVisible
		if !m.sidebarVisible && m.focus == ui.PaneSidebar {
			// Focus never rides a hidden pane: hand off to the list
			// (what sidebar.close used to do; it ships unbound now
			// that "[" is the sole show/hide key, FR-C6).
			m.focus = ui.PaneList
		}
		m.resizeViewport()
		return m, nil
	case ui.ActToggleLayout:
		m.toggleLayout()
		return m, nil

	// --- list pane ---
	case ui.ActListDown:
		return m.moveCursor(1)
	case ui.ActListUp:
		return m.moveCursor(-1)
	case ui.ActListHalfDown:
		return m.moveCursor(max(listPage(m)/2, 1))
	case ui.ActListHalfUp:
		return m.moveCursor(-max(listPage(m)/2, 1))
	case ui.ActListNextUnread:
		return m.jumpUnread(1)
	case ui.ActListPrevUnread:
		return m.jumpUnread(-1)
	case ui.ActSort:
		return m.openSortPicker()
	case ui.ActListTop:
		return m, m.jump(sync.JumpStart)
	case ui.ActListBottom:
		return m, m.jump(sync.JumpEnd)
	case ui.ActListPageDown:
		return m.moveCursor(listPage(m))
	case ui.ActListPageUp:
		return m.moveCursor(-listPage(m))
	case ui.ActToggleThread:
		// In the Drafts mailbox, Enter edits the draft instead of
		// expanding a thread (FR-C3 edit-aware mode).
		if _, ok := m.cursorDraft(); ok {
			_, cmd := m.openCompose(composeDraft)
			return m, cmd
		}
		owner := m.ownerAccount()
		// Target the row we rendered, not the engine cursor: the unified
		// cursor is app-side and no engine cursor ever moves there
		// (PLAN §4.3), so a cursor-relative toggle expands whatever the
		// engine last had selected — a different message entirely.
		target := m.cursorID()
		return m, m.opOn(owner, "toggle-thread", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
			if err := eng.ToggleThread(ctx, target); err != nil {
				return sync.Snapshot{}, err
			}
			return eng.Snapshot(), nil
		})
	case ui.ActToggleSize:
		m.showSize = !m.showSize
		return m, nil

	// --- triage (M3, FR-G1..G5) ---
	case ui.ActToggleRead:
		return m, m.keywordCmd(sync.TriageRead)
	case ui.ActToggleStar:
		return m, m.keywordCmd(sync.TriageStar)
	case ui.ActToggleSelect:
		if key := m.cursorKey(); key != "" {
			if m.sel == nil {
				m.sel = map[mail.ID]bool{}
			}
			if m.sel[key] {
				delete(m.sel, key)
			} else {
				m.sel[key] = true
			}
		}
		return m, nil
	case ui.ActMove:
		m.openPicker(pickerMove)
		return m, nil
	case ui.ActCopy:
		m.openPicker(pickerCopy)
		return m, nil
	case ui.ActArchive:
		return m.archiveAction()
	case ui.ActDelete:
		return m.deleteAction()
	case ui.ActUndo:
		return m.undoAction()
	case ui.ActSaveAttach:
		return m.openFilePicker()

	// --- search (M4, FR-F1..F3) ---
	case ui.ActSearch:
		// "/": open the bar, or re-focus it over existing results. The
		// bar owns the keyboard while it is focused, so this never runs
		// mid-typing — "/" stays typeable in a query (M7 docs gate: the
		// "/ /" chord is gone, ctrl+s is the fielded form's key).
		if m.search == nil {
			return m, m.openSearch()
		}
		return m, m.refocusSearch()
	case ui.ActSearchAdv:
		// ctrl+s always reaches the fielded form — opening the bar first
		// when the search view is closed (FR-F2).
		if m.search == nil {
			cmd := m.openSearch()
			m.openAdvSearch()
			return m, cmd
		}
		if m.search.adv == nil {
			m.openAdvSearch()
		}
		return m, nil
	case ui.ActSearchClear:
		// esc is the contextual back chain: close an
		// open search first (FR-F1), else leave full-screen view
		// (FR-E5), else drop a multi-select (FR-G3); with none of those
		// it is a no-op. Modals and the composer intercept esc before
		// the keymap ever gets here.
		switch {
		case m.search != nil:
			return m, m.closeSearch()
		case m.fullscreen:
			m.toggleFullscreen()
			return m, nil
		case len(m.sel) > 0:
			m.sel = map[mail.ID]bool{}
			return m, nil
		}
		return m, nil

	// --- compose (M5, FR-H1..H5) ---
	case ui.ActCompose:
		return m.openCompose(composeNew)
	case ui.ActReply:
		return m.openCompose(composeReply)
	case ui.ActReplyAll:
		return m.openCompose(composeReplyAll)
	case ui.ActForward:
		return m.openCompose(composeForward)

	// --- full-screen message view (FR-E5) ---
	case ui.ActFullscreen:
		m.toggleFullscreen()
		return m, nil

	// --- sidebar pane (FR-C1, FR-C5) ---
	case ui.ActSidebarDown:
		m.sidebarMove(1)
		return m, nil
	case ui.ActSidebarUp:
		m.sidebarMove(-1)
		return m, nil
	case ui.ActSidebarTop:
		m.sidebarSelect(0)
		return m, nil
	case ui.ActSidebarBottom:
		m.sidebarSelect(len(m.sidebarRows()) - 1)
		return m, nil
	case ui.ActSidebarMoveUp:
		m.moveAccountBlock(-1)
		return m, nil
	case ui.ActSidebarMoveDown:
		m.moveAccountBlock(1)
		return m, nil
	case ui.ActOpenMailbox:
		return m.openSidebarRow()
	case ui.ActSidebarCollapse:
		m.sidebarCollapse()
		return m, nil
	case ui.ActSidebarExpand:
		m.sidebarExpand()
		return m, nil
	case ui.ActSidebarClose:
		m.sidebarVisible = false
		m.focus = ui.PaneList
		m.resizeViewport()
		return m, nil

	// --- preview pane ---
	case ui.ActPreviewDown:
		m.vp.ScrollDown(1)
		return m, nil
	case ui.ActPreviewUp:
		m.vp.ScrollUp(1)
		return m, nil
	case ui.ActPreviewHalf:
		m.vp.HalfPageDown()
		return m, nil
	case ui.ActPreviewHalfUp:
		m.vp.HalfPageUp()
		return m, nil
	case ui.ActPreviewTop:
		m.vp.GotoTop()
		return m, nil
	case ui.ActPreviewBottom:
		m.vp.GotoBottom()
		return m, nil
	case ui.ActPreviewPageDown:
		m.vp.PageDown()
		return m, nil
	case ui.ActPreviewPageUp:
		m.vp.PageUp()
		return m, nil
	}
	m.err = ""
	return m, nil
}

// toggleLayout flips the pane layout between side-by-side and stacked
// (FR-I10) and remembers the choice in prefs.toml (FR-J1: the app writes
// only prefs, never config.toml). Session-only runs toggle without saving.
func (m *Model) toggleLayout() {
	m.stacked = !m.stacked
	if m.opts.Prefs != nil {
		if m.stacked {
			m.opts.Prefs.Layout = config.LayoutStacked
		} else {
			m.opts.Prefs.Layout = ""
		}
		if m.opts.PrefsPath != "" {
			if err := config.SavePrefs(m.opts.PrefsPath, m.opts.Prefs); err != nil {
				m.err = "layout not remembered: " + err.Error()
			}
		}
	}
	m.resizeViewport()
}

func listPage(m *Model) int {
	_, _, h := m.paneHeights()
	if h <= 2 {
		return 10
	}
	return h - 2
}

// nextPane cycles focus among panes that are actually visible at the
// current width (FR-I1 responsive rules). Fullscreen pins focus to the
// preview (FR-E5).
func (m *Model) nextPane(dir int) ui.Pane {
	if m.fullscreen {
		return ui.PanePreview
	}
	visible := []ui.Pane{}
	if m.sidebarVisible && m.width >= 60 {
		visible = append(visible, ui.PaneSidebar)
	}
	if m.width >= 60 {
		visible = append(visible, ui.PaneList, ui.PanePreview)
	} else {
		visible = append(visible, ui.PaneSidebar, ui.PaneList, ui.PanePreview)
	}
	for i, p := range visible {
		if p == m.focus {
			j := (i + dir + len(visible)) % len(visible)
			return visible[j]
		}
	}
	return visible[0]
}

func (m *Model) moveCursor(delta int) (tea.Model, tea.Cmd) {
	// Touching the list acknowledges fresh arrivals: drop the highlight.
	if len(m.snap.Fresh) > 0 {
		m.clearFresh()
	}
	if m.unified {
		// The unified cursor is app-side: rows interleave across
		// accounts, so no engine cursor moves (PLAN §4.3).
		rows := m.snap.Rows
		if len(rows) == 0 {
			return m, nil
		}
		idx := min(max(m.snap.Cursor+delta, 0), len(rows)-1)
		m.uCursorID = rows[idx].ID
		m.cursorOwner = rows[idx].Account
		return m.applyUnified()
	}
	m.snap = m.engine.MoveCursor(delta)
	return m.applySnapshot(m.activeID, m.snap)
}

func (m *Model) jump(t sync.JumpTarget) tea.Cmd {
	if m.unified {
		rows := m.snap.Rows
		if len(rows) == 0 {
			return nil
		}
		idx := 0
		if t == sync.JumpEnd {
			idx = len(rows) - 1
		}
		m.uCursorID = rows[idx].ID
		m.cursorOwner = rows[idx].Account
		_, cmd := m.applyUnified()
		return cmd
	}
	return m.opOn(m.activeID, "jump", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
		if err := eng.Jump(ctx, t); err != nil {
			return sync.Snapshot{}, err
		}
		return eng.Snapshot(), nil
	})
}

// resizeViewport sizes the preview viewport to the current layout.
func (m *Model) resizeViewport() {
	m.resizeComposer()
	l, ok := m.layout()
	if !ok {
		return
	}
	m.vp.SetWidth(max(l.PreviewW-1, 1))
	m.vp.SetHeight(max(l.BodyH, 1))
}

func (m *Model) layout() (ui.Layout, bool) {
	if m.width == 0 || m.height == 0 {
		return ui.Layout{}, false
	}
	return ui.ComputeLayout(m.width, m.height, m.uiState()), true
}

func (m *Model) paneHeights() (int, int, int) {
	l, _ := m.layout()
	return l.SidebarW, l.ListW, l.BodyH
}

// uiState assembles the pure render state.
func (m *Model) uiState() ui.State {
	st := ui.State{
		Theme:          m.opts.Theme,
		Version:        m.opts.Version,
		Snap:           m.snap,
		Focus:          m.focus,
		SidebarVisible: m.sidebarVisible,
		ShowSize:       m.showSize,
		Now:            time.Now(),
		Selected:       m.sel,
	}
	// The folder column is the flat multi-account list (FR-C5); the
	// cursor index derives from its key for this frame only.
	st.SidebarRows = m.sidebarRows()
	st.SidebarSel = sidebarIndexOf(st.SidebarRows, m.sidebarKey)
	if m.picker != nil {
		st.Picker = m.pickerView()
	}
	if m.fp != nil {
		st.FilePick = &ui.FilePickView{
			Title: "Save attachments to…",
			Path:  m.fp.fp.CurrentDirectory,
			Hint:  "j/k move · l open · h back · enter save · esc cancel",
			View:  m.fp.fp.View(),
		}
	}
	if m.search != nil {
		st.Search = m.searchView()
		if m.search.adv != nil {
			st.AdvSearch = m.advSearchView()
		}
	}
	if m.compose != nil {
		st.Compose = m.composeView()
	}
	if m.attachPick != nil && m.compose != nil {
		st.FilePick = &ui.FilePickView{
			Title: "Attach a file…",
			Path:  m.attachPick.fp.CurrentDirectory,
			Hint:  "j/k move · l open · h back · enter attach · esc cancel",
			View:  m.attachPick.fp.View(),
		}
	}
	st.Fullscreen = m.fullscreen
	st.Stacked = m.stacked
	if m.toast != nil {
		st.Toast = m.toast.text
		st.ToastHint = m.toast.hint
	}
	if m.helpOpen {
		st.HelpOpen = true
		st.HelpSec = m.opts.Keys.Help(m.focus)
	}
	// Multi-account chrome (M6): identities for the footer chip, the
	// switcher, and the unified preview's owner line (FR-A5). Single-account configs leave
	// Accounts empty-irrelevant (len 1 still renders the chip only when
	// more than one account exists — see ui.renderFooter).
	st.Accounts = m.accountViews()
	if len(m.accounts) > 0 {
		st.Account = m.accountName(m.activeID)
	}
	st.Unified = m.unified
	if len(m.accounts) > 0 {
		names := make(map[string]string, len(m.accounts))
		idx := make(map[string]int, len(m.accounts))
		for i, a := range m.accounts {
			names[a.ID] = a.Name
			idx[a.ID] = i
		}
		st.AccountNames = names
		st.AccountIndex = idx
	}
	if m.switcher != nil {
		st.AccountSwitch = &ui.SwitchView{Accounts: m.accountViews(), Sel: m.switcher.sel}
	}
	if m.contacts != nil {
		st.Contacts = m.contactsView()
	}
	if m.contactForm != nil {
		st.ContactForm = m.contactFormView()
	}
	return st
}

// View renders the frame. The view claims the alternate screen buffer
// (FR-K3): quitting restores the pre-launch screen exactly, instead of
// leaving the last frame on the scrollback under the shell prompt.
func (m *Model) View() tea.View {
	st := m.uiState()
	if l, ok := m.layout(); ok && l.PreviewW > 0 && m.vpBodyID == m.cursorKey() {
		st.VpView = m.vp.View()
	}
	if m.err != "" && st.Err == "" {
		st.Err = m.err
	}
	v := tea.NewView(ui.Render(m.width, m.height, st))
	v.AltScreen = true
	return v
}

// ManageRequested reports that the user opened the account wizard from
// the running TUI (ctrl+a, FR-I8): the command layer runs it and starts a
// fresh session with the result.
func (m *Model) ManageRequested() bool { return m.manageAccounts }

// Cancel exposes the root context cancel for clean shutdown from outside.
func (m *Model) Cancel() { m.cancel() }

// Ctx returns the root context, cancelled on quit — used to tie the tea
// program lifetime to in-flight engine work (FR-K1).
func (m *Model) Ctx() context.Context { return m.ctx }
