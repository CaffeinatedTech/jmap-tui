package jmapclient

import (
	"context"
	"errors"
	"reflect"
	"testing"

	jmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

func newContactsClient(t *testing.T) (*Client, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", testPassword, fixtures())
	t.Cleanup(srv.Close)
	c := New(Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: testPassword})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c, srv
}

// TestContactsSupported gates on the session capability (FR-L6).
func TestContactsSupported(t *testing.T) {
	c, srv := newContactsClient(t)
	if !c.ContactsSupported() {
		t.Fatal("mock advertises contacts; ContactsSupported should be true")
	}
	srv.DisableContacts()
	c2 := New(Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: testPassword})
	if err := c2.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if c2.ContactsSupported() {
		t.Fatal("capability removed from session; ContactsSupported should be false")
	}
	if _, err := c2.Contacts(context.Background()); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("Contacts without capability: got %v, want ErrNotSupported", err)
	}
}

func TestAddressBooksFetchesDefault(t *testing.T) {
	c, _ := newContactsClient(t)
	list, err := c.AddressBooks(context.Background())
	if err != nil {
		t.Fatalf("AddressBooks: %v", err)
	}
	if list.State == "" {
		t.Fatal("AddressBook/get must return a /changes bootstrap state")
	}
	if len(list.Books) != 1 {
		t.Fatalf("got %d books, want 1", len(list.Books))
	}
	b := list.Books[0]
	if b.ID != "ab1" || !b.IsDefault || !b.IsSubscribed || b.Name == "" {
		t.Fatalf("default book shape wrong: %+v", b)
	}
}

func TestContactsConversion(t *testing.T) {
	// Pref ordering: absent pref = least preferred (RFC 9553 §1.5.3).
	unpref := uint(3)
	pref1 := uint(1)
	card := &jsCard{
		ID:             "x1",
		AddressBookIDs: map[jmap.ID]bool{"ab1": true, "ab2": true},
		Kind:           "individual",
		Name: &jsName{
			Components: []jsNameComponent{
				{Kind: "given", Value: "Ada"},
				{Kind: "surname", Value: "Lovelace"},
			},
			Full: "Ada Lovelace",
		},
		Emails: map[string]jsEntry{
			"b": {Address: "second@example.com"},
			"a": {Address: "ada@example.com", Label: "work", Pref: &pref1},
			"c": {Address: "third@example.com", Pref: &unpref},
		},
		Phones:        map[string]jsEntry{"0": {Number: "+44 123", Label: "mobile"}},
		Organizations: map[string]jsEntry{"o1": {Name: "Analytical Engines"}, "o2": {Name: "Second Org"}},
		Titles:        map[string]jsEntry{"t1": {Name: "Mathematician"}},
		Notes:         map[string]jsEntry{"n1": {Note: "First note"}, "n2": {Note: "Second note"}},
	}
	got, keep := convertContact(card)
	if !keep {
		t.Fatal("individual card must be kept")
	}
	if got.GivenName != "Ada" || got.Surname != "Lovelace" || got.DisplayName != "Ada Lovelace" {
		t.Fatalf("name conversion wrong: %+v", got)
	}
	wantEmails := []mail.ContactEmail{
		{Address: "ada@example.com", Label: "work"}, // pref 1
		{Address: "third@example.com"},              // pref 3
		{Address: "second@example.com"},             // no pref → last
	}
	if !reflect.DeepEqual(got.Emails, wantEmails) {
		t.Fatalf("email order/labels wrong:\n got %+v\nwant %+v", got.Emails, wantEmails)
	}
	if len(got.Phones) != 1 || got.Phones[0].Label != "mobile" {
		t.Fatalf("phones wrong: %+v", got.Phones)
	}
	if len(got.Orgs) != 2 || got.Orgs[0].Key != "o1" || got.Orgs[1].Value != "Second Org" {
		t.Fatalf("org entries wrong: %+v", got.Orgs)
	}
	if len(got.Notes) != 2 || got.Notes[0].Value != "First note" {
		t.Fatalf("note entries wrong: %+v", got.Notes)
	}
	if len(got.AddressBookIDs) != 2 {
		t.Fatalf("books wrong: %+v", got.AddressBookIDs)
	}

	// Group cards are out of scope and must be dropped (CONTACTS_PLAN §0).
	if _, keep := convertContact(&jsCard{ID: "g1", Kind: "group"}); keep {
		t.Fatal("group card must be dropped")
	}

	// full-only name (no components) still yields a display name.
	c2, _ := convertContact(&jsCard{ID: "x2", Name: &jsName{Full: "Plato"}})
	if c2.DisplayName != "Plato" {
		t.Fatalf("full-only name: got %q", c2.DisplayName)
	}
}

func TestSetContactsCreateRoundTrip(t *testing.T) {
	c, _ := newContactsClient(t)
	ctx := context.Background()

	res, err := c.SetContacts(ctx, mail.ContactMutation{
		Create: map[string]mail.ContactDraft{
			"h1": {
				GivenName:      "Grace",
				Surname:        "Hopper",
				Emails:         []mail.ContactEmail{{Address: "grace@example.com", Label: "work"}},
				Phones:         []mail.ContactPhone{{Number: "+1 555"}},
				Org:            "Navy",
				Title:          "Rear Admiral",
				Note:           "Compiler pioneer",
				AddressBookIDs: []mail.ID{"ab1"},
			},
		},
	})
	if err != nil {
		t.Fatalf("SetContacts create: %v", err)
	}
	id, ok := res.Created["h1"]
	if !ok || id == "" {
		t.Fatalf("no created id: %+v", res)
	}
	if res.NewState == "" {
		t.Fatal("create must advance the contact state")
	}

	list, err := c.Contacts(ctx)
	if err != nil {
		t.Fatalf("Contacts: %v", err)
	}
	if len(list.Contacts) != 1 {
		t.Fatalf("got %d contacts, want 1", len(list.Contacts))
	}
	got := list.Contacts[0]
	if got.DisplayName != "Grace Hopper" || got.GivenName != "Grace" {
		t.Fatalf("created name wrong: %+v", got)
	}
	if len(got.Emails) != 1 || got.Emails[0].Label != "work" {
		t.Fatalf("created emails wrong: %+v", got.Emails)
	}
	if got.Orgs[0].Value != "Navy" || got.Titles[0].Value != "Rear Admiral" || got.Notes[0].Value != "Compiler pioneer" {
		t.Fatalf("created collections wrong: orgs=%+v titles=%+v notes=%+v", got.Orgs, got.Titles, got.Notes)
	}

	// A create with no address book is rejected locally before the network
	// (a card must belong to a book, RFC 9610 §3).
	if _, err := c.SetContacts(ctx, mail.ContactMutation{
		Create: map[string]mail.ContactDraft{"h2": {GivenName: "No"}},
	}); err == nil {
		t.Fatal("create without address book must fail locally")
	}
}

func TestSetContactsUpdatePatch(t *testing.T) {
	c, _ := newContactsClient(t)
	ctx := context.Background()

	if _, err := c.SetContacts(ctx, mail.ContactMutation{
		Create: map[string]mail.ContactDraft{"h1": {
			GivenName: "Alan", Surname: "Turing",
			Org: "Bletchley", AddressBookIDs: []mail.ID{"ab1"},
		}},
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	list, err := c.Contacts(ctx)
	if err != nil || len(list.Contacts) != 1 {
		t.Fatalf("seed read: %v (n=%d)", err, len(list.Contacts))
	}
	cur := list.Contacts[0]

	// Change the surname only: name travels, org is untouched (no key).
	res, err := c.SetContacts(ctx, mail.ContactMutation{
		Update: map[mail.ID]mail.ContactUpdate{
			cur.ID: {
				Draft:   mail.ContactDraft{GivenName: "Alan", Surname: "Mathison Turing", Org: "Bletchley"},
				Current: cur,
			},
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(res.Updated) != 1 || res.Updated[0] != cur.ID {
		t.Fatalf("updated wrong: %+v", res)
	}

	// An untouched draft must produce no patch at all — nothing to send.
	back, err := c.Contacts(ctx)
	if err != nil || len(back.Contacts) != 1 {
		t.Fatalf("reread: %v", err)
	}
	noop, err := c.SetContacts(ctx, mail.ContactMutation{
		Update: map[mail.ID]mail.ContactUpdate{
			cur.ID: {
				Draft:   mail.ContactDraft{GivenName: "Alan", Surname: "Mathison Turing", Org: "Bletchley"},
				Current: back.Contacts[0],
			},
		},
	})
	if err != nil {
		t.Fatalf("noop update: %v", err)
	}
	if len(noop.Updated) != 0 {
		t.Fatalf("unchanged draft should send nothing, got %+v", noop)
	}

	if back.Contacts[0].Surname != "Mathison Turing" || back.Contacts[0].Orgs[0].Value != "Bletchley" {
		t.Fatalf("update not applied: %+v", back.Contacts[0])
	}

	// Clear the org: whole-property removal for the only entry.
	if _, err := c.SetContacts(ctx, mail.ContactMutation{
		Update: map[mail.ID]mail.ContactUpdate{
			cur.ID: {Draft: mail.ContactDraft{GivenName: "Alan", Surname: "Mathison Turing"}, Current: back.Contacts[0]},
		},
	}); err != nil {
		t.Fatalf("clear org: %v", err)
	}
	back, err = c.Contacts(ctx)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if len(back.Contacts[0].Orgs) != 0 {
		t.Fatalf("org should be cleared: %+v", back.Contacts[0].Orgs)
	}

	// Destroy and verify gone.
	if _, err := c.SetContacts(ctx, mail.ContactMutation{Destroy: []mail.ID{cur.ID}}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	final, err := c.Contacts(ctx)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if len(final.Contacts) != 0 {
		t.Fatalf("contact should be destroyed, got %d", len(final.Contacts))
	}
}

// TestContactUpdatePatchIsMinimal proves the diff rules directly: untouched
// collections never appear in the patch (siblings and sub-fields survive),
// changed ones path-patch the first entry only.
func TestContactUpdatePatchIsMinimal(t *testing.T) {
	cur := mail.Contact{
		ID:        "x",
		GivenName: "Ada",
		Surname:   "Lovelace",
		Emails:    []mail.ContactEmail{{Address: "ada@example.com", Label: "work"}},
		Orgs:      []mail.ContactEntry{{Key: "o9", Value: "Analytical Engines"}},
		Notes:     []mail.ContactEntry{{Key: "n1", Value: "kept"}, {Key: "n2", Value: "sibling"}},
	}
	base := mail.ContactDraft{
		GivenName: "Ada", Surname: "Lovelace",
		Emails: []mail.ContactEmail{{Address: "ada@example.com", Label: "work"}},
		Org:    "Analytical Engines", Note: "kept",
	}

	if patch := contactUpdatePatch(mail.ContactUpdate{Draft: base, Current: cur}); len(patch) != 0 {
		t.Fatalf("identical draft must patch nothing, got %+v", patch)
	}

	// Note changed → path patch on the first entry; the sibling stays out
	// of the patch entirely.
	p := contactUpdatePatch(mail.ContactUpdate{Draft: func() mail.ContactDraft {
		d := base
		d.Note = "edited"
		return d
	}(), Current: cur})
	if len(p) != 1 || p["notes/n1"] == nil {
		t.Fatalf("note edit should path-patch notes/n1: %+v", p)
	}
	if _, ok := p["notes"]; ok {
		t.Fatalf("sibling note must not be rewritten wholesale: %+v", p)
	}

	// Org cleared with a single entry → whole-property removal.
	p = contactUpdatePatch(mail.ContactUpdate{Draft: func() mail.ContactDraft {
		d := base
		d.Org = ""
		return d
	}(), Current: cur})
	if v, ok := p["organizations"]; !ok || v != nil {
		t.Fatalf("single org clear should null the property: %+v", p)
	}

	// Org cleared while siblings exist → path removal only.
	multi := cur
	multi.Orgs = []mail.ContactEntry{{Key: "o9", Value: "AE"}, {Key: "o8", Value: "Other"}}
	p = contactUpdatePatch(mail.ContactUpdate{Draft: func() mail.ContactDraft {
		d := base
		d.Org = ""
		return d
	}(), Current: multi})
	if v, ok := p["organizations/o9"]; !ok || v != nil {
		t.Fatalf("multi-org clear should path-remove o9: %+v", p)
	}
	if _, ok := p["organizations"]; ok {
		t.Fatalf("sibling org must survive: %+v", p)
	}

	// Email added → whole map rewrite (the form owns the list).
	p = contactUpdatePatch(mail.ContactUpdate{Draft: func() mail.ContactDraft {
		d := base
		d.Emails = []mail.ContactEmail{{Address: "ada@example.com", Label: "work"}, {Address: "ada@eng.example"}}
		return d
	}(), Current: cur})
	if _, ok := p["emails"]; !ok {
		t.Fatalf("email list change should rewrite emails: %+v", p)
	}

	// Name cleared → null (name is optional in JSContact).
	p = contactUpdatePatch(mail.ContactUpdate{Draft: func() mail.ContactDraft {
		d := base
		d.GivenName, d.Surname = "", ""
		return d
	}(), Current: cur})
	if v, ok := p["name"]; !ok || v != nil {
		t.Fatalf("cleared name should be null: %+v", p)
	}
}

func TestContactChangesDelta(t *testing.T) {
	c, srv := newContactsClient(t)
	ctx := context.Background()

	// Bootstrap at the current state.
	list, err := c.Contacts(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	empty, err := c.ContactChanges(ctx, list.State)
	if err != nil {
		t.Fatalf("no-op changes: %v", err)
	}
	if len(empty.Updated) != 0 || len(empty.Destroyed) != 0 {
		t.Fatalf("clean state should be empty: %+v", empty)
	}

	// External edit: two new contacts appear in the delta.
	srv.SetContacts([]mockjmap.Contact{
		{ID: "ext1", Given: "New", Surname: "Person", Emails: []mockjmap.ContactValue{{Value: "new@example.com"}}, AddressBookIDs: []string{"ab1"}},
		{ID: "ext2", Surname: "Singleton", Emails: []mockjmap.ContactValue{{Value: "s@example.com"}}, AddressBookIDs: []string{"ab1"}},
	})
	delta, err := c.ContactChanges(ctx, list.State)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(delta.Updated) != 2 || delta.Updated[0] != "ext1" || delta.Updated[1] != "ext2" {
		t.Fatalf("updated ids wrong: %+v", delta.Updated)
	}
	if delta.NewState == list.State {
		t.Fatal("new state must advance")
	}

	// Fetch the changed ids, then destroy one and see it in the delta.
	got, err := c.Contacts(ctx)
	if err != nil || len(got.Contacts) != 2 {
		t.Fatalf("refetch: %v (n=%d)", err, len(got.Contacts))
	}
	srv.DestroyContacts("ext1")
	after, err := c.ContactChanges(ctx, delta.NewState)
	if err != nil {
		t.Fatalf("changes after destroy: %v", err)
	}
	if len(after.Destroyed) != 1 || after.Destroyed[0] != "ext1" {
		t.Fatalf("destroyed wrong: %+v", after.Destroyed)
	}

	// cannotCalculateChanges travels as the mail sentinel (FR-B5).
	srv.SetCannotCalculateChanges(true)
	if _, err := c.ContactChanges(ctx, after.NewState); !errors.Is(err, mail.ErrCannotCalculateChanges) {
		t.Fatalf("got %v, want ErrCannotCalculateChanges", err)
	}
}

// TestAddressBookChangesDelta covers the book journal.
func TestAddressBookChangesDelta(t *testing.T) {
	c, srv := newContactsClient(t)
	ctx := context.Background()
	books, err := c.AddressBooks(ctx)
	if err != nil {
		t.Fatalf("AddressBooks: %v", err)
	}
	srv.SetAddressBooks([]mockjmap.AddressBook{
		{ID: "ab1", Name: "Address Book", IsDefault: true, IsSubscribed: true, SortOrder: 0},
		{ID: "ab2", Name: "Work", SortOrder: 1, IsSubscribed: true},
	})
	delta, err := c.AddressBookChanges(ctx, books.State)
	if err != nil {
		t.Fatalf("AddressBook/changes: %v", err)
	}
	if len(delta.Updated) != 2 {
		t.Fatalf("updated books wrong: %+v", delta.Updated)
	}
	again, err := c.AddressBooks(ctx)
	if err != nil || len(again.Books) != 2 {
		t.Fatalf("refetch books: %v (n=%d)", err, len(again.Books))
	}
	if again.Books[0].ID != "ab1" || again.Books[1].ID != "ab2" {
		t.Fatalf("sort order wrong: %+v", again.Books)
	}
}
