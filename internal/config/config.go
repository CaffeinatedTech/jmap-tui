// Package config loads and validates the jmap-tui TOML configuration
// (FR-J1). The user and the M7 wizard write the file; the running app
// rewrites exactly one key of it — default_account, kept on the account
// at the top of the sidebar order (FR-C5, issue #3) — and nothing else.
// Validation is strict so typos and smuggled secrets fail fast with
// actionable errors (FR-J3).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// ErrNoAccounts is the validation failure that means "this config has no
// [accounts.*] table at all" — the wizard's cue to offer first-run setup
// (FR-I8) instead of an error the user cannot act on.
var ErrNoAccounts = errors.New("no accounts defined; add an [accounts.<id>] table or run `jmap-tui login`")

// Account describes one server account.
//
// Every field carries omitempty so the wizard's SaveAccount writes only
// what was actually chosen (config.toml is user-facing output, FR-J1).
type Account struct {
	// DisplayName is a user-facing label shown in the switcher.
	DisplayName string `toml:"display_name,omitempty"`

	// URL is the server base URL; the session resource is discovered at
	// /.well-known/jmap unless SessionURL is set.
	URL string `toml:"url"`

	// SessionURL overrides discovery with an explicit session endpoint.
	SessionURL string `toml:"session_url,omitempty"`

	// Username is the principal used for authentication (FR-A2).
	Username string `toml:"username"`

	// Auth selects the Authorization scheme: "" (or "basic") sends the
	// secret as HTTP Basic; "bearer" sends it as `Authorization: Bearer`
	// with an API token — Fastmail's JMAP API accepts nothing else
	// (verified live 2026-09-27, FR-A2). The wizard sets it on a
	// successful probe; hand-editing is fine.
	Auth string `toml:"auth,omitempty"`

	// PasswordKeyring requests keyring-backed secrets; true when nil
	// (the default).
	PasswordKeyring *bool `toml:"password_keyring,omitempty"`

	// PasswordFile is an explicit opt-in escape hatch: a chmod-600 file
	// containing the app password (FR-J2). Its use is warned about at
	// password-resolution time.
	PasswordFile string `toml:"password_file,omitempty"`

	// DefaultIdentity optionally pins the composer's From for this
	// account (FR-A1): matched against the server's Identity/get results
	// by email, then by id. Empty means the account's first identity.
	DefaultIdentity string `toml:"default_identity,omitempty"`

	// InitialMailbox is the mailbox the app opens first for this account,
	// chosen in the wizard (FR-I8); empty (or an id no longer in the
	// tree) falls back to the inbox. Mailbox ids are server-side and
	// stable, so a re-created mailbox is the only thing that orphans it.
	InitialMailbox string `toml:"initial_mailbox,omitempty"`
}

// Config is the parsed configuration document.
type Config struct {
	// DefaultAccount is the account id to activate at startup; empty means
	// the sole configured account, or an error when several exist.
	DefaultAccount string `toml:"default_account,omitempty"`

	// Accounts maps a stable account id to its settings.
	Accounts map[string]*Account `toml:"accounts"`

	// Window bounds the rolling query window (FR-D3); zero values mean
	// defaults.
	Window Window `toml:"window,omitempty"`

	// Keys remaps actions to keystrokes: action id → key (FR-I3). Keys are
	// validated when the keymap is built.
	Keys map[string]string `toml:"keys,omitempty"`

	// Theme selects the palette: "dark", "light", or "auto" (default).
	Theme string `toml:"theme,omitempty"`

	// Compose holds composer settings (M5).
	Compose Compose `toml:"compose,omitempty"`
}

// Compose tunes the composer (FR-H5). The app never writes this section:
// config.toml stays user-owned (FR-J1).
type Compose struct {
	// UndoDelay is how long Send holds the submission before it reaches
	// the server — the client-side undo window FR-H5 describes. Go
	// duration syntax ("5s", "500ms"); empty or "0" submits immediately.
	// The server's own EmailSubmission undo window (reported back after
	// submit) is separate and server-controlled (PLAN §7).
	UndoDelay string `toml:"undo_delay,omitempty"`
}

// Window holds the rolling-window tuning knobs (FR-D3). Zero fields fall
// back to sync defaults.
type Window struct {
	Chunk    int `toml:"chunk,omitempty"`
	Cap      int `toml:"cap,omitempty"`
	Prefetch int `toml:"prefetch,omitempty"`
}

// DefaultPath returns the conventional config location:
// ${XDG_CONFIG_HOME:-~/.config}/jmap-tui/config.toml (FR-J1).
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "jmap-tui", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "jmap-tui", "config.toml"), nil
}

// Load reads and validates the config at path. A missing file is reported as
// os.ErrNotExist-wrapped error so callers can fall back to flags.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg := &Config{}
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := validate(cfg, md); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// Account returns the account with the given id.
func (c *Config) Account(id string) (*Account, bool) {
	a, ok := c.Accounts[id]
	return a, ok
}

// PrimaryAccount resolves which account to use at startup: DefaultAccount if
// set, otherwise the only account, otherwise an error listing the choices.
func (c *Config) PrimaryAccount() (string, *Account, error) {
	if c.DefaultAccount != "" {
		a, ok := c.Accounts[c.DefaultAccount]
		if !ok {
			return "", nil, fmt.Errorf("default_account %q does not match any [accounts.*] table", c.DefaultAccount)
		}
		return c.DefaultAccount, a, nil
	}
	if len(c.Accounts) == 1 {
		for id, a := range c.Accounts {
			return id, a, nil
		}
	}
	ids := make([]string, 0, len(c.Accounts))
	for id := range c.Accounts {
		ids = append(ids, id)
	}
	return "", nil, fmt.Errorf("no default_account set; configure one of: %v", ids)
}

func validate(cfg *Config, md toml.MetaData) error {
	// FR-J2: plaintext passwords in config are a config error, never a
	// fallback. Catch them even though the schema has no such field, before
	// reporting any other unknown key.
	undecoded := md.Undecoded()
	for _, key := range undecoded {
		if len(key) > 0 && key[len(key)-1] == "password" {
			return fmt.Errorf("%s: plaintext passwords are not allowed in config; use the OS keyring (default) or a password_file", key)
		}
	}
	if len(undecoded) > 0 {
		return fmt.Errorf("%s: unknown config key; check spelling against the schema", undecoded[0])
	}
	if cfg.DefaultAccount != "" {
		if _, ok := cfg.Accounts[cfg.DefaultAccount]; !ok {
			return fmt.Errorf("default_account %q does not match any [accounts.*] table", cfg.DefaultAccount)
		}
	}
	if len(cfg.Accounts) == 0 {
		return ErrNoAccounts
	}
	for id, a := range cfg.Accounts {
		if err := validateAccount(id, a); err != nil {
			return err
		}
	}
	if err := validateWindow(cfg.Window); err != nil {
		return err
	}
	switch cfg.Theme {
	case "", "dark", "light", "auto":
	default:
		return fmt.Errorf("theme %q is not one of: dark, light, auto", cfg.Theme)
	}
	if cfg.Compose.UndoDelay != "" {
		d, err := time.ParseDuration(cfg.Compose.UndoDelay)
		if err != nil || d < 0 {
			return fmt.Errorf("compose: undo_delay %q must be a non-negative duration such as \"5s\" or \"0s\"", cfg.Compose.UndoDelay)
		}
	}
	for act, key := range cfg.Keys {
		if act == "" {
			return fmt.Errorf("keys: empty action id")
		}
		if key == "" {
			return fmt.Errorf("keys: %q: empty key binding", act)
		}
	}
	return nil
}

func validateWindow(w Window) error {
	if w.Chunk < 0 || w.Cap < 0 || w.Prefetch < 0 {
		return fmt.Errorf("window: chunk, cap, and prefetch must be positive integers")
	}
	if w.Chunk > 0 && w.Cap > 0 && w.Cap < w.Chunk {
		return fmt.Errorf("window: cap (%d) must be >= chunk (%d)", w.Cap, w.Chunk)
	}
	return nil
}

func validateAccount(id string, a *Account) error {
	if a.URL == "" {
		return fmt.Errorf("[accounts.%s] url is required, e.g. url = %q", id, "https://mail.example.com")
	}
	if a.Username == "" {
		return fmt.Errorf("[accounts.%s] username is required, e.g. username = %q", id, "you@example.com")
	}
	if err := ValidateServerURL(a.URL); err != nil {
		return fmt.Errorf("[accounts.%s] url: %w", id, err)
	}
	if a.SessionURL != "" {
		if err := ValidateServerURL(a.SessionURL); err != nil {
			return fmt.Errorf("[accounts.%s] session_url: %w", id, err)
		}
	}
	switch a.Auth {
	case "", "basic", "bearer":
	default:
		return fmt.Errorf("[accounts.%s] auth = %q: must be \"basic\" (password) or \"bearer\" (API token)", id, a.Auth)
	}
	if a.PasswordFile == "" && a.PasswordKeyring != nil && !*a.PasswordKeyring {
		return fmt.Errorf("[accounts.%s] password_keyring = false but no password_file is configured; no password source remains", id)
	}
	return nil
}
