// Package keyring resolves account passwords without ever letting plaintext
// land in config (FR-J2). Resolution order:
//
//  1. the JMAP_TUI_PASSWORD_<ACCOUNT> environment variable — for automated
//     testing only (FR-J2); never a deployment technique, and only ever
//     test-account credentials
//  2. the account's password_file (opt-in escape hatch, chmod 600, warned)
//  3. the OS keyring, service "jmap-tui", entry per account id
package keyring

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/zalando/go-keyring"
)

// Service is the OS keyring service name for all jmap-tui secrets.
const Service = "jmap-tui"

// Backend abstracts the OS keyring so callers and tests can inject a fake.
// It returns the secret for (service, user) or an error (typically
// keyring.ErrNotFound).
type Backend func(service, user string) (string, error)

// Store abstracts the write side of the keyring (the wizard stores the
// secret it just proved works, FR-I8).
type Store func(service, user, secret string) error

// DefaultBackend reads from the OS keyring.
func DefaultBackend(service, user string) (string, error) {
	return keyring.Get(service, user)
}

// DefaultStore writes to the OS keyring.
func DefaultStore(service, user, secret string) error {
	return keyring.Set(service, user, secret)
}

// Set stores the secret for accountID in the OS keyring (service
// Service). The wizard calls it after the connection test succeeds
// (FR-I8); store may be nil for the real keyring. The secret is never
// logged or echoed back.
func Set(accountID, secret string, store Store) error {
	if accountID == "" {
		return fmt.Errorf("keyring: empty account id")
	}
	if store == nil {
		store = DefaultStore
	}
	if err := store(Service, accountID, secret); err != nil {
		return fmt.Errorf("keyring: store secret for account %q: %w", accountID, err)
	}
	return nil
}

// EnvVar derives the password environment variable name for an account id:
// characters outside [A-Z0-9] become underscores. FR-J2: the env var is a
// testing mechanism, not a deployment technique — only test-account
// credentials belong in it.
func EnvVar(accountID string) string {
	var b strings.Builder
	b.WriteString("JMAP_TUI_PASSWORD_")
	for _, r := range strings.ToUpper(accountID) {
		if unicode.IsLetter(r) && r <= 'Z' || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Password resolves the secret for accountID. passwordFile may be empty. The
// returned warnings are for the caller to surface (e.g. on stderr); they are
// never secret material.
func Password(accountID, passwordFile string, backend Backend) (secret string, warnings []string, err error) {
	if envVar := EnvVar(accountID); os.Getenv(envVar) != "" {
		return os.Getenv(envVar), []string{
			fmt.Sprintf("using password for account %q from %s (testing)", accountID, envVar),
		}, nil
	}

	if passwordFile != "" {
		secret, err := readPasswordFile(passwordFile)
		if err != nil {
			return "", nil, fmt.Errorf("keyring: account %q: %w", accountID, err)
		}
		return secret, []string{
			fmt.Sprintf("using password for account %q from %s (plaintext on disk — consider the OS keyring)", accountID, passwordFile),
		}, nil
	}

	if backend == nil {
		backend = DefaultBackend
	}
	secret, err = backend(Service, accountID)
	if err != nil {
		if err == keyring.ErrNotFound {
			return "", nil, fmt.Errorf("keyring: no secret for account %q in service %q; store it in the OS keyring or configure password_file", accountID, Service)
		}
		return "", nil, fmt.Errorf("keyring: account %q: %w", accountID, err)
	}
	return secret, nil, nil
}

func readPasswordFile(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("read password file: %s is not a regular file", path)
	}
	// A password file readable by group/others defeats the point of keeping
	// the secret out of config; refuse rather than warn-and-continue.
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("read password file: %s must be chmod 600, is %o", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// WritePasswordFile stores secret in an explicit chmod-600 file — the
// FR-J2 escape hatch the wizard offers when the OS keyring is unavailable
// (SSH sessions and headless machines with no secret service). The mode is
// re-asserted after the write because os.WriteFile keeps an existing
// file's permissions.
func WritePasswordFile(path, secret string) error {
	if path == "" {
		return fmt.Errorf("keyring: empty password file path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("keyring: %w", err)
	}
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		return fmt.Errorf("keyring: write %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("keyring: chmod %s: %w", path, err)
	}
	return nil
}
