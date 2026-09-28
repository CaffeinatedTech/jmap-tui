package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// --- key helpers (mirroring the app tests' message shapes) ---

func keyRune(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func keyEnter() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }
func keyEsc() tea.KeyPressMsg   { return tea.KeyPressMsg{Code: tea.KeyEsc} }
func keyTab(shift bool) tea.KeyPressMsg {
	m := tea.KeyPressMsg{Code: tea.KeyTab}
	if shift {
		m.Mod = tea.ModShift
	}
	return m
}

func keyCtrlU() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
}
func keyDown() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyDown} }
func keyUp() tea.KeyPressMsg   { return tea.KeyPressMsg{Code: tea.KeyUp} }

// send delivers a message and returns the follow-up command.
func send(m *wizardModel, msg tea.Msg) tea.Cmd {
	_, cmd := m.Update(msg)
	return cmd
}

// drain executes a command (flattening batches) so tests can feed the
// resulting messages back into the model synchronously.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, drain(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// deliver executes a command (flattening batches) and feeds every message
// it produces back into the model, following the commands Update hands
// back — discovery answers with the connection test, so one round is not
// enough. Spinner ticks are delivered but their re-arm is not run: the
// spinner re-arms forever, and a test that followed it would too.
func deliver(m *wizardModel, cmd tea.Cmd) {
	for _, msg := range drain(cmd) {
		next := send(m, msg)
		if _, isTick := msg.(spinner.TickMsg); isTick || next == nil {
			continue
		}
		deliver(m, next)
	}
}

func typeText(m *wizardModel, s string) {
	for _, r := range s {
		send(m, keyRune(r))
	}
}

// testFetch is the fake server: fixed rows (or error) for the connection
// test.
func testFetch(items []ui.PickerItem, err error) fetchFunc {
	return func(_ context.Context, _, _, _ string) ([]ui.PickerItem, string, error) {
		return items, "", err
	}
}

// testDiscover is the default discovery seam (FR-A7): the classic
// answer, so the add-mode flow — email → discover → test — runs without a
// DNS stack. Individual tests overwrite m.discover to steer it or count
// attempts.
func testDiscover(url, sessionURL string, err error) discoverFunc {
	return func(context.Context, string) (string, string, error) {
		return url, sessionURL, err
	}
}

func mailboxItems() []ui.PickerItem {
	return []ui.PickerItem{
		{ID: "mb-inbox", Label: "Inbox"},
		{ID: "mb-sent", Label: "Sent"},
		{ID: "mb-arch", Label: "Archive"},
	}
}

func newTestWizard(t *testing.T, cfg *config.Config, fetch fetchFunc, store storeFunc) (*wizardModel, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	m := newWizardModel(wizardOptions{
		Path:    path,
		Timeout: time.Second,
		Fetch:   fetch,
		Store:   store,
	}, cfg)
	m.discover = testDiscover("https://mail.example.com", "", nil)
	m.Init() // production start: picker when the config has accounts
	return m, path
}

// walkForm types the add-mode fields — email, password, name — pressing
// enter between them, and returns the command from the final enter: the
// one that asks for discovery (FR-A7).
func walkForm(m *wizardModel, email, pass, name string) tea.Cmd {
	typeText(m, email)
	send(m, keyEnter())
	typeText(m, pass)
	send(m, keyEnter())
	typeText(m, name)
	return send(m, keyEnter())
}

// walkFormURL is the manual-entry variant: ctrl+u reveals the Server URL
// field, which is filled first because it rides last in the tab order.
func walkFormURL(m *wizardModel, server, email, pass, name string) tea.Cmd {
	send(m, keyCtrlU())
	typeText(m, server)
	send(m, keyTab(false))
	typeText(m, email)
	send(m, keyTab(false))
	typeText(m, pass)
	send(m, keyTab(false))
	typeText(m, name)
	send(m, keyEnter()) // name → the revealed Server URL, the last field
	return send(m, keyEnter())
}

// runSave triggers the save step (enter on the mailbox screen, or a
// fallback key after a failure) and runs the resulting command.
func runSave(m *wizardModel, trigger tea.KeyPressMsg) {
	deliver(m, send(m, trigger))
}

func TestNormalizeServerURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://mail.example.com", "https://mail.example.com"},
		{"https://mail.example.com/", "https://mail.example.com"},
		{"mail.example.com", "https://mail.example.com"},
		{"  mail.example.com  ", "https://mail.example.com"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
	}
	for _, c := range cases {
		got, err := normalizeServerURL(c.in)
		if err != nil || got != c.want {
			t.Errorf("normalizeServerURL(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{
		"",
		"   ",
		"ftp://mail.example.com",
		"://",
		"http://mail.example.com",            // non-loopback cleartext
		"http://192.168.1.10:8080",           // cleartext LAN host
		"https://user:pass@mail.example.com", // userinfo
	} {
		if _, err := normalizeServerURL(bad); err == nil {
			t.Errorf("normalizeServerURL(%q) accepted; want an error", bad)
		}
	}
}

func TestHostSlugAndAccountID(t *testing.T) {
	if got := hostSlug("https://mail.example.com"); got != "mail-example-com" {
		t.Errorf("hostSlug = %q", got)
	}
	if got := hostSlug("https://Mail.Example.COM:8080"); got != "mail-example-com" {
		t.Errorf("hostSlug with port/case = %q", got)
	}
	if !config.ValidAccountID(hostSlug("https://mail.example.com")) {
		t.Error("derived id must be a bare TOML key")
	}

	cfg := &config.Config{Accounts: map[string]*config.Account{
		"work":             {URL: "https://mail.example.com", Username: "me@example.com"},
		"mail-example-com": {URL: "https://mail.example.com", Username: "someone-else@example.com"},
	}}
	// Same server + username on a re-run: reuse the id (update, not
	// duplicate).
	if got := accountIDFor(cfg, "https://mail.example.com", "me@example.com"); got != "work" {
		t.Errorf("re-run id = %q, want work", got)
	}
	// Same host, different user: the slug is taken, so de-duplicate.
	if got := accountIDFor(cfg, "https://mail.example.com", "other@example.com"); got != "mail-example-com-2" {
		t.Errorf("second account id = %q, want mail-example-com-2", got)
	}
	// Fresh config: plain slug.
	if got := accountIDFor(nil, "https://mail.example.com", "me@example.com"); got != "mail-example-com" {
		t.Errorf("fresh id = %q", got)
	}
}

func TestDefaultAccountPin(t *testing.T) {
	if got := defaultAccountPin(nil, "new"); got != "new" {
		t.Errorf("fresh config pin = %q, want new", got)
	}
	if got := defaultAccountPin(&config.Config{}, "new"); got != "new" {
		t.Errorf("empty config pin = %q, want new", got)
	}
	pinned := &config.Config{DefaultAccount: "b", Accounts: map[string]*config.Account{"a": {}, "b": {}}}
	if got := defaultAccountPin(pinned, "new"); got != "" {
		t.Errorf("existing default pin = %q, want empty (never overridden)", got)
	}
	unpinned := &config.Config{Accounts: map[string]*config.Account{"zeta": {}, "alpha": {}}}
	if got := defaultAccountPin(unpinned, "new"); got != "alpha" {
		t.Errorf("pin = %q, want alpha (the existing account keeps startup)", got)
	}
}

func TestWizardHappyPath(t *testing.T) {
	stored := ""
	m, path := newTestWizard(t, nil,
		testFetch(mailboxItems(), nil),
		func(id, secret, _ string, useFile bool) (string, error) {
			if id != "mail-example-com" || secret != "s3cret" || useFile {
				t.Errorf("store(%q, %q, file=%v)", id, secret, useFile)
			}
			stored = id
			return "OS keyring", nil
		})

	// An empty email refuses to advance (FR-J3): the Server URL field is
	// not on the form, so the address is the only way in (FR-A7).
	if cmd := send(m, keyEnter()); cmd != nil || !strings.Contains(m.err, "email is required") {
		t.Fatalf("empty email: err=%q cmd=%v", m.err, cmd)
	}

	cmd := walkForm(m, "me@example.com", "s3cret", "")
	if cmd == nil {
		t.Fatal("final enter produced no discovery command")
	}
	// No URL was typed: the final enter asks for discovery, which runs on
	// the connection screen before the test itself starts.
	if m.step != wizTest || !m.discovering {
		t.Fatalf("step = %v discovering=%v, want wizTest while discovering", m.step, m.discovering)
	}
	deliver(m, cmd)

	if m.step != wizMailbox {
		t.Fatalf("step = %v (err %q), want wizMailbox", m.step, m.err)
	}
	// The account id and display name derive from the discovered server
	// and the address, and the found URL has joined the form.
	if m.accountID != "mail-example-com" {
		t.Errorf("derived id = %q", m.accountID)
	}
	if m.displayName != "me" {
		t.Errorf("displayName = %q, want me (username local part)", m.displayName)
	}
	if !m.showURL || m.inputs[0].Value() != "https://mail.example.com" {
		t.Errorf("showURL = %v url = %q, want the found server on the form", m.showURL, m.inputs[0].Value())
	}
	if m.sel != 0 {
		t.Errorf("sel = %d, want 0 (inbox preselected)", m.sel)
	}
	send(m, keyDown())
	if m.sel != 1 {
		t.Errorf("sel after down = %d, want 1", m.sel)
	}
	send(m, keyUp())

	saveCmd := send(m, keyEnter())
	if m.step != wizSave || !m.saving {
		t.Fatalf("step = %v saving=%v, want wizSave", m.step, m.saving)
	}
	if cmd := send(m, keyEnter()); cmd != nil {
		t.Fatal("keys must be ignored mid-save")
	}
	deliver(m, saveCmd)

	if m.step != wizDone || m.res == nil {
		t.Fatalf("step = %v res=%+v err=%q, want wizDone", m.step, m.res, m.err)
	}
	if m.res.SecretKind != "OS keyring" || m.res.Mailbox != "Inbox" {
		t.Errorf("result = %+v", m.res)
	}
	if stored != "mail-example-com" {
		t.Errorf("stored for %q", stored)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultAccount != "mail-example-com" {
		t.Errorf("default_account = %q", cfg.DefaultAccount)
	}
	a, _ := cfg.Account("mail-example-com")
	if a == nil {
		t.Fatal("account not saved")
	}
	if a.URL != "https://mail.example.com" || a.Username != "me@example.com" || a.InitialMailbox != "mb-inbox" {
		t.Errorf("saved account = %+v", a)
	}
	if a.PasswordFile != "" {
		t.Errorf("password_file set on the keyring path: %q", a.PasswordFile)
	}
	if m.render() == "" {
		t.Error("done screen renders empty")
	}
}

// TestWizardSecretFallback covers the FR-J2 escape hatch: when the OS
// keyring is unreachable the wizard offers a password file instead of
// dead-ending (FR-J3-shaped actionable failure).
func TestWizardSecretFallback(t *testing.T) {
	var useFile bool
	m, path := newTestWizard(t, nil,
		testFetch(mailboxItems(), nil),
		func(_, _, _ string, file bool) (string, error) {
			useFile = file
			if !file {
				return "", errors.New("no secret service")
			}
			return "password file", nil
		})
	deliver(m, walkForm(m, "me@example.com", "s3cret", ""))
	if m.step != wizMailbox {
		t.Fatalf("step = %v err=%q", m.step, m.err)
	}
	runSave(m, keyEnter())

	if !m.secretFailed || m.step != wizSave {
		t.Fatalf("secretFailed=%v step=%v err=%q", m.secretFailed, m.step, m.err)
	}
	if useFile {
		t.Fatal("keyring was not attempted first")
	}
	runSave(m, keyRune('f'))
	if !useFile {
		t.Fatal("f did not switch to the password-file target")
	}

	if m.step != wizDone || m.res == nil {
		t.Fatalf("step = %v err = %q", m.step, m.err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, _ := cfg.Account("mail-example-com")
	if a == nil || a.PasswordFile == "" {
		t.Fatalf("password_file not written: %+v", a)
	}
	if want := filepath.Join(filepath.Dir(path), "mail-example-com.password"); a.PasswordFile != want {
		t.Errorf("password_file = %q, want %q", a.PasswordFile, want)
	}
	// The fake store writes nothing; realStore + keyring.WritePasswordFile
	// cover the 0600 file itself (TestWritePasswordFile).
}

func TestWizardPasswordFromEnv(t *testing.T) {
	var testedWith string
	m, path := newTestWizard(t, nil,
		func(_ context.Context, _, _, pass string) ([]ui.PickerItem, string, error) {
			testedWith = pass
			return mailboxItems(), "", nil
		},
		func(string, string, string, bool) (string, error) {
			t.Fatal("env-provided password must not be stored")
			return "", nil
		})
	t.Setenv("JMAP_TUI_PASSWORD_MAIL_EXAMPLE_COM", "from-env")
	deliver(m, walkForm(m, "me@example.com", "", ""))
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q (empty password should pass with the env var set)", m.step, m.err)
	}
	if testedWith != "from-env" {
		t.Errorf("connection test authenticated with %q, want the env secret", testedWith)
	}
	runSave(m, keyEnter())
	if m.step != wizDone {
		t.Fatalf("step = %v err = %q", m.step, m.err)
	}
	if !strings.Contains(m.res.SecretKind, "JMAP_TUI_PASSWORD_MAIL_EXAMPLE_COM") {
		t.Errorf("secret kind = %q, want the env var named", m.res.SecretKind)
	}
	if _, err := config.Load(path); err != nil {
		t.Errorf("config: %v", err)
	}
}

func TestWizardRejectsEmptyPassword(t *testing.T) {
	m, _ := newTestWizard(t, nil, testFetch(mailboxItems(), nil), nil)
	deliver(m, walkForm(m, "me@example.com", "", ""))
	if !strings.Contains(m.err, "password is required") {
		t.Fatalf("err = %q", m.err)
	}
	if m.step != wizForm {
		t.Errorf("step = %v, want wizForm (the test must not start)", m.step)
	}
	if m.focus != 2 {
		t.Errorf("focus = %d, want the password field", m.focus)
	}
}

func TestWizardCancelAndStaleResults(t *testing.T) {
	m, _ := newTestWizard(t, nil, testFetch(mailboxItems(), nil), nil)
	if cmd := send(m, keyEsc()); cmd == nil {
		t.Fatal("esc should quit")
	}
	if !m.cancelled {
		t.Fatal("esc did not cancel the run")
	}

	// A discovery superseded by esc-during-discovery must not land in a
	// later screen either: the seq stamp drops it like any stale result.
	m2, _ := newTestWizard(t, nil, testFetch(mailboxItems(), nil), nil)
	cmd := walkForm(m2, "me@example.com", "s3cret", "")
	m2.seq++ // what esc-during-discovery does: stamp the next job
	m2.discovering = false
	m2.testing = false
	m2.step = wizForm
	deliver(m2, cmd)
	if m2.step != wizForm {
		t.Fatalf("stale discovery result moved the wizard to %v", m2.step)
	}
}

func TestWizardValidationOnTab(t *testing.T) {
	m, _ := newTestWizard(t, nil, nil, nil)
	send(m, keyEnter()) // email still empty
	if !strings.Contains(m.err, "email is required") {
		t.Fatalf("err = %q", m.err)
	}
	typeText(m, "me@example.com")
	if cmd := send(m, keyTab(false)); cmd == nil {
		t.Error("tab produced no focus command")
	}
	if m.focus != 2 {
		t.Errorf("focus = %d, want 2 (password)", m.focus)
	}
	send(m, keyTab(true))
	if m.focus != 1 {
		t.Errorf("shift+tab focus = %d, want 1 (email)", m.focus)
	}
	if m.step != wizForm {
		t.Errorf("step = %v", m.step)
	}
}

func TestWizardConnectionError(t *testing.T) {
	m, _ := newTestWizard(t, nil, testFetch(nil, errors.New("connect to https://x: 401 unauthorized")), nil)
	deliver(m, walkForm(m, "me@example.com", "wrong", ""))
	if m.step != wizTest || m.testing {
		t.Fatalf("step = %v testing = %v", m.step, m.testing)
	}
	if !strings.Contains(m.err, "401") {
		t.Fatalf("err = %q", m.err)
	}
	if m.render() == "" {
		t.Error("error screen renders empty")
	}
	// Retry re-issues the test.
	m.fetch = testFetch(mailboxItems(), nil)
	cmd := send(m, keyEnter())
	if cmd == nil || !m.testing {
		t.Fatal("enter did not retry the connection test")
	}
	deliver(m, cmd)
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q", m.step, m.err)
	}
}

func TestMailboxPickerItemsDepth(t *testing.T) {
	mbs := []mail.Mailbox{
		{ID: "a", Name: "Inbox", Role: mail.RoleInbox},
		{ID: "b", Name: "Nested", ParentID: "a"},
		{ID: "c", Name: "Deep", ParentID: "b"},
		{ID: "orphan", Name: "Orphan", ParentID: "gone"},
	}
	items := mailboxPickerItems(mbs)
	if len(items) != 4 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].Depth != 0 || items[1].Depth != 1 || items[2].Depth != 2 || items[3].Depth != 0 {
		t.Errorf("depths = %d,%d,%d,%d want 0,1,2,0", items[0].Depth, items[1].Depth, items[2].Depth, items[3].Depth)
	}
	if inboxIndex(items) != 0 {
		t.Errorf("inboxIndex = %d", inboxIndex(items))
	}
}

// TestWizardRealFetchAgainstMockJMAP drives the production connection
// test (session discovery + mailbox list) against the in-process JMAP
// fake — the wizard's only network path (no TTY needed at this level).
func TestWizardRealFetchAgainstMockJMAP(t *testing.T) {
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 1},
		{ID: "mb-2026", Name: "2026", ParentID: "mb-archive", SortOrder: 2},
	})
	t.Cleanup(srv.Close)

	fetch := realFetch(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, _, err := fetch(ctx, srv.URL(), "tester@example.com", "correct-horse")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d (%+v), want 3", len(items), items)
	}
	if items[0].Label != "Inbox" || inboxIndex(items) != 0 {
		t.Errorf("inbox pick: %+v", items)
	}
	if items[2].Depth != 1 {
		t.Errorf("2026 depth = %d, want 1 (child of Archive)", items[2].Depth)
	}

	if _, _, err := fetch(ctx, srv.URL(), "tester@example.com", "wrong"); err == nil {
		t.Fatal("bad credentials accepted; want an error")
	}
}

// TestRealFetchProbesBearerRequirement is the FR-A2 probe: a server that
// refuses Basic the way Fastmail does (401 "not bearer") is reached by
// retrying Bearer, and realFetch reports the scheme that worked so the
// wizard persists auth = "bearer". The gate proxies to mockjmap with
// Basic credentials, so all JMAP logic stays mock-served.
func TestRealFetchProbesBearerRequirement(t *testing.T) {
	mock := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0},
	})
	t.Cleanup(mock.Close)

	const token = "tok-123"
	target, err := url.Parse(mock.URL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("tester@example.com:correct-horse"))
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+token {
			r.Header.Set("Authorization", basic)
			proxy.ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "Invalid Authorization header, not bearer")
	}))
	t.Cleanup(gate.Close)

	fetch := realFetch(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	items, used, err := fetch(ctx, gate.URL, "tester@example.com", token)
	if err != nil {
		t.Fatalf("fetch over bearer gate: %v", err)
	}
	if used != "bearer" {
		t.Errorf("scheme = %q, want bearer", used)
	}
	if len(items) == 0 {
		t.Error("no mailbox rows")
	}

	// A server that wants Bearer still fails on a wrong token, with the
	// original Basic-attempt error surfaced (the credential the user typed).
	if _, _, err := fetch(ctx, gate.URL, "tester@example.com", "wrong"); err == nil {
		t.Fatal("wrong token accepted; want an error")
	}
}

// TestWizardLiveStalwart is the M7 live gate for the wizard's only
// network path: URL normalisation, session discovery, and the mailbox
// list against the live Stalwart test account (read-only — AGENTS.md
// rules), then a full model save into a temp config. Skips without
// JMAP_TUI_TEST_* creds; never prints them.
func TestWizardLiveStalwartFetch(t *testing.T) {
	liveURL := os.Getenv("JMAP_TUI_TEST_URL")
	liveUser := os.Getenv("JMAP_TUI_TEST_USER")
	livePass := os.Getenv("JMAP_TUI_TEST_PASSWORD")
	if liveURL == "" || liveUser == "" || livePass == "" {
		t.Skip("live Stalwart creds not set (JMAP_TUI_TEST_URL / _USER / _PASSWORD)")
	}
	serverURL, err := normalizeServerURL(liveURL)
	if err != nil {
		t.Fatalf("normalizeServerURL(%q): %v", liveURL, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	items, _, err := realFetch(20*time.Second)(ctx, serverURL, liveUser, livePass)
	if err != nil {
		t.Fatalf("fetch against live server: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("live server reported no mailboxes")
	}
	if inboxIndex(items) < 0 || items[inboxIndex(items)].Label == "" {
		t.Fatalf("no inbox row among %d mailboxes", len(items))
	}

	// Full wizard flow over the real fetch: form → test → pick → save.
	m, path := newTestWizard(t, nil,
		realFetch(20*time.Second),
		func(string, string, string, bool) (string, error) {
			return "OS keyring", nil // the fake store: no real keyring write
		})
	// The unit-test default (1s) is too tight for a live server — and for
	// the bearer probe's extra round trip on Fastmail.
	m.opts.Timeout = 20 * time.Second
	deliver(m, walkFormURL(m, liveURL, liveUser, livePass, "live-gate"))
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q", m.step, m.err)
	}
	if len(m.items) != len(items) {
		t.Errorf("model items = %d, fetch returned %d", len(m.items), len(items))
	}
	runSave(m, keyEnter())
	if m.step != wizDone || m.res == nil {
		t.Fatalf("step = %v err = %q", m.step, m.err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config round trip: %v", err)
	}
	if len(cfg.Accounts) != 1 || cfg.DefaultAccount == "" {
		t.Errorf("saved config: default=%q accounts=%d", cfg.DefaultAccount, len(cfg.Accounts))
	}
}

// existingSecret is the fake FR-J2 resolver for edit flows.
func existingSecret(secret string, err error) resolveSecretFunc {
	return func(string, string) (string, error) { return secret, err }
}

// TestWizardPickerAddAndEdit covers the FR-I8 manage flow: with accounts
// configured the wizard opens on the picker; "+ add" resets the form;
// choosing an account prefills it, tests with the *existing* secret when
// the password field stays empty, keeps the current opening mailbox
// selected, and preserves config fields the wizard never asks about.
func TestWizardPickerAddAndEdit(t *testing.T) {
	cfg := &config.Config{
		DefaultAccount: "work",
		Accounts: map[string]*config.Account{
			"work": {
				DisplayName:     "Work",
				URL:             "https://mail.example.com",
				Username:        "me@work.example.com",
				SessionURL:      "https://mail.example.com/.well-known/jmap",
				DefaultIdentity: "me@work.example.com",
				InitialMailbox:  "mb-sent",
			},
			"home": {
				DisplayName: "Home",
				URL:         "https://home.example.com",
				Username:    "me@example.com",
			},
		},
	}
	var testedPass string
	m, path := newTestWizard(t, cfg,
		func(_ context.Context, _, _, pass string) ([]ui.PickerItem, string, error) {
			testedPass = pass
			return mailboxItems(), "", nil
		},
		func(string, string, string, bool) (string, error) {
			t.Fatal("empty password must not store a new secret")
			return "", nil
		})
	m.resolve = existingSecret("existing-secret", nil)

	if m.step != wizAccounts {
		t.Fatalf("step = %v, want the account picker (config has accounts)", m.step)
	}
	if got := m.render(); !strings.Contains(got, "Accounts") {
		t.Errorf("picker render missing title:\n%s", got)
	}

	// Row 0 is "+ add a new account" → blank form.
	send(m, keyEnter())
	if m.step != wizForm || m.editID != "" || m.inputs[0].Value() != "" {
		t.Fatalf("add: step=%v editID=%q url=%q", m.step, m.editID, m.inputs[0].Value())
	}
	// esc goes back to the picker (there are accounts to choose from).
	send(m, keyEsc())
	if m.step != wizAccounts {
		t.Fatalf("esc on the form = %v, want the picker", m.step)
	}

	// Rows sort by id: add, home, work → two downs selects "work".
	send(m, keyDown())
	send(m, keyDown())
	send(m, keyEnter())
	if m.step != wizForm || m.editID != "work" {
		t.Fatalf("edit: step=%v editID=%q", m.step, m.editID)
	}
	if m.inputs[0].Value() != "https://mail.example.com" ||
		m.inputs[1].Value() != "me@work.example.com" ||
		m.inputs[3].Value() != "Work" {
		t.Errorf("prefill: url=%q user=%q name=%q",
			m.inputs[0].Value(), m.inputs[1].Value(), m.inputs[3].Value())
	}
	if m.inputs[2].Value() != "" {
		t.Error("password field must start empty on an edit")
	}

	// Enter through the prefilled form → connection test with the
	// resolver's secret, not a fresh one.
	var cmd tea.Cmd
	for i := 0; i < 4; i++ {
		cmd = send(m, keyEnter())
	}
	if m.step != wizTest || !m.testing {
		t.Fatalf("step = %v testing=%v (err %q)", m.step, m.testing, m.err)
	}
	deliver(m, cmd)
	if testedPass != "existing-secret" {
		t.Errorf("test authenticated with %q, want the existing secret", testedPass)
	}
	if m.step != wizMailbox {
		t.Fatalf("step = %v err=%q", m.step, m.err)
	}
	if m.sel != 1 {
		t.Errorf("sel = %d, want 1 (the account's current mailbox, mb-sent)", m.sel)
	}

	deliver(m, send(m, keyEnter())) // confirm mailbox → save
	if m.step != wizDone || m.res == nil {
		t.Fatalf("step = %v err = %q", m.step, m.err)
	}
	if !strings.Contains(m.res.SecretKind, "unchanged") && !strings.Contains(m.res.SecretKind, "environment") {
		t.Errorf("secret kind = %q, want an unchanged/kept description", m.res.SecretKind)
	}

	cfg2, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, _ := cfg2.Account("work")
	if a == nil {
		t.Fatal("work not saved")
	}
	if a.DefaultIdentity != "me@work.example.com" || a.SessionURL != "https://mail.example.com/.well-known/jmap" {
		t.Errorf("hand-written fields lost: %+v", a)
	}
	if a.InitialMailbox != "mb-sent" || a.DisplayName != "Work" {
		t.Errorf("saved = %+v", a)
	}
	if a.PasswordFile != "" || a.PasswordKeyring != nil {
		t.Errorf("password source changed on an untouched secret: %+v", a)
	}
}

// TestWizardEditRotatesSecret: typing a password on an edit stores it and
// clears a stale password_file — resolution is file-first (FR-J2), so the
// leftover would shadow the new keyring secret forever.
func TestWizardEditRotatesSecret(t *testing.T) {
	cfg := &config.Config{Accounts: map[string]*config.Account{
		"work": {
			DisplayName:    "Work",
			URL:            "https://mail.example.com",
			Username:       "me@work.example.com",
			PasswordFile:   "/tmp/opencode/stale.password",
			InitialMailbox: "mb-inbox",
		},
	}}
	var stored bool
	m, path := newTestWizard(t, cfg,
		func(_ context.Context, _, _, pass string) ([]ui.PickerItem, string, error) {
			if pass != "new-secret" {
				t.Errorf("test secret = %q, want the typed one", pass)
			}
			return mailboxItems(), "", nil
		},
		func(id, secret, _ string, useFile bool) (string, error) {
			stored = true
			if id != "work" || secret != "new-secret" || useFile {
				t.Errorf("store(%q, %q, file=%v)", id, secret, useFile)
			}
			return "OS keyring", nil
		})
	m.resolve = existingSecret("old-secret", nil)

	send(m, keyDown()) // row 1 = work (only account)
	send(m, keyEnter())
	// Focus the password field and type.
	send(m, keyTab(false))
	send(m, keyTab(false))
	typeText(m, "new-secret")
	send(m, keyEnter()) // password → name
	cmd := send(m, keyEnter())
	if m.step != wizTest || !m.testing {
		t.Fatalf("step = %v testing=%v err = %q", m.step, m.testing, m.err)
	}
	deliver(m, cmd)
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q", m.step, m.err)
	}
	deliver(m, send(m, keyEnter()))
	if m.step != wizDone || m.res == nil {
		t.Fatalf("step = %v err = %q", m.step, m.err)
	}
	if !stored {
		t.Fatal("typed password was not stored")
	}
	cfg2, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, _ := cfg2.Account("work")
	if a == nil {
		t.Fatal("work not saved")
	}
	if a.PasswordFile != "" || a.PasswordKeyring != nil {
		t.Errorf("stale password_file survived: %+v", a)
	}
}

// --- discovery in the wizard (FR-A7) ---

// discoverCounter wraps a discovery fake with a call count and the last
// email it was handed.
type discoverCounter struct {
	calls int
	email string
	fn    discoverFunc
}

func (d *discoverCounter) seam() discoverFunc {
	return func(ctx context.Context, email string) (string, string, error) {
		d.calls++
		d.email = email
		return d.fn(ctx, email)
	}
}

// TestWizardDiscoveryFillsServerURL is the add-mode happy path: no
// Server URL row on the form, discovery on the final enter, and the found
// server on screen (and editable) afterwards.
func TestWizardDiscoveryFillsServerURL(t *testing.T) {
	m, _ := newTestWizard(t, nil, testFetch(mailboxItems(), nil), nil)
	disc := &discoverCounter{fn: testDiscover("https://mail.example.com", "", nil)}
	m.discover = disc.seam()

	frame := m.render()
	if strings.Contains(frame, "Server URL") {
		t.Fatalf("server URL row on the form before discovery:\n%s", frame)
	}
	if !strings.Contains(frame, "ctrl+u server URL") {
		t.Fatalf("hint does not name the reveal key:\n%s", frame)
	}

	deliver(m, walkForm(m, "me@example.com", "s3cret", "Work"))
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q, want wizMailbox", m.step, m.err)
	}
	if disc.calls != 1 || disc.email != "me@example.com" {
		t.Errorf("discovery calls = %d email = %q, want one attempt for the address", disc.calls, disc.email)
	}
	if m.inputs[0].Value() != "https://mail.example.com" || !m.showURL {
		t.Errorf("url = %q showURL = %v, want the found server on the form", m.inputs[0].Value(), m.showURL)
	}

	// Back to the form: the field stays, and it rides last so nothing the
	// user filled in moved.
	send(m, keyEsc())
	frame = m.render()
	if !strings.Contains(frame, "Server URL") {
		t.Fatalf("discovered server URL missing from the form:\n%s", frame)
	}
	if m.visible[len(m.visible)-1] != 0 {
		t.Errorf("visible = %v, want the URL slot appended", m.visible)
	}
}

// TestWizardDiscoveryFailureRevealsServerURL: nothing found → the error
// names both attempts, the Server URL field appears focused, and the next
// enter asks for a URL rather than re-running discovery.
func TestWizardDiscoveryFailureRevealsServerURL(t *testing.T) {
	m, _ := newTestWizard(t, nil, testFetch(mailboxItems(), nil), nil)
	disc := &discoverCounter{fn: testDiscover("", "", errors.New(
		"couldn't find a JMAP server for example.com: SRV _jmap._tcp.example.com: no such host; https://example.com: HTTP 404"))}
	m.discover = disc.seam()

	deliver(m, walkForm(m, "me@example.com", "s3cret", ""))
	if m.step != wizForm {
		t.Fatalf("step = %v (err %q), want the form back", m.step, m.err)
	}
	if m.focus != 0 {
		t.Errorf("focus = %d, want 0 (the revealed Server URL)", m.focus)
	}
	if !m.showURL || m.visible[len(m.visible)-1] != 0 {
		t.Errorf("showURL = %v visible = %v, want the URL revealed last", m.showURL, m.visible)
	}
	if !strings.Contains(m.err, "couldn't find a JMAP server for example.com") ||
		!strings.Contains(m.err, "enter the server URL") {
		t.Errorf("err = %q, want both attempts and the way out", m.err)
	}
	if frame := m.render(); !strings.Contains(frame, "Server URL") {
		t.Fatalf("revealed field missing from the frame:\n%s", frame)
	}

	// Enter on the empty revealed field validates it instead of
	// discovering again — the message already said to type a URL.
	if cmd := send(m, keyEnter()); cmd != nil || !strings.Contains(m.err, "server URL is required") {
		t.Fatalf("enter on the empty revealed field: err = %q cmd = %v", m.err, cmd)
	}
	if disc.calls != 1 {
		t.Errorf("discovery calls = %d, want 1", disc.calls)
	}

	// Typing a URL and pressing enter goes straight to the test.
	typeText(m, "https://mail.example.com")
	cmd := send(m, keyEnter())
	if disc.calls != 1 {
		t.Errorf("discovery ran again over a typed URL: calls = %d", disc.calls)
	}
	if m.step != wizTest || !m.testing {
		t.Fatalf("step = %v testing = %v (err %q), want the connection test", m.step, m.testing, m.err)
	}
	deliver(m, cmd)
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q, want wizMailbox", m.step, m.err)
	}
}

// TestWizardCtrlURevealsServerURL: the manual escape hatch. It appears
// while the field is hidden, disappears once it is not, and a URL typed
// there skips discovery entirely.
func TestWizardCtrlURevealsServerURL(t *testing.T) {
	m, _ := newTestWizard(t, nil, testFetch(mailboxItems(), nil), nil)
	disc := &discoverCounter{fn: testDiscover("https://mail.example.com", "", nil)}
	m.discover = disc.seam()

	if m.showURL {
		t.Fatal("add mode starts with the Server URL field shown")
	}
	send(m, keyCtrlU())
	if !m.showURL || m.focus != 0 {
		t.Fatalf("showURL = %v focus = %d, want the field revealed and focused", m.showURL, m.focus)
	}
	frame := m.render()
	if !strings.Contains(frame, "Server URL") {
		t.Fatalf("revealed field missing:\n%s", frame)
	}
	if strings.Contains(frame, "ctrl+u server URL") {
		t.Fatalf("hint still offers a key that has already done its job:\n%s", frame)
	}

	// Fill the revealed field and tab through the rest: a typed URL goes
	// straight to the test, discovery untouched.
	typeText(m, "https://mail.example.com")
	send(m, keyTab(false))
	typeText(m, "me@example.com")
	send(m, keyTab(false))
	typeText(m, "s3cret")
	send(m, keyEnter()) // password → account name
	send(m, keyEnter()) // account name → the revealed URL, the last field
	cmd := send(m, keyEnter())
	if disc.calls != 0 {
		t.Errorf("discovery ran although a URL was typed: calls = %d", disc.calls)
	}
	if m.step != wizTest || !m.testing {
		t.Fatalf("step = %v testing = %v (err %q), want the connection test", m.step, m.testing, m.err)
	}
	deliver(m, cmd)
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q, want wizMailbox", m.step, m.err)
	}
}

// TestWizardBareUsernameOffersTheURLField: a login that is not an email
// address cannot be discovered from, so the form says so and points at
// ctrl+u instead of guessing a server.
func TestWizardBareUsernameOffersTheURLField(t *testing.T) {
	m, _ := newTestWizard(t, nil, testFetch(mailboxItems(), nil), nil)
	disc := &discoverCounter{fn: testDiscover("https://mail.example.com", "", nil)}
	m.discover = disc.seam()

	deliver(m, walkForm(m, "server-login", "s3cret", ""))
	if m.step != wizForm {
		t.Fatalf("step = %v, want the form back", m.step)
	}
	if !strings.Contains(m.err, "ctrl+u") || !strings.Contains(m.err, "email address") {
		t.Errorf("err = %q, want the address error naming ctrl+u", m.err)
	}
	if m.focus != 1 {
		t.Errorf("focus = %d, want 1 (the address field)", m.focus)
	}
	if disc.calls != 0 {
		t.Errorf("discovery ran without an email domain: calls = %d", disc.calls)
	}
}

// TestWizardEditDiscoversOnlyWhenTheURLIsCleared pins the edit-mode rule
// (interviewed 2026-09-28): a configured account never rediscovers on its
// own, but clearing the field hands it back to discovery.
func TestWizardEditDiscoversOnlyWhenTheURLIsCleared(t *testing.T) {
	cfg := &config.Config{
		DefaultAccount: "work",
		Accounts: map[string]*config.Account{
			"work": {DisplayName: "Work", URL: "https://mail.example.com", Username: "me@work.example.com"},
		},
	}

	// Prefilled URL: four enters and no discovery anywhere.
	m, _ := newTestWizard(t, cfg, testFetch(mailboxItems(), nil), nil)
	m.resolve = existingSecret("existing-secret", nil)
	disc := &discoverCounter{fn: testDiscover("https://discovered.example.net", "", nil)}
	m.discover = disc.seam()
	send(m, keyDown()) // rows: add, work
	send(m, keyEnter())
	var cmd tea.Cmd
	for i := 0; i < 4; i++ {
		cmd = send(m, keyEnter())
	}
	if m.step != wizTest || !m.testing {
		t.Fatalf("step = %v testing = %v (err %q), want the connection test", m.step, m.testing, m.err)
	}
	deliver(m, cmd)
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q, want wizMailbox", m.step, m.err)
	}
	if disc.calls != 0 {
		t.Errorf("edit with a configured URL discovered: calls = %d", disc.calls)
	}

	// Cleared URL: the same walk ends in discovery.
	m2, _ := newTestWizard(t, cfg, testFetch(mailboxItems(), nil), nil)
	m2.resolve = existingSecret("existing-secret", nil)
	disc2 := &discoverCounter{fn: testDiscover("https://discovered.example.net", "", nil)}
	m2.discover = disc2.seam()
	send(m2, keyDown())
	send(m2, keyEnter())
	m2.inputs[0].SetValue("") // the user clears the server URL
	send(m2, keyTab(false))   // leave the empty field behind
	for i := 0; i < 3; i++ {
		cmd = send(m2, keyEnter())
	}
	if m2.step != wizTest || !m2.discovering {
		t.Fatalf("step = %v discovering = %v (err %q), want discovery to start", m2.step, m2.discovering, m2.err)
	}
	deliver(m2, cmd)
	if m2.step != wizMailbox {
		t.Fatalf("step = %v err = %q, want wizMailbox", m2.step, m2.err)
	}
	if disc2.calls != 1 {
		t.Errorf("discovery calls = %d, want 1", disc2.calls)
	}
	if m2.inputs[0].Value() != "https://discovered.example.net" {
		t.Errorf("url = %q, want the discovered server", m2.inputs[0].Value())
	}
}

// TestWizardDiscoveryWritesSessionURL: a session document answered from
// another origin is persisted as session_url, which is exactly what the
// client's trust anchor is built for (FR-A7).
func TestWizardDiscoveryWritesSessionURL(t *testing.T) {
	m, path := newTestWizard(t, nil, testFetch(mailboxItems(), nil),
		func(string, string, string, bool) (string, error) { return "OS keyring", nil })
	m.discover = testDiscover("https://mail.example.com", "https://session.example.com/jmap", nil)

	deliver(m, walkForm(m, "me@example.com", "s3cret", ""))
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q, want wizMailbox", m.step, m.err)
	}
	runSave(m, keyEnter())
	if m.step != wizDone {
		t.Fatalf("step = %v err = %q, want wizDone", m.step, m.err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, _ := cfg.Account("mail-example-com")
	if a == nil {
		t.Fatal("account not saved")
	}
	if a.URL != "https://mail.example.com" {
		t.Errorf("url = %q", a.URL)
	}
	if a.SessionURL != "https://session.example.com/jmap" {
		t.Errorf("session_url = %q, want the cross-origin endpoint discovery found", a.SessionURL)
	}
}

// TestWizardLiveDiscovery is the M10 live gate: the form exactly as a
// user fills it — email, password, name, no URL — discovers the server
// (SRV or the domain probe), authenticates, and saves. Read-only against
// the live account apart from a temp config; the fake store keeps the
// keyring untouched (AGENTS.md rules).
func TestWizardLiveDiscovery(t *testing.T) {
	liveUser := os.Getenv("JMAP_TUI_TEST_USER")
	livePass := os.Getenv("JMAP_TUI_TEST_PASSWORD")
	if liveUser == "" || livePass == "" {
		t.Skip("live Stalwart creds not set (JMAP_TUI_TEST_USER / _PASSWORD)")
	}
	m, path := newTestWizard(t, nil, realFetch(20*time.Second),
		func(string, string, string, bool) (string, error) { return "OS keyring", nil })
	// The unit-test default (1s) is far too tight for DNS plus a probe.
	m.opts.Timeout = 20 * time.Second
	m.discover = realDiscover(20 * time.Second)

	deliver(m, walkForm(m, liveUser, livePass, "live-discovery"))
	if m.step != wizMailbox {
		t.Fatalf("step = %v err = %q, want the mailbox picker", m.step, m.err)
	}
	if m.serverURL == "" || !m.showURL || m.inputs[0].Value() == "" {
		t.Fatalf("serverURL = %q showURL = %v field = %q: discovery never filled the form",
			m.serverURL, m.showURL, m.inputs[0].Value())
	}
	t.Logf("discovered %s (session_url %q)", m.serverURL, m.sessionURL)

	runSave(m, keyEnter())
	if m.step != wizDone || m.res == nil {
		t.Fatalf("step = %v err = %q, want wizDone", m.step, m.err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config round trip: %v", err)
	}
	a, _ := cfg.Account(m.accountID)
	if a == nil || a.URL != m.serverURL {
		t.Fatalf("saved account = %+v, want the discovered url %q", a, m.serverURL)
	}
	if a.Username != liveUser {
		t.Errorf("username = %q, want %q", a.Username, liveUser)
	}
}
