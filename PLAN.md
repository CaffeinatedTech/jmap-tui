# Plan — jmap-tui

How we build what [REQUIREMENTS.md](REQUIREMENTS.md) specifies. Architecture first, then the designs that make or break this project (rolling window, live sync, multi-account), then milestones, testing, and risks.

---

## 1. Architecture

Layered, with a hard rule: **the UI never talks to the network, and the sync layer never renders.** All cross-layer traffic flows through typed messages and a store snapshot.

```
┌──────────────────────────── terminal (Bubble Tea v2) ─────────────────────────────┐
│  ui/        sidebar · messagelist (rolling window) · preview · compose · modals   │
└──────────────▲─────────────────────────────────────────────────────▲──────────────┘
               │ user events (tea.Msg)                               │ view models + cmds
┌──────────────┴─────────────────────────────────────────────────────▲──────────────┐
│  app/       keymap router · pane focus · command dispatcher · toasts│store snapshot│
└──────────────▲─────────────────────────────────────────────────────┴──────────────┘
               │ queries / mutations (typed)          ▲ change notifications (tea.Msg)
┌──────────────┴──────────────────────────────────────┴─────────────────────────────┐
│  sync/     one engine per account (goroutine + hub)                               │
│   • EventSource listener  • /changes reconciliation  • query window manager       │
│   • in-memory store: Mailbox tree · Thread map · Email summaries · body LRU       │
└──────────────▲─────────────────────────────────────────────────────▲──────────────┘
               │ batched JMAP requests                                │ push events
┌──────────────┴─────────────────────────────────────────────────────┴──────────────┐
│  jmapclient/  thin layer over go-jmap: session, /query /get /set /changes,        │
│               upload/download blobs, EventSource, auth (keyring), retry/backoff   │
└───────────────────────────────────────────────────────────────────────────────────┘
```

### Package layout

```
jmap-tui/
├── cmd/jmap-tui/            # entrypoint, flags, subcommands (login)
├── internal/
│   ├── mail/                # domain types + Provider interface (IMAP future seam)
│   ├── jmapclient/          # the ONLY package importing go-jmap
│   ├── sync/                # per-account engine, store, window manager
│   ├── app/                 # Bubble Tea program, routing, dispatch
│   ├── ui/                  # components + theme (Lipgloss)
│   ├── config/              # TOML schema, validation
│   └── keyring/             # OS keyring w/ env-var + file escape hatches
├── test/
│   ├── mockjmap/            # in-process fake JMAP server (httptest)
│   └── golden/              # bubbletea golden files
├── deploy/docker-compose.yml  # Stalwart for CI/local integration
├── README.md · REQUIREMENTS.md · PLAN.md · AGENTS.md · LICENSE
└── .github/workflows/ci.yml
```

**Dependency rule:** `ui` and `app` may import `sync` and `mail` only. `sync` may import `jmapclient` and `mail`. Nothing above `jmapclient` imports `go-jmap` — that is the swap point if the library disappoints.

### Provider seam (IMAP future)

`mail.Provider` is defined in M0 and used by `sync` from day one:

```go
type Provider interface {
    Connect(ctx) error
    Mailboxes(ctx) ([]Mailbox, error)
    OpenQuery(ctx, QuerySpec) (QueryHandle, error)   // paged id stream
    FetchSummaries(ctx, ids) ([]EmailSummary, error)
    FetchBody(ctx, id) (EmailBody, error)
    Mutate(ctx, Mutation) error                       // flags/move/copy/destroy
    Send(ctx, Draft) (SendReceipt, error)
    Subscribe(ctx) (<-chan Change, func() error)      // live changes; nil ⇒ polling
}
```

JMAP implements it natively (storage-free). A future IMAP provider implements the same interface but is *allowed* a local cache — the no-storage rule is a JMAP property, not a product property (see REQUIREMENTS FR-B4).

---

## 2. Data flow example — "user opens Inbox"

1. App start → config load → keyring auth → session discovery (M0).
2. `sync` engine per account: `Mailbox/query`+`get` → store. UI renders tree immediately (skeleton list).
3. Active mailbox = inbox → `Email/query` `{inMailbox: inboxId, collapseThreads: true, sort: [receivedAt desc], limit: 50, position: 0}`.
4. Response: `ids[0..49]`, `total`, `queryState`, `position`. Engine fetches summaries (`Email/get`, properties: `threadId, mailboxIds, keywords, from, subject, receivedAt, size, hasAttachment, preview`), stores, publishes snapshot.
5. UI renders 50 rows; user scrolls; at row 40 the window manager prefetches `position: 50` (§4.1) — user never waits.
6. EventSource fires `StateChange{Email: …}` → `/changes` → store patch → snapshot publish → list re-renders, scroll preserved by id.

---

## 3. In-memory store

- `Store` per account: `mailboxes` (tree), `emails` (id → summary), `threads` (id → email ids), plus *windows*: ordered id slices bound to (mailbox, query) pairs.
- **Bodies LRU**: ≤ 100 full bodies (default), evicted LRU; attachments fetched to temp only when saved, then deleted after handoff.
- Snapshots published via a versioned immutable struct; UI holds only the latest — no locks in render path (single-writer engine, copy-on-write maps or RWMutex with short critical sections).
- Eviction of *summaries* happens when a window is trimmed (§4.1) or mailbox switches (keep last N mailboxes warm).

## 4. The two hard problems

### 4.1 Rolling window pagination (endless scroll that never chugs)

State per open query: `{sort, filter, chunk=50, window: []id, anchorState: queryState, total}`.

- **Forward/backward extension:** when the cursor nears either edge of `window`, issue `Email/query` with `position` = windowEnd / `position` = max(0, windowStart−chunk) using the **same sort+filter**; merge id slices; fetch summaries for new ids only.
- **Deep-jump re-anchor (window > cap, default 2,000):** silently drop the far edge, `G`/`gg` issues a fresh query with `anchor` = nearest known id and `position` offset (JMAP supports anchor+offset); this keeps big mailboxes honest without page numbers.
- **Live changes while windowed:** `StateChange` → `Email/changes(sinceState)` → three cases:
  1. changed/destroyed ids **in window** → patch/evict, adjust `total`;
  2. server reports `cannotCalculateChanges` (rare) → full re-query preserving cursor id;
  3. new ids arrived → for *inbox-like sorted-by-date desc* views, new mail **slides in at top** with a subtle highlight (this is the delight moment); for other sorts, only update `total` and a "new below/above" hint.
- **Position preservation:** cursor and viewport track ids, not row indexes. On window mutation, re-derive row offsets; if the cursor's id was destroyed, land on its neighbour.
- **Debounce:** scroll-driven prefetches coalesce (80ms); re-anchors coalesce (300ms). Only one outstanding query per window — superseded results are discarded by monotonic request id.
- **Threads:** collapsed rows are thread representatives. Expanding runs `Email/query {inThread: id}` locally-scoped fetch — window math is unaffected because thread expansion is a sub-list, not a window mutation.

### 4.2 Live sync engine

```
EventSource ──StateChange──▶ change hub ──▶ per-type /changes ──▶ store patch ──▶ tea.Cmd(snap)
     │                            │
     └─ reconnect w/ backoff      └─ mailbox counts via Mailbox/changes (FR-B6)
        (poll fallback at 60s)
```

- One EventSource per account. Parse `StateChange`; ignore types we don't track.
- `/changes` results folded into the store; **all mutations made by the client itself also flow through `/changes` responses** (JMAP `/set` returns `newState`) — a single reconciliation path, no dual-write bugs.
- Optimistic mutations (FR-B7) write a pending-op overlay in the store; the next `/changes` or `/set` response clears or reverts it.
- Push subscriptions (per-account JMAP push extension) are [FUTURE] — EventSource needs no subscription on Fastmail/Stalwart.
- Clock discipline: never trust client time for sync; state strings only.

### 4.3 Multi-account

- `sync.Hub` owns engines; each engine is a goroutine with its own store + EventSource.
- UI: active account pointer; `S` opens switcher (M6); unified view = read-only merge of selected accounts' inbox windows, interleaved by `receivedAt` with account badges; actions on unified rows dispatch to the owning engine (selection map account→ids).
- Failure isolation: one account's auth failure never blocks others; status line shows per-account state (FR-I5).

## 5. UI design notes (minimal & elegant)

- **Type:** rows are single-height, generous cell padding, muted secondary text; unread = bold + accent dot; selection = soft background wash, not a harsh inverse.
- **Color:** exactly one accent; semantic muted/success/danger; everything else terminal-adaptive. Lipgloss v2 `AdaptiveColor`; two themes (dark, light) + `--theme` flag; user theme file [FUTURE].
- **Chrome:** no full-screen borders by default; panes separated by whitespace and a hairline vertical rule; header = mailbox breadcrumb; footer = status line (FR-I5).
- **Motion:** none gratuitous; the only "animation" is the new-mail slide-in highlight (4.1) and toast fade.
- Bubbles used: `textinput` (search, compose headers), `textarea` (compose body), `viewport` (preview), `filepicker` (attachments), `spinner`, `help`, `table`-like custom list (custom — Bubbles `list` is too opinionated for rolling windows).

## 6. Milestones

Each milestone is a gate (acceptance criteria in REQUIREMENTS §7). Rough effort, not a deadline promise.

| # | Scope | Key deliverables |
|---|---|---|
| **M0** | Scaffold & spike | Repo, CI, module layout, config+keyring, `jmapclient` wrapper, smoke CLI: connect to Stalwart test account, dump session/mailboxes. `mail.Provider` interface defined. mockjmap server v0. |
| **M1** | Reader | Sidebar tree, rolling-window list (4.1), preview + HTML→text, threads expand, single-pane/two-pane responsive, help overlay. **This is the make-or-break milestone.** |
| **M2** | Live sync | EventSource + `/changes` engine, optimistic-overlay plumbing, status line, reconnect/poll fallback, mailbox counts live. |
| **M3** | Triage | Flags/read/star/move/copy/delete, multi-select batched `/set`, archive, undo toasts, crash-safe terminal restore. |
| **M4** | Search | Query bar, advanced modal, scope toggle, unified search. |
| **M5** | Compose | Composer, reply/all/forward, drafts autosave, identities, attachments up/down, send + undo delay. |
| **M6** | Multi-account | Switcher, unified inbox, per-account status, failure isolation. |
| **M7** | Polish & v0.1 | Wizard (`login`), themes (dark/light), keymap polish, Fastmail verification pass, goreleaser, docs, tag v0.1. |
| **[FUTURE]** | IMAP provider | Behind `mail.Provider`; requires cache design doc before any code. |

Sequencing rule: M1 lands before M2 (window math must exist to be sync'd), M3 may overlap M2 (optimistic ops independent of push).

## 7. Server degradation matrix (living doc)

| Capability | Stalwart | Fastmail | Fallback |
|---|---|---|---|
| EventSource push | ✔ | ✔ | poll 60s |
| WebSocket (RFC 8887) | ✔ | ? | EventSource |
| Threads/collapse | ✔ | ✔ | flat list |
| EmailSubmission undo | ✔ | ✔ (short window) | client-side delay only |
| Blob upload/download | ✔ | ✔ | n/a |
| Sieve (RFC 9291) | ✔ | ✔ | hide feature |
| Quota (RFC 9425) | ✔ | ✔ | hide feature |
| `collapseThreads` in query | ✔ | ✔ | client-side group |

Fill `?` cells during M2/M7 verification; new servers get a row here + integration config.

## 8. Testing strategy

- **Unit:** window manager (extension, trim, re-anchor, position preservation — table-driven, the most-tested code in the repo), change reconciliation, config, keyring fallbacks.
- **mockjmap:** deterministic in-process JMAP server (httptest) with scriptable event injection — drives sync engine tests without network.
- **Golden tests:** bubbletea v2 golden files for list/preview/compose/modals at 3 terminal sizes (120×40, 100×35, 60×25) × dark/light.
- **Integration:** docker-compose Stalwart (CI); live Stalwart test account via env creds only (AGENTS.md rules), tests skip when unset.
- **Soak:** script that runs 10 minutes of synthetic push events while scrolling — asserts RSS bound (NFR-2).

## 9. Risks & mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| go-jmap lacks coverage (upload, EventSource auth quirks, RFC 8887) | client rework | wrapper isolation (§1); budget to extend or replace in M0/M2 |
| Fastmail EventSource auth quirks (known historical 401s with tokens) | can't verify Fastmail path | test Basic-vs-Bearer at M0 spike; document in degradation matrix |
| Rolling-window + live-sync races | UI glitches, dup rows | single-writer store, monotonic request ids, id-based cursor (4.1); soak test |
| `cannotCalculateChanges` storms on volatile mailboxes | constant re-query | debounce re-anchors; hint UI instead of full refresh |
| Scope creep in M1 (the fun part) | never ship | M1 acceptance is *reader only*; everything else gated |
| Terminal renderer perf at 2k rows | chug — the thing we exist to prevent | Bubble Tea v2 renderer is fast, but cap window (4.1) and profile at M1 with 10k-row folder |
| Stalwart live-account mutations by agents | data loss on user's real server | AGENTS.md test-account rules; dedicated test mailboxes; no destructive ops outside them |

## 10. Definition of done (v0.1)

REQUIREMENTS §7 gates all pass on both Stalwart (live + docker) and Fastmail; NFR-1/2 verified with numbers in the PR; docs (README, degradation matrix, keymap) updated; goreleaser artifacts; tag v0.1.0.
