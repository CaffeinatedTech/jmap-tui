package mockjmap

// Contacts support (RFC 9610) for the fake server: address books and
// contact cards with a /changes journal and push states, mirroring the live
// Stalwart shapes verified in CONTACTS_PLAN.md §8. Cards are stored as wire
// objects so ContactCard/set path patches (RFC 8620 §5.3 "prop/key") apply
// generically.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ContactValue is one labeled entry of a fixture contact (email or phone).
type ContactValue struct {
	Value string
	Label string
}

// Contact is a fixture contact card. Fixture-friendly fields convert to the
// JSContact wire shape on set; the stored form is the wire object itself.
type Contact struct {
	ID             string
	AddressBookIDs []string
	Kind           string // "" renders as "individual"
	Given          string
	Surname        string
	Emails         []ContactValue
	Phones         []ContactValue
	Org            string
	Title          string
	Note           string
}

// AddressBook is a fixture address book.
type AddressBook struct {
	ID           string
	Name         string
	SortOrder    uint64
	IsDefault    bool
	IsSubscribed bool
}

// SetAddressBooks replaces the address-book fixtures and journals the
// change (external-edit tests).
func (s *Server) SetAddressBooks(books []AddressBook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.books = append([]AddressBook(nil), books...)
	s.addressBookVersion++
	ids := make([]string, 0, len(s.books))
	for _, b := range s.books {
		ids = append(ids, b.ID)
	}
	s.journal = append(s.journal, journalEntry{typ: "AddressBook", updated: ids, version: s.addressBookVersion})
}

// SetContacts replaces the contact fixtures and journals every id as
// updated (external-edit tests).
func (s *Server) SetContacts(contacts []Contact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cards = make([]map[string]any, 0, len(contacts))
	ids := make([]string, 0, len(contacts))
	for _, c := range contacts {
		s.cards = append(s.cards, contactWire(c))
		ids = append(ids, c.ID)
	}
	s.contactVersion++
	s.journal = append(s.journal, journalEntry{typ: "ContactCard", updated: ids, version: s.contactVersion})
}

// DestroyContacts removes fixture contacts and journals the destruction.
func (s *Server) DestroyContacts(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	kept := s.cards[:0]
	for _, card := range s.cards {
		if id, _ := card["id"].(string); !gone[id] {
			kept = append(kept, card)
		}
	}
	s.cards = kept
	s.contactVersion++
	s.journal = append(s.journal, journalEntry{typ: "ContactCard", destroyed: append([]string(nil), ids...), version: s.contactVersion})
}

// DisableContacts drops the contacts capability from the session (FR-L6
// gating tests). The API answers unknownMethod for contacts calls.
func (s *Server) DisableContacts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noContacts = true
}

// contactsDisabled reports the DisableContacts state under the lock.
func (s *Server) contactsDisabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noContacts
}

// contactWire converts a fixture into the JSContact wire object the client
// decodes (mirrors what the live Stalwart returned for the probe card).
func contactWire(c Contact) map[string]any {
	kind := c.Kind
	if kind == "" {
		kind = "individual"
	}
	books := map[string]any{}
	for _, id := range c.AddressBookIDs {
		books[id] = true
	}
	obj := map[string]any{
		"id":             c.ID,
		"addressBookIds": books,
		"kind":           kind,
	}
	if c.Given != "" || c.Surname != "" {
		comps := []any{}
		if c.Given != "" {
			comps = append(comps, map[string]any{"kind": "given", "value": c.Given})
		}
		if c.Surname != "" {
			comps = append(comps, map[string]any{"kind": "surname", "value": c.Surname})
		}
		obj["name"] = map[string]any{
			"components": comps,
			"full":       strings.TrimSpace(c.Given + " " + c.Surname),
			"isOrdered":  true,
		}
	}
	if len(c.Emails) > 0 {
		m := map[string]any{}
		for i, e := range c.Emails {
			entry := map[string]any{"address": e.Value}
			if e.Label != "" {
				entry["label"] = e.Label
			}
			m[fmt.Sprintf("%d", i)] = entry
		}
		obj["emails"] = m
	}
	if len(c.Phones) > 0 {
		m := map[string]any{}
		for i, p := range c.Phones {
			entry := map[string]any{"number": p.Value}
			if p.Label != "" {
				entry["label"] = p.Label
			}
			m[fmt.Sprintf("%d", i)] = entry
		}
		obj["phones"] = m
	}
	if c.Org != "" {
		obj["organizations"] = map[string]any{"0": map[string]any{"name": c.Org}}
	}
	if c.Title != "" {
		obj["titles"] = map[string]any{"0": map[string]any{"name": c.Title}}
	}
	if c.Note != "" {
		obj["notes"] = map[string]any{"0": map[string]any{"note": c.Note}}
	}
	return obj
}

func (s *Server) addressBookGetResponse(args json.RawMessage) map[string]any {
	var req struct {
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(args, &req)

	s.mu.Lock()
	defer s.mu.Unlock()
	list := []map[string]any{}
	notFound := []string{}
	for _, b := range s.books {
		if len(req.IDs) > 0 && !contains(req.IDs, b.ID) {
			continue
		}
		list = append(list, map[string]any{
			"id":           b.ID,
			"name":         b.Name,
			"sortOrder":    b.SortOrder,
			"isDefault":    b.IsDefault,
			"isSubscribed": b.IsSubscribed,
		})
	}
	if len(req.IDs) > 0 {
		for _, id := range req.IDs {
			found := false
			for _, b := range s.books {
				if b.ID == id {
					found = true
					break
				}
			}
			if !found {
				notFound = append(notFound, id)
			}
		}
		sort.Strings(notFound)
	}
	return map[string]any{
		"accountId": "acc1",
		"state":     s.addressBookState(),
		"list":      list,
		"notFound":  notFound,
	}
}

func (s *Server) contactCardGetResponse(args json.RawMessage) map[string]any {
	var req struct {
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(args, &req)

	s.mu.Lock()
	defer s.mu.Unlock()
	list := []map[string]any{}
	found := map[string]bool{}
	for _, card := range s.cards {
		id, _ := card["id"].(string)
		if len(req.IDs) > 0 && !contains(req.IDs, id) {
			continue
		}
		found[id] = true
		list = append(list, card)
	}
	notFound := []string{}
	for _, id := range req.IDs {
		if !found[id] {
			notFound = append(notFound, id)
		}
	}
	sort.Strings(notFound)
	return map[string]any{
		"accountId": "acc1",
		"state":     s.contactState(),
		"list":      list,
		"notFound":  notFound,
	}
}

// contactSetResponse serves ContactCard/set: creates mint ids, updates
// apply RFC 8620 §5.3 patches (plain and "prop/key" path forms), destroys
// remove. One state bump per request; journal entries feed /changes.
func (s *Server) contactSetResponse(args json.RawMessage) map[string]any {
	var req struct {
		Create  map[string]map[string]any  `json:"create"`
		Update  map[string]json.RawMessage `json:"update"`
		Destroy []string                   `json:"destroy"`
	}
	_ = json.Unmarshal(args, &req)

	s.mu.Lock()
	defer s.mu.Unlock()
	oldState := s.contactState()

	created := map[string]any{}
	notCreated := map[string]any{}
	for handle, obj := range req.Create {
		if books, _ := obj["addressBookIds"].(map[string]any); len(books) == 0 {
			notCreated[handle] = map[string]any{
				"type":        "invalidProperties",
				"description": "contact requires at least one address book",
				"properties":  []string{"addressBookIds"},
			}
			continue
		}
		s.contactSeq++
		obj["id"] = fmt.Sprintf("c%d", s.contactSeq)
		s.cards = append(s.cards, obj)
		created[handle] = map[string]any{"id": obj["id"]}
	}

	updated := map[string]bool{}
	notUpdated := map[string]any{}
	for id, rawPatch := range req.Update {
		idx := -1
		for i, card := range s.cards {
			if cid, _ := card["id"].(string); cid == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			notUpdated[id] = map[string]any{"type": "notFound"}
			continue
		}
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(rawPatch, &patch); err != nil {
			notUpdated[id] = map[string]any{"type": "invalidArguments", "description": err.Error()}
			continue
		}
		applyPatch(s.cards[idx], patch)
		updated[id] = true
	}

	destroyed := []string{}
	notDestroyed := map[string]any{}
	if len(req.Destroy) > 0 {
		gone := map[string]bool{}
		for _, id := range req.Destroy {
			gone[id] = false
		}
		kept := s.cards[:0]
		for _, card := range s.cards {
			cid, _ := card["id"].(string)
			if _, wanted := gone[cid]; wanted {
				gone[cid] = true
				destroyed = append(destroyed, cid)
				continue
			}
			kept = append(kept, card)
		}
		s.cards = kept
		for id, found := range gone {
			if !found {
				notDestroyed[id] = map[string]any{"type": "notFound"}
			}
		}
		sort.Strings(destroyed)
	}

	journalUpdated := append([]string{}, keysOf(created)...)
	for id := range updated {
		journalUpdated = append(journalUpdated, id)
	}
	sort.Strings(journalUpdated)
	if len(journalUpdated) > 0 || len(destroyed) > 0 {
		s.contactVersion++
		s.journal = append(s.journal, journalEntry{
			typ:       "ContactCard",
			updated:   journalUpdated,
			destroyed: destroyed,
			version:   s.contactVersion,
		})
	}

	out := map[string]any{
		"accountId": "acc1",
		"oldState":  oldState,
		"newState":  s.contactState(),
	}
	if len(created) > 0 {
		out["created"] = created
	}
	if len(notCreated) > 0 {
		out["notCreated"] = notCreated
	}
	if len(updated) > 0 {
		// Map form with null values, the shape the live Stalwart answered
		// (CONTACTS_PLAN §8); the wrapper's flexible decoder takes both.
		um := map[string]any{}
		for id := range updated {
			um[id] = nil
		}
		out["updated"] = um
	}
	if len(notUpdated) > 0 {
		out["notUpdated"] = notUpdated
	}
	if len(destroyed) > 0 {
		out["destroyed"] = destroyed
	}
	if len(notDestroyed) > 0 {
		out["notDestroyed"] = notDestroyed
	}
	return out
}

// applyPatch implements RFC 8620 §5.3 against one stored card: plain keys
// set/remove the property; "prop/key" path keys set/remove one entry of a
// map-valued property, leaving siblings alone.
func applyPatch(card map[string]any, patch map[string]json.RawMessage) {
	for key, raw := range patch {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		if strings.Contains(key, "/") {
			parts := strings.SplitN(key, "/", 2)
			prop, entryKey := parts[0], parts[1]
			m, _ := card[prop].(map[string]any)
			if value == nil {
				if m == nil {
					continue
				}
				delete(m, entryKey)
				if len(m) == 0 {
					delete(card, prop)
				}
				continue
			}
			if m == nil {
				m = map[string]any{}
				card[prop] = m
			}
			m[entryKey] = value
			continue
		}
		if value == nil {
			delete(card, key)
		} else {
			card[key] = value
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
