# CONTACTS_PLAN — JMAP contacts (M9)

How we build the contacts feature specified in REQUIREMENTS FR-L. Protocol first,
then layers, then UX, then tests and gates. Scope decisions were interviewed
2026-09-26 (see §0); this file is the design record PLAN.md §6 M9 points at.

---

## 0. Scope decisions (interviewed 2026-09-26)

| # | Question | Decision |
|---|---|---|
| 1 | Placement vs Release v0.1 | **New milestone M9, before Release** — Fastmail pass + tag still land after it. |
| 2 | Manage scope | **Contact CRUD only.** Address books are listed and selectable (where a new contact goes), never created/renamed/deleted. No groups (`kind=group` hidden). **No sharing** (RFC 9670 out of scope: `shareWith`/`myRights` never rendered, never written). |
| 3 | Form fields | **Middle set:** name (given+surname), emails (multi, labeled), phones (multi), organization, title, note. Other JSContact properties survive untouched — `/set update` patches only the properties we send. |
| 4 | Compose suggest UX | **Popup under the focused To/Cc/Bcc field:** `up`/`down` navigate, `enter` accepts (stays in field), `tab` accepts when open, `esc` dismisses first (extends the esc-back chain). |
| 5 | Keys | `c` = contacts screen, `shift+n` = new contact (prefilled from the sender when reading), `ctrl+g` = quick contact search inside To/Cc/Bcc (compose-internal loop like `ctrl+i`, not a keymap Action). |
| 6 | Suggestion source | **All accounts** with the capability, merged and deduped by email (case-insensitive). Creation targets a chosen/default address book on one account. |

## 1. Protocol facts (verified, not assumed)

- Live Stalwart advertises `urn:ietf:params:jmap:contacts` **and**
  `urn:ietf:params:jmap:contacts:parse` (verified 2026-09-26 via
  `jmap-tui smoke` — session capability list). `SessionInfo.Capabilities`
  already surfaces every advertised URN; only consumption was missing.
- RFC 9610 names, confirmed in Stalwart docs + changelog:
  `AddressBook/get|set|changes`, `ContactCard/get|set|query|changes`
  (+ non-standard `ContactCard/parse`, out of scope). Stalwart auto-creates a
  default address book per account on first access. Capability URN:
  `urn:ietf:params:jmap:contacts`.
- `go-jmap` has **no contacts package** → hand-rolled `jmap.Method`
  implementations with the sanctioned `sync.Once` + `jmap.RegisterMethod`
  registration (`jmapclient/mutate.go:21` precedent — no `init()` side effects),
  or raw `Invocation` decode.
- **No `ContactCard/query` in v1.** Sort and filter client-side: one batched
  initial fetch, liveness via `/changes`. This sidesteps the Stalwart
  token-only matching trap found in the M4 mail search (PLAN §7) and keeps the
  suggestion path instant and predictable.
- Live probe checklist (first implementation step, results in §8):
  `AddressBook/get {ids:null}`; `ContactCard/get {ids:null, properties:[…]}`;
  `ContactCard/set` create/update/destroy; `ContactCard/changes` +
  `AddressBook/changes`; push `StateChange` types include `ContactCard`/
  `AddressBook`.

## 2. Architecture by layer

Dependency rule unchanged (PLAN §1): UI never talks to the network, UI never
imports `go-jmap`, no contact data touches disk (NFR-4 — contacts are PII too).

### 2.1 `internal/jmapclient/contacts.go` (new)

- `AddressBooks(ctx, acct)` → `AddressBook/get {ids:null}`.
- `ContactCards(ctx, acct, props)` → `ContactCard/get {ids:null, properties:[…]}`
  — summary set only: `id, addressBookIds, uid, kind, name, emails, phones,
  organizations, jobTitle, note, updated`. Full cards are never fetched: the
  form only edits these properties, and `/set update` leaves everything else
  alone.
- `ContactChanges(ctx, acct, since)` / `AddressBookChanges(ctx, acct, since)`.
- `ContactSet(ctx, acct, spec)` → `ContactCard/set` create/update/destroy.
- JSContact wire structs live here; conversion to `mail.Contact` happens here
  (golden rule 3). `name.components` *or* `name.full` both resolve to a display
  name; `kind == "group"` cards are dropped at conversion.

### 2.2 `internal/mail` types (`mail/contacts.go`)

```go
type AddressBook struct { ID, Name, SortOrder, IsDefault, IsSubscribed }
type ContactEmail struct { Address, Label string }
type ContactPhone struct { Number, Label  string }
type ContactEntry struct { Key, Value string } // keyed orgs/titles/notes entries
type Contact struct {
    ID, Kind, GivenName, Surname, DisplayName string
    AddressBookIDs []ID
    Emails []ContactEmail; Phones []ContactPhone
    Orgs, Titles, Notes []ContactEntry // every entry, server key order
}
// mail.ContactLess(a, b) — the ONE display comparator (SortKey → id),
// shared by the provider's initial sort and the engine's live re-sorts.
```

### 2.3 `internal/sync` (`sync/contacts.go`, new)

Per-engine (not a new engine) state under the existing mutex:

```go
contactsLoaded, contactsLoading bool
contactBooks []mail.AddressBook
contacts map[mail.ID]mail.Contact
contactOrder []mail.ID         // display order (mail.ContactLess)
contactState, bookState string
contactVersion uint64; contactErr string
contactUpdates chan ContactSnapshot
```

- `LoadContacts(ctx)` — batched `AddressBook/get` + `ContactCard/get`
  (concurrent callers coalesce on `contactsLoading`), store swapped under
  the lock, publish.
- `Contacts()` → `ContactSnapshot{Version, Loaded, Err, Books, Contacts}` —
  immutable, published on a **separate buffered(1) latest-wins channel**
  (`ContactUpdates()`) so the mail `Snapshot`/`applyView` path is untouched.
- `SetContact(ctx, spec)` → `ContactCard/set`; **confirmed-then-patch** —
  the mutation's `newState` folds into `contactState` and the touched ids
  refetch (`FetchContacts`), so the store always holds server truth. No
  optimistic overlay: a contact save is one deliberate action, not a
  keystroke stream (the mail overlay machinery is deliberately not reused).
  Creates with no book resolve the default engine-side (`withDefaultBooks`).
- `reconcileContacts(ctx)` — `ContactCard/changes`: changed/created →
  `FetchContacts(ids)`; destroyed → evict; a requested id the server no
  longer returns → evict; `cannotCalculateChanges` → full reload keeping
  old rows until the new set lands. `reconcileAddressBooks` refetches the
  book list.
- Live loop: `reconcileTypes` (`live.go`) gains `ContactCard`/`AddressBook`
  branches that run **only when `contactsLoaded`** — zero cost for users who
  never open the feature. `pollOnce` includes the contact types only while
  warm.
- **Lazy load:** first open of the contacts screen *or* the composer arms the
  load; it stays warm afterwards. Never on the startup critical path (NFR-3).

### 2.4 `internal/app`

- `contactMsg{acct, snap, live}` + `contactOpMsg{seq, acct, op, done, snap,
  err}` (the `snapMsg`/`errMsg` shape); `contactSaveCmd` wraps every mutation
  (engine captured on the UI thread, like `opOn`); `waitContactsFor(acct)`
  waiter armed per account from `Init` — it idles until the store publishes.
- `contactsState` (pointer field, nil = closed) owning its **own key loop** —
  the full-frame pattern of `switchState`, cursor keyed by
  `(account, contactID)` (`rowKey` precedent), rows stamped with the owner's
  tint (FR-A5).
- Merged suggestion index = pure function over `m.contactSnaps` (all accounts,
  deduped by email), unit-tested.
- Modal priority (handleKey **and** `ui.Render`, as built): help → switcher →
  picker (**the quick search is a picker** — `pickerContactQuick`, so it
  already layers over the composer like the identity picker) → filepick →
  **contactForm** → compose (with its suggest popup inside) → search →
  **contactsScreen** → panes/keymap. A five-key whitelist (`ctrl+z`, `?`,
  `q`, `shift+a`, `ctrl+a`) stays live over the screen.

### 2.5 `internal/ui`

`contacts.go` (frame + columns), `contactForm` (advState-style fielded form),
suggestion dropdown, quick-pick render. Goldens at 3 sizes × dark/light.

## 3. UX

### 3.1 Contacts screen (`c`) — full frame, responsive like FR-I1

```
Accounts & books        Contacts                       Detail
▸ All accounts          │ Ada Lovelace   ada@…         Name
  agent-test1           │ Alan Turing    ala@…         Emails
    ◉ Default        ▌  (▌ = owner tint)               Phones
    Work                                     …         Org / Title
▸ second@account                          [n]ew [e]dit [d]elete [esc]
```

- Left column: account headers (M8 sidebar grammar) → "All contacts" + each
  book (sorted by `sortOrder`). **Unified scope is the default** when ≥2
  accounts have the capability; rows merge and sort by name with an owner tint
  bar. Edit/delete route to the owner engine (unified precedent).
- Keys (own loop): `j/k/g/G/ctrl+f/b/ctrl+d/u` motion, `tab`/`shift+tab` cycle
  columns, `enter` detail, `n` new, `e` edit, `d` delete, `/` type-to-filter,
  `esc` back to mail. Cursor tracks ids, never indexes.
- **Delete** holds the destroy for a 5s undo toast cancelled with `ctrl+z`
  (the `pendingDestroy` machinery — destroy is irreversible, so `/set` is not
  sent until the toast expires).
- No capability → `c` shows a non-fatal toast; the binding stays in help.

### 3.2 Contact form (modal; shared by `n`, `e`, `shift+n`)

First name · Last name · Email(s, comma-separated) · Phone(s) · Organization ·
Title · Note · Address book (cycles the target account's books, default =
`isDefault`). `tab`/`shift+tab` fields, `enter` saves, `esc` cancels. Create
sends `addressBookIds`; update patches only the form's properties. Success
toast on save.

### 3.3 Add sender as contact (`shift+n`, reading a message)

Prefill name (display name split on first space) + email from the message's
From. **If a merged contact already holds that email, open the form in edit
mode** prefilled instead. Owner account = the cursor row's owner (unified-safe).

### 3.4 Compose suggestions (To/Cc/Bcc)

- Compose open → arm the contacts load; popup shows `loading…` until warm, then
  silently (no capability ⇒ never renders).
- Match the token after the last comma: substring over name+email,
  case-insensitive, prefix hits ranked first, top 5, **one row per email**
  (`Name <email>` with a muted account suffix).
- Popup-open keys are intercepted in `composeKey` *before* zone handling:
  `down`/`up` move, `enter`/`tab` accept (insert replaces the current token,
  focus stays in the field), `esc` dismisses and never reaches the discard
  flow, any other key types and refilters. Closed popup ⇒ zero behavior
  change.

### 3.5 Quick search (`ctrl+g`, focus in To/Cc/Bcc)

Type-to-filter picker over the merged set (name + email + account); `enter`
inserts at the cursor token. Compose-internal key loop; documented in README's
composer section.

## 4. Docs (same change, AGENTS rule 7)

- **REQUIREMENTS:** §4 non-goal line split (calendar stays out; contacts comes
  in); new **FR-L — Contacts** (L1 gated screen + unified, L2 CRUD +
  delete-hold undo, L3 compose suggestions + `ctrl+g`, L4 add-sender, L5
  in-memory only, L6 degradation); FR-H1 amended (completion source = JMAP
  contacts); Open Q2 (drop the CardDAV mislabel) and Q5 (source = server
  capability) resolved; **M9 gate** in §7.
- **PLAN:** §6 M9 row (before Release), §4.5 contacts design note, §7
  degradation row (Stalwart ✔ verified / Fastmail `?` ⇒ hidden), §8 testing,
  §9 risk row.
- **README:** keys (`c`, `shift+n`), composer (`ctrl+g`, suggestions), feature
  blurb.
- **KEYMAP_PLAN:** addendum — two new global actions, both `Validate()`-clean.

## 5. Testing

**Status: all layers landed and green 2026-09-26** — build, vet, golangci-lint,
gofumpt, full suite, 11 new golden frames (9 contacts + regenerated help).

- **mockjmap:** capability advertised by default (`DisableContacts()` drops it
  for FR-L6 gate tests), fixtures + `SetContacts`/`SetAddressBooks`/
  `DestroyContacts` with a `ContactCard`/`AddressBook` state journal, handlers
  for `AddressBook/get|changes` and `ContactCard/get|set|changes` — including
  RFC 8620 §5.3 plain **and path** patches — `Notify()` push carrying the
  contact types, session `accountCapabilities` matching.
- **jmapclient (10 tests):** conversion (components vs `full`, pref-ordered
  emails, label survival, group drop), property-subset get, create round-trip
  + local no-book rejection, **minimal-patch unit rules** (untouched
  collections omitted, sibling path patches, clear semantics), `/changes`
  deltas both types, `cannotCalculateChanges` sentinel, book sorting.
- **sync (8 tests + live gate):** load/publish/name-ordered snapshot, both
  unsupported seams (no interface, no capability), engine CRUD with default
  book resolution, cold-store no-op on direct reconcile / push stream / poll,
  live push reconcile (create, destroy, book rename), poll-only fold,
  full reload on `cannotCalculateChanges`.
- **app (11 tests):** screen open/toggle, sorted rows + detail + label
  survival, filter esc-chain, column cycling, **delete hold → ctrl+z cancel →
  commit receipt**, create round-trip + inline validation, edit round-trip
  with label preservation, add-sender prefill **and** existing→edit, popup
  ranking/accept/dismiss/reopen latch, `ctrl+g` quick pick + type-to-filter,
  matcher rules, capability toast.
- **Golden:** `contacts{,-light}`, `contacts-medium{,-light}`,
  `contacts-compact{,-light}`, `contact-form`, `compose-suggest` (+ regenerated
  `help`).
- **Live gate (`sync/contacts_live_test.go`, env-gated):** see §5a.

## 5a. Live gate results (2026-09-26, live Stalwart `agent-test1`)

| Check | Result |
|---|---|
| Capability + lazy load | ✔ `ContactsSupported` true; `LoadContacts` fetched 1 book (default id `b`) + 0 cards in **2 ms**. |
| Create → store | ✔ `SetContact(create)` minted id `c`, landed immediately with name/org/title/note/label intact, snapshot version bumped (confirmed-then-patch). |
| Push liveness | ✔ `ContactCard` StateChange observed on the live EventSource within the create window. |
| Update patch | ✔ surname change applied through `ContactCard/set update` with `Current` diffing; store shows the rename. |
| Destroy | ✔ accepted; card gone from the store. |
| Cleanup | ✔ final live read: **0 contacts** on the account (this gate and the §8 probe both cleaned up; a `@example.invalid` sweep runs on failure). |

## 6. Implementation order (as executed)

1. Live protocol probe → record in §8.
2. `jmapclient/contacts.go` + `mail` types + mockjmap + tests.
3. `sync/contacts.go` + tests.
4. Contacts screen (frame, columns, form, delete-hold) + goldens.
5. Compose suggestions + `ctrl+g` quick-pick + `shift+n` add-sender.
6. Docs, gates (build/vet/lint/gofumpt/full test), live verification + cleanup.

## 7. Risks

| Risk | Mitigation |
|---|---|
| Stalwart `properties` filtering quirks | Probe first; fall back to full-card get (smaller win, same semantics). |
| One-batch fetch size on huge books | RSS check during probe; paged `ContactCard/query` + cap only if measured — not built speculatively. |
| Suggestion popup key interception leaking into discard/send paths | Dedicated app routing tests for enter/tab/esc with popup open vs closed. |
| go-jmap has no contacts types | In-repo wire structs + registered decoders; the swap seam (§1) already isolates go-jmap. |

## 8. Live probe results (2026-09-26, live Stalwart `agent-test1`, temporary probe test — deleted after capture)

| Check | Result |
|---|---|
| Session capability | `urn:ietf:params:jmap:contacts` + `contacts:parse` advertised. |
| `AddressBook/get {ids:null}` | ✔ 1 book: id `b`, `name="Stalwart Address Book (<user>)"`, `isDefault=true`; returns `description, id, isDefault, isSubscribed, myRights, name, shareWith, sortOrder` (we ignore the sharing fields). State `s7upa`. |
| `ContactCard/get {ids:null, properties:[…]}` | ✔ property filter honored (only requested keys came back). **`uid`, `created`, `updated` were requested but not returned** — Stalwart does not surface them ⇒ client-side name sort, no dependency on those fields. |
| `AddressBook/changes` / `ContactCard/changes` | ✔ standard RFC 8620 §5.2 shape (`oldState/newState/hasMoreChanges/created/updated/destroyed`). |
| `ContactCard/set create` | ✔ **`jobTitle`/`note` are invalid JSContact properties** — rejected with `invalidProperties`. Correct shapes: `titles: {k:{name}}`, `notes: {k:{note}}`, `organizations: {k:{name}}`, `emails: {k:{address,label}}`, `phones: {k:{number}}`, `name: {components:[…], isOrdered:true}` (server also returns `name.full` and back-fills `titles.*.kind="title"`). Server generated the id (`b`) and advanced state `s7upa→s7ypa`. |
| `ContactCard/set update` | ✔ `updated` came back in **map form** (`{"b":null}`) — the existing `setIDList` flexible decoder handles both forms. |
| `ContactCard/changes` since pre-create state | ✔ `created:["b"]`, `newState` advanced. |
| Push `StateChange` | ✔ **`ContactCard` type observed on the live EventSource** within the create window. |
| Destroy | ✔ `destroyed:["b"]`; follow-up get → `notFound:["b"]`. Cleanup verified, account left with 0 contacts (its pre-probe count). |
| Id collisions | ContactCard id and AddressBook id may be equal (`b` vs `b`) — ids are unique *per type*; contact rows and book rows keep separate key spaces. |

**Design consequences:** no query, no `uid`/`created`/`updated` dependency; wire
shape for the middle form = `name` (components), `emails`, `phones`,
`organizations`, `titles`, `notes`; keyed collections beyond the form's first
entry (extra notes/orgs/titles) and per-entry `label`s are preserved on save
(see §2.4/§3.2 edit rules).
