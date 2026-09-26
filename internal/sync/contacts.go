package sync

// Contacts (M9, FR-L): a per-engine in-memory contact store on its own
// publish channel — deliberately NOT part of the mail Snapshot, so the
// message-list path (applyView, ViewKey, windows) stays untouched
// (CONTACTS_PLAN §2.3). Lazy: nothing here runs until the composer or the
// contacts screen asks for it, then live push/changes keeps it warm.
// Nothing is ever written to disk (NFR-4, FR-L5).

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// ErrNoContacts is returned when the account's provider has no contacts
// seam or the server never advertised the capability (FR-L6). Callers
// degrade — the app toasts, the screen never opens — never crash.
var ErrNoContacts = errors.New("contacts are not supported by this account")

// ContactSnapshot is the immutable contacts view: address books plus every
// contact in display order, published on its own latest-wins channel.
type ContactSnapshot struct {
	// Version is the store's monotonic stamp; the app applies only newer
	// snapshots, mirroring the mail Snapshot rule.
	Version uint64

	// Loaded is false until the first successful LoadContacts — the UI
	// shows a loading hint (or waits) until then.
	Loaded bool

	// Err is the last contacts-specific failure (load or reconcile),
	// cleared by the next success. Mail sync status is unaffected.
	Err string

	Books    []mail.AddressBook
	Contacts []mail.Contact // display order (mail.ContactLess)
}

// contactProvider type-asserts the engine's provider for the contacts seam.
func (e *Engine) contactProvider() (mail.ContactProvider, bool) {
	cp, ok := e.p.(mail.ContactProvider)
	if !ok || !cp.ContactsSupported() {
		return nil, false
	}
	return cp, true
}

// ContactsSupported reports whether this account can do contacts at all
// (FR-L6): the provider implements the seam and the server advertised
// urn:ietf:params:jmap:contacts.
func (e *Engine) ContactsSupported() bool {
	_, ok := e.contactProvider()
	return ok
}

// ContactUpdates exposes the latest-wins contacts broadcast (the contacts
// twin of Updates). The app drains it from a Cmd, same as mail snapshots.
func (e *Engine) ContactUpdates() <-chan ContactSnapshot { return e.contactUpdates }

// Contacts returns the current contacts view without waiting on the
// channel (the contacts twin of Snapshot).
func (e *Engine) Contacts() ContactSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.contactSnapshotLocked()
}

// ContactDefaultBook resolves where a created card lands (RFC 9610 §2): the
// default address book, else the first — "" when the account has none (the
// caller surfaces that as an error, never a silent drop).
func (e *Engine) ContactDefaultBook() mail.ID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.defaultBookLocked()
}

func (e *Engine) defaultBookLocked() mail.ID {
	for _, b := range e.contactBooks {
		if b.IsDefault {
			return b.ID
		}
	}
	if len(e.contactBooks) > 0 {
		return e.contactBooks[0].ID
	}
	return ""
}

// LoadContacts fetches address books and cards once (lazy warm-up: first
// compose or first contacts-screen open) and publishes. Concurrent callers
// coalesce onto the in-flight load; later calls are no-ops — live changes
// keep the store fresh from here (FR-L1).
func (e *Engine) LoadContacts(ctx context.Context) error {
	cp, ok := e.contactProvider()
	if !ok {
		return ErrNoContacts
	}
	e.mu.Lock()
	if e.contactsLoaded || e.contactsLoading {
		e.mu.Unlock()
		return nil
	}
	e.contactsLoading = true
	e.mu.Unlock()

	err := e.fetchAndStoreContacts(ctx, cp)

	e.mu.Lock()
	e.contactsLoading = false
	if err != nil {
		e.contactErr = err.Error()
	}
	e.mu.Unlock()
	return err
}

// fetchAndStoreContacts performs the network fetch and swaps the store in.
// On failure the previous data (if any) stays put. Caller must hold no
// lock; it takes e.mu for the swap.
func (e *Engine) fetchAndStoreContacts(ctx context.Context, cp mail.ContactProvider) error {
	books, err := cp.AddressBooks(ctx)
	if err != nil {
		return fmt.Errorf("sync: load address books: %w", err)
	}
	cards, err := cp.Contacts(ctx)
	if err != nil {
		return fmt.Errorf("sync: load contacts: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.contactBooks = books.Books
	e.bookState = books.State
	e.contactState = cards.State
	e.contacts = make(map[mail.ID]mail.Contact, len(cards.Contacts))
	for _, c := range cards.Contacts {
		e.contacts[c.ID] = c
	}
	e.rebuildContactOrderLocked()
	e.contactsLoaded = true
	e.contactErr = ""
	e.publishContactsLocked()
	return nil
}

// SetContact applies one user action (create/edit/delete) as a single
// batched ContactCard/set (FR-K4), then refreshes the touched ids from the
// server so the store always holds server truth — confirmed-then-patch, no
// client-side guesswork (FR-L2). The mutation's newState folds into the
// store state so the next /changes never replays our own writes
// (PLAN §4.2).
func (e *Engine) SetContact(ctx context.Context, mutation mail.ContactMutation) (mail.ContactMutationResult, error) {
	cp, ok := e.contactProvider()
	if !ok {
		return mail.ContactMutationResult{}, ErrNoContacts
	}
	// A save implies a warm store (the screen/composer loaded it); self-
	// heal if a caller beat us here.
	if err := e.LoadContacts(ctx); err != nil {
		return mail.ContactMutationResult{}, err
	}
	mutation, err := e.withDefaultBooks(mutation)
	if err != nil {
		return mail.ContactMutationResult{}, err
	}

	res, err := cp.SetContacts(ctx, mutation)
	if err != nil {
		e.setContactErr(err)
		return mail.ContactMutationResult{}, err
	}

	// Refresh every id the server touched: creates need their assigned id
	// resolved to full data, updates need the patched card back, destroys
	// just drop locally.
	var touched []mail.ID
	for _, id := range res.Created {
		touched = append(touched, id)
	}
	touched = append(touched, res.Updated...)
	fresh, err := cp.FetchContacts(ctx, touched)
	if err != nil {
		e.setContactErr(err)
		return res, fmt.Errorf("sync: refresh saved contact: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range res.Destroyed {
		delete(e.contacts, id)
	}
	// Ids requested but absent are gone (destroyed mid-flight) or not
	// modelled (a group): drop them rather than keep a stale row.
	freshByID := make(map[mail.ID]mail.Contact, len(fresh))
	for _, c := range fresh {
		freshByID[c.ID] = c
	}
	for _, id := range touched {
		if _, ok := freshByID[id]; !ok {
			delete(e.contacts, id)
		}
	}
	for _, c := range fresh {
		e.contacts[c.ID] = c
	}
	if res.NewState != "" {
		e.contactState = res.NewState
	}
	e.rebuildContactOrderLocked()
	e.contactErr = ""
	e.publishContactsLocked()
	return res, nil
}

// withDefaultBooks resolves each create's target book (RFC 9610 §2): the
// account's default when the form didn't choose, and a clear error when the
// account has no book at all — a card must always belong to one.
func (e *Engine) withDefaultBooks(mutation mail.ContactMutation) (mail.ContactMutation, error) {
	needsDefault := false
	for _, d := range mutation.Create {
		if len(d.AddressBookIDs) == 0 {
			needsDefault = true
			break
		}
	}
	if !needsDefault {
		return mutation, nil
	}
	e.mu.Lock()
	def := e.defaultBookLocked()
	e.mu.Unlock()
	if def == "" {
		return mail.ContactMutation{}, errors.New("sync: this account has no address book to create contacts in")
	}
	// Copy before filling: the caller's map stays theirs.
	out := mail.ContactMutation{
		Create:  make(map[string]mail.ContactDraft, len(mutation.Create)),
		Update:  mutation.Update,
		Destroy: mutation.Destroy,
	}
	for handle, d := range mutation.Create {
		if len(d.AddressBookIDs) == 0 {
			d.AddressBookIDs = []mail.ID{def}
		}
		out.Create[handle] = d
	}
	return out, nil
}

// reconcileContacts folds ContactCard/changes deltas into the store
// (FR-L1): updated ids refetch, destroyed ids evict, and a store whose
// state the server can no longer answer reloads wholesale (FR-B5's
// cannotCalculateChanges rule applied to contacts). A no-op before the
// first load — nothing to keep fresh yet.
func (e *Engine) reconcileContacts(ctx context.Context) {
	cp, ok := e.contactProvider()
	if !ok {
		return
	}
	e.mu.Lock()
	loaded, since := e.contactsLoaded, e.contactState
	e.mu.Unlock()
	if !loaded || since == "" {
		return // not warm yet; the next open loads fresh
	}

	var all mail.ContactChangeSet
	state := since
	for i := 0; i < maxChangeLoops; i++ {
		set, err := cp.ContactChanges(ctx, state)
		if err != nil {
			if errors.Is(err, mail.ErrCannotCalculateChanges) {
				e.reloadContacts(ctx, cp)
				return
			}
			e.setContactErr(err)
			return
		}
		all.Updated = append(all.Updated, set.Updated...)
		all.Destroyed = append(all.Destroyed, set.Destroyed...)
		all.NewState = set.NewState
		if !set.HasMore {
			break
		}
		state = set.NewState
	}
	if all.NewState == since && len(all.Updated) == 0 && len(all.Destroyed) == 0 {
		return // nothing moved
	}

	var fresh []mail.Contact
	if len(all.Updated) > 0 {
		var err error
		fresh, err = cp.FetchContacts(ctx, all.Updated)
		if err != nil {
			// Without the data, applying the delta would delete live
			// rows: keep the store and let the next event retry.
			e.setContactErr(err)
			return
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range all.Destroyed {
		delete(e.contacts, id)
	}
	freshByID := make(map[mail.ID]mail.Contact, len(fresh))
	for _, c := range fresh {
		freshByID[c.ID] = c
	}
	for _, id := range all.Updated {
		if _, ok := freshByID[id]; !ok {
			delete(e.contacts, id)
		}
	}
	for _, c := range fresh {
		e.contacts[c.ID] = c
	}
	e.contactState = all.NewState
	e.rebuildContactOrderLocked()
	e.contactErr = ""
	e.publishContactsLocked()
}

// reconcileAddressBooks refreshes the book list from AddressBook/changes —
// books change rarely (CardDAV edits), so a refetch beats a delta fold.
func (e *Engine) reconcileAddressBooks(ctx context.Context) {
	cp, ok := e.contactProvider()
	if !ok {
		return
	}
	e.mu.Lock()
	loaded := e.contactsLoaded
	e.mu.Unlock()
	if !loaded {
		return
	}
	books, err := cp.AddressBooks(ctx)
	if err != nil {
		e.setContactErr(err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.contactBooks = books.Books
	e.bookState = books.State
	e.contactErr = ""
	e.publishContactsLocked()
}

// reloadContacts re-fetches everything after cannotCalculateChanges,
// keeping the old rows visible until the new set lands.
func (e *Engine) reloadContacts(ctx context.Context, cp mail.ContactProvider) {
	if err := e.fetchAndStoreContacts(ctx, cp); err != nil {
		e.setContactErr(err)
	}
}

// setContactErr records a contacts-specific failure and republishes so the
// screen can show it. It never touches the mail sync status: a contacts
// hiccup must not claim the mailbox is broken.
func (e *Engine) setContactErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.contactErr = err.Error()
	e.publishContactsLocked()
}

// rebuildContactOrderLocked re-derives the display order with the one
// shared comparator. Caller holds mu.
func (e *Engine) rebuildContactOrderLocked() {
	order := make([]mail.ID, 0, len(e.contacts))
	for id := range e.contacts {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool {
		return mail.ContactLess(e.contacts[order[i]], e.contacts[order[j]])
	})
	e.contactOrder = order
}

// publishContactsLocked bumps the contacts version and broadcasts the
// newest snapshot (latest wins, the mail publish pattern).
func (e *Engine) publishContactsLocked() {
	e.contactVersion++
	contacts := make([]mail.Contact, 0, len(e.contactOrder))
	for _, id := range e.contactOrder {
		if c, ok := e.contacts[id]; ok {
			contacts = append(contacts, c)
		}
	}
	snap := ContactSnapshot{
		Version:  e.contactVersion,
		Loaded:   e.contactsLoaded,
		Err:      e.contactErr,
		Books:    e.contactBooks,
		Contacts: contacts,
	}
	select {
	case <-e.contactUpdates:
	default:
	}
	select {
	case e.contactUpdates <- snap:
	default:
	}
}

// contactSnapshotLocked builds the current view. Caller holds mu.
func (e *Engine) contactSnapshotLocked() ContactSnapshot {
	contacts := make([]mail.Contact, 0, len(e.contactOrder))
	for _, id := range e.contactOrder {
		if c, ok := e.contacts[id]; ok {
			contacts = append(contacts, c)
		}
	}
	return ContactSnapshot{
		Version:  e.contactVersion,
		Loaded:   e.contactsLoaded,
		Err:      e.contactErr,
		Books:    e.contactBooks,
		Contacts: contacts,
	}
}

// contactsWarm reports whether the live loop should reconcile contact
// types (used by reconcileTypes/pollOnce — zero cost when the feature has
// never been opened).
func (e *Engine) contactsWarm() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.contactsLoaded
}
