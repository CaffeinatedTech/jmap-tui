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
├── README.md · REQUIREMENTS.md · PLAN.md · AGENTS.md · LICENSE
└── (no CI — gates run locally before commit)
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
  - Transport note (M2): the stream client lives in `jmapclient/sse.go`, not go-jmap's `push` package — see the M2 milestone row for why. Ping is requested at 30s; a watchdog ends a stream silent past 75s so the reconnect loop replaces it.
- `/changes` results folded into the store; **all mutations made by the client itself also flow through `/changes` responses** (JMAP `/set` returns `newState`) — a single reconciliation path, no dual-write bugs.
- Optimistic mutations (FR-B7) write a pending-op overlay in the store; the next `/changes` or `/set` response clears or reverts it. The overlay machinery landed in M2 (`sync/overlay.go`): keyword/mailbox deltas, destroy-hides-row, re-apply when server updates race a pending op, confirm/revert paths — driven by M3 actions.
- Clock discipline: never trust client time for sync; state strings only.

### 4.3 Multi-account

- `sync.Hub` owns engines; each engine is a goroutine with its own store + EventSource.
- UI: active account pointer; `S` opens switcher (M6); unified view = read-only merge of selected accounts' inbox windows, interleaved by `receivedAt` with account badges; actions on unified rows dispatch to the owning engine (selection map account→ids).
- Failure isolation: one account's auth failure never blocks others; status line shows per-account state (FR-I5).

### 4.4 Fuzzy search fallback (M4 addendum)

Servers verify token-only search (PLAN §7: Stalwart matches whole words on every condition; partial words return nothing). The query bar therefore runs two tiers:

1. **Fast path** — the debounced server query as typed. Non-zero results render with exact totals; full-word body search lives here.
2. **Fuzzy LIKE scan** — engaged automatically when the fast path returns zero results and the query has words: the engine walks the scope newest-first (`Email/query` without the text condition, 500-id pages), fetches each batch's summaries, substring-matches subject/from/to client-side (every typed word must appear, AND), and streams matches into the list. The header shows `scanning n/N`; `esc` or any new search supersedes the scan (generation counter). Only match ids are held — memory stays bounded, nothing is written to disk, and no local index persists across sessions (REQUIREMENTS non-goals intact).

Scan views are flat (no thread expansion, no server-side jump positions); triage and live-change patches apply to scan matches as usual.

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
| **M0** | Scaffold & spike | Repo, module layout, config+keyring, `jmapclient` wrapper, smoke CLI: connect to Stalwart test account, dump session/mailboxes. `mail.Provider` interface defined. mockjmap server v0. **Landed 2026-09-21: smoke verified against live Stalwart (session, 16 capabilities, mailbox tree with roles); config/keyring/jmapclient tested via mockjmap; live integration is env-gated; no CI, no docker — gates run locally pre-commit.** |
| **M1** | Reader | Sidebar tree, rolling-window list (4.1), preview + HTML→text, threads expand, single-pane/two-pane responsive, help overlay. **This is the make-or-break milestone.** **Landed 2026-09-21:** full reader (sidebar/list/preview, help overlay, responsive 3/2/1-pane, dark+light themes, ctrl-c-twice exit, redacted `--log-file`, crash reports). Verified: 12k-message endless scroll vs mockjmap — RSS 20 MB (50 MB budget), worst local interaction 3.6 ms (16 ms budget), summaries bounded at cap+chunk; live Stalwart reader verification (collapsed queries, inThread expansion, HTML→text) with full fixture cleanup; 10 golden frames (120×40 / 99×35 / 59×25 × dark/light + help/loading/no-sidebar). Thread expansion uses `Email/query {inThread}` per §4.1 — FR-D2's `Thread/get` wording is superseded (drift flagged, REQUIREMENTS untouched: behaviour is identical). |
| **M2** | Live sync | EventSource + `/changes` engine, optimistic-overlay plumbing, status line, reconnect/poll fallback, mailbox counts live. **Landed 2026-09-21:** EventSource transport implemented in-repo (go-jmap's `push.EventSource` appends query params instead of expanding the RFC 8620 §7.3 `{types}/{closeafter}/{ping}` template, takes no context, and has no liveness watchdog — wrapper-extended per §9, `jmapclient/sse.go`). Engine loop: push → backoff (1s→30s, jitter) → poll at 60s after 3 failed attempts, auto-upgrading to push on any successful reconnect; `/changes` reconciliation for Email (destroy eviction + cursor repair, summary patch, new-mail slide-in at top for date-desc views, cannotCalculateChanges → cursor-anchored re-query) and Mailbox (tree refetch, counts, ghost-mailbox fallback to inbox); state strings tracked per type (FR-B5); optimistic-overlay plumbing with confirm/revert/re-apply (FR-B7 groundwork, no UI actions until M3/M4); footer status line (FR-I5); new-above hint; fresh-row highlight with fade. Verified: mockjmap suites (slide-in, patch, destroy, reconnect, poll fallback + upgrade, cannotCalculateChanges, overlays), window live-patch table tests, live-push soak with bounded RSS/latency, and the live Stalwart gate — send-to-self via EmailSubmission appeared in the open client in **1.05s**, flag patch **0.95s**, unread count 2→1 via Mailbox/changes, all within the ~1s target. |
| **M3** | Triage | Flags/read/star/move/copy/delete, multi-select batched `/set`, archive, undo toasts, crash-safe terminal restore; attachment save (FR-E4, deferred from M2 — filepicker dir-pick UX decided 2026-09-21). **Landed 2026-09-21:** full triage set — read/unread (`space`/`u`), star (`*`), multi-select (`x`) rendered as a marker gutter, move (`m`) and copy (`C`) via a type-to-filter mailbox picker modal, delete (`#` → role-trash, permanent destroy inside Trash as a 5s delayed destroy cancelled with `ctrl+z`), archive (`y`) with role → prefs → one-time-picker fallback remembered in app-managed `prefs.toml` (REQUIREMENTS FR-J1 updated same PR), undo toasts (5s) backed by exact reverse-delta `TriageSpec`s (move-undo restores original memberships), attachment save (`s` in preview: bubbles filepicker dir-pick from `~/Downloads`, no-overwrite collision suffixes, bytes only touch disk at save — NFR-4). Engine: `sync.Triage` applies optimistic overlays (FR-B7) then one batched `Email/set` per action (FR-G3, FR-K4), confirms via refetch, and re-anchors the window on membership changes; dirty-window re-anchor now driven by `Prefetch` (fixes a latent M2 gap where live-destroyed windows could not extend). `mail.Mutation`/`EmailPatch`/`MutationResult` finalised; wrapper decodes both RFC 8620 §5.3 IdSet wire forms for `updated` (Stalwart sends the array form; go-jmap's typed field only accepts the map form — wrapper-extended per §9). Verified: mockjmap suites (keywords/move/copy/destroy/undo/batch-is-one-set/partial-failure/pending-overlay), engine + app key-routing tests, 14 golden frames incl. selection/picker/toast/save-attachments, and the live Stalwart gate — full triage workflow server-verified via independent `Email/get` (read batch 2–5 ms), attachment upload→download round-trip byte-exact. Crash-safe terminal restore was already satisfied in M1 (FR-K3, `cmd/jmap-tui/tui.go`). |
| **M4** | Search | Query bar, advanced modal, scope toggle, unified search. **Landed 2026-09-22:** server-side search through the rolling window (FR-F1) — `/` opens a query bar in the header with a 300 ms debounce, `Esc` restores the parked mailbox window with cursor position preserved by id; `Tab` toggles current-mailbox ↔ all-mailboxes scope (FR-F3); `ctrl+s` (or `/` in the bar) opens the advanced fielded modal — text/from/to/subject/after/before/keyword/attachments → RFC 8621 §4.4.1 FilterCondition (FR-F2); `v` full-screen message view (FR-E5). Engine: search views are first-class `Query` windows (`SearchOpen`/`SearchClose` park/restore the mailbox view; parked-window summaries stay resident so Esc renders instantly); live patch/destroy works in search views, new-mail slide-in is suppressed there (server-side matchability is unknowable client-side) and the restored window re-anchors around the cursor on close (FR-B5). Unified-account search rides M6 (needs the Hub — REQUIREMENTS FR-F3 note). Verified: mockjmap filter suites (token-semantics server double, mirroring Stalwart), engine restore/scope/slide-in suites, 50k-message search first page in **224 ms** (< 1 s gate), search-soak RSS 20.9 MB / worst interaction 4.8 ms; live Stalwart gate passed — 3,000-message fixture search first page in **13.7–22.2 ms**, all filter fields + all-mailbox scope verified, and the fuzzy fallback verified live (partial-word scan 3000/3000, §7 observations). Follow-ups landed after user feedback (2026-09-22): `ctrl+s` now opens the advanced modal directly (it previously only opened the bar), and zero-result searches auto-fall-back to the client-side fuzzy LIKE scan (§4.4 below, PLAN §7 finding: Stalwart is token-only). |
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

M0 live-Stalwart observations: `eventSourceUrl` and the `websocket` capability are both advertised — push (M2) and WS fallback paths look available. `blob`, `sieve`, `quota`, `submission`, `vacationresponse` capabilities also present.

M1 live-Stalwart observations: `collapseThreads` honoured on `Email/query`; `inThread` works but go-jmap v0.5.3 omits it from `FilterCondition` — the wrapper extends it (`threadQuery`, see §9 mitigation, exercised). `Email/set` create + destroy verified in the test mailbox. Not yet exercised: EmailSubmission undo window, `Email/changes` behaviour (M2).

M2 live-Stalwart observations: EventSource push verified — a submitted message lands in the open client in ~1.05s, `$seen` patches in ~0.95s, and `Mailbox/changes` moves unread counts within the same window; `eventSourceUrl` uses the RFC 8620 §7.3 path-placeholder template (in-wrapper expansion). `Email/changes` and `Mailbox/changes` honoured on the live server. `cannotCalculateChanges` exercised in mockjmap only (Stalwart path unexercised — it advertises `canCalculateChanges`). Stalwart answers the implicit `Email/set` from `onSuccessDestroyEmail` reusing the submission's call id — batch responses must be scanned in order, not indexed by call id. EmailSubmission undo window still unexercised (M5).

M3 live-Stalwart observations: `Email/set` honoured for per-keyword patches (`keywords/$seen: true|null`) and mailbox membership patches (`mailboxIds/<id>: true|false` — move and copy both verified). `updated` travels in the array IdSet form; `destroyed` is an id array; per-id rejections land in `notUpdated`/`notDestroyed`. A 2-id batched triage round-trip completes in 2–5 ms. Permanent destroy inside Trash is immediate and irreversible — the client holds it behind a 5s cancel window instead. Blob upload (`POST uploadUrl`) and download (`downloadUrl` template with `{accountId}/{blobId}/{name}` placeholders) verified byte-exact.

M4 live-Stalwart observations: all FR-F1 filter fields honoured — `text` (full-text over body + headers), `from`, `to`, `subject`, `hasKeyword`, `hasAttachment`, and exclusive `after`/`before` on `receivedAt` (10/2980 window counts exact over a 3,000-message spread); `inMailbox` omitted = all-mailbox scope works. First page of a 3,000-message search: **13.7–22.2 ms** server-side (acceptance gate: < 1 s) — client overhead negligible. **Stalwart's index is token-only** (probed 2026-09-22): partial words match nothing on `text`/`subject`/`from`/`to`/`body`, wildcards are literal, and the `header` condition does not work for search — so true LIKE semantics are impossible server-side. The client therefore auto-falls-back to a progressive LIKE scan (§4.4): zero server results → newest-first walk over the scope's ids in 500-message batches, substring-matching subject/from/to in memory, matches streaming into the list with a header "scanning n/N" indicator; esc cancels; only match ids are held. Verified live: partial-word "fixtu" scan matched 3000/3000 fixture messages. Gotcha: the full-text index settles asynchronously after bulk creation (2972/3000 right after seeding) — the gate settle-waits. Persistent fixture mailbox `agent-test/search-fixture` (3,000 msgs) is left on the account for future development; `TestLiveSearchVerification` is idempotent (tops up, reuses).

Fill `?` cells during M2/M7 verification; new servers get a row here + integration config.

## 8. Testing strategy

- **Unit:** window manager (extension, trim, re-anchor, position preservation — table-driven, the most-tested code in the repo), change reconciliation, config, keyring fallbacks.
- **mockjmap:** deterministic in-process JMAP server (httptest) with scriptable event injection — drives sync engine tests without network.
- **Golden tests:** bubbletea v2 golden files for list/preview/compose/modals at 3 terminal sizes (120×40, 100×35, 60×25) × dark/light.
- **Integration:** live Stalwart test account via env creds only (AGENTS.md rules), run locally or agent-side; tests skip when unset. No local/docker Stalwart — the user's live server is the only integration target.
- **Soak:** script that runs 10 minutes of synthetic push events while scrolling — asserts RSS bound (NFR-2). Runs agent-side before commits touching `sync/`.

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

REQUIREMENTS §7 gates all pass on both Stalwart (live) and Fastmail; NFR-1/2 verified with numbers in the PR; docs (README, degradation matrix, keymap) updated; goreleaser artifacts; tag v0.1.0.
