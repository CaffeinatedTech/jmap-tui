// Package app wires the sync engine to the UI: a Bubble Tea model that
// routes keys, schedules engine operations as Cmds, and renders ui.State.
// Nothing in Update blocks: every network hop runs in a Cmd goroutine and
// lands as a message (NFR-1).
package app

import (
	"context"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// Options configure the app model.
type Options struct {
	Provider mail.Provider
	Keys     *ui.KeyMap
	Theme    ui.Theme // resolved dark/light at startup (FR-I2)

	// AccountID is the config account id in use; app-managed preferences
	// (prefs.toml, FR-J1) key off it. Empty disables preference writes.
	AccountID string

	// Prefs is the app-managed preference store (FR-J1) and PrefsPath
	// where it persists; either may be empty (session-only memory).
	Prefs     *config.Prefs
	PrefsPath string
}

// Model is the Bubble Tea model for the reader.
type Model struct {
	opts   Options
	engine *sync.Engine
	snap   sync.Snapshot

	width  int
	height int

	focus          ui.Pane
	sidebarVisible bool
	sidebarSel     int
	showSize       bool
	helpOpen       bool

	vp       viewport.Model
	vpBodyID mail.ID

	lastCtrlC     time.Time
	err           string
	freshArmed    bool
	activeMailbox mail.ID // last seen active mailbox (selection resets)

	// Triage state (M3): multi-select set, modal overlays, toast, and the
	// prepared (delayed) destroy (FR-G2..G5).
	sel            map[mail.ID]bool
	picker         *pickerState
	fp             *filepickState
	toast          *toastState
	pendingDestroy *pendingDestroy
	seq            int

	ctx    context.Context
	cancel context.CancelFunc
}

// New returns the root model.
func New(opts Options) *Model {
	ctx, cancel := context.WithCancel(context.Background())
	return &Model{
		opts:           opts,
		engine:         sync.NewEngine(opts.Provider, sync.Config{}),
		focus:          ui.PaneList,
		sidebarVisible: true,
		sidebarSel:     0,
		vp:             viewport.New(),
		ctx:            ctx,
		cancel:         cancel,
	}
}

// snapMsg carries a fresh engine snapshot. live marks snapshots delivered
// by the sync loop's broadcast (as opposed to an operation's return), which
// re-arm the waiter.
type snapMsg struct {
	snap sync.Snapshot
	live bool
}

// errMsg carries a failed engine operation.
type errMsg struct {
	op  string
	err error
}

// engineOp wraps a blocking engine call as a Cmd.
func (m *Model) engineOp(op string, f func(ctx context.Context) (sync.Snapshot, error)) tea.Cmd {
	return func() tea.Msg {
		snap, err := f(m.ctx)
		if err != nil {
			return errMsg{op: op, err: err}
		}
		return snapMsg{snap: snap}
	}
}

// waitUpdates consumes the engine's latest-wins broadcast; each delivery
// re-arms itself so exactly one waiter is outstanding at a time (FR-B2:
// live changes repaint with no user action).
func (m *Model) waitUpdates() tea.Cmd {
	return func() tea.Msg {
		snap, ok := <-m.engine.Updates()
		if !ok {
			return nil
		}
		return snapMsg{snap: snap, live: true}
	}
}

// freshFade is the timer that clears the new-mail highlight (the slide-in
// fades, PLAN §4.1 case 3).
func (m *Model) freshFade() tea.Cmd {
	return tea.Tick(sync.FreshTTL, func(time.Time) tea.Msg {
		snap := m.engine.ClearFresh()
		return snapMsg{snap: snap}
	})
}

// Init starts the sync loop, loads identities + the mailbox tree, and arms
// the live-update waiter; the first mailbox snapshot then opens the inbox.
func (m *Model) Init() tea.Cmd {
	m.engine.Start(m.ctx)
	return tea.Batch(m.waitUpdates(), m.loadAccountCmd())
}

// loadAccountCmd fetches identities and the mailbox tree (FR-B1).
func (m *Model) loadAccountCmd() tea.Cmd {
	return m.engineOp("load-account", func(ctx context.Context) (sync.Snapshot, error) {
		// Identity absence or failure never blocks reading (FR-A6):
		// compose needs identities in M5, the reader does not.
		_ = m.engine.LoadIdentities(ctx)
		if err := m.engine.LoadMailboxes(ctx); err != nil {
			return sync.Snapshot{}, err
		}
		return m.engine.Snapshot(), nil
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
		var cmd tea.Cmd
		if msg.snap.Version > m.snap.Version {
			_, cmd = m.applySnapshot(msg.snap)
		}
		if msg.live {
			return m, tea.Batch(cmd, m.waitUpdates())
		}
		return m, cmd

	case errMsg:
		m.err = truncateErr(msg.op, msg.err)
		return m, nil

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

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func truncateErr(op string, err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:117] + "…"
	}
	return op + ": " + s
}

// applySnapshot stores a new snapshot and schedules follow-up work:
// first-open of the inbox, prefetch at window edges, lazy body loads, and
// the fresh-row fade timer.
func (m *Model) applySnapshot(snap sync.Snapshot) (tea.Model, tea.Cmd) {
	m.snap = snap
	m.err = ""

	var cmds []tea.Cmd

	// A mailbox switch resets the multi-select (FR-G3): the selection is
	// a view over the open mailbox's rows.
	if m.sel != nil && snap.ActiveMailbox != "" && snap.ActiveMailbox != m.activeMailbox {
		m.sel = map[mail.ID]bool{}
	}
	m.activeMailbox = snap.ActiveMailbox

	// Fresh-row fade (PLAN §4.1 case 3): one timer per highlight batch.
	if len(snap.Fresh) > 0 {
		if !m.freshArmed {
			m.freshArmed = true
			cmds = append(cmds, m.freshFade())
		}
	} else {
		m.freshArmed = false
	}

	// First load: open the inbox (FR-B1 initial window).
	if snap.ActiveMailbox == "" && len(snap.Mailboxes) > 0 {
		for i, node := range snap.Mailboxes {
			if node.Mailbox.Role == mail.RoleInbox {
				m.sidebarSel = i
				cmds = append(cmds, m.openMailbox(node.Mailbox.ID))
				break
			}
		}
		return m, tea.Batch(cmds...)
	}

	// Keep the sidebar selection on the active mailbox.
	for i, node := range snap.Mailboxes {
		if node.Mailbox.ID == snap.ActiveMailbox {
			m.sidebarSel = i
			break
		}
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
		cmds = append(cmds, m.engineOp("prefetch", func(ctx context.Context) (sync.Snapshot, error) {
			if err := m.engine.Prefetch(ctx); err != nil {
				return sync.Snapshot{}, err
			}
			return m.engine.Snapshot(), nil
		}))
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) cursorID() mail.ID {
	if m.snap.Cursor < 0 || m.snap.Cursor >= len(m.snap.Rows) {
		return ""
	}
	return m.snap.Rows[m.snap.Cursor].ID
}

func (m *Model) openMailbox(id mail.ID) tea.Cmd {
	return m.engineOp("open-mailbox", func(ctx context.Context) (sync.Snapshot, error) {
		if err := m.engine.OpenMailbox(ctx, id); err != nil {
			return sync.Snapshot{}, err
		}
		m.vpBodyID = ""
		m.vp.SetContent("")
		return m.engine.Snapshot(), nil
	})
}

func (m *Model) loadBody(id mail.ID) tea.Cmd {
	return m.engineOp("load-body", func(ctx context.Context) (sync.Snapshot, error) {
		if err := m.engine.LoadBody(ctx, id); err != nil {
			return sync.Snapshot{}, err
		}
		return m.engine.Snapshot(), nil
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

	case ui.ActCyclePane:
		m.focus = m.nextPane(1)
		return m, nil
	case ui.ActCyclePaneRev:
		m.focus = m.nextPane(-1)
		return m, nil
	case ui.ActToggleSidebar:
		m.sidebarVisible = !m.sidebarVisible
		m.resizeViewport()
		return m, nil

	// --- list pane ---
	case ui.ActListDown:
		return m.moveCursor(1)
	case ui.ActListUp:
		return m.moveCursor(-1)
	case ui.ActListTop:
		return m, m.jump(sync.JumpStart)
	case ui.ActListBottom:
		return m, m.jump(sync.JumpEnd)
	case ui.ActListPageDown:
		return m.moveCursor(listPage(m))
	case ui.ActListPageUp:
		return m.moveCursor(-listPage(m))
	case ui.ActToggleThread:
		return m, m.engineOp("toggle-thread", func(ctx context.Context) (sync.Snapshot, error) {
			if err := m.engine.ToggleThread(ctx); err != nil {
				return sync.Snapshot{}, err
			}
			return m.engine.Snapshot(), nil
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
		if id := m.cursorID(); id != "" {
			if m.sel == nil {
				m.sel = map[mail.ID]bool{}
			}
			if m.sel[id] {
				delete(m.sel, id)
			} else {
				m.sel[id] = true
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

	// --- sidebar pane ---
	case ui.ActSidebarDown:
		if m.sidebarSel < len(m.snap.Mailboxes)-1 {
			m.sidebarSel++
		}
		return m, nil
	case ui.ActSidebarUp:
		if m.sidebarSel > 0 {
			m.sidebarSel--
		}
		return m, nil
	case ui.ActOpenMailbox:
		if m.sidebarSel < len(m.snap.Mailboxes) {
			id := m.snap.Mailboxes[m.sidebarSel].Mailbox.ID
			return m, m.openMailbox(id)
		}
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
	}
	m.err = ""
	return m, nil
}

func listPage(m *Model) int {
	_, _, h := m.paneHeights()
	if h <= 2 {
		return 10
	}
	return h - 2
}

// nextPane cycles focus among panes that are actually visible at the
// current width (FR-I1 responsive rules).
func (m *Model) nextPane(dir int) ui.Pane {
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
		m.engine.ClearFresh()
	}
	m.snap = m.engine.MoveCursor(delta)
	return m.applySnapshot(m.snap)
}

func (m *Model) jump(t sync.JumpTarget) tea.Cmd {
	return m.engineOp("jump", func(ctx context.Context) (sync.Snapshot, error) {
		if err := m.engine.Jump(ctx, t); err != nil {
			return sync.Snapshot{}, err
		}
		return m.engine.Snapshot(), nil
	})
}

// resizeViewport sizes the preview viewport to the current layout.
func (m *Model) resizeViewport() {
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
		Snap:           m.snap,
		Focus:          m.focus,
		SidebarVisible: m.sidebarVisible,
		ShowSize:       m.showSize,
		SidebarSel:     m.sidebarSel,
		Now:            time.Now(),
		Selected:       m.sel,
	}
	if m.picker != nil {
		st.Picker = m.pickerView()
	}
	if m.fp != nil {
		st.FilePick = &ui.FilePickView{
			Title: "Save attachments to…",
			Path:  m.fp.fp.CurrentDirectory,
			View:  m.fp.fp.View(),
		}
	}
	if m.toast != nil {
		st.Toast = m.toast.text
		st.ToastHint = m.toast.hint
	}
	if m.helpOpen {
		st.HelpOpen = true
		st.HelpSec = m.opts.Keys.Help(m.focus)
	}
	return st
}

// View renders the frame.
func (m *Model) View() tea.View {
	st := m.uiState()
	if l, ok := m.layout(); ok && l.PreviewW > 0 && m.vpBodyID == m.cursorID() {
		st.VpView = m.vp.View()
	}
	if m.err != "" && st.Err == "" {
		st.Err = m.err
	}
	return tea.NewView(ui.Render(m.width, m.height, st))
}

// Cancel exposes the root context cancel for clean shutdown from outside.
func (m *Model) Cancel() { m.cancel() }

// Ctx returns the root context, cancelled on quit — used to tie the tea
// program lifetime to in-flight engine work (FR-K1).
func (m *Model) Ctx() context.Context { return m.ctx }
