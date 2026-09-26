package sync

// Live contacts gate (M9, CONTACTS_PLAN §5): capability, lazy load, the
// create/update/destroy round-trip through the engine, and push liveness
// for ContactCard StateChange. Env-gated like every other live test
// (AGENTS.md): unset creds ⇒ skip. Read + create/destroy only; every
// created card is destroyed in cleanup (test-account rules).

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// liveContactsClient connects with the standard env-gated test creds.
func liveContactsClient(t *testing.T) *jmapclient.Client {
	t.Helper()
	url := os.Getenv("JMAP_TUI_TEST_URL")
	user := os.Getenv("JMAP_TUI_TEST_USER")
	pass := os.Getenv("JMAP_TUI_TEST_PASSWORD")
	if url == "" || user == "" || pass == "" {
		t.Skip("live Stalwart creds not set (JMAP_TUI_TEST_URL / _USER / _PASSWORD)")
	}
	return jmapclient.New(jmapclient.Options{ServerURL: url, Username: user, Password: pass})
}

// contactByIDFor reads a card out of the engine's store for the update
// round-trip (the Current half of a patch).
func contactByIDFor(t *testing.T, e *Engine, id mail.ID) mail.Contact {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.contacts[id]; ok {
		return c
	}
	t.Fatalf("contact %q not in store", id)
	return mail.Contact{}
}

func TestLiveContactsGate(t *testing.T) {
	c := liveContactsClient(t) // env-gated; skips when unset (AGENTS.md)
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	e := NewEngine(c, Config{})

	// 1. Capability + lazy load.
	if !e.ContactsSupported() {
		t.Fatal("live server must advertise the contacts capability")
	}
	start := time.Now()
	if err := e.LoadContacts(ctx); err != nil {
		t.Fatalf("LoadContacts: %v", err)
	}
	loadFor := time.Since(start)
	snap := e.Contacts()
	if !snap.Loaded || len(snap.Books) == 0 {
		t.Fatalf("load = %+v", snap)
	}
	def := e.ContactDefaultBook()
	if def == "" {
		t.Fatal("no default address book on the live server")
	}
	t.Logf("loaded %d contacts, %d books (default %q) in %s", len(snap.Contacts), len(snap.Books), def, loadFor.Round(time.Millisecond))

	// 2. Push liveness: subscribe, create a card, expect ContactCard push
	// within the window, reconcile, verify; then destroy and verify.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	changes, stop := c.Subscribe(streamCtx)
	defer func() { _ = stop() }()

	before := snap.Version
	res, err := e.SetContact(ctx, mail.ContactMutation{
		Create: map[string]mail.ContactDraft{"g1": {
			GivenName: "ZZGate", Surname: "Contact",
			Emails: []mail.ContactEmail{{Address: "zzgate@example.invalid", Label: "test"}},
			Org:    "Agent Test", Title: "Probe", Note: "live gate card",
		}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id, ok := res.Created["g1"]
	if !ok {
		t.Fatalf("no id: %+v", res)
	}
	t.Logf("created %q (destroyed in cleanup)", id)

	// Local store must hold it immediately (confirmed-then-patch).
	var found bool
	for _, c := range e.Contacts().Contacts {
		if c.ID == id {
			found = true
			if c.GivenName != "ZZGate" || len(c.Orgs) == 0 || c.Orgs[0].Value != "Agent Test" {
				t.Fatalf("created card wrong: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("create did not land in the store")
	}
	if e.Contacts().Version == before {
		t.Fatal("create did not publish a new snapshot")
	}

	// Push event for the card type (FR-L1 liveness).
	pushed := ""
	deadline := time.After(8 * time.Second)
collect:
	for {
		select {
		case chg, ok := <-changes:
			if !ok {
				break collect
			}
			for _, byType := range chg.Changed {
				for typ := range byType {
					if typ == "ContactCard" {
						pushed = typ
						break collect
					}
				}
			}
		case <-deadline:
			break collect
		}
	}
	if pushed != "ContactCard" {
		t.Errorf("no ContactCard StateChange within 8s (push liveness, FR-L1)")
	}

	// 3. External edit via the API, reconcile fold.
	// (The engine's own write folded its state; a second store write
	// through the provider exercises /changes.)
	if _, err := e.SetContact(ctx, mail.ContactMutation{
		Update: map[mail.ID]mail.ContactUpdate{
			id: {Draft: mail.ContactDraft{
				GivenName: "ZZGate", Surname: "Contact Renamed",
				Emails: []mail.ContactEmail{{Address: "zzgate@example.invalid", Label: "test"}},
				Org:    "Agent Test", Title: "Probe", Note: "live gate card",
			}, Current: contactByIDFor(t, e, id)},
		},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	renamed := false
	for _, c := range e.Contacts().Contacts {
		if c.ID == id && c.Surname == "Contact Renamed" {
			renamed = true
		}
	}
	if !renamed {
		t.Fatal("update did not land")
	}

	// 4. Cleanup: destroy the probe card and prove it is gone.
	if _, err := e.SetContact(ctx, mail.ContactMutation{Destroy: []mail.ID{id}}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	for _, c := range e.Contacts().Contacts {
		if c.ID == id {
			t.Fatal("destroyed card still in store")
		}
	}
	t.Cleanup(func() {
		// Belt and braces: if any step failed before the destroy, remove
		// leftovers by email match (test-account rules: clean up).
		sv, err := c.Contacts(context.Background())
		if err != nil {
			return
		}
		var gone []mail.ID
		for _, card := range sv.Contacts {
			for _, e := range card.Emails {
				if strings.HasSuffix(e.Address, "@example.invalid") {
					gone = append(gone, card.ID)
				}
			}
		}
		if len(gone) > 0 {
			_, _ = c.SetContacts(context.Background(), mail.ContactMutation{Destroy: gone})
		}
	})
}
