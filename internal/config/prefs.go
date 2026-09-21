package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Prefs is the app-managed preferences document (prefs.toml, next to
// config.toml). The app owns and writes only this file — the user-authored
// config.toml is never rewritten by the app (FR-J1). Choices the user makes
// in-app (remembered archive destination, per account; later: style
// settings) persist here.
type Prefs struct {
	// Accounts maps a config account id to its remembered choices.
	Accounts map[string]AccountPrefs `toml:"accounts"`
}

// AccountPrefs holds one account's remembered in-app choices.
type AccountPrefs struct {
	// ArchiveMailbox is the mailbox id used by archive (FR-G4) when the
	// server exposes no role-archive mailbox.
	ArchiveMailbox string `toml:"archive_mailbox"`
}

// DefaultPrefsPath returns the prefs file location: prefs.toml in the same
// directory as the config file (FR-J1).
func DefaultPrefsPath() (string, error) {
	cfg, err := DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfg), "prefs.toml"), nil
}

// LoadPrefs reads the prefs file. A missing file yields empty prefs —
// first-run is not an error.
func LoadPrefs(path string) (*Prefs, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Prefs{}, nil
		}
		return nil, fmt.Errorf("prefs: read %s: %w", path, err)
	}
	p := &Prefs{}
	if _, err := toml.Decode(string(data), p); err != nil {
		return nil, fmt.Errorf("prefs: parse %s: %w", path, err)
	}
	return p, nil
}

// SavePrefs writes the prefs document atomically (temp file + rename) so a
// crash mid-write never truncates it.
func SavePrefs(path string, p *Prefs) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("prefs: %w", err)
	}
	data, err := toml.Marshal(p)
	if err != nil {
		return fmt.Errorf("prefs: encode: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("prefs: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("prefs: rename: %w", err)
	}
	return nil
}

// ArchiveMailbox returns the remembered archive destination for the account
// (empty when none).
func (p *Prefs) ArchiveMailbox(accountID string) string {
	if p == nil || p.Accounts == nil {
		return ""
	}
	return p.Accounts[accountID].ArchiveMailbox
}

// SetArchiveMailbox remembers the archive destination for the account.
func (p *Prefs) SetArchiveMailbox(accountID, mailboxID string) {
	if p.Accounts == nil {
		p.Accounts = map[string]AccountPrefs{}
	}
	a := p.Accounts[accountID]
	a.ArchiveMailbox = mailboxID
	p.Accounts[accountID] = a
}
