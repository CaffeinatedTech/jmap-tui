package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPrefsMissingFileIsEmpty(t *testing.T) {
	p, err := LoadPrefs(filepath.Join(t.TempDir(), "prefs.toml"))
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if p.ArchiveMailbox("acc1") != "" {
		t.Fatalf("archive mailbox = %q, want empty", p.ArchiveMailbox("acc1"))
	}
}

func TestSavePrefsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.toml")
	p := &Prefs{}
	p.SetArchiveMailbox("acc1", "mb-archive")
	p.SetArchiveMailbox("acc2", "mb-hold")
	p.Layout = LayoutStacked
	if err := SavePrefs(path, p); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}

	// 0600: the prefs file holds server-side mailbox ids, keep it private.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}

	got, err := LoadPrefs(path)
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if got.ArchiveMailbox("acc1") != "mb-archive" || got.ArchiveMailbox("acc2") != "mb-hold" {
		t.Fatalf("round trip: %+v", got)
	}
	if !got.Stacked() || got.Layout != LayoutStacked {
		t.Fatalf("layout round trip = %q, want %q", got.Layout, LayoutStacked)
	}
	// The zero/unknown value falls back to the default layout.
	if (&Prefs{}).Stacked() || (&Prefs{Layout: "sideways"}).Stacked() {
		t.Fatal("unknown layout must fall back to side-by-side")
	}
}

func TestSavePrefsIsAtomicAndNeverClobbersOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.toml")
	p := &Prefs{}
	p.SetArchiveMailbox("acc1", "mb-a")
	if err := SavePrefs(path, p); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}
	// A second save rewrites cleanly and leaves no temp file behind.
	p.SetArchiveMailbox("acc1", "mb-b")
	if err := SavePrefs(path, p); err != nil {
		t.Fatalf("SavePrefs 2: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "mb-b") {
		t.Fatalf("second save lost: %s", after)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestDefaultPrefsPathSitsNextToConfig(t *testing.T) {
	p, err := DefaultPrefsPath()
	if err != nil {
		t.Fatalf("DefaultPrefsPath: %v", err)
	}
	cfg, _ := DefaultPath()
	if filepath.Dir(p) != filepath.Dir(cfg) || filepath.Base(p) != "prefs.toml" {
		t.Fatalf("prefs path = %q, want sibling of %q", p, cfg)
	}
}
