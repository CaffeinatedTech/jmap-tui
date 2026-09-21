// Package keyring resolves account passwords without ever letting plaintext
// land in config (FR-J2). Resolution order:
//
//  1. the JMAP_TUI_PASSWORD_<ACCOUNT> environment variable (headless/CI)
//  2. the account's password_file (opt-in escape hatch, chmod 600, warned)
//  3. the OS keyring, service "jmap-tui", entry per account id
package keyring

import (
	"fmt"
	"os"
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

// DefaultBackend reads from the OS keyring.
func DefaultBackend(service, user string) (string, error) {
	return keyring.Get(service, user)
}

// EnvVar derives the password environment variable name for an account id:
// characters outside [A-Z0-9] become underscores. FR-J2.
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
			fmt.Sprintf("using password for account %q from %s (headless mode)", accountID, envVar),
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
			return "", nil, fmt.Errorf("keyring: no secret for account %q in service %q; store it in the OS keyring, set %s, or configure password_file", accountID, Service, EnvVar(accountID))
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
