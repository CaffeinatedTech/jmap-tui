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
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/app"
	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/keyring"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// runTUI is the daily-driver reader (M1). It resolves config + keyring,
// connects, and runs the Bubble Tea program. Panics are caught, the
// terminal is restored (bubbletea handles its own teardown), and a
// sanitised crash report is written for filing bugs (FR-K3).
func runTUI(args []string) error {
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
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("tui: unexpected arguments: %v", fs.Args())
	}

	logger, closeLog, err := setupLogger(*logFile, *logLevel)
	if err != nil {
		return err
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
	provider, cfg, err := connectAccount(copts)
	if err != nil {
		return err
	}

	keys, err := buildKeymap(cfg)
	if err != nil {
		return err
	}
	pal := resolvePalette(first(*theme, cfg.Theme))

	// App-managed preferences (FR-J1): prefs.toml next to the config file.
	prefsPath, err := config.DefaultPrefsPath()
	if err != nil {
		prefsPath = ""
	}
	prefs, err := config.LoadPrefs(prefsPath)
	if err != nil {
		return fmt.Errorf("prefs: %w", err)
	}

	m := app.New(app.Options{
		Provider:  provider,
		Keys:      keys,
		Theme:     ui.NewTheme(pal),
		AccountID: copts.accountID,
		Prefs:     prefs,
		PrefsPath: prefsPath,
		UndoDelay: resolveUndoDelay(cfg),
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

	_, err = program.Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
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

// connectAccount resolves config + keyring and returns a connected provider
// with the config it resolved from.
func connectAccount(opts connectOpts) (*jmapclient.Client, *config.Config, error) {
	if opts.configPath == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return nil, nil, err
		}
		opts.configPath = p
	}

	var acct *config.Account
	cfg, err := config.Load(opts.configPath)
	switch {
	case err == nil:
		id := opts.accountID
		if id == "" {
			id, acct, err = cfg.PrimaryAccount()
			if err != nil {
				return nil, nil, err
			}
			opts.accountID = id
		} else {
			var ok bool
			if acct, ok = cfg.Account(id); !ok {
				return nil, nil, fmt.Errorf("account %q not found in %s", id, opts.configPath)
			}
		}
	case errors.Is(err, os.ErrNotExist) && (opts.url != "" || opts.user != ""):
		// Flags-only mode; config is optional there.
		if opts.accountID == "" {
			opts.accountID = "default"
		}
	default:
		return nil, nil, fmt.Errorf("%w (or pass --url/--user for ad-hoc use)", err)
	}

	serverURL := first(opts.url, accountURL(acct))
	username := first(opts.user, accountUsername(acct))
	if serverURL == "" || username == "" {
		return nil, nil, errors.New("both --url and --user (or a configured account) are required")
	}

	secret, warnings, err := keyring.Password(opts.accountID, first(opts.passwordFile, accountPasswordFile(acct)), nil)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}

	client := jmapclient.New(jmapclient.Options{
		ServerURL:  serverURL,
		SessionURL: accountSessionURL(acct),
		Username:   username,
		Password:   secret,
		Timeout:    opts.timeout,
		Logger:     opts.logger,
	})
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		return nil, nil, fmt.Errorf("connect to %s: %w", serverURL, err)
	}
	return client, cfg, nil
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
// credentials (NFR-4, NFR-5).
func writeCrashReport(v any, stack []byte) (string, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("jmap-tui-crash-%d.log", time.Now().Unix()))
	var b strings.Builder
	fmt.Fprintf(&b, "jmap-tui crash report — %s\n\npanic: %v\n\n", time.Now().Format(time.RFC3339), v)
	b.Write(stack)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
