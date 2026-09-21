// Package config loads and validates the jmap-tui TOML configuration
// (FR-J1). The app never rewrites the file; only the user or the M7 wizard
// writes it. Validation is strict so typos and smuggled secrets fail fast
// with actionable errors (FR-J3).
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Account describes one server account.
type Account struct {
	// DisplayName is a user-facing label shown in the switcher.
	DisplayName string `toml:"display_name"`

	// URL is the server base URL; the session resource is discovered at
	// /.well-known/jmap unless SessionURL is set.
	URL string `toml:"url"`

	// SessionURL overrides discovery with an explicit session endpoint.
	SessionURL string `toml:"session_url"`

	// Username is the principal used for HTTP Basic auth (FR-A2).
	Username string `toml:"username"`

	// PasswordKeyring requests keyring-backed secrets; true when nil
	// (the default).
	PasswordKeyring *bool `toml:"password_keyring"`

	// PasswordFile is an explicit opt-in escape hatch: a chmod-600 file
	// containing the app password (FR-J2). Its use is warned about at
	// password-resolution time.
	PasswordFile string `toml:"password_file"`
}

// Config is the parsed configuration document.
type Config struct {
	// DefaultAccount is the account id to activate at startup; empty means
	// the sole configured account, or an error when several exist.
	DefaultAccount string `toml:"default_account"`

	// Accounts maps a stable account id to its settings.
	Accounts map[string]*Account `toml:"accounts"`
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
			return fmt.Errorf("%s: plaintext passwords are not allowed in config; use the OS keyring (default), password_file, or the JMAP_TUI_PASSWORD_<ACCOUNT> env var", key)
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
		return fmt.Errorf("no accounts defined; add an [accounts.<id>] table")
	}
	for id, a := range cfg.Accounts {
		if err := validateAccount(id, a); err != nil {
			return err
		}
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
	if a.PasswordFile == "" && a.PasswordKeyring != nil && !*a.PasswordKeyring {
		return fmt.Errorf("[accounts.%s] password_keyring = false but no password_file is configured; no password source remains", id)
	}
	return nil
}
