package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAccountFreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "config.toml")
	a := &Account{
		DisplayName:    "Work",
		URL:            "https://mail.example.com",
		Username:       "me@example.com",
		InitialMailbox: "mb-1",
	}
	if err := SaveAccount(path, "work", a, "work"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v\n%s", err, mustRead(t, path))
	}
	if cfg.DefaultAccount != "work" {
		t.Errorf("DefaultAccount = %q, want work", cfg.DefaultAccount)
	}
	got, _ := cfg.Account("work")
	if got == nil || got.URL != a.URL || got.InitialMailbox != "mb-1" {
		t.Errorf("round trip = %+v", got)
	}
	// Empty optional fields stay out of the file (omitempty) — a hand-
	// writable config, not an encoder dump.
	body := mustRead(t, path)
	for _, unwanted := range []string{"display_name = \"\"", "[window]", "[keys]", "[compose]", "password_file"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("fresh config contains %q:\n%s", unwanted, body)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestSaveAccountPreservesExistingFile(t *testing.T) {
	path := write(t, `# my jmap-tui config
default_account = "personal"

[accounts.personal]
url = "https://mail.example.com"   # main account
username = "me@example.com"

# the work account
[accounts.work]
url = "https://work.example.com"
username = "me@work.example.com"

[compose]
undo_delay = "10s"   # hold sends
`)
	if err := SaveAccount(path, "side", &Account{
		URL: "https://side.example.com", Username: "me@side.example.com",
	}, ""); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	body := mustRead(t, path)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, body)
	}
	if len(cfg.Accounts) != 3 {
		t.Fatalf("accounts = %d, want 3:\n%s", len(cfg.Accounts), body)
	}
	// Everything the user wrote survives verbatim.
	for _, want := range []string{
		"# my jmap-tui config",
		`default_account = "personal"`,
		"url = \"https://mail.example.com\"   # main account",
		"# the work account",
		"undo_delay = \"10s\"   # hold sends",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// The new account landed as its own table, before [compose].
	if !strings.Contains(body, "[accounts.side]") {
		t.Errorf("no [accounts.side] table:\n%s", body)
	}
	// An existing default_account is never overridden ("" was passed, but
	// even with an id it must not steal the key).
	if err := SaveAccount(path, "other", &Account{URL: "https://o.example.com", Username: "o"}, "other"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	body = mustRead(t, path)
	if !strings.Contains(body, `default_account = "personal"`) {
		t.Errorf("default_account was rewritten:\n%s", body)
	}
	if strings.Count(body, "default_account") != 1 {
		t.Errorf("duplicate default_account:\n%s", body)
	}
}

func TestSaveAccountReplacesExistingBlock(t *testing.T) {
	path := write(t, `[accounts.work]
url = "https://old.example.com"
username = "old@example.com"

[accounts.home]
url = "https://home.example.com"
username = "home@example.com"
`)
	if err := SaveAccount(path, "work", &Account{
		URL: "https://new.example.com", Username: "new@example.com",
		InitialMailbox: "mb-42",
	}, ""); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2 (replace, not duplicate)", len(cfg.Accounts))
	}
	w, _ := cfg.Account("work")
	if w.URL != "https://new.example.com" || w.InitialMailbox != "mb-42" {
		t.Errorf("work = %+v", w)
	}
	if _, ok := cfg.Account("home"); !ok {
		t.Error("neighbour account lost by the rewrite")
	}
	body := mustRead(t, path)
	if strings.Contains(body, "old@example.com") {
		t.Errorf("stale block content remains:\n%s", body)
	}
	// Blank line between the two tables survives the splice.
	if !strings.Contains(body, "\n\n[accounts.home]") {
		t.Errorf("table spacing lost:\n%s", body)
	}
}

func TestSaveAccountInsertsMissingDefault(t *testing.T) {
	// Two accounts and no default_account is an invalid config: pinning
	// the default must land before the first table header (TOML top-level
	// keys precede tables).
	path := write(t, `# comment on top
[accounts.solo]
url = "https://s.example.com"
username = "s"
`)
	if err := SaveAccount(path, "new", &Account{URL: "https://n.example.com", Username: "n"}, "solo"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	body := mustRead(t, path)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, body)
	}
	if cfg.DefaultAccount != "solo" {
		t.Errorf("DefaultAccount = %q, want solo", cfg.DefaultAccount)
	}
	if !strings.Contains(body, "# comment on top\ndefault_account = \"solo\"\n[accounts.solo]") {
		t.Errorf("default_account misplaced:\n%s", body)
	}
	if cfg.Accounts["new"] == nil {
		t.Error("new account missing")
	}
}

func TestSaveAccountRejectsBadID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, id := range []string{"has space", `a"b`, "a.b", ""} {
		if err := SaveAccount(path, id, &Account{URL: "https://x", Username: "u"}, ""); err == nil {
			t.Errorf("id %q accepted; want an error", id)
		}
	}
}

func TestSaveAccountKeepsFileMode(t *testing.T) {
	path := write(t, "[accounts.a]\nurl = \"https://a.example.com\"\nusername = \"a\"\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveAccount(path, "b", &Account{URL: "https://b.example.com", Username: "b"}, ""); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want the original 644", fi.Mode().Perm())
	}
}

func TestErrNoAccounts(t *testing.T) {
	path := write(t, "")
	_, err := Load(path)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "no accounts") {
		t.Errorf("error %q should keep the human phrasing", err)
	}
	if !errors.Is(err, ErrNoAccounts) {
		t.Errorf("error %v does not match ErrNoAccounts — the wizard would never launch", err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
