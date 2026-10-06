package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/app"
	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/keyring"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// runTUI is the daily-driver reader (M1). It runs sessions back-to-back:
// when one ends by asking for the account wizard (ctrl+a, FR-I8) the
// wizard runs in the gap and a fresh session starts with whatever it
// saved — cancelled, and the session simply comes back.
func runTUI(args []string) error {
	for {
		manage, cfgPath, err := runTUISession(args)
		if err != nil {
			return err
		}
		if !manage {
			return nil
		}
		if _, werr := runWizard(wizardOptions{Path: cfgPath, Launch: true}); werr != nil && !errors.Is(werr, errWizardCancelled) {
			return werr
		}
	}
}

// runTUISession is one run of the reader: resolve config + keyring,
// connect, run the Bubble Tea program. Panics are caught, the terminal is
// restored (bubbletea handles its own teardown), and a sanitised crash
// report is written for filing bugs (FR-K3). The first result reports a
// pending account-wizard request (ctrl+a); the second is the config path
// the wizard should use.
func runTUISession(args []string) (manage bool, cfgPath string, retErr error) {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path (default: $XDG_CONFIG_HOME/jmap-tui/config.toml)")
	accountID := fs.String("account", "", "account id from config (default: default_account, else the only account)")
	url := fs.String("url", "", "server base URL (overrides config)")
	user := fs.String("user", "", "username (overrides config)")
	passwordFile := fs.String("password-file", "", "chmod-600 file containing the app password (overrides config)")
	theme := fs.String("theme", "", "dark, light, or auto (overrides config)")
	logFile := fs.String("log-file", "", "write a redacted debug log here (off by default, FR-K2)")
	logLevel := fs.String("log-level", "debug", "log level when --log-file is set")
	timeout := fs.Duration("timeout", 30*time.Second, "network timeout")
	if err := fs.Parse(args); err != nil {
		return false, "", err
	}
	if fs.NArg() > 0 {
		return false, "", fmt.Errorf("tui: unexpected arguments: %v", fs.Args())
	}
	if *configPath == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return false, "", err
		}
		*configPath = p
	}

	logger, closeLog, err := setupLogger(*logFile, *logLevel)
	if err != nil {
		return false, "", err
	}
	if closeLog != nil {
		defer closeLog()
	}

	copts := connectOpts{
		configPath:   *configPath,
		accountID:    *accountID,
		url:          *url,
		user:         *user,
		passwordFile: *passwordFile,
		timeout:      *timeout,
		logger:       logger,
	}
	accounts, cfg, activeID, err := connectAccounts(copts)
	if err != nil {
		return false, "", err
	}

	keys, err := buildKeymap(cfg)
	if err != nil {
		return false, "", err
	}
	pal := resolvePalette(first(*theme, cfgTheme(cfg)))

	// App-managed preferences (FR-J1): prefs.toml next to the config file.
	prefsPath, err := config.DefaultPrefsPath()
	if err != nil {
		prefsPath = ""
	}
	prefs, err := config.LoadPrefs(prefsPath)
	if err != nil {
		return false, "", fmt.Errorf("prefs: %w", err)
	}

	// The app keeps config.toml's default_account on the account heading
	// the sidebar (FR-J1's one app-written key, issue #3); flags-only
	// mode has no config file, so it stays memory-only.
	appCfgPath, appDefault := "", ""
	if cfg != nil {
		appCfgPath, appDefault = *configPath, cfg.DefaultAccount
	}

	m := app.New(app.Options{
		Accounts:       accounts,
		Keys:           keys,
		Theme:          ui.NewTheme(pal),
		AccountID:      activeID,
		Prefs:          prefs,
		PrefsPath:      prefsPath,
		ConfigPath:     appCfgPath,
		DefaultAccount: appDefault,
		UndoDelay:      resolveUndoDelay(cfg),
		Version:        version,
	})
	program := tea.NewProgram(m, tea.WithContext(m.Ctx()))

	defer func() {
		if r := recover(); r != nil {
			path, werr := writeCrashReport(r, debug.Stack())
			if werr != nil {
				fmt.Fprintf(os.Stderr, "panic: %v (crash report write failed: %v)\n", r, werr)
				return
			}
			fmt.Fprintf(os.Stderr, "jmap-tui crashed. The terminal has been restored.\nCrash report: %s\n", path)
		}
	}()

	final, err := program.Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return false, "", fmt.Errorf("tui: %w", err)
	}
	if fm, ok := final.(*app.Model); ok && fm.ManageRequested() {
		return true, *configPath, nil
	}
	return false, *configPath, nil
}

// connectOpts carries the credential resolution inputs shared by the smoke
// and TUI commands.
type connectOpts struct {
	configPath   string
	accountID    string
	url          string
	user         string
	passwordFile string
	timeout      time.Duration
	logger       *slog.Logger
}

// cfgTheme reads the configured theme, tolerating the flags-only mode
// where no config file loaded (nil cfg).
func cfgTheme(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Theme
}

// connectAccounts resolves every configured account and pre-flights a
// Connect to each, in parallel (M6, FR-A1). One account's failure never
// blocks another: failures enroll with Connected=false and the Hub
// retries them with backoff while the rest keep running (failure
// isolation, PLAN §4.3). When *every* account fails there is nothing to
// render, so the full list exits as one actionable error (FR-A3).
//
// The returned id is the active account: --account, else default_account,
// else the first in config order.
func connectAccounts(opts connectOpts) ([]app.AccountOpt, *config.Config, string, error) {
	if opts.configPath == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return nil, nil, "", err
		}
		opts.configPath = p
	}

	cfg, err := config.Load(opts.configPath)
	adHoc := opts.url != "" || opts.user != ""
	if needsWizard(err) && !adHoc {
		// FR-I8 first run: no account configured yet, so the wizard runs
		// before the reader and the TUI opens with what it saved.
		fmt.Fprintln(os.Stderr, "No account configured — starting setup.")
		if _, werr := runWizard(wizardOptions{Path: opts.configPath, Launch: true}); werr != nil {
			if errors.Is(werr, errWizardCancelled) {
				return nil, nil, "", fmt.Errorf("%w (run `jmap-tui login` to add one)", errWizardCancelled)
			}
			return nil, nil, "", werr
		}
		cfg, err = config.Load(opts.configPath)
	}
	type target struct {
		id   string
		acct *config.Account
	}
	var targets []target
	var activeID string

	switch {
	case err == nil:
		// Every configured account: default_account first, then id order
		// (config maps have no order of their own).
		ids := make([]string, 0, len(cfg.Accounts))
		for id := range cfg.Accounts {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if cfg.DefaultAccount != "" {
			ordered := []string{cfg.DefaultAccount}
			for _, id := range ids {
				if id != cfg.DefaultAccount {
					ordered = append(ordered, id)
				}
			}
			ids = ordered
		}
		for _, id := range ids {
			targets = append(targets, target{id: id, acct: cfg.Accounts[id]})
		}
		activeID = opts.accountID
		if activeID == "" {
			if _, _, err := cfg.PrimaryAccount(); err != nil {
				return nil, nil, "", err
			}
			activeID = cfg.DefaultAccount
			if activeID == "" && len(ids) > 0 {
				activeID = ids[0]
			}
		}
		if _, ok := cfg.Account(activeID); !ok {
			return nil, nil, "", fmt.Errorf("account %q not found in %s", activeID, opts.configPath)
		}
	case errors.Is(err, os.ErrNotExist) && adHoc:
		// Flags-only mode; config is optional there.
		if opts.accountID == "" {
			opts.accountID = "default"
		}
		activeID = opts.accountID
		targets = append(targets, target{id: activeID})
	default:
		return nil, nil, "", fmt.Errorf("%w (or pass --url/--user for ad-hoc use)", err)
	}

	// Pre-flight every account concurrently; the flags override the
	// active account only (they are overrides for ad-hoc use, not a
	// blanket replacement for the whole config).
	type result struct {
		opt app.AccountOpt
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = result{opt: connectOne(opts, t.id, t.acct, t.id == activeID)}
		}()
	}
	wg.Wait()

	out := make([]app.AccountOpt, 0, len(results))
	var failures []string
	for _, r := range results {
		if r.opt.Err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.opt.ID, r.opt.Err))
			// Enroll anyway: the Hub keeps retrying (its error shows in
			// the status line) when at least one other account works.
		}
		out = append(out, r.opt)
	}
	if len(failures) == len(results) {
		return nil, nil, "", fmt.Errorf("connect: %s", strings.Join(failures, "; "))
	}
	return out, cfg, activeID, nil
}

// needsWizard reports whether a config load failure means "no account is
// set up yet" — the wizard's cue (FR-I8) — rather than a broken file the
// user must fix.
func needsWizard(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, config.ErrNoAccounts)
}

// connectOne resolves one account's credentials (flags win for the active
// account) and pre-flights Connect. A failure is reported through
// Connected=false + Err — never fatal on its own (M6 failure isolation).
func connectOne(opts connectOpts, id string, acct *config.Account, isActive bool) app.AccountOpt {
	out := app.AccountOpt{ID: id, Name: id}
	if acct != nil && acct.DisplayName != "" {
		out.Name = acct.DisplayName
	}
	if acct != nil {
		out.DefaultIdentity = acct.DefaultIdentity
		out.InitialMailbox = mail.ID(acct.InitialMailbox)
	}

	serverURL := accountURL(acct)
	username := accountUsername(acct)
	pwFile := accountPasswordFile(acct)
	if isActive {
		serverURL = first(opts.url, serverURL)
		username = first(opts.user, username)
		pwFile = first(opts.passwordFile, pwFile)
	}
	out.Username = username
	if serverURL == "" || username == "" {
		out.Err = errors.New("both --url and --user (or a configured account) are required")
		return out
	}
	// Config URLs are validated at Load; --url is not, so the merged
	// value gets the URL policy check (https, no userinfo, cleartext only
	// for loopback) before any credential is sent.
	if err := config.ValidateServerURL(serverURL); err != nil {
		out.Err = fmt.Errorf("server URL: %w", err)
		return out
	}

	secret, warnings, err := keyring.Password(id, pwFile, nil)
	if err != nil {
		out.Err = err
		return out
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}

	client := jmapclient.New(jmapclient.Options{
		ServerURL:  serverURL,
		SessionURL: accountSessionURL(acct),
		Username:   username,
		Password:   secret,
		Auth:       acct.Auth,
		Timeout:    opts.timeout,
		Logger:     opts.logger,
	})
	out.Provider = client
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		out.Err = fmt.Errorf("connect to %s: %w", serverURL, err)
		return out
	}
	out.Connected = true
	return out
}

// resolveUndoDelay reads [compose].undo_delay (FR-H5), defaulting to the
// 5s cancel window REQUIREMENTS specifies. A zero value means the
// submission leaves immediately with no client-side hold.
func resolveUndoDelay(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.Compose.UndoDelay == "" {
		return 5 * time.Second
	}
	d, err := time.ParseDuration(cfg.Compose.UndoDelay)
	if err != nil || d < 0 {
		return 5 * time.Second
	}
	return d
}

// buildKeymap applies config key remaps (FR-I3); unknown actions and
// conflicts are config errors.
func buildKeymap(cfg *config.Config) (*ui.KeyMap, error) {
	remaps := map[ui.Action]string{}
	for act, key := range cfg.Keys {
		remaps[ui.Action(act)] = key
	}
	km, err := ui.NewKeyMap(remaps)
	if err != nil {
		return nil, fmt.Errorf("keys: %w", err)
	}
	if err := km.Validate(); err != nil {
		return nil, fmt.Errorf("keys: %w", err)
	}
	return km, nil
}

// resolvePalette picks the theme: explicit setting wins, else terminal
// background detection (FR-I2).
func resolvePalette(setting string) ui.Palette {
	var dark bool
	switch setting {
	case "light":
		dark = false
	case "dark":
		dark = true
	default:
		dark = detectDarkBackground()
	}
	if dark {
		return ui.DarkTheme()
	}
	return ui.LightTheme()
}

// detectDarkBackground uses the conventional COLORFGBG/BG environment
// hints; the default is dark (most terminal setups, and safe for SSH).
func detectDarkBackground() bool {
	if v := os.Getenv("COLORFGBG"); v != "" {
		// format "fg;bg" or "fg;bg;mode"
		parts := strings.Split(v, ";")
		bg := parts[len(parts)-1]
		switch bg {
		case "0", "1", "2", "3", "4", "5", "6", "8":
			return true
		case "7", "9", "10", "11", "12", "13", "14", "15":
			return false
		}
	}
	if v := os.Getenv("BG"); v != "" {
		return !strings.Contains(v, "light")
	}
	return true
}

// setupLogger wires the opt-in debug log (FR-K2): file only when asked,
// levels honoured, records redacted by construction (jmapclient logs no
// credential material).
func setupLogger(path, level string) (*slog.Logger, func(), error) {
	if path == "" {
		return nil, nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, fmt.Errorf("log file: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("log file: %w", err)
	}
	// A pre-existing file keeps whatever mode it was created with —
	// tighten it so a world-writable debug log can't survive (mirrors
	// keyring.WritePasswordFile).
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("log file: %w", err)
	}
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		_ = f.Close()
		return nil, nil, fmt.Errorf("log-level %q is not one of: debug, info, warn, error", level)
	}
	logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: lvl}))
	return logger, func() { _ = f.Close() }, nil
}

// writeCrashReport dumps a panic report to the temp directory. The report
// contains the panic value and stack only — never message content or
// credentials (NFR-4, NFR-5). os.CreateTemp gives the file a random name
// with O_EXCL at mode 0600: a symlink planted at a predictable $TMPDIR
// name can never be opened, so the report can't clobber a victim file.
func writeCrashReport(v any, stack []byte) (string, error) {
	f, err := os.CreateTemp(os.TempDir(), "jmap-tui-crash-*.log")
	if err != nil {
		return "", err
	}
	path := f.Name()
	var b strings.Builder
	fmt.Fprintf(&b, "jmap-tui crash report — %s\n\npanic: %v\n\n", time.Now().Format(time.RFC3339), v)
	b.Write(stack)
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
