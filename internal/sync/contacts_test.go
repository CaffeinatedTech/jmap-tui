package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// contactsFixture: two contacts in reverse display order, one without a
// name (renders by email), plus a second book for default-resolution tests.
func contactsFixture() ([]mockjmap.Contact, []mockjmap.AddressBook) {
	contacts := []mockjmap.Contact{
		{
			ID: "ct2", Given: "Alan", Surname: "Turing", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "alan@example.com"}},
		},
		{
			ID: "ct1", Given: "Ada", Surname: "Lovelace", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "ada@example.com", Label: "work"}},
			Org:    "Analytical Engines",
		},
		{
			ID: "ct3", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "noone@example.com"}},
		},
	}
	books := []mockjmap.AddressBook{
		{ID: "ab1", Name: "Address Book", IsDefault: true, IsSubscribed: true},
		{ID: "ab2", Name: "Work", SortOrder: 1, IsSubscribed: true},
	}
	return contacts, books
}

// newContactsEngine is the usual fixture engine with contacts seeded.
func newContactsEngine(t *testing.T) (*Engine, *mockjmap.Server) {
	t.Helper()
	e, srv := newTestEngine(t, nil)
	contacts, books := contactsFixture()
	srv.SetContacts(contacts)
	srv.SetAddressBooks(books)
	return e, srv
}

// plainProvider hides the contacts seam entirely (a provider that never
// implements ContactProvider).
type plainProvider struct{ mail.Provider }

func TestContactsLoadPublishesOrderedSnapshot(t *testing.T) {
	e, _ := newContactsEngine(t)
	ctx := context.Background()

	if !e.ContactsSupported() {
		t.Fatal("mock client implements the contacts seam")
	}
	if e.Contacts().Loaded {
		t.Fatal("store must start cold (lazy load, FR-L1)")
	}
	if err := e.LoadContacts(ctx); err != nil {
		t.Fatalf("LoadContacts: %v", err)
	}

	snap := e.Contacts()
	if !snap.Loaded || snap.Version == 0 {
		t.Fatalf("snapshot not published: %+v", snap)
	}
	if len(snap.Books) != 2 || snap.Books[0].ID != "ab1" || !snap.Books[0].IsDefault {
		t.Fatalf("books wrong: %+v", snap.Books)
	}
	if got := e.ContactDefaultBook(); got != "ab1" {
		t.Fatalf("default book = %q, want ab1", got)
	}
	ids := make([]string, 0, len(snap.Contacts))
	for _, c := range snap.Contacts {
		ids = append(ids, string(c.ID))
	}
	// mail.ContactLess: "ada lovelace" < "alan turing" < "noone@example.com".
	if len(ids) != 3 || ids[0] != "ct1" || ids[1] != "ct2" || ids[2] != "ct3" {
		t.Fatalf("display order wrong: %v", ids)
	}
	if snap.Contacts[0].Orgs[0].Value != "Analytical Engines" || snap.Contacts[0].Emails[0].Label != "work" {
		t.Fatalf("contact payload wrong: %+v", snap.Contacts[0])
	}

	// The channel carries the newest snapshot (latest wins).
	select {
	case ch := <-e.ContactUpdates():
		if ch.Version != snap.Version {
			t.Fatalf("channel version %d != snapshot %d", ch.Version, snap.Version)
		}
	case <-time.After(time.Second):
		t.Fatal("no contact snapshot on the channel")
	}

	// Idempotent warm reload.
	if err := e.LoadContacts(ctx); err != nil {
		t.Fatalf("warm LoadContacts: %v", err)
	}
}

func TestContactsUnsupportedSeams(t *testing.T) {
	// Provider without the seam at all.
	e, _ := newTestEngineWithProvider(t, nil, func(p mail.Provider) mail.Provider {
		return plainProvider{p}
	})
	if e.ContactsSupported() {
		t.Fatal("plain provider must not report contacts support")
	}
	if err := e.LoadContacts(context.Background()); !errors.Is(err, ErrNoContacts) {
		t.Fatalf("LoadContacts: got %v, want ErrNoContacts", err)
	}

	// Seam present, capability absent (server without contacts, FR-L6).
	_, srv := newTestEngine(t, nil)
	srv.DisableContacts()
	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	e2 := NewEngine(c, Config{})
	if e2.ContactsSupported() {
		t.Fatal("capability removed; ContactsSupported should be false")
	}
	if err := e2.LoadContacts(context.Background()); !errors.Is(err, ErrNoContacts) {
		t.Fatalf("LoadContacts: got %v, want ErrNoContacts", err)
	}
}

func TestContactsSetCreateUpdateDestroy(t *testing.T) {
	e, _ := newContactsEngine(t)
	ctx := context.Background()
	if err := e.LoadContacts(ctx); err != nil {
		t.Fatalf("LoadContacts: %v", err)
	}

	// Create with no book chosen → the default book resolves (RFC 9610 §2).
	res, err := e.SetContact(ctx, mail.ContactMutation{
		Create: map[string]mail.ContactDraft{"h1": {
			GivenName: "Grace", Surname: "Hopper",
			Emails: []mail.ContactEmail{{Address: "grace@example.com"}},
		}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, ok := res.Created["h1"]
	if !ok {
		t.Fatalf("no created id: %+v", res)
	}
	snap := e.Contacts()
	if len(snap.Contacts) != 4 {
		t.Fatalf("store should hold 4 contacts, got %d", len(snap.Contacts))
	}
	var created *mail.Contact
	for i := range snap.Contacts {
		if snap.Contacts[i].ID == id {
			created = &snap.Contacts[i]
		}
	}
	if created == nil || created.GivenName != "Grace" {
		t.Fatalf("created contact missing: %+v", snap.Contacts)
	}
	if len(created.AddressBookIDs) != 1 || created.AddressBookIDs[0] != "ab1" {
		t.Fatalf("create did not land in the default book: %+v", created.AddressBookIDs)
	}
	// Sorted in: "grace hopper" sits between "ada lovelace" and "alan turing"?  No —
	// "grace" > "alan", so it lands after Alan, before the email-only card.
	if snap.Contacts[2].ID != id {
		t.Fatalf("created contact out of order: %v", snap.Contacts)
	}

	// Update through the store.
	if _, err := e.SetContact(ctx, mail.ContactMutation{
		Update: map[mail.ID]mail.ContactUpdate{
			id: {
				Draft:   mail.ContactDraft{GivenName: "Grace", Surname: "Hopper", Title: "Rear Admiral", Emails: []mail.ContactEmail{{Address: "grace@example.com"}}},
				Current: *created,
			},
		},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	snap = e.Contacts()
	for _, c := range snap.Contacts {
		if c.ID == id {
			if len(c.Titles) != 1 || c.Titles[0].Value != "Rear Admiral" {
				t.Fatalf("update not applied: %+v", c)
			}
			created = &c
		}
	}

	// Destroy removes it and advances state.
	if _, err := e.SetContact(ctx, mail.ContactMutation{Destroy: []mail.ID{id}}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	snap = e.Contacts()
	for _, c := range snap.Contacts {
		if c.ID == id {
			t.Fatalf("destroyed contact still present: %+v", c)
		}
	}
	if len(snap.Contacts) != 3 {
		t.Fatalf("want 3 contacts, got %d", len(snap.Contacts))
	}
}

// TestContactsColdStoreStaysCold: reconcilers no-op before the first load —
// a user who never opened contacts pays nothing (FR-L1).
func TestContactsColdStoreStaysCold(t *testing.T) {
	e, srv := newContactsEngine(t)
	ctx := context.Background()
	contacts, _ := contactsFixture()
	srv.SetContacts(append(append([]mockjmap.Contact(nil), contacts...), mockjmap.Contact{
		ID: "ct9", Given: "Zoe", Surname: "New", AddressBookIDs: []string{"ab1"},
		Emails: []mockjmap.ContactValue{{Value: "zoe@example.com"}},
	}))

	// Even a direct reconcile leaves the cold store cold.
	e.reconcileContacts(ctx)
	if e.contactsWarm() {
		t.Fatal("reconcile must not load a cold store")
	}

	// First load fetches everything (including the externally added card)…
	if err := e.LoadContacts(ctx); err != nil {
		t.Fatalf("LoadContacts: %v", err)
	}
	found := false
	for _, c := range e.Contacts().Contacts {
		if c.ID == "ct9" {
			found = true
		}
	}
	if !found {
		t.Fatalf("load missed the seeded contact: %d", len(e.Contacts().Contacts))
	}

	// …and from now on /changes folds keep it fresh.
	more := append(append([]mockjmap.Contact(nil), contacts...),
		mockjmap.Contact{
			ID: "ct10", Given: "Newest", Surname: "Arrival", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "new@example.com"}},
		})
	srv.SetContacts(append(more, mockjmap.Contact{
		ID: "ct9", Given: "Zoe", Surname: "New", AddressBookIDs: []string{"ab1"},
		Emails: []mockjmap.ContactValue{{Value: "zoe@example.com"}},
	}))
	e.reconcileContacts(ctx)
	found = false
	for _, c := range e.Contacts().Contacts {
		if c.ID == "ct10" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warm store missed the /changes fold: %d contacts", len(e.Contacts().Contacts))
	}
}

func TestContactsColdStoreIgnoresPushStream(t *testing.T) {
	e, srv := newContactsEngine(t)
	e.liveCfg.pollInterval = 25 * time.Millisecond
	e.liveCfg.backoffBase = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return srv.StreamCount() == 1 })

	// External edit + push while the store is cold: no lazy load, no
	// error, version untouched.
	before := e.Contacts().Version
	contacts, _ := contactsFixture()
	srv.SetContacts(append(append([]mockjmap.Contact(nil), contacts...), mockjmap.Contact{
		ID: "ctX", Given: "X", AddressBookIDs: []string{"ab1"},
		Emails: []mockjmap.ContactValue{{Value: "x@example.com"}},
	}))
	srv.Notify()

	// Let several poll ticks pass (25ms interval) — the invariant is that
	// nothing happens.
	time.Sleep(80 * time.Millisecond)
	snap := e.Contacts()
	if snap.Loaded {
		t.Fatal("push must not lazily load a cold store")
	}
	if snap.Version != before {
		t.Fatalf("cold store version moved %d → %d", before, snap.Version)
	}
}

func TestContactsLiveReconcile(t *testing.T) {
	e, srv := newContactsEngine(t)
	e.liveCfg.pollInterval = 25 * time.Millisecond
	e.liveCfg.backoffBase = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.LoadContacts(ctx); err != nil {
		t.Fatalf("LoadContacts: %v", err)
	}
	e.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return srv.StreamCount() == 1 })

	// External create + push → row appears.
	contacts, _ := contactsFixture()
	srv.SetContacts(append(append([]mockjmap.Contact(nil), contacts...), mockjmap.Contact{
		ID: "ctLive", Given: "Live", Surname: "Arrival", AddressBookIDs: []string{"ab1"},
		Emails: []mockjmap.ContactValue{{Value: "live@example.com"}},
	}))
	srv.Notify()
	waitFor(t, 2*time.Second, func() bool {
		for _, c := range e.Contacts().Contacts {
			if c.ID == "ctLive" {
				return true
			}
		}
		return false
	})

	// External destroy + push → row evicts.
	srv.DestroyContacts("ctLive")
	srv.Notify()
	waitFor(t, 2*time.Second, func() bool {
		for _, c := range e.Contacts().Contacts {
			if c.ID == "ctLive" {
				return false
			}
		}
		return len(e.Contacts().Contacts) == 3
	})

	// Book change + push → book list refreshes.
	srv.SetAddressBooks([]mockjmap.AddressBook{
		{ID: "ab1", Name: "Address Book", IsDefault: true, IsSubscribed: true},
		{ID: "ab2", Name: "Renamed Book", SortOrder: 1, IsSubscribed: true},
	})
	srv.Notify()
	waitFor(t, 2*time.Second, func() bool {
		for _, b := range e.Contacts().Books {
			if b.Name == "Renamed Book" {
				return true
			}
		}
		return false
	})
}

func TestContactsPollFallbackFoldsChanges(t *testing.T) {
	e, srv := newContactsEngine(t)
	ctx := context.Background()
	if err := e.LoadContacts(ctx); err != nil {
		t.Fatalf("LoadContacts: %v", err)
	}

	contacts, _ := contactsFixture()
	srv.SetContacts(append(append([]mockjmap.Contact(nil), contacts...), mockjmap.Contact{
		ID: "ctPolled", Given: "Polled", AddressBookIDs: []string{"ab1"},
		Emails: []mockjmap.ContactValue{{Value: "polled@example.com"}},
	}))
	// No Notify: the poll path must fold it purely from state strings
	// (FR-B3 fallback).
	e.pollOnce(ctx)
	found := false
	for _, c := range e.Contacts().Contacts {
		if c.ID == "ctPolled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("poll did not fold the contact delta: %d contacts", len(e.Contacts().Contacts))
	}
}

func TestContactsCannotCalculateChangesReloads(t *testing.T) {
	e, srv := newContactsEngine(t)
	ctx := context.Background()
	if err := e.LoadContacts(ctx); err != nil {
		t.Fatalf("LoadContacts: %v", err)
	}

	contacts, _ := contactsFixture()
	srv.SetContacts(append(append([]mockjmap.Contact(nil), contacts...), mockjmap.Contact{
		ID: "ctReload", Given: "Reloaded", AddressBookIDs: []string{"ab1"},
		Emails: []mockjmap.ContactValue{{Value: "reload@example.com"}},
	}))
	srv.SetCannotCalculateChanges(true)
	e.reconcileContacts(ctx)

	found := false
	for _, c := range e.Contacts().Contacts {
		if c.ID == "ctReload" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cannotCalculateChanges must full-reload the store: %d contacts", len(e.Contacts().Contacts))
	}
	if e.Contacts().Err != "" {
		t.Fatalf("reload should clear the error, got %q", e.Contacts().Err)
	}
}
