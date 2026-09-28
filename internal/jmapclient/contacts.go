package jmapclient

// Contacts implements RFC 9610 (JMAP for Contacts) on top of the wrapper:
// hand-rolled method types plus JSContact (RFC 9553) wire structs, because
// go-jmap ships no contacts package (PLAN §9 — extend the wrapper, not the
// app). The wire shape stays here: above this layer everything speaks
// internal/mail types (golden rule 3).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	jmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// contactsURI is the RFC 9610 capability. go-jmap has no constant for it.
const contactsURI = jmap.URI("urn:ietf:params:jmap:contacts")

// contactCardProperties is the ContactCard/get subset the UI needs (FR-D4
// analogue): the summary/list fields and everything the contact form owns.
// uid/created/updated are deliberately absent — the live Stalwart does not
// return them and nothing sorts by them.
var contactCardProperties = []string{
	"id", "addressBookIds", "kind", "name",
	"emails", "phones", "organizations", "titles", "notes",
}

// registerContacts installs the response decoders for every contacts method.
// go-jmap rejects any unregistered method name during response unmarshal
// (invocation.go), so each method we call must be registered once. Idempotent;
// called at the top of every contacts method (no init() side effects).
func registerContacts() {
	registerContactsOnce.Do(func() {
		jmap.RegisterMethod("AddressBook/get", func() jmap.MethodResponse { return &addressBookGetResponse{} })
		jmap.RegisterMethod("AddressBook/changes", func() jmap.MethodResponse { return &changesResponse{} })
		jmap.RegisterMethod("ContactCard/get", func() jmap.MethodResponse { return &contactCardGetResponse{} })
		jmap.RegisterMethod("ContactCard/changes", func() jmap.MethodResponse { return &changesResponse{} })
		jmap.RegisterMethod("ContactCard/set", func() jmap.MethodResponse { return &flexibleSetResponse{} })
	})
}

var registerContactsOnce sync.Once

// --- method requests ---

type addressBookGet struct {
	Account jmap.ID   `json:"accountId,omitempty"`
	IDs     []jmap.ID `json:"ids,omitempty"`
}

func (m *addressBookGet) Name() string         { return "AddressBook/get" }
func (m *addressBookGet) Requires() []jmap.URI { return []jmap.URI{contactsURI} }

type contactCardGet struct {
	Account    jmap.ID   `json:"accountId,omitempty"`
	IDs        []jmap.ID `json:"ids,omitempty"`
	Properties []string  `json:"properties,omitempty"`
}

func (m *contactCardGet) Name() string         { return "ContactCard/get" }
func (m *contactCardGet) Requires() []jmap.URI { return []jmap.URI{contactsURI} }

// changesRequest is shared wire shape for {AddressBook,ContactCard}/changes;
// the method name lives on the struct so Name() can differ.
type changesRequest struct {
	Account    jmap.ID `json:"accountId,omitempty"`
	SinceState string  `json:"sinceState,omitempty"`
	MaxChanges uint64  `json:"maxChanges,omitempty"`
}

type addressBookChanges changesRequest

func (m *addressBookChanges) Name() string         { return "AddressBook/changes" }
func (m *addressBookChanges) Requires() []jmap.URI { return []jmap.URI{contactsURI} }

type contactCardChanges changesRequest

func (m *contactCardChanges) Name() string         { return "ContactCard/changes" }
func (m *contactCardChanges) Requires() []jmap.URI { return []jmap.URI{contactsURI} }

type contactCardSet struct {
	Account jmap.ID                `json:"accountId,omitempty"`
	Create  map[string]any         `json:"create,omitempty"`
	Update  map[jmap.ID]jmap.Patch `json:"update,omitempty"`
	Destroy []jmap.ID              `json:"destroy,omitempty"`
}

func (m *contactCardSet) Name() string         { return "ContactCard/set" }
func (m *contactCardSet) Requires() []jmap.URI { return []jmap.URI{contactsURI} }

// --- method responses ---

type addressBookGetResponse struct {
	Account  jmap.ID          `json:"accountId,omitempty"`
	State    string           `json:"state,omitempty"`
	List     []*jsAddressBook `json:"list,omitempty"`
	NotFound []jmap.ID        `json:"notFound,omitempty"`
}

type contactCardGetResponse struct {
	Account  jmap.ID   `json:"accountId,omitempty"`
	State    string    `json:"state,omitempty"`
	List     []*jsCard `json:"list,omitempty"`
	NotFound []jmap.ID `json:"notFound,omitempty"`
}

// changesResponse is the RFC 8620 §5.2 delta shape, identical for both
// contacts data types.
type changesResponse struct {
	Account        jmap.ID   `json:"accountId,omitempty"`
	OldState       string    `json:"oldState,omitempty"`
	NewState       string    `json:"newState,omitempty"`
	HasMoreChanges bool      `json:"hasMoreChanges,omitempty"`
	Created        []jmap.ID `json:"created,omitempty"`
	Updated        []jmap.ID `json:"updated,omitempty"`
	Destroyed      []jmap.ID `json:"destroyed,omitempty"`
}

// --- JSContact wire structs (RFC 9553 subset) ---

type jsAddressBook struct {
	ID           jmap.ID `json:"id"`
	Name         string  `json:"name"`
	SortOrder    uint    `json:"sortOrder,omitempty"`
	IsDefault    bool    `json:"isDefault,omitempty"`
	IsSubscribed *bool   `json:"isSubscribed,omitempty"`
}

type jsNameComponent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type jsName struct {
	Components []jsNameComponent `json:"components"`
	Full       string            `json:"full"`
	IsOrdered  bool              `json:"isOrdered"`
}

type jsEntry struct {
	// EmailAddress / Phone / Organization / Title / Note payloads share the
	// fields we read; absent JSON keys stay zero.
	Address string `json:"address"`
	Number  string `json:"number"`
	Name    string `json:"name"`
	Note    string `json:"note"`
	Label   string `json:"label"`
	Pref    *uint  `json:"pref"`
}

type jsCard struct {
	ID             jmap.ID            `json:"id"`
	AddressBookIDs map[jmap.ID]bool   `json:"addressBookIds"`
	Kind           string             `json:"kind"`
	Name           *jsName            `json:"name"`
	Emails         map[string]jsEntry `json:"emails"`
	Phones         map[string]jsEntry `json:"phones"`
	Organizations  map[string]jsEntry `json:"organizations"`
	Titles         map[string]jsEntry `json:"titles"`
	Notes          map[string]jsEntry `json:"notes"`
}

// --- client methods ---

// ContactsSupported implements mail.ContactProvider: whether the session
// advertised urn:ietf:params:jmap:contacts (FR-L6).
func (c *Client) ContactsSupported() bool {
	if c.session == nil {
		return false
	}
	_, ok := c.session.RawCapabilities[contactsURI]
	return ok
}

// AddressBooks implements mail.ContactProvider: AddressBook/get with a null
// ids argument fetches every book in one call (RFC 9610 §2.1, Figure 1).
func (c *Client) AddressBooks(ctx context.Context) (mail.AddressBookList, error) {
	registerContacts()
	if err := c.requireContacts(); err != nil {
		return mail.AddressBookList{}, err
	}
	inv, err := c.do(ctx, &addressBookGet{Account: jmap.ID(c.accountID)})
	if err != nil {
		return mail.AddressBookList{}, fmt.Errorf("jmapclient: AddressBook/get: %w", err)
	}
	gr, ok := inv.Args.(*addressBookGetResponse)
	if !ok {
		return mail.AddressBookList{}, fmt.Errorf("jmapclient: AddressBook/get: unexpected response type %T", inv.Args)
	}
	out := make([]mail.AddressBook, 0, len(gr.List))
	for _, b := range gr.List {
		if b == nil {
			continue
		}
		out = append(out, convertAddressBook(b))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return mail.AddressBookList{Books: out, State: gr.State}, nil
}

// Contacts implements mail.ContactProvider: one batched ContactCard/get
// with the summary property subset. Group cards are dropped (out of
// scope). No ContactCard/query — sort and filter happen client side.
func (c *Client) Contacts(ctx context.Context) (mail.ContactList, error) {
	registerContacts()
	if err := c.requireContacts(); err != nil {
		return mail.ContactList{}, err
	}
	inv, err := c.do(ctx, &contactCardGet{
		Account:    jmap.ID(c.accountID),
		Properties: contactCardProperties,
	})
	if err != nil {
		return mail.ContactList{}, fmt.Errorf("jmapclient: ContactCard/get: %w", err)
	}
	gr, ok := inv.Args.(*contactCardGetResponse)
	if !ok {
		return mail.ContactList{}, fmt.Errorf("jmapclient: ContactCard/get: unexpected response type %T", inv.Args)
	}
	out := make([]mail.Contact, 0, len(gr.List))
	for _, card := range gr.List {
		if card == nil {
			continue
		}
		contact, keep := convertContact(card)
		if keep {
			out = append(out, contact)
		}
	}
	sortContacts(out)
	return mail.ContactList{Contacts: out, State: gr.State}, nil
}

// FetchContacts implements mail.ContactProvider: ContactCard/get for the
// named ids. Ids the server does not know (and group cards) are absent from
// the result, never an error.
func (c *Client) FetchContacts(ctx context.Context, ids []mail.ID) ([]mail.Contact, error) {
	registerContacts()
	if err := c.requireContacts(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	inv, err := c.do(ctx, &contactCardGet{
		Account:    jmap.ID(c.accountID),
		IDs:        jmapIDs(ids),
		Properties: contactCardProperties,
	})
	if err != nil {
		return nil, fmt.Errorf("jmapclient: ContactCard/get: %w", err)
	}
	gr, ok := inv.Args.(*contactCardGetResponse)
	if !ok {
		return nil, fmt.Errorf("jmapclient: ContactCard/get: unexpected response type %T", inv.Args)
	}
	out := make([]mail.Contact, 0, len(gr.List))
	for _, card := range gr.List {
		if card == nil {
			continue
		}
		if contact, keep := convertContact(card); keep {
			out = append(out, contact)
		}
	}
	sortContacts(out)
	return out, nil
}

// ContactChanges implements mail.ContactProvider (ContactCard/changes).
func (c *Client) ContactChanges(ctx context.Context, sinceState string) (mail.ContactChangeSet, error) {
	registerContacts()
	if err := c.requireContacts(); err != nil {
		return mail.ContactChangeSet{}, err
	}
	inv, err := c.do(ctx, &contactCardChanges{Account: jmap.ID(c.accountID), SinceState: sinceState})
	if err != nil {
		return mail.ContactChangeSet{}, wrapChangesErr("ContactCard/changes", err)
	}
	return convertChanges(inv, "ContactCard/changes")
}

// AddressBookChanges implements mail.ContactProvider (AddressBook/changes).
func (c *Client) AddressBookChanges(ctx context.Context, sinceState string) (mail.ContactChangeSet, error) {
	registerContacts()
	if err := c.requireContacts(); err != nil {
		return mail.ContactChangeSet{}, err
	}
	inv, err := c.do(ctx, &addressBookChanges{Account: jmap.ID(c.accountID), SinceState: sinceState})
	if err != nil {
		return mail.ContactChangeSet{}, wrapChangesErr("AddressBook/changes", err)
	}
	return convertChanges(inv, "AddressBook/changes")
}

// SetContacts implements mail.ContactProvider: one batched ContactCard/set
// carrying the whole user action (FR-K4). Updates travel as patches so
// properties outside the form (photos, addresses, …) are never touched.
func (c *Client) SetContacts(ctx context.Context, mutation mail.ContactMutation) (mail.ContactMutationResult, error) {
	registerContacts()
	if err := c.requireContacts(); err != nil {
		return mail.ContactMutationResult{}, err
	}
	if mutation.Empty() {
		return mail.ContactMutationResult{}, nil
	}
	set := &contactCardSet{Account: jmap.ID(c.accountID)}
	if len(mutation.Create) > 0 {
		set.Create = make(map[string]any, len(mutation.Create))
		for handle, draft := range mutation.Create {
			if len(draft.AddressBookIDs) == 0 {
				return mail.ContactMutationResult{}, fmt.Errorf("jmapclient: contact %q has no address book", handle)
			}
			set.Create[handle] = contactCreateObject(draft)
		}
	}
	if len(mutation.Update) > 0 {
		set.Update = make(map[jmap.ID]jmap.Patch, len(mutation.Update))
		for id, upd := range mutation.Update {
			patch := contactUpdatePatch(upd)
			if len(patch) == 0 {
				continue // nothing changed — nothing to send
			}
			set.Update[jmap.ID(id)] = patch
		}
		if len(set.Update) == 0 && set.Create == nil && len(set.Destroy) == 0 {
			return mail.ContactMutationResult{}, nil
		}
	}
	if len(mutation.Destroy) > 0 {
		set.Destroy = jmapIDs(mutation.Destroy)
	}

	inv, err := c.do(ctx, set)
	if err != nil {
		return mail.ContactMutationResult{}, fmt.Errorf("jmapclient: ContactCard/set: %w", err)
	}
	sr, ok := inv.Args.(*flexibleSetResponse)
	if !ok {
		return mail.ContactMutationResult{}, fmt.Errorf("jmapclient: ContactCard/set: unexpected response type %T", inv.Args)
	}
	return convertContactSetResponse(sr), nil
}

// requireContacts guards every contacts call: not connected, or the server
// never advertised the capability (FR-A6 — degrade, never fatal).
func (c *Client) requireContacts() error {
	if c.session == nil {
		return errors.New("jmapclient: not connected")
	}
	if !c.ContactsSupported() {
		return fmt.Errorf("jmapclient: server does not support contacts (%s): %w", contactsURI, ErrNotSupported)
	}
	return nil
}

// --- conversion ---

func convertAddressBook(b *jsAddressBook) mail.AddressBook {
	subscribed := true
	if b.IsSubscribed != nil {
		subscribed = *b.IsSubscribed
	}
	return mail.AddressBook{
		ID:           mail.ID(b.ID),
		Name:         b.Name,
		SortOrder:    int(b.SortOrder),
		IsDefault:    b.IsDefault,
		IsSubscribed: subscribed,
	}
}

// convertContact maps one JSContact card into UI terms. Reports keep=false
// for cards the UI does not model (groups).
func convertContact(card *jsCard) (mail.Contact, bool) {
	if card.Kind == "group" {
		return mail.Contact{}, false
	}
	books := make([]mail.ID, 0, len(card.AddressBookIDs))
	for id, on := range card.AddressBookIDs {
		if on {
			books = append(books, mail.ID(id))
		}
	}
	sort.Slice(books, func(i, j int) bool { return books[i] < books[j] })

	c := mail.Contact{
		ID:             mail.ID(card.ID),
		Kind:           card.Kind,
		AddressBookIDs: books,
		Emails:         convertEmails(card.Emails),
		Phones:         convertPhones(card.Phones),
		Orgs:           convertEntries(card.Organizations, entryName),
		Titles:         convertEntries(card.Titles, entryName),
		Notes:          convertEntries(card.Notes, entryNote),
	}
	if card.Name != nil {
		for _, comp := range card.Name.Components {
			switch comp.Kind {
			case "given":
				if c.GivenName == "" {
					c.GivenName = comp.Value
				}
			case "surname":
				if c.Surname == "" {
					c.Surname = comp.Value
				}
			}
		}
		c.DisplayName = strings.TrimSpace(card.Name.Full)
		if c.DisplayName == "" {
			c.DisplayName = joinName(c.GivenName, c.Surname)
		}
	}
	return c, true
}

// joinName composes the display fallback from the parsed components.
func joinName(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

func entryName(e jsEntry) string { return e.Name }
func entryNote(e jsEntry) string { return e.Note }

// convertEmails orders by JSContact pref (1 = most preferred; absent = least)
// with the map key as tiebreak — map iteration order must never leak into
// the UI (RFC 9553 §1.5.3).
func convertEmails(in map[string]jsEntry) []mail.ContactEmail {
	keys := sortedKeys(in, func(e jsEntry) int {
		if e.Pref == nil {
			return 101
		}
		return int(*e.Pref)
	})
	out := make([]mail.ContactEmail, 0, len(keys))
	for _, k := range keys {
		e := in[k]
		if e.Address == "" {
			continue
		}
		out = append(out, mail.ContactEmail{Address: e.Address, Label: e.Label})
	}
	return out
}

func convertPhones(in map[string]jsEntry) []mail.ContactPhone {
	keys := sortedKeys(in, func(e jsEntry) int {
		if e.Pref == nil {
			return 101
		}
		return int(*e.Pref)
	})
	out := make([]mail.ContactPhone, 0, len(keys))
	for _, k := range keys {
		e := in[k]
		if e.Number == "" {
			continue
		}
		out = append(out, mail.ContactPhone{Number: e.Number, Label: e.Label})
	}
	return out
}

// convertEntries keeps every keyed entry (not just the first) so a later
// edit can path-patch one entry without clobbering its siblings.
func convertEntries(in map[string]jsEntry, value func(jsEntry) string) []mail.ContactEntry {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]mail.ContactEntry, 0, len(keys))
	for _, k := range keys {
		v := strings.TrimSpace(value(in[k]))
		if v == "" {
			continue
		}
		out = append(out, mail.ContactEntry{Key: k, Value: v})
	}
	return out
}

// sortedKeys returns the map's keys ordered by rank then key itself.
func sortedKeys[V any](in map[string]V, rank func(V) int) []string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := rank(in[keys[i]]), rank(in[keys[j]])
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	return keys
}

// sortContacts orders the list the contacts screen renders with the one
// shared comparator (mail.ContactLess), so provider order and engine
// re-order after a live upsert can never drift.
func sortContacts(contacts []mail.Contact) {
	sort.Slice(contacts, func(i, j int) bool {
		return mail.ContactLess(contacts[i], contacts[j])
	})
}

func convertChanges(inv *jmap.Invocation, call string) (mail.ContactChangeSet, error) {
	cr, ok := inv.Args.(*changesResponse)
	if !ok {
		return mail.ContactChangeSet{}, fmt.Errorf("jmapclient: %s: unexpected response type %T", call, inv.Args)
	}
	return mail.ContactChangeSet{
		Updated:   convertIDs(append(append([]jmap.ID{}, cr.Created...), cr.Updated...)),
		Destroyed: convertIDs(cr.Destroyed),
		NewState:  cr.NewState,
		HasMore:   cr.HasMoreChanges,
	}, nil
}

// convertContactSetResponse maps the shared RFC 8620 §5.3 response into
// contact terms. Created handles decode from raw: Stalwart returns only
// the assigned id.
func convertContactSetResponse(sr *flexibleSetResponse) mail.ContactMutationResult {
	out := mail.ContactMutationResult{
		OldState:     sr.OldState,
		NewState:     sr.NewState,
		Created:      make(map[string]mail.ID, len(sr.Created)),
		NotCreated:   make(map[string]error, len(sr.NotCreated)),
		NotUpdated:   make(map[mail.ID]error, len(sr.NotUpdated)),
		NotDestroyed: make(map[mail.ID]error, len(sr.NotDestroyed)),
	}
	for handle, raw := range sr.Created {
		var withID struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &withID) == nil && withID.ID != "" {
			out.Created[handle] = mail.ID(withID.ID)
		}
	}
	out.Updated = append(out.Updated, sr.Updated...)
	sort.Slice(out.Updated, func(i, j int) bool { return out.Updated[i] < out.Updated[j] })
	out.Destroyed = convertIDs(sr.Destroyed)
	sort.Slice(out.Destroyed, func(i, j int) bool { return out.Destroyed[i] < out.Destroyed[j] })
	for handle, se := range sr.NotCreated {
		out.NotCreated[handle] = setErr(se)
	}
	for id, se := range sr.NotUpdated {
		out.NotUpdated[mail.ID(id)] = setErr(se)
	}
	for id, se := range sr.NotDestroyed {
		out.NotDestroyed[mail.ID(id)] = setErr(se)
	}
	return out
}

// --- create / patch construction ---

// contactCreateObject builds the JSContact card for a create. Only
// non-empty fields travel; the server generates id and uid.
func contactCreateObject(draft mail.ContactDraft) map[string]any {
	obj := map[string]any{
		"addressBookIds": addressBookSet(draft.AddressBookIDs),
		"kind":           "individual",
	}
	if name := jsNameObject(draft.GivenName, draft.Surname); name != nil {
		obj["name"] = name
	}
	if emails := emailMap(draft.Emails); emails != nil {
		obj["emails"] = emails
	}
	if phones := phoneMap(draft.Phones); phones != nil {
		obj["phones"] = phones
	}
	if org := strings.TrimSpace(draft.Org); org != "" {
		obj["organizations"] = map[string]any{"0": map[string]any{"name": org}}
	}
	if title := strings.TrimSpace(draft.Title); title != "" {
		obj["titles"] = map[string]any{"0": map[string]any{"name": title}}
	}
	if note := strings.TrimSpace(draft.Note); note != "" {
		obj["notes"] = map[string]any{"0": map[string]any{"note": note}}
	}
	return obj
}

// contactUpdatePatch diffs form state against the loaded card so untouched
// collections — and every property the form does not model — are omitted
// from the patch (RFC 8620 §5.3). Path patches keep sibling entries alive.
func contactUpdatePatch(u mail.ContactUpdate) jmap.Patch {
	cur, draft := u.Current, u.Draft
	patch := jmap.Patch{}

	if draft.GivenName != cur.GivenName || draft.Surname != cur.Surname {
		if draft.GivenName == "" && draft.Surname == "" {
			patch["name"] = nil
		} else {
			patch["name"] = jsNameObject(draft.GivenName, draft.Surname)
		}
	}
	if !emailsEqual(draft.Emails, cur.Emails) {
		if len(draft.Emails) == 0 {
			patch["emails"] = nil
		} else {
			patch["emails"] = emailMap(draft.Emails)
		}
	}
	if !phonesEqual(draft.Phones, cur.Phones) {
		if len(draft.Phones) == 0 {
			patch["phones"] = nil
		} else {
			patch["phones"] = phoneMap(draft.Phones)
		}
	}
	patchEntry(patch, "organizations", cur.Orgs, strings.TrimSpace(draft.Org), "name")
	patchEntry(patch, "titles", cur.Titles, strings.TrimSpace(draft.Title), "name")
	patchEntry(patch, "notes", cur.Notes, strings.TrimSpace(draft.Note), "note")
	return patch
}

// patchEntry writes the minimal patch for a keyed collection the form
// edits entry-by-entry: nothing when unchanged, a path patch when the first
// entry changed (siblings untouched), a whole-property write when creating
// or clearing the only entry.
func patchEntry(patch jmap.Patch, prop string, current []mail.ContactEntry, want, valueKey string) {
	have := ""
	if len(current) > 0 {
		have = current[0].Value
	}
	if have == want {
		return // untouched: sub-fields (org units) and siblings survive
	}
	if want == "" {
		if len(current) <= 1 {
			patch[prop] = nil
		} else {
			patch[prop+"/"+current[0].Key] = nil
		}
		return
	}
	value := map[string]any{valueKey: want}
	if len(current) == 0 {
		patch[prop] = map[string]any{"0": value}
		return
	}
	patch[prop+"/"+current[0].Key] = value
}

// jsNameObject builds the JSContact name for the form's two fields. A blank
// name sends null (name is optional). Rebuilding drops components the form
// does not model (honorifics) — documented in FR-L.
func jsNameObject(given, surname string) any {
	given, surname = strings.TrimSpace(given), strings.TrimSpace(surname)
	if given == "" && surname == "" {
		return nil
	}
	comps := make([]any, 0, 2)
	if given != "" {
		comps = append(comps, map[string]any{"kind": "given", "value": given})
	}
	if surname != "" {
		comps = append(comps, map[string]any{"kind": "surname", "value": surname})
	}
	return map[string]any{
		"components": comps,
		"full":       joinName(given, surname),
		"isOrdered":  true,
	}
}

func addressBookSet(ids []mail.ID) map[string]any {
	out := make(map[string]any, len(ids))
	for _, id := range ids {
		out[string(id)] = true
	}
	return out
}

func emailMap(addrs []mail.ContactEmail) map[string]any {
	if len(addrs) == 0 {
		return nil
	}
	out := make(map[string]any, len(addrs))
	for i, a := range addrs {
		e := map[string]any{"address": a.Address}
		if a.Label != "" {
			e["label"] = a.Label
		}
		out[fmt.Sprintf("%d", i)] = e
	}
	return out
}

func phoneMap(nums []mail.ContactPhone) map[string]any {
	if len(nums) == 0 {
		return nil
	}
	out := make(map[string]any, len(nums))
	for i, p := range nums {
		e := map[string]any{"number": p.Number}
		if p.Label != "" {
			e["label"] = p.Label
		}
		out[fmt.Sprintf("%d", i)] = e
	}
	return out
}

func emailsEqual(a, b []mail.ContactEmail) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Address != b[i].Address || a[i].Label != b[i].Label {
			return false
		}
	}
	return true
}

func phonesEqual(a, b []mail.ContactPhone) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Number != b[i].Number || a[i].Label != b[i].Label {
			return false
		}
	}
	return true
}
