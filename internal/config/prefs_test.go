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
	p.Unified = true
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
	if !got.Unified {
		t.Fatal("unified round trip = false, want true")
	}
	// The default (unified off) is omitted: prefs stay minimal.
	p.Unified = false
	if err := SavePrefs(path, p); err != nil {
		t.Fatalf("SavePrefs (unified off): %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prefs: %v", err)
	}
	if strings.Contains(string(data), "unified") {
		t.Fatalf("unified = false must be omitted from prefs:\n%s", data)
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

// TestIdentityRoundTrip: the composer's remembered From (FR-H1) is stored
// per account, keyed by identity email, and omitted when unset.
func TestIdentityRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.toml")
	p := &Prefs{}
	p.SetIdentity("acc1", "work@example.test")
	if err := SavePrefs(path, p); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}
	got, err := LoadPrefs(path)
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if got.Identity("acc1") != "work@example.test" {
		t.Fatalf("identity round trip = %q", got.Identity("acc1"))
	}
	if got.Identity("acc2") != "" {
		t.Fatalf("unknown account identity = %q, want empty", got.Identity("acc2"))
	}
	// The default (no choice) is omitted: prefs stay minimal.
	p.SetIdentity("acc1", "")
	if err := SavePrefs(path, p); err != nil {
		t.Fatalf("SavePrefs (cleared): %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read prefs: %v", err)
	}
	if strings.Contains(string(data), "identity") {
		t.Fatalf("empty identity must be omitted from prefs:\n%s", data)
	}
}

func TestAccountOrderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.toml")
	p := &Prefs{}
	p.SetAccountOrder([]string{"work", "personal"})
	if err := SavePrefs(path, p); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}
	got, err := LoadPrefs(path)
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if len(got.AccountOrder) != 2 || got.AccountOrder[0] != "work" || got.AccountOrder[1] != "personal" {
		t.Fatalf("account_order round trip = %v", got.AccountOrder)
	}
	// The setter copies: mutating the caller's slice later must not
	// rewrite the prefs document in place.
	ids := []string{"a", "b"}
	p.SetAccountOrder(ids)
	ids[0] = "mutated"
	if p.AccountOrder[0] != "a" {
		t.Fatalf("SetAccountOrder aliased the caller's slice: %v", p.AccountOrder)
	}
}

func TestMergeAccountOrder(t *testing.T) {
	base := []string{"default", "alpha", "zeta"} // default first, then id
	cases := []struct {
		name  string
		saved []string
		want  []string
	}{
		{name: "empty keeps base", saved: nil, want: []string{"default", "alpha", "zeta"}},
		{name: "saved wins", saved: []string{"zeta", "default", "alpha"}, want: []string{"zeta", "default", "alpha"}},
		{name: "partial overlay", saved: []string{"alpha"}, want: []string{"alpha", "default", "zeta"}},
		{name: "unknown dropped", saved: []string{"gone", "zeta"}, want: []string{"zeta", "default", "alpha"}},
		{name: "duplicates collapse", saved: []string{"zeta", "zeta"}, want: []string{"zeta", "default", "alpha"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeAccountOrder(base, tc.saved)
			if len(got) != len(tc.want) {
				t.Fatalf("merge = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("merge = %v, want %v", got, tc.want)
				}
			}
		})
	}
	// A newly added account (in base, not in saved) never vanishes.
	got := MergeAccountOrder([]string{"default", "alpha", "zeta", "newborn"}, []string{"zeta"})
	found := false
	for _, id := range got {
		if id == "newborn" {
			found = true
		}
	}
	if !found {
		t.Fatalf("new account dropped: %v", got)
	}
}
