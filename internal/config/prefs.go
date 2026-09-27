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
// in-app (remembered archive destination, per account; pane layout; later:
// style settings) persist here.
type Prefs struct {
	// Accounts maps a config account id to its remembered choices.
	Accounts map[string]AccountPrefs `toml:"accounts"`

	// Layout is the pane layout mode: "" or LayoutSide (default) shows the
	// list beside the preview; LayoutStacked shows the list above it.
	Layout string `toml:"layout,omitempty"`

	// AccountOrder is the user's display order for the sidebar's account
	// blocks and the account switcher (FR-C5): account ids, first listed
	// first. Ids unknown to the config are dropped at merge time;
	// configured accounts missing from the list follow in their base
	// order, so a newly added account never vanishes or reshuffles the
	// arrangement.
	AccountOrder []string `toml:"account_order,omitempty"`

	// CollapsedAccounts lists the account trees folded shut in the
	// sidebar (FR-C6): account ids. Entries for accounts unknown to the
	// config are dropped when the fold set is saved.
	CollapsedAccounts []string `toml:"collapsed_accounts,omitempty"`

	// CollapsedFolders maps an account id to the mailbox ids whose
	// subtrees are folded shut (FR-C6). Ids only — never names or any
	// other mail data (NFR-4); entries whose mailbox no longer exists
	// are pruned at save time.
	CollapsedFolders map[string][]string `toml:"collapsed_folders,omitempty"`
}

// Pane layout modes (FR-I10).
const (
	LayoutSide    = "side-by-side"
	LayoutStacked = "stacked"
)

// Stacked reports whether prefs ask for the top/bottom layout; unknown
// values fall back to the default side-by-side.
func (p *Prefs) Stacked() bool { return p != nil && p.Layout == LayoutStacked }

// AccountPrefs holds one account's remembered in-app choices.
type AccountPrefs struct {
	// ArchiveMailbox is the mailbox id used by archive (FR-G4) when the
	// server exposes no role-archive mailbox.
	ArchiveMailbox string `toml:"archive_mailbox"`

	// Sort is the list order id (FR-D8: "newest", "oldest", "sender",
	// "subject", "size"); empty means the default, newest first.
	Sort string `toml:"sort,omitempty"`
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

// SavePrefs writes the prefs document atomically (temp file + rename,
// writeAtomic's O_EXCL random-name discipline — finding F-11) so a crash
// mid-write never truncates it.
func SavePrefs(path string, p *Prefs) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("prefs: %w", err)
	}
	data, err := toml.Marshal(p)
	if err != nil {
		return fmt.Errorf("prefs: encode: %w", err)
	}
	if err := writeAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("prefs: %w", err)
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

// Sort returns the account's remembered list order id (FR-D8); empty is
// the default.
func (p *Prefs) Sort(accountID string) string {
	if p == nil || p.Accounts == nil {
		return ""
	}
	return p.Accounts[accountID].Sort
}

// SetSort remembers the list order for the account.
func (p *Prefs) SetSort(accountID, order string) {
	if p.Accounts == nil {
		p.Accounts = map[string]AccountPrefs{}
	}
	a := p.Accounts[accountID]
	a.Sort = order
	p.Accounts[accountID] = a
}

// SetAccountOrder remembers the account display order (FR-C5). The slice
// is copied: the model keeps mutating its own order, which must not alias
// the prefs document.
func (p *Prefs) SetAccountOrder(ids []string) {
	p.AccountOrder = append([]string(nil), ids...)
}

// MergeAccountOrder overlays the saved display order (FR-C5) on the base
// sequence — the enrollment order: default_account first, then id (config
// maps have no order of their own). Saved ids that no longer exist are
// dropped; base ids absent from the saved list keep their relative order
// after the listed ones.
func MergeAccountOrder(base, saved []string) []string {
	if len(saved) == 0 {
		return append([]string(nil), base...)
	}
	known := make(map[string]bool, len(base))
	for _, id := range base {
		known[id] = true
	}
	seen := make(map[string]bool, len(base))
	out := make([]string, 0, len(base))
	for _, id := range saved {
		if known[id] && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	for _, id := range base {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out
}
