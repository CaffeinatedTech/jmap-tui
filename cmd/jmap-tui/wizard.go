package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/keyring"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// wizStep is one wizard screen (FR-I8).
type wizStep int

const (
	wizForm     wizStep = iota
	wizAccounts         // pick an existing account to edit, or add a new one
	wizTest
	wizMailbox
	wizSave
	wizDone
)

// fetchFunc connects to the server and returns the mailbox picker rows.
// Tests inject a fake; production uses realFetch.
type fetchFunc func(ctx context.Context, serverURL, username, password string) ([]ui.PickerItem, error)

// storeFunc persists the secret and reports how it was stored (FR-I8).
// useFile selects the FR-J2 password-file fallback after the keyring is
// unavailable.
type storeFunc func(accountID, secret, filePath string, useFile bool) (string, error)

// resolveSecretFunc returns the secret an account already has — FR-J2
// resolution order (env var, password file, OS keyring) — so an edit with
// an empty password field can still test the connection and keep the
// current secret.
type resolveSecretFunc func(accountID, passwordFile string) (string, error)

// wizardOptions configures one wizard run.
type wizardOptions struct {
	// Path is the config file to write (FR-J1).
	Path string

	// Theme is the resolved palette setting for the wizard chrome.
	Theme string

	// Launch is what the summary screen promises: "start jmap-tui" when
	// the wizard is the first-run prelude, plain confirmation otherwise.
	Launch bool

	// Timeout bounds the connection test.
	Timeout time.Duration

	// Fetch/Store are the testable seams (nil → real network/keyring).
	Fetch fetchFunc
	Store storeFunc
}

// wizardResult is what a completed wizard wrote (FR-I8).
type wizardResult struct {
	AccountID  string
	Account    *config.Account
	Path       string
	SecretKind string
	Mailbox    string // chosen opening mailbox, "Inbox (default)" when none
}

// errWizardCancelled is the runWizard outcome when the user quit without
// saving anything.
var errWizardCancelled = errors.New("setup cancelled — no account was saved")

type wizTestedMsg struct {
	seq   int
	items []ui.PickerItem
	err   error
}

type wizSavedMsg struct {
	seq  int
	kind string
	err  error
}

// secretStoreError marks a save that failed before config was touched —
// the one failure the wizard can route around (password file, FR-J2).
type secretStoreError struct{ err error }

func (e *secretStoreError) Error() string { return "store secret: " + e.err.Error() }
func (e *secretStoreError) Unwrap() error { return e.err }

// wizardModel drives the full-screen setup flow: credentials → connection
// test → opening mailbox → save (FR-I8). Network and keyring work happens
// only inside tea.Cmds; Update mutates state and never blocks (NFR-1).
type wizardModel struct {
	opts  wizardOptions
	theme ui.Theme
	w, h  int

	step   wizStep
	inputs [4]textinput.Model
	focus  int
	spin   spinner.Model

	// seq stamps every async job; results from a superseded job are
	// dropped (same discipline as the window manager's request ids).
	seq int

	serverURL   string
	username    string
	password    string // as typed — the only value ever stored
	testSecret  string // typed password, or the JMAP_TUI_PASSWORD_* value
	accountID   string
	displayName string
	filePath    string
	useFile     bool

	items        []ui.PickerItem
	sel          int
	mailbox      mail.ID
	mailboxName  string
	status       string
	err          string
	testing      bool
	saving       bool
	secretFailed bool

	// editID is the account being modified ("" = adding); editAcct is its
	// config as loaded, so hand-written fields (session_url,
	// default_identity) survive the rewrite.
	editID   string
	editAcct *config.Account
	choices  []string // picker row → account id (row 0 is "add new")

	cfg        *config.Config
	pin        string // default_account to pin when none exists
	secretKind string
	res        *wizardResult
	cancelled  bool

	fetch   fetchFunc
	store   storeFunc
	resolve resolveSecretFunc
}

// newWizardModel builds the wizard against an existing (possibly empty)
// config: cfg may be nil when the file does not exist yet.
func newWizardModel(opts wizardOptions, cfg *config.Config) *wizardModel {
	if cfg == nil {
		cfg = &config.Config{}
	}
	m := &wizardModel{
		opts:    opts,
		theme:   ui.NewTheme(resolvePalette(opts.Theme)),
		spin:    spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		cfg:     cfg,
		fetch:   opts.Fetch,
		store:   opts.Store,
		resolve: realResolve,
	}
	if m.fetch == nil {
		m.fetch = realFetch(opts.Timeout)
	}
	if m.store == nil {
		m.store = realStore()
	}
	m.inputs = wizardInputs()
	return m
}

// wizardInputs builds the four form fields (FR-I8: server, username,
// secret, then the cosmetic account name).
func wizardInputs() [4]textinput.Model {
	mk := func(placeholder string) textinput.Model {
		in := textinput.New()
		in.Prompt = ""
		in.Placeholder = placeholder
		in.SetWidth(40)
		return in
	}
	var ins [4]textinput.Model
	ins[0] = mk("https://mail.example.com")
	ins[1] = mk("you@example.com")
	ins[2] = mk("required")
	ins[2].EchoMode = textinput.EchoPassword
	ins[2].EchoCharacter = '•'
	ins[3] = mk("defaults to your username")
	return ins
}

func (m *wizardModel) Init() tea.Cmd {
	if len(m.cfg.Accounts) > 0 {
		m.openPicker()
	} else {
		m.step = wizForm
	}
	return tea.Batch(m.inputs[0].Focus(), m.spin.Tick)
}

func (m *wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case spinner.TickMsg:
		if !m.testing && !m.saving {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case wizTestedMsg:
		if msg.seq != m.seq {
			return m, nil
		}
		m.testing = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err = ""
		m.items = msg.items
		m.step = wizMailbox
		m.sel = inboxIndex(msg.items)
		if m.editAcct != nil && m.editAcct.InitialMailbox != "" {
			for i, it := range msg.items {
				if string(it.ID) == m.editAcct.InitialMailbox {
					m.sel = i
					break
				}
			}
		}
		if len(msg.items) == 0 {
			m.status = "connected — the server reported no mailboxes; the inbox will be used"
		} else {
			m.status = fmt.Sprintf("connected — %d mailboxes", len(msg.items))
		}
		return m, nil

	case wizSavedMsg:
		if msg.seq != m.seq {
			return m, nil
		}
		m.saving = false
		if msg.err != nil {
			m.err = msg.err.Error()
			var ss *secretStoreError
			m.secretFailed = errors.As(msg.err, &ss)
			return m, nil
		}
		m.err = ""
		m.secretKind = msg.kind
		m.step = wizDone
		m.res = &wizardResult{
			AccountID:  m.accountID,
			Account:    m.buildAccount(),
			Path:       m.opts.Path,
			SecretKind: msg.kind,
			Mailbox:    m.mailboxName,
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey routes a keystroke to the active screen.
func (m *wizardModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Keystroke()
	if key == "ctrl+c" {
		// Setup screen: one press quits (the config write is atomic, so
		// an in-flight save either lands whole or not at all).
		m.cancelled = true
		return m, tea.Quit
	}
	switch m.step {
	case wizForm:
		return m.formKey(msg, key)
	case wizAccounts:
		return m.accountsKey(key)
	case wizTest:
		return m.testKey(key)
	case wizMailbox:
		return m.mailboxKey(key)
	case wizSave:
		return m.saveKey(key)
	default:
		if key == "enter" || key == "q" || key == "esc" {
			return m, tea.Quit
		}
		return m, nil
	}
}

func (m *wizardModel) formKey(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		if len(m.cfg.Accounts) > 0 {
			m.err = ""
			m.step = wizAccounts
			return m, nil
		}
		m.cancelled = true
		return m, tea.Quit
	case "tab":
		return m.focusField((m.focus + 1) % len(m.inputs))
	case "shift+tab":
		return m.focusField((m.focus + len(m.inputs) - 1) % len(m.inputs))
	case "enter":
		if m.focus < len(m.inputs)-1 {
			if err := m.validateField(m.focus); err != nil {
				m.err = err.Error()
				return m, nil
			}
			m.err = ""
			return m.focusField(m.focus + 1)
		}
		return m, m.startTest()
	}
	in := m.inputs[m.focus]
	in, cmd := in.Update(msg)
	m.inputs[m.focus] = in
	return m, cmd
}

func (m *wizardModel) testKey(key string) (tea.Model, tea.Cmd) {
	switch {
	case m.testing:
		if key == "esc" {
			m.seq++ // drop the in-flight result
			m.testing = false
			m.step = wizForm
			return m.focusField(m.focus)
		}
		return m, nil
	default:
		switch key {
		case "enter", "r":
			return m, m.startTest()
		case "esc":
			m.step = wizForm
			return m.focusField(m.focus)
		}
		return m, nil
	}
}

func (m *wizardModel) mailboxKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "j", "down":
		m.sel = min(m.sel+1, max(len(m.items)-1, 0))
		return m, nil
	case "k", "up":
		m.sel = max(m.sel-1, 0)
		return m, nil
	case "g":
		m.sel = 0
		return m, nil
	case "shift+g":
		m.sel = max(len(m.items)-1, 0)
		return m, nil
	case "enter":
		if m.sel >= 0 && m.sel < len(m.items) {
			m.mailbox = m.items[m.sel].ID
			m.mailboxName = m.items[m.sel].Label
		} else {
			m.mailboxName = "Inbox (default)"
		}
		return m, m.startSave()
	case "esc":
		m.step = wizForm
		return m.focusField(m.focus)
	}
	return m, nil
}

func (m *wizardModel) saveKey(key string) (tea.Model, tea.Cmd) {
	if m.saving {
		return m, nil // one save at a time; no keys until it lands
	}
	if m.secretFailed {
		// The keyring is unreachable (headless/SSH): route around it
		// with the FR-J2 password file instead of dead-ending (FR-J3).
		switch key {
		case "r":
			m.useFile = false
			return m, m.startSave()
		case "f":
			m.useFile = true
			return m, m.startSave()
		case "esc":
			m.cancelled = true
			return m, tea.Quit
		}
		return m, nil
	}
	switch key {
	case "enter", "r":
		return m, m.startSave()
	case "esc":
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

// accountsKey drives the picker: row 0 adds a new account, the rest edit
// an existing one (FR-I8).
func (m *wizardModel) accountsKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "j", "down":
		m.sel = min(m.sel+1, max(len(m.items)-1, 0))
		return m, nil
	case "k", "up":
		m.sel = max(m.sel-1, 0)
		return m, nil
	case "g":
		m.sel = 0
		return m, nil
	case "shift+g":
		m.sel = max(len(m.items)-1, 0)
		return m, nil
	case "enter":
		m.err = ""
		if m.sel == 0 || m.sel-1 >= len(m.choices) {
			m.beginAdd()
		} else {
			m.beginEdit(m.choices[m.sel-1])
		}
		m.step = wizForm
		return m.focusField(0)
	case "esc":
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

// openPicker shows the account chooser (only ever built when the config
// already has accounts).
func (m *wizardModel) openPicker() {
	m.step = wizAccounts
	m.items = []ui.PickerItem{{Label: "+ add a new account"}}
	m.choices = nil
	ids := make([]string, 0, len(m.cfg.Accounts))
	for id := range m.cfg.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := m.cfg.Accounts[id]
		if a == nil {
			continue
		}
		label := id
		if a.DisplayName != "" {
			label = a.DisplayName
		}
		if a.Username != "" {
			label += " — " + a.Username
		}
		m.items = append(m.items, ui.PickerItem{Label: label})
		m.choices = append(m.choices, id)
	}
	m.sel = 0
}

// beginAdd resets the form for a brand-new account.
func (m *wizardModel) beginAdd() {
	m.editID = ""
	m.editAcct = nil
	m.inputs = wizardInputs()
	m.focus = 0
}

// beginEdit prefills the form from an existing account. The password field
// is deliberately empty: the secret never round-trips through config, so
// leaving it blank keeps whatever the account already uses (FR-J2).
func (m *wizardModel) beginEdit(id string) {
	a, ok := m.cfg.Accounts[id]
	if !ok || a == nil {
		m.beginAdd()
		return
	}
	cp := *a
	m.editID = id
	m.editAcct = &cp
	m.inputs = wizardInputs()
	m.inputs[0].SetValue(a.URL)
	m.inputs[1].SetValue(a.Username)
	m.inputs[2].Placeholder = "leave empty to keep the current password"
	m.inputs[3].SetValue(a.DisplayName)
	m.focus = 0
}

// focusField moves keyboard focus between the form inputs.
func (m *wizardModel) focusField(i int) (tea.Model, tea.Cmd) {
	return m, m.refocus(i)
}

// refocus blurs the current input and focuses index i (also used when a
// validation failure jumps focus back to the offending field — without
// it the text cursor would not follow the washed row).
func (m *wizardModel) refocus(i int) tea.Cmd {
	m.inputs[m.focus].Blur()
	m.focus = i
	return m.inputs[i].Focus()
}

// validateField checks one field as the user tabs past it (FR-J3-shaped:
// precise, actionable errors).
func (m *wizardModel) validateField(i int) error {
	switch i {
	case 0:
		_, err := normalizeServerURL(m.inputs[0].Value())
		if err != nil {
			return err
		}
	case 1:
		if strings.TrimSpace(m.inputs[1].Value()) == "" {
			return errors.New("username is required")
		}
	}
	return nil
}

// startTest derives the account id, then connects and lists mailboxes.
func (m *wizardModel) startTest() tea.Cmd {
	serverURL, err := normalizeServerURL(m.inputs[0].Value())
	if err != nil {
		m.err = err.Error()
		m.step = wizForm
		return m.refocus(0)
	}
	m.err = ""
	m.serverURL = serverURL
	m.username = strings.TrimSpace(m.inputs[1].Value())
	m.password = m.inputs[2].Value() // typed — the only value ever stored
	if m.editID != "" {
		m.accountID = m.editID
	} else {
		m.accountID = accountIDFor(m.cfg, m.serverURL, m.username)
	}
	m.pin = defaultAccountPin(m.cfg, m.accountID)
	m.displayName = strings.TrimSpace(m.inputs[3].Value())
	if m.displayName == "" {
		if m.editAcct != nil && m.editAcct.DisplayName != "" {
			m.displayName = m.editAcct.DisplayName
		} else {
			// Default from the username, not the host slug: two accounts
			// on one server should read as alice/bob in the switcher.
			m.displayName = localPart(m.username)
		}
	}
	m.filePath = filepath.Join(filepath.Dir(m.opts.Path), m.accountID+".password")
	// The connection test must authenticate as the client will: a typed
	// password, else the secret the account already has (FR-J2: env var,
	// password file, keyring). An edit with an empty field therefore
	// tests with — and keeps — the current secret.
	m.testSecret = m.password
	if m.testSecret == "" {
		pwFile := ""
		if m.editAcct != nil {
			pwFile = m.editAcct.PasswordFile
		}
		if got, rerr := m.resolve(m.accountID, pwFile); rerr == nil {
			m.testSecret = got
		}
	}
	if m.testSecret == "" {
		m.err = fmt.Sprintf("password is required (or set %s)", keyring.EnvVar(m.accountID))
		m.step = wizForm
		return m.refocus(2)
	}
	m.step = wizTest
	m.testing = true
	m.err = ""
	m.seq++
	return tea.Batch(m.spin.Tick, m.fetchCmd())
}

func (m *wizardModel) fetchCmd() tea.Cmd {
	seq := m.seq
	fetch := m.fetch
	serverURL, user, pass := m.serverURL, m.username, m.testSecret
	timeout := m.opts.Timeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		items, err := fetch(ctx, serverURL, user, pass)
		return wizTestedMsg{seq: seq, items: items, err: err}
	}
}

// startSave stores the secret and writes the account in one command: a
// crash between the two leaves no orphaned keyring entry, and a failure
// reports exactly which half broke.
func (m *wizardModel) startSave() tea.Cmd {
	acct := m.buildAccount()
	m.step = wizSave
	m.saving = true
	m.err = ""
	m.secretFailed = false
	m.seq++
	seq := m.seq
	store := m.store
	accountID, secret, filePath, useFile := m.accountID, m.password, m.filePath, m.useFile
	path, pin := m.opts.Path, m.pin
	unchanged := existingSecretKind(accountID, m.editAcct, secret != "")
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		kind := fmt.Sprintf("environment (%s)", keyring.EnvVar(accountID))
		if secret != "" {
			k, err := store(accountID, secret, filePath, useFile)
			if err != nil {
				return wizSavedMsg{seq: seq, err: &secretStoreError{err}}
			}
			kind = k
		} else if unchanged != "" {
			kind = unchanged
		}
		if err := config.SaveAccount(path, accountID, acct, pin); err != nil {
			return wizSavedMsg{seq: seq, err: fmt.Errorf("write %s: %w", path, err)}
		}
		return wizSavedMsg{seq: seq, kind: kind}
	})
}

// buildAccount is the config table the wizard writes (FR-I8). On an edit
// it starts from the account as loaded, so fields the wizard does not ask
// about (session_url, default_identity) and the user's own comments-adjacent
// choices survive; only what the form collected is overwritten.
func (m *wizardModel) buildAccount() *config.Account {
	a := &config.Account{}
	if m.editAcct != nil {
		cp := *m.editAcct
		a = &cp
	}
	a.DisplayName = m.displayName
	a.URL = m.serverURL
	a.Username = m.username
	a.InitialMailbox = string(m.mailbox)
	switch {
	case m.useFile:
		off := false
		a.PasswordKeyring = &off
		a.PasswordFile = m.filePath
	case m.password != "":
		// Stored in the keyring: a leftover password_file must stop
		// shadowing it — resolution is file-first (FR-J2).
		a.PasswordKeyring = nil
		a.PasswordFile = ""
	default:
		// Empty password on an edit: the secret is untouched, so keep the
		// account's existing password source as-is.
	}
	return a
}

// View renders the active screen. The wizard claims the alternate screen
// buffer like the TUI (FR-K3): exiting restores the pre-launch screen.
func (m *wizardModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// render draws the active screen as a plain frame (tests assert on it).
func (m *wizardModel) render() string {
	picker := len(m.cfg.Accounts) > 0
	total := 4
	if picker {
		total = 5
	}
	// Stage numbers shift by one when the picker is in the flow.
	num := func(n int) int {
		if picker {
			return n + 1
		}
		return n
	}
	stage := func(n int, name string) string {
		return fmt.Sprintf("%d of %d · %s", num(n), total, name)
	}

	v := ui.WizardView{Err: m.err}
	switch {
	case m.step == wizAccounts:
		v.Title = "Accounts"
	case m.editID != "":
		v.Title = "Edit account · " + m.editID
	default:
		v.Title = "Add an account"
	}
	switch m.step {
	case wizAccounts:
		v.Step = fmt.Sprintf("1 of %d · account", total)
		v.Items = m.items
		v.Sel = m.sel
		v.Hint = "j/k choose · enter select · esc quit"
	case wizForm:
		v.Step = stage(1, "details")
		v.Fields = m.fieldViews()
		if picker {
			v.Hint = "tab next · enter continue · esc back"
		} else {
			v.Hint = "tab next · enter continue · esc quit"
		}
	case wizTest:
		v.Step = stage(2, "connection")
		v.Fields = m.fieldViews()
		if m.testing {
			v.Status = m.spin.View() + " testing connection…"
			v.Hint = "esc cancel"
		} else {
			v.Hint = "enter retry · esc edit details"
		}
	case wizMailbox:
		v.Step = stage(3, "opening mailbox")
		v.Items = m.items
		v.Sel = m.sel
		v.Status = m.status
		v.Hint = "j/k choose · enter continue · esc back"
	case wizSave:
		v.Step = stage(4, "save")
		v.Fields = m.fieldViews()
		switch {
		case m.saving:
			v.Status = m.spin.View() + " saving…"
			v.Hint = ""
		case m.secretFailed:
			v.Status = m.theme.Muted.Render("the OS keyring is unavailable — pick another home for the password")
			v.Hint = "r retry keyring · f use password file · esc quit"
		default:
			v.Hint = "enter retry · esc quit"
		}
	case wizDone:
		v.Lines = m.summaryLines()
		if m.opts.Launch {
			v.Hint = "enter continue — jmap-tui starts next"
		} else {
			v.Hint = "enter close"
		}
	}
	return ui.RenderWizard(m.w, m.h, m.theme, v)
}

// fieldViews renders the four inputs for the form/connection/save screens.
func (m *wizardModel) fieldViews() []ui.WizardField {
	labels := [4]string{"Server URL", "Username", "Password", "Account name"}
	out := make([]ui.WizardField, len(m.inputs))
	for i := range m.inputs {
		out[i] = ui.WizardField{
			Label:   labels[i],
			Value:   m.inputs[i].View(),
			Focused: m.step == wizForm && i == m.focus,
		}
	}
	return out
}

// summaryLines is the saved screen: what was written and where.
func (m *wizardModel) summaryLines() []string {
	lines := []string{
		"account\t" + m.accountID,
		"name\t" + m.displayName,
		"config\t" + m.opts.Path,
		"mailbox\t" + m.mailboxName,
		"secret\t" + m.secretKind,
	}
	return lines
}

// --- seams (production implementations) ---

// realFetch performs the connection test: session discovery + mailbox
// list (FR-I8).
func realFetch(timeout time.Duration) fetchFunc {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return func(ctx context.Context, serverURL, username, password string) ([]ui.PickerItem, error) {
		client := jmapclient.New(jmapclient.Options{
			ServerURL: serverURL,
			Username:  username,
			Password:  password,
			Timeout:   timeout,
		})
		if err := client.Connect(ctx); err != nil {
			return nil, fmt.Errorf("connect to %s: %w", serverURL, err)
		}
		list, err := client.Mailboxes(ctx)
		if err != nil {
			return nil, fmt.Errorf("list mailboxes: %w", err)
		}
		return mailboxPickerItems(list.Mailboxes), nil
	}
}

// realStore writes the secret to the OS keyring, or to the FR-J2
// password-file escape hatch after a keyring failure.
func realStore() storeFunc {
	return func(accountID, secret, filePath string, useFile bool) (string, error) {
		if useFile {
			if err := keyring.WritePasswordFile(filePath, secret); err != nil {
				return "", err
			}
			return "password file " + filePath, nil
		}
		if err := keyring.Set(accountID, secret, nil); err != nil {
			return "", err
		}
		return "OS keyring", nil
	}
}

// mailboxPickerItems flattens the server's mailbox list into picker rows
// with hierarchy depth (FR-I8's initial-mailbox choice walks the tree).
func mailboxPickerItems(mbs []mail.Mailbox) []ui.PickerItem {
	depth := map[mail.ID]int{}
	for _, mb := range mbs {
		depth[mb.ID] = 0
	}
	// Parent chains are short; the guard stops a malformed cycle from
	// spinning (unknown parents simply count as top-level).
	for _, mb := range mbs {
		d, cur, guard := 0, mb.ParentID, 0
		for cur != "" && guard < 32 {
			if _, ok := depth[cur]; !ok {
				break
			}
			d++
			parent := -1
			for i := range mbs {
				if mbs[i].ID == cur {
					parent = i
					break
				}
			}
			if parent < 0 {
				break
			}
			cur = mbs[parent].ParentID
			guard++
		}
		depth[mb.ID] = min(d, 6)
	}
	items := make([]ui.PickerItem, 0, len(mbs))
	for _, mb := range mbs {
		items = append(items, ui.PickerItem{ID: mb.ID, Label: mb.Name, Depth: depth[mb.ID]})
	}
	return items
}

// inboxIndex selects the inbox row in the picker (the default choice).
func inboxIndex(items []ui.PickerItem) int {
	for i, it := range items {
		if strings.EqualFold(it.Label, "inbox") {
			return i
		}
	}
	return 0
}

// --- helpers (pure, unit-tested) ---

// realResolve is the production secret resolver: FR-J2 order (env var,
// password file, OS keyring), same resolution the client itself uses.
func realResolve(accountID, passwordFile string) (string, error) {
	secret, _, err := keyring.Password(accountID, passwordFile, nil)
	return secret, err
}

// existingSecretKind summarises where an untouched secret lives for the
// saved screen. toStore=false means the password field was left empty.
func existingSecretKind(accountID string, acct *config.Account, toStore bool) string {
	if toStore {
		return ""
	}
	if os.Getenv(keyring.EnvVar(accountID)) != "" {
		return fmt.Sprintf("environment (%s)", keyring.EnvVar(accountID))
	}
	if acct != nil && acct.PasswordFile != "" {
		return "password file (unchanged)"
	}
	return "OS keyring (unchanged)"
}

// localPart is the display-name default: the address before "@" (alice),
// or the username as typed.
func localPart(username string) string {
	if i := strings.Index(username, "@"); i > 0 {
		return username[:i]
	}
	return username
}

// normalizeServerURL accepts what users type ("mail.example.com") and
// returns what the client needs (scheme + host, no trailing slash).
func normalizeServerURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("server URL is required, e.g. https://mail.example.com")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	s = strings.TrimRight(s, "/")
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("server URL %q is not a valid URL (e.g. https://mail.example.com)", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server URL must be http or https, got %q", u.Scheme)
	}
	return s, nil
}

var slugClean = regexp.MustCompile(`[^a-z0-9]+`)

// hostSlug turns a server host into a bare TOML key id
// (mail.example.com → mail-example-com).
func hostSlug(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil || u.Hostname() == "" {
		return "account"
	}
	slug := slugClean.ReplaceAllString(strings.ToLower(u.Hostname()), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	if slug == "" {
		return "account"
	}
	return slug
}

// accountIDFor picks the id for a (server, username) pair: a re-run of
// `jmap-tui login` for an account that is already configured reuses its id
// (so the secret and block are updated, not duplicated); anything else
// derives one from the host and de-duplicates with -2, -3, …
func accountIDFor(cfg *config.Config, serverURL, username string) string {
	if cfg != nil {
		for id, a := range cfg.Accounts {
			if a == nil {
				continue
			}
			if strings.EqualFold(strings.TrimRight(a.URL, "/"), serverURL) && a.Username == username {
				return id
			}
		}
	}
	slug := hostSlug(serverURL)
	id := slug
	for i := 2; ; i++ {
		if cfg == nil {
			return id
		}
		if _, taken := cfg.Accounts[id]; !taken {
			return id
		}
		id = fmt.Sprintf("%s-%d", slug, i)
	}
}

// defaultAccountPin decides the default_account key the save may pin: the
// new account on a fresh config, nothing when a default already exists,
// and the lexicographically-first existing account otherwise — adding an
// account must never change which one opens first.
func defaultAccountPin(cfg *config.Config, newID string) string {
	if cfg == nil || len(cfg.Accounts) == 0 {
		return newID
	}
	if cfg.DefaultAccount != "" {
		return ""
	}
	first := ""
	for id := range cfg.Accounts {
		if first == "" || id < first {
			first = id
		}
	}
	return first
}

// --- command entry points ---

// runWizard drives one full wizard run against opts.Path and returns what
// it saved.
func runWizard(opts wizardOptions) (*wizardResult, error) {
	cfg, err := config.Load(opts.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, config.ErrNoAccounts) {
		return nil, err
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	final, err := tea.NewProgram(newWizardModel(opts, cfg)).Run()
	if err != nil {
		return nil, fmt.Errorf("wizard: %w", err)
	}
	m, ok := final.(*wizardModel)
	if !ok {
		return nil, errors.New("wizard: unexpected program state")
	}
	if m.cancelled || m.res == nil {
		return nil, errWizardCancelled
	}
	return m.res, nil
}

// runLogin is the re-runnable account wizard: `jmap-tui login` adds (or
// re-saves) one account and reports what it wrote (FR-I8).
func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path (default: $XDG_CONFIG_HOME/jmap-tui/config.toml)")
	themeFlag := fs.String("theme", "", "dark, light, or auto (default)")
	timeout := fs.Duration("timeout", 30*time.Second, "network timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("login: unexpected arguments: %v", fs.Args())
	}
	path := *configPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return err
		}
		path = p
	}
	res, err := runWizard(wizardOptions{
		Path:    path,
		Theme:   *themeFlag,
		Timeout: *timeout,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Saved account %q to %s\n", res.AccountID, res.Path)
	fmt.Printf("  opening mailbox: %s\n", res.Mailbox)
	fmt.Printf("  secret: %s\n", res.SecretKind)
	fmt.Println("Run `jmap-tui` to start.")
	return nil
}
