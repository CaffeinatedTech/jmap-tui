package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// bareKey matches a TOML bare key: the only shape the wizard ever emits as
// an account id, so block headers can be matched without quoting logic.
var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidAccountID reports whether id can be written as a bare TOML key
// ([accounts.<id>]). The wizard derives and sanitises ids against this.
func ValidAccountID(id string) bool { return bareKey.MatchString(id) }

// SaveAccount adds or replaces the [accounts.<id>] table at path. The rest
// of the file is preserved byte-for-byte — comments, key order, other
// tables — because config.toml is user-owned and the wizard is its only
// sanctioned writer (FR-J1, FR-I8); re-running `jmap-tui login` must never
// destroy what the user wrote by hand.
//
// defaultAccount, when non-empty, pins default_account only if the file has
// no top-level default_account key yet: an existing choice is never
// overridden, but a config that would otherwise gain a second account with
// no default (an invalid config) gets one.
//
// The file is written atomically (temp file + rename) at 0600, keeping an
// existing file's mode.
func SaveAccount(path, id string, a *Account, defaultAccount string) error {
	if !ValidAccountID(id) {
		return fmt.Errorf("config: account id %q is not a bare key (letters, digits, -, _)", id)
	}
	if a == nil {
		return fmt.Errorf("config: account %q: nothing to save", id)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if os.IsNotExist(err) {
		// Fresh file: marshal the whole document — there is nothing to
		// preserve yet.
		cfg := &Config{Accounts: map[string]*Account{id: a}}
		if defaultAccount != "" {
			cfg.DefaultAccount = defaultAccount
		}
		data, err := toml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("config: encode: %w", err)
		}
		data = []byte(strings.TrimRight(string(data), "\n") + "\n")
		if err := writeAtomic(path, data, 0o600); err != nil {
			return fmt.Errorf("config: %w", err)
		}
		return nil
	}

	text := string(existing)
	text = upsertAccountBlock(text, id, a)
	text, err = upsertDefaultAccount(text, defaultAccount)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	if err := writeAtomic(path, []byte(text), mode); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// upsertAccountBlock replaces the [accounts.<id>] section of text, or
// appends it when the account is new. Everything outside that section —
// comments, [compose], [keys], other accounts — is untouched, including
// the blank line that separated this table from the next.
func upsertAccountBlock(text, id string, a *Account) string {
	block, err := accountBlock(id, a)
	if err != nil {
		// Cannot happen for a validated id: SaveAccount checked it and
		// Marshal only fails on unencodable values.
		panic(err)
	}
	lines := strings.Split(text, "\n")
	start := headerLine(lines, id)
	if start < 0 {
		out := strings.TrimRight(text, "\n")
		if out != "" {
			out += "\n\n"
		}
		return out + block
	}
	end := len(lines)
	for j := start + 1; j < len(lines); j++ {
		if strings.HasPrefix(strings.TrimSpace(lines[j]), "[") {
			end = j
			break
		}
	}
	sep := end
	for sep > start+1 && strings.TrimSpace(lines[sep-1]) == "" {
		sep--
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString(strings.Join(lines[:start], "\n"))
		b.WriteString("\n")
	}
	b.WriteString(block)
	if sep < len(lines) {
		b.WriteString(strings.Join(lines[sep:], "\n"))
	}
	return b.String()
}

// headerLine returns the index of the [accounts.<id>] table header (bare or
// quoted id), or -1.
func headerLine(lines []string, id string) int {
	bare := "[accounts." + id + "]"
	quoted := fmt.Sprintf("[accounts.%q]", id)
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == bare || t == quoted || strings.HasPrefix(t, bare+" ") {
			return i
		}
	}
	return -1
}

// accountBlock renders one account as a complete TOML table. The field
// lines come from Marshal so quoting and escaping stay the encoder's job
// (and match the schema's tags).
func accountBlock(id string, a *Account) (string, error) {
	body, err := toml.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("config: encode account %q: %w", id, err)
	}
	return "[accounts." + id + "]\n" + strings.TrimRight(string(body), "\n") + "\n", nil
}

// upsertDefaultAccount ensures a top-level default_account key exists when
// asked for. Top-level keys must precede any table header (TOML), so the
// line is inserted before the first one, or appended when the file has no
// tables at all.
func upsertDefaultAccount(text, id string) (string, error) {
	if id == "" {
		return text, nil
	}
	if hasDefaultAccount(text) {
		return text, nil
	}
	line := fmt.Sprintf("default_account = %q", id)
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "[") {
			out := append(lines[:i:i], line)
			return strings.Join(append(out, lines[i:]...), "\n"), nil
		}
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + line + "\n", nil
}

// hasDefaultAccount reports whether a top-level default_account key is
// already present (inside a table doesn't count — that would be a
// different key).
func hasDefaultAccount(text string) bool {
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			return false
		}
		if strings.HasPrefix(t, "default_account") {
			rest := strings.TrimPrefix(t, "default_account")
			if rest == "" || rest[0] == ' ' || rest[0] == '=' || rest[0] == '\t' {
				return true
			}
		}
	}
	return false
}

// writeAtomic writes data to path via a temp file + rename so a crash
// mid-write never truncates the config. The temp file is created with
// O_EXCL under a random name in the destination directory, so a symlink
// planted at a predictable "<path>.tmp" can never be followed and
// clobbered (finding F-11); the mode is applied to the file descriptor
// we own, never re-opened by path. Callers wrap the error with their own
// package prefix.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}
