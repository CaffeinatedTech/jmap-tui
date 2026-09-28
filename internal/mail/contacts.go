package mail

import (
	"context"
	"strings"
)

// Contacts are modelled after RFC 9610 (JMAP for Contacts) but stay
// provider-agnostic: the UI and sync layers never see JSContact wire shapes
// (golden rule 3). Only the summary subset the contacts screen and the
// composer's suggestion path need is held in memory; nothing here is ever
// written to disk (NFR-4, FR-L5).

// AddressBook is a named collection of contacts (RFC 9610 §2). Sharing
// fields (shareWith/myRights) are deliberately absent: address-book sharing
// is out of scope (REQUIREMENTS FR-L).
type AddressBook struct {
	ID           ID
	Name         string
	SortOrder    int
	IsDefault    bool
	IsSubscribed bool
}

// ContactEmail is one email address of a contact (JSContact emails entry).
type ContactEmail struct {
	Address string
	Label   string
}

// ContactPhone is one phone number of a contact (JSContact phones entry).
type ContactPhone struct {
	Number string
	Label  string
}

// ContactEntry is one keyed entry of a JSContact collection the contact form
// does not fully own (organizations, titles, notes). Key is the server's map
// key — kept so an edit path-patches just this entry and leaves siblings
// untouched (RFC 8620 §5.3); Value is the entry's display string.
type ContactEntry struct {
	Key   string
	Value string
}

// Contact is the summary of one contact card (RFC 9610 §3). Cards with
// kind=group are dropped by the provider: groups are out of scope.
// Orgs/Titles/Notes hold every entry of their collection in server key
// order; the form edits the first one.
type Contact struct {
	ID             ID
	Kind           string
	GivenName      string
	Surname        string
	DisplayName    string
	AddressBookIDs []ID
	Emails         []ContactEmail
	Phones         []ContactPhone
	Orgs           []ContactEntry
	Titles         []ContactEntry
	Notes          []ContactEntry
}

// HasEmail reports whether the contact carries at least one usable address —
// the suggestion path never offers a card without one.
func (c Contact) HasEmail() bool {
	for _, e := range c.Emails {
		if e.Address != "" {
			return true
		}
	}
	return false
}

// SortKey is the contact's display identity for ordering and suggestions.
func (c Contact) SortKey() string {
	if c.DisplayName != "" {
		return strings.ToLower(c.DisplayName)
	}
	if len(c.Emails) > 0 {
		return strings.ToLower(c.Emails[0].Address)
	}
	return string(c.ID)
}

// ContactLess orders contacts for display: SortKey, then id for total
// stability. The single comparator both the provider (initial load) and the
// engine (live upserts) sort with, so ordering never drifts between them.
func ContactLess(a, b Contact) bool {
	if ka, kb := a.SortKey(), b.SortKey(); ka != kb {
		return ka < kb
	}
	return a.ID < b.ID
}

// AddressBookList is the full book list plus the AddressBook/get state
// string it was read at — the bootstrap value for AddressBook/changes.
type AddressBookList struct {
	Books []AddressBook
	State string
}

// ContactList is every contact card of an account plus the ContactCard/get
// state string it was read at (the bootstrap for ContactCard/changes).
type ContactList struct {
	Contacts []Contact
	State    string
}

// ContactChangeSet is one /changes delta for either contacts or address
// books (RFC 8620 §5.2). Updated holds created and modified ids alike.
type ContactChangeSet struct {
	Updated   []ID
	Destroyed []ID
	NewState  string
	HasMore   bool
}

// ContactDraft is the state of the contact form: the fields the form
// owns, before any preservation rules are applied at patch time.
// AddressBookIDs matters on create only — a card's book is not
// editable in v1.
type ContactDraft struct {
	GivenName string
	Surname   string
	Emails    []ContactEmail
	Phones    []ContactPhone
	Org       string
	Title     string
	Note      string

	// AddressBookIDs selects where a created card lands. Empty means the
	// account's default address book (resolved by the caller).
	AddressBookIDs []ID
}

// ContactUpdate is one updated card: Draft is the form state, Current is the
// card as loaded. The provider diffs them so untouched collections (and the
// sub-properties the form does not model, e.g. organization units) are
// omitted from the patch instead of clobbered (RFC 8620 §5.3 path patches).
type ContactUpdate struct {
	Draft   ContactDraft
	Current Contact
}

// ContactMutation is one batched ContactCard/set (RFC 9610 §3.5): a create
// handle map, per-id updates, and destroys — one round-trip for the whole
// action (FR-K4). The engine issues one mutation per user action, so undo
// bookkeeping stays one reversal.
type ContactMutation struct {
	Create  map[string]ContactDraft
	Update  map[ID]ContactUpdate
	Destroy []ID
}

// Empty reports whether the mutation would send nothing.
func (m ContactMutation) Empty() bool {
	return len(m.Create) == 0 && len(m.Update) == 0 && len(m.Destroy) == 0
}

// ContactMutationResult reports what SetContacts did. Rejections carry the
// server's per-id error so the caller can revert exactly the failed part.
type ContactMutationResult struct {
	OldState string
	NewState string

	// Created maps the caller's create handle to the server-assigned id.
	Created   map[string]ID
	Updated   []ID
	Destroyed []ID

	NotCreated   map[string]error
	NotUpdated   map[ID]error
	NotDestroyed map[ID]error
}

// ContactProvider is the optional contacts seam (RFC 9610, FR-L). It is a
// separate interface rather than part of Provider: contacts are not mail,
// a future IMAP provider may never grow them, and the sync layer type-
// asserts for it — absence means the feature is hidden, never fatal
// (FR-A6, FR-L6).
type ContactProvider interface {
	// ContactsSupported reports whether the server advertised
	// urn:ietf:params:jmap:contacts on the session (FR-L6).
	ContactsSupported() bool

	// AddressBooks returns every address book with its /changes bootstrap
	// state.
	AddressBooks(ctx context.Context) (AddressBookList, error)

	// Contacts returns every contact card (group cards excluded) with its
	// /changes bootstrap state.
	Contacts(ctx context.Context) (ContactList, error)

	// FetchContacts returns the named cards by id (missing ids and dropped
	// groups are simply absent). The reconcile path uses it to refresh
	// changed ids without refetching the whole account.
	FetchContacts(ctx context.Context, ids []ID) ([]Contact, error)

	// ContactChanges fetches the ContactCard delta since a state string.
	// A cannotCalculateChanges failure is wrapped with
	// ErrCannotCalculateChanges so the engine can full-refetch (FR-B5).
	ContactChanges(ctx context.Context, sinceState string) (ContactChangeSet, error)

	// AddressBookChanges fetches the AddressBook delta since a state
	// string.
	AddressBookChanges(ctx context.Context, sinceState string) (ContactChangeSet, error)

	// SetContacts applies one batched create/update/destroy and returns
	// the per-handle/per-id outcome.
	SetContacts(ctx context.Context, mutation ContactMutation) (ContactMutationResult, error)
}
