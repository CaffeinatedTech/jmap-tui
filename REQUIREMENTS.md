# Requirements — jmap-tui

**This document is the source of truth for scope.** PLAN.md describes *how*; this describes *what*. Changes to scope go here first.

Status markers: **[M1]…[M7]** = milestone in which the requirement lands (see PLAN.md), **[Release]** = the post-M7 release milestone (Fastmail gate, packaging, tag), **[FUTURE]** = explicitly out of v1.

---

## 1. Purpose

A terminal email client that treats JMAP as a first-class protocol — not a shim over IMAP. It should feel instant, look beautiful, and never store mail locally when talking to a JMAP server: the server is the source of truth, push events keep the client live, and incremental changes keep it cheap.

## 2. Glossary

| Term | Meaning |
|---|---|
| **JMAP** | JSON Meta Application Protocol — RFC 8620 (core), RFC 8621 (mail) |
| **Session** | Discovery resource at `/.well-known/jmap`; declares capabilities, API/upload/download/push URLs |
| **State string** | Monotonic token per data type per account; `/changes` converts one state into the next |
| **Query** | Server-side `Email/query` — sorted, filtered, offset/anchor-windowed ID list |
| **Rolling window** | The client-side slice of query results actually held in memory; extended on demand |
| **EmailSubmission** | JMAP's send mechanism, with server-side undo window |
| **Account** | One JMAP session (server + credentials + primary mail account id) |

## 3. Goals

1. **Instant feel.** Every keystroke responds immediately; network I/O never blocks the UI.
2. **Bounded resources.** Memory and bandwidth stay constant-ish regardless of mailbox size.
3. **Live.** The visible state matches the server within ~1 second of a push event.
4. **Beautiful.** Minimal & elegant: generous spacing, one restrained accent, typographic hierarchy, no visual noise.
5. **Zero local mail storage (JMAP).** No message database, no spool, no cache files. Everything refetchable.
6. **Full client.** Read *and* send, search *and* triage — a daily-driver, not a demo.
7. **Multi-account from day one.** Real support: per-account sync, account switcher, unified views.

## 4. Non-goals (v1)

- IMAP/POP3 support (future; provider interface designed for it, no code)
- Local full-text indexing or offline mode
- HTML/CSS rendering, images in the preview pane, remote content loading
- Calendar / contacts (JMAP has them; mail only for v1)
- Multi-user/CRM features, rules/sieve *editing* (display quota/sieve later, edit later)
- Mouse-driven UX as a primary path (TUI is keyboard-first; basic mouse scrolling is fine)

## 5. Constraints

- **Language/runtime:** Go ≥ 1.24, no cgo.
- **UI stack:** Bubble Tea v2, Lipgloss v2, Bubbles v2 (`charm.land/*`).
- **JMAP stack:** `git.sr.ht/~rockorager/go-jmap` as the protocol base (core + mail + push packages); a thin in-repo `jmapclient` layer wraps it. If go-jmap proves inadequate, replace the wrapper, not the app — UI and sync layers must not import go-jmap types directly.
- **Specs to honour:** RFC 8620 (core, incl. §7.3 EventSource push), RFC 8621 (Mail), RFC 8887 (WebSocket, optional transport), RFC 9291 (Sieve, display-later), RFC 9425 (quota, display-later), RFC 9007 (MDN), vacation-response capability (`urn:ietf:params:jmap:vacationresponse`).
- **Primary test server:** the user's live **Stalwart** instance (test credentials supplied per-environment; see AGENTS.md). Secondary: **Fastmail**. No local/docker test server.
- **Platform targets:** Linux, macOS, BSD; Windows best-effort.

---

## FR-A — Accounts & connection

- **[FR-A1] [M6]** Configure multiple accounts. Each account: display name, server URL or hostname (JMAP session discovered at `/.well-known/jmap`; explicit URL override), username, optional default identity.
- **[FR-A2] [M0]** Authentication via **app password / API token over HTTP Basic** for all endpoints (API, upload, download, EventSource). No cookies, no browser.
- **[FR-A3] [M1]** Every request and push connection carries a timeout, retry with exponential backoff (cap ~30s), and jitter. 401/403 surfaces a re-auth prompt, not a crash.
- **[FR-A4] [M6]** Account switcher (`S`): instant switch between accounts; each account has independent sync state.
- **[FR-A5] [M6]** **Unified view** merging the inboxes of selected accounts, interleaved by `receivedAt`. Unified mode is a view; actions route back to the owning account. Each row is led by a one-cell **colour bar** identifying its owning account (tints assigned in account order, cycling past the palette) and the preview header names that account in text. *(`colour bar` wording added at the M7 gate 2026-09-25 — the original name-badge column was replaced per user proposal; the attribution requirement is unchanged.)*
- **[FR-A6] [M1]** Per-account capability detection: features degrade gracefully (no push → poll; no submission → read-only banner). Unknown capabilities are ignored, never fatal.

## FR-B — Sync & state

- **[FR-B1] [M2]** On connect: fetch session, load mailboxes (`Mailbox/query` + `Mailbox/get`), identities, and the initial query window for the active mailbox.
- **[FR-B2] [M2]** **Live sync** via EventSource (`eventSourceUrl`), one stream per account. `StateChange` events trigger `/changes` for affected types; the in-memory store is updated incrementally.
- **[FR-B3] [M2]** Reconnect on stream failure with exponential backoff; after `n` failed attempts fall back to polling (`{Type}/changes` at 60s) and show a sync-status indicator.
- **[FR-B4] [M2]** **All mail state is in-memory.** Nothing message-related is written to disk. Process restart = clean refetch. The only persisted files are config and keyring entries.
- **[FR-B5] [M2]** State strings tracked per type per account. On receiving a `queryState` mismatch for an open query, re-anchor the query (see PLAN.md §4.2) rather than guessing.
- **[FR-B6] [M2]** Mailbox unread/total counts come from server-maintained `Mailbox` properties (`unreadEmails`, `totalEmails`) kept fresh via `Mailbox/changes`.
- **[FR-B7] [M4]** Optimistic UI: local actions (mark read, star, move, delete) reflect instantly and reconcile with server results; on server rejection, revert and toast the error.

## FR-C — Mailbox browsing

- **[FR-C1] [M1]** Sidebar mailbox tree: hierarchy, per-mailbox unread count, special mailboxes identified by role (`inbox`, `drafts`, `sent`, `trash`, `archive`, `junk`).
- **[FR-C2] [M1]** Selecting a mailbox issues a fresh `Email/query` for that mailbox and resets the list window, and **moves focus to the list** — Enter/`l` on a folder means reading starts there. *(Focus hand-off added post-M7 2026-09-25: the folder had opened but the cursor stayed in the sidebar.)*
- **[FR-C3] [M1]** Special semantics: opening **Drafts** opens in edit-aware mode (Enter on a `$draft` row edits it in the composer — the composer itself lands in M5); deleting from **Trash** offers permanent delete (server `/set` destroy); **Sent** shows recipients in the "from" column.
- **[FR-C4] [M1]** Sidebar collapsible (`[`) to give the list/preview more room.

## FR-D — Message list & rolling window

- **[FR-D1] [M1]** Rows show: flags (unread dot, star, attachment paperclip, answered/replied markers), sender, subject, date (relative < 7d, absolute otherwise), size on request, thread depth chevron. The list renders a window clamped to the panel height that **scrolls to keep the cursor on screen** (centered when the list overflows, clamped at both ends); the sidebar tree follows its selection the same way. *(Scroll-to-cursor added post-M7 2026-09-25: rows beyond the pane were rendered from the top, so a deep cursor ran off the panel and the selection disappeared.)*
- **[FR-D2] [M1]** Threads collapsed by default via `collapseThreads=true`; expanding loads full thread (`Thread/get` + `Email/get`) in place. Collapse state is per-view, in-memory.
- **[FR-D3] [M1]** **Rolling window:** the query runs server-side with `limit` = chunk (default 50). Scrolling within 10 rows of a loaded edge prefetches the next chunk (`position`/`anchor` offsets) so the user only ever experiences smooth endless scroll. Window may grow to a configurable cap (default 2,000 rows); scrolling further re-anchors and trims the far edge, silently — to the user it is still endless.
- **[FR-D4] [M1]** Only **summaries** are fetched for the window (`Email/get` with small property set). Bodies load lazily on open.
- **[FR-D5] [M2]** While a live update rearranges the list, preserve the user's visual position by message id, not row index. *(`snapshotLocked` now re-anchors the engine cursor from its tracked id on every publish — extensions and live patches previously shifted a raw index and moved the selection; found via the keymap v2 pass, 2026-09-25.)*
- **[FR-D6] [M1]** List must render its first frame from data already in memory within **16 ms**; any fetch shows skeletons/spinner rows, never a freeze.
- **[FR-D7] [M7]** `J`/`K` jump the cursor to the next/previous unread message. The scan runs engine-side and force-extends the window while it looks (unlike the scrolling prefetch it ignores the cursor-proximity threshold), bounded to a fixed number of chunk fetches per press; nothing found ⇒ the non-fatal notice "no more unread". The unified view scans its loaded merged rows. *(Added post-M7 2026-09-25, KEYMAP_PLAN.)*
- **[FR-D8] [M7]** Server-side list sort (`s`/`o` picker): newest first (default), oldest first, by sender, by subject, by size. The window re-queries like a mailbox open (one outstanding query, cursor to top), the choice persists per account in `prefs.toml` (FR-J1), and a fresh client restores it. Thread and search queries keep their own order; the unified merge stays date-ordered. *(Added post-M7 2026-09-25, KEYMAP_PLAN.)*

## FR-E — Reading

- **[FR-E1] [M1]** Preview pane shows headers (from, to, date, subject, mailbox), body, attachments strip.
- **[FR-E2] [M1]** Body preference: `text/plain` part. If none, convert the `text/html` part with an in-repo converter (strip script/style, unwrap links, footnote URLs). No remote fetches, ever.
- **[FR-E3] [M1]** Viewport paging (j/k/d/Ctrl-f etc.) inside the preview; scroll position resets per message. `PgUp`/`PgDn` page the preview **from any pane** — focus stays where it is, and the preview's own keys are unchanged while it is focused. *(Global paging added post-M7 2026-09-25: reading a long message should not require leaving the list first.)*
- **[FR-E4] [M3]** Attachment list with name, size, type; `s` saves via Bubbles filepicker (default `~/Downloads`); downloads use the session `downloadUrl` (blob ids). *(Moved from M2 at implementation: M2 stayed sync-focused and attachment save belongs with the M3 action set; approved 2026-09-21.)*
- **[FR-E5] [M4]** Full-screen message view toggle (`v`) hiding sidebar/list.
- **[FR-E6] [FUTURE]** Image preview via terminal graphics protocols; external pager pipe (`|`).

## FR-F — Search

- **[FR-F1] [M4]** `/` opens the query bar; typing issues debounced server-side `Email/query` filters (`text`, `from`, `to`, `subject`, `after`/`before`, `hasKeyword`, `inMailbox`, `hasAttachment`). Results reuse the list component; `Esc` returns to the mailbox view with position preserved — `Esc` is the general back key (close search → exit full-screen → clear selection; KEYMAP_PLAN §4).
- **[FR-F2] [M4]** Advanced search modal (`ctrl-s`): fielded form generating filter operators. *(`/ /` was the original wording — dropped at the M7 docs gate 2026-09-25: `/` must stay typeable in queries, and `ctrl-s` reaches the form from the bar or from a closed search view.)*
- **[FR-F3] [M4]** Search scope defaults to current mailbox; toggle to all mailboxes. Unified-account mode searches across selected accounts (parallel queries, merged). *(The unified-account sentence rides M6 — it needs the multi-account Hub (FR-A4/A5); deferred at the M4 gate 2026-09-22.)*

## FR-G — Actions & triage

- **[FR-G1] [M3]** Toggle read/unread (`Email/set` `keywords` `$seen`), star/flag (`$flagged`), answered/replied indicators (`$answered`, `$draft`), custom keywords [FUTURE UI].
- **[FR-G2] [M3]** Move (`m` → mailbox picker) and copy between mailboxes; delete moves to role-`trash` mailbox; permanent delete inside Trash.
- **[FR-G3] [M3]** Multi-select (`x`) with all actions applying to the selection as one batched `/set`.
- **[FR-G4] [M3]** Archive (`y`) → role-`archive` mailbox; if the server has no archive role, fall back to a user-chosen mailbox with a one-time prompt, remembered per account in app-managed prefs (FR-J1).
- **[FR-G5] [M3]** Every destructive action shows a brief undo toast (5s default) — implemented as a second `/set` reversal, not a modal.

## FR-H — Compose & send

- **[FR-H1] [M5]** Compose buffer with To/Cc/Bcc/Subject/Body; header/body focus zones; `Identity/get` populates From (`ctrl+i` picks when an account has several). Address completion from recent mail headers is **deferred past M5**: it needs an address source, not a control (see Open question 5). *(Deferred at the M5 gate 2026-09-25; it was marked M5-stretch.)*
- **[FR-H2] [M5]** Reply, reply-all, forward — quoting with attribution line (`On <date>, <name> <email> wrote:` then `> `-prefixed lines; forwards open with a `---------- Forwarded message ----------` block). Threading is carried by the immutable `inReplyTo` and `references` properties set on draft create (RFC 8621 §4.1.2.5). *(Was "handled by JMAP `inReplyToEmailId`" — no such property exists in RFC 8621; it appeared only in a 2017 draft of draft-ietf-jmap-mail and was dropped before the RFC was published. Corrected 2026-09-25 after verifying the replacement against the RFC text and live Stalwart.)*
- **[FR-H3] [M5]** Attachments via session `uploadUrl`; progress + cancel; multiple files.
- **[FR-H4] [M5]** Drafts: autosave (debounced `Email/set` into role-`drafts`) on blur/interval; discard confirmation (`y` discards and destroys the server draft, `n` keeps it). An edit writes a *new* Email and retires the old one: message content is immutable (RFC 8621 §4.1.2), so `Email/set update` may only patch keywords and mailbox membership — verified against live Stalwart 2026-09-25.
- **[FR-H5] [M5]** Send via `EmailSubmission/set`; **undo send** = client holds submission for a configurable delay (default 5s, `[compose] undo_delay` in config.toml) with a cancel toast, then submits; after submit, the `EmailSubmission`'s server undo window is reported if available.
- **[FR-H6] [M5]** Sending updates the Sent mailbox view optimistically; server reconciliation corrects any drift.

## FR-I — UI/UX

- **[FR-I1] [M1]** Layout: three panes (sidebar 24–30 cols · list · preview), responsive: two-pane < 100 cols, single-pane with stack navigation < 60 cols.
- **[FR-I2] [M1]** **Minimal & elegant theme system:** Lipgloss v2 adaptive colors (dark default, light aware), one accent color, semantic colors only (unread, danger, success, muted). Border styles used sparingly; whitespace does the separating. No gradients/emoji chrome in v1.
- **[FR-I3] [M1]** Vim-style keys as listed in README, fully remappable in config. Unambiguous defaults; conflicts forbidden. *Defaults redesigned as keymap v2 (2026-09-25, KEYMAP_PLAN.md): motion letters are motion-only (`h` archive became `e`), `Space` pages, `d` deletes, `e` archives, sizes moved to `S` so the account switcher could take `A`, `esc` is a contextual back chain — action ids unchanged, existing `[keys]` remaps keep working.*
- **[FR-I4] [M1]** Help overlay (`?`) auto-generated from the keymap, context-sensitive per pane.
- **[FR-I5] [M2]** Status line: connection state per account, last-sync time, sync errors, current mailbox counts.
- **[FR-I6] [M5]** Toasts (bottom-right) for async results; errors always actionable ("Retry", "Show details").
- **[FR-I7] [M6]** Account switcher UI + unified view toggle.
- **[FR-I8] [M7]** First-run wizard: add account, test connection, store secret, pick initial mailbox. Re-runnable via `jmap-tui login` **or from the running TUI (`ctrl+a`)** — the TUI exits, runs the wizard, and relaunches with the change applied. When accounts already exist the wizard opens on an account picker: selecting one **edits that account in place** (re-test, rotate the secret, re-pick the opening mailbox) while preserving hand-written config fields (`session_url`, `default_identity`, comments) and the existing secret when the password field is left empty. *(`ctrl+a` and in-place editing added at the M7 gate 2026-09-25 after user testing: there was no way to reach the wizard, or to fix a mistake, from inside the app.)*

- **[FR-I9] [M7]** Column chrome: every visible column leads with a top rule — **heavy accent rule under the focused column, hairline under the others**, so focus is visible for all three panes (including the cursor-less preview) and survives terminals without colour. The sidebar's first row is an **account label**: the display name of the account whose folder tree is shown (always — in the unified view the tree is still the active account's), bracketed by accent-filled end cells so it cannot be mistaken for a mailbox row. *(Added post-M7 2026-09-25: the sidebar had no indication of whose folders it listed — most confusing in the unified view — and focus was only inferable from the selection wash, which the preview never has.)*

- **[FR-I10] [M7]** Pane layout toggle (`z`): side-by-side (default) ↔ **stacked**, where the message list sits above the preview at a 50/50 height split (the preview never drops below its chrome plus one body row). The sidebar stays left at full height in both modes; at 60–99 cols stacked shows the list and preview together instead of swapping by focus (FR-I1's swap still applies side-by-side); below 60 cols the single-pane stack is unchanged, and full-screen view (`v`) is unaffected. The choice persists in `prefs.toml` (FR-J1) and is restored at startup. *(Added post-M7 2026-09-25: a top/bottom alternative for reading mail.)*

## FR-J — Configuration & credentials

- **[FR-J1] [M0]** Config: TOML at `${XDG_CONFIG_HOME:-~/.config}/jmap-tui/config.toml`; flags and env override config; config written only by the wizard/user, never rewritten silently by the app. App-managed preferences (remembered in-app choices, e.g. the archive destination per account) live in `prefs.toml` next to the config file; the app owns and writes only that file, never `config.toml`.
- **[FR-J2] [M0]** **Secrets live in the OS keyring** (service `jmap-tui`, entry per account). Config may reference `password_keyring = true`. Explicit opt-in escape hatch: `password_file` (chmod 600, warned) or `JMAP_TUI_PASSWORD_<ACCOUNT>` env var for headless use. Plaintext-in-config is a config-error, not a fallback.
- **[FR-J3] [M0]** Config schema validated at startup with precise, actionable errors.

## FR-K — Reliability, safety, observability

- **[FR-K1] [M1]** All network ops context-cancellable; `ctrl-c` twice exits cleanly, cancelling in-flight work.
- **[FR-K2] [M1]** Structured debug log (`--log-file`, `--log-level`) with **secrets redacted** (Authorization headers, passwords, full tokens). Log file never written unless requested.
- **[FR-K3] [M1]** Panic recovery with a crash report path; the TUI always restores the terminal.
- **[FR-K4] [M3]** Rate-limit courtesy: batch method calls per round-trip (JMAP's strength), debounce search/scroll fetches, single EventSource per account.

---

## 6. Non-functional requirements

| ID | Requirement |
|---|---|
| **NFR-1** | Keystroke→render latency ≤ 16 ms for purely local state changes. |
| **NFR-2** | Memory: message summaries for ≤ 2,000 rows + LRU of ≤ 100 fetched bodies + attachments metadata. Hard budget ~50 MB RSS with one account, large mailbox. |
| **NFR-3** | Cold start to interactive (config valid, server reachable): ≤ 1.5 s on broadband; mailbox tree renders before first query resolves. |
| **NFR-4** | Zero disk writes from mail data. Audit: no message content in config/log/cache unless user saves an attachment or export. |
| **NFR-5** | No secrets in memory dumps/logs/`%v` of config. Keyring accessed only at auth time. |
| **NFR-6** | Single static binary; `go install` and goreleaser artifacts. |
| **NFR-7** | Graceful degradation matrix documented per server (Stalwart, Fastmail) in PLAN.md §7. |
| **NFR-8** | Test coverage: protocol/sync layers ≥ 80%; UI golden tests for all major screens. |

## 7. Acceptance criteria (per milestone gate)

- **M0** — connects to the live Stalwart account, dumps session + mailbox tree in a CLI smoke command; local gate green (build, vet, lint, test).
- **M1** — daily-drivable *reader*: browse mailboxes, endless scroll a 10k+ message folder smoothly (bounded RSS verified), read messages incl. HTML→text, threads expand. Golden tests pass.
- **M2** — with the client open, mail sent to the account appears/flags change/counts update within ~1 s, without any manual refresh; kill the push stream and it recovers.
- **M3** — full triage workflow (select → read → star → archive → delete → undo) is smooth; server state verified to match via independent JMAP client.
- **M4** — search across 50k messages returns first page < 1 s (server-bound), advanced modal works, unified search merges accounts.
- **M5** — compose → attach → send → appears in Sent; undo cancels a send; draft survives restart (because it lives on the server).
- **M6** — two accounts configured; switch is instant; unified inbox interleaves correctly; actions never cross accounts.
- **M7** — wizard, themes, help, keymap, docs complete.
- **Release v0.1** **[Release]** — every gate above passes on Fastmail as well as Stalwart (PLAN §7), NFR-6 artifacts built, tag v0.1.0. *(Split out of M7 at the M7 scoping 2026-09-25: Fastmail verification, goreleaser, and the tag ride their own milestone so M7 stays polish.)*

## 8. Open questions

1. **HTML conversion fidelity** — in-repo converter is v1; do we ever want an optional external renderer (glow/w3m/pandoc) pipe? *(lean no for v1, revisit)*
2. **Address book integration** (JMAP CardDAV capability) for autocomplete — post-v1?
3. **Windows terminal support depth** — best-effort accepted for v0.1; confirm no blockers at M1. *(M1 note: no blockers observed — pure Go, no cgo, no platform-specific code paths; RSS measurement gracefully skips off-Linux.)*
4. **Default archive behaviour** when server exposes no archive role — create one (needs write perms) or prompt? *(Resolved M3 2026-09-21: prompt once via the mailbox picker, remember per account in app-managed prefs.toml — FR-J1, FR-G4.)*
5. **Address completion & address book** — FR-H1's completion and a browsable address book both need somewhere to get addresses from: the JMAP server (an address-book capability, if the account has one) or a local store. A local store collides with the zero-mail-storage rule (NFR-4) unless it is explicitly user-managed, so the source has to be chosen before either feature starts. *(Raised at the M5 gate 2026-09-25 when address completion was deferred.)*
