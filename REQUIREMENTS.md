# Requirements — jmap-tui

**This document is the source of truth for scope.** PLAN.md describes *how*; this describes *what*. Changes to scope go here first.

Status markers: **[M1]…[M8]** = milestone in which the requirement lands (see PLAN.md), **[Release]** = the post-M7 release milestone (Fastmail gate, packaging, tag), **[FUTURE]** = explicitly out of v1.

---

## 1. Purpose

A terminal email client that treats JMAP as a first-class protocol — JMAP only, no second protocol anywhere on the roadmap. It should feel instant, look beautiful, and never store mail locally: the server is the source of truth, push events keep the client live, and incremental changes keep it cheap.

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

- Any mail protocol other than JMAP (the `mail.Provider` interface is an internal seam, not an invitation — scope is JMAP only)
- Local full-text indexing or offline mode
- HTML/CSS rendering, images in the preview pane, remote content loading
- Calendar (JMAP has it; mail + contacts for v1)
- Address-book **sharing** and contact groups (RFC 9670 / JSContact `kind=group` are out of scope — see FR-L)
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
- **[FR-A2] [M0]** Authentication via **app password over HTTP Basic** (default) or **API token as `Authorization: Bearer`**, selected per account by the `auth` config key (`"bearer"` / default `basic`). The wizard's connection test probes Basic first and remembers Bearer when that is the only scheme the server accepts (Fastmail's JMAP API accepts Bearer only — verified live 2026-09-27). The chosen scheme applies uniformly to all endpoints (API, upload, download, EventSource) through the same origin-gated transport: credentials cover the origins the user configured **plus the endpoints the authenticated session itself advertises over `https`** (regional API hosts and blob CDNs — Fastmail needs this, verified live 2026-09-27; never cleartext, never IP literals), and never cross-origin redirects. No cookies, no browser.
- **[FR-A3] [M1]** Every request and push connection carries a timeout, retry with exponential backoff (cap ~30s), and jitter. 401/403 surfaces a re-auth prompt, not a crash.
- **[FR-A4] [M6]** Account switcher (`S`): instant switch between accounts; each account has independent sync state.
- **[FR-A5] [M6]** **Unified view** merging the inboxes of selected accounts, interleaved by `receivedAt`. Unified mode is a view; actions route back to the owning account. Each row is led by a one-cell **colour bar** identifying its owning account (tints assigned in account order, cycling past the palette) and the preview header names that account in text. **The on/off choice survives a restart**: it is remembered in `prefs.toml` (FR-J1) and restored at startup, but only with the two accounts the view needs — with a single enrolled account the toggle is a no-op, so nothing is restored (the reader is never stranded in a view it cannot leave). A restored start opens each account's inbox for the merge and records its configured starting mailbox (FR-I8 `initial_mailbox`) as the pre-unified position, so leaving unified lands where a non-unified session would have started. *(`colour bar` wording added at the M7 gate 2026-09-25 — the original name-badge column was replaced per user proposal; the attribution requirement is unchanged. Tint = account **identity**, not position: it is keyed to enrollment order and never moves when the sidebar display order changes — FR-C5. Persistence added 2026-09-28 for issue #2.)*
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
- **[FR-C2] [M1]** Selecting a mailbox issues a fresh `Email/query` for that mailbox and resets the list window, and **moves focus to the list** — Enter on a folder means reading starts there. *(Focus hand-off added post-M7 2026-09-25: the folder had opened but the cursor stayed in the sidebar. `l` retired as an open key 2026-09-26 — FR-C6: `l`/`→` expand the folder tree.)*
- **[FR-C3] [M1]** Special semantics: opening **Drafts** opens in edit-aware mode (Enter on a `$draft` row edits it in the composer — the composer itself lands in M5); deleting from **Trash** offers permanent delete (server `/set` destroy); **Sent** shows recipients in the "from" column.
- **[FR-C4] [M1]** Sidebar collapsible (`[`) to give the list/preview more room.
- **[FR-C5] [M8]** **Multi-account folder column**: the sidebar shows **every** connected account as one flow — an account header (display name bracketed by end-caps in that account's colour tint, FR-A5) followed by its tree, in display order. Headers are cursor rows: `Enter` on a header activates that account and keeps the cursor in the column; `Enter` on another account's folder switches to that account, opens the folder, and moves focus to the list (FR-C2 unchanged). `ctrl+up`/`ctrl+down` moves the cursor's whole account block one place — persisted as `account_order` in `prefs.toml` (FR-J1), restored at startup, and governing the switcher's order too — and the block that heads the order is the startup account: whatever is on top has `default_account` in `config.toml` set to it (FR-J1's one app-written key). Colour is identity: tints stay keyed to enrollment order, never moved by reorders. The cursor tracks row keys (account / account+mailbox), never indexes, so reorders and live tree changes re-anchor instead of stranding the selection. *(Added M8 2026-09-26 per user request. Folding an account's tree open/closed — designed-for in M8, its header is already a row — landed the same day as FR-C6. `l` retired as an activate key there too: Enter alone activates. `default_account` sync added 2026-09-28 for issue #3 — the top of the display order is what opens first.)*
- **[FR-C6] [M8]** **Folder folding**: `h`/`←` folds the row under the cursor shut; `l`/`→` unfolds it. With nothing to fold (leaf folder, already-folded row), `h` climbs to the parent row instead — folder → parent folder → account header — and a folded header is the end of the line. Folds are per account: an account header hides its whole tree, a folder hides its whole subtree (rows are rebuilt from the snapshot's pre-order walk each frame). A folded folder's unread count **rolls up** to the sum of what it hides; expanded rows keep their own server count. Rows with a subtree show a dim `▸` (folded) / `▾` (open) chevron in a two-cell slot before the name — leaf rows and folded-shut rows keep name alignment; account headers wear the same chevron. `l` never opens: **Enter alone** opens a folder or activates an account (FR-C2/FR-C5 amended). The fold set persists in `prefs.toml` as account ids + mailbox ids only (never names — NFR-4), pruned of stale ids and unenrolled accounts at save time (FR-J1). `[` is the sole sidebar show/hide key (FR-C4): the legacy `sidebar.close` action id stays remappable but ships unbound. *(Added 2026-09-26 per user request; supersedes the `l`-opens clauses in FR-C2/FR-C5.)*

## FR-D — Message list & rolling window

- **[FR-D1] [M1]** Rows show: flags (unread dot, star, attachment paperclip, answered/replied markers), sender, subject, date (relative < 7d, absolute otherwise), size on request, and a **two-cell thread slot** in front of the subject — `▸` when a collapsed thread has replies beneath it (Enter expands it), `▾` while it is open, blank otherwise; an open thread's members indent under that chevron with `└`. One cell is reserved after the slot so a subject never runs into the size/date column, and every row's slot is exactly two cells wide, so a chevron never moves the subject column (the folder tree's fold slot does the same, FR-C6). The member count behind `▸` is fetched once per view and again as rows appear; a single-message thread, a fuzzy-scan row and an unsized row all render the blank slot — no chevron without something beneath it. The list renders a window clamped to the panel height that **scrolls to keep the cursor on screen** (centered when the list overflows, clamped at both ends); the sidebar tree follows its selection the same way. *(Scroll-to-cursor added post-M7 2026-09-25: rows beyond the pane were rendered from the top, so a deep cursor ran off the panel and the selection disappeared.)*
- **[FR-D2] [M1]** Threads collapsed by default via `collapseThreads=true`; expanding loads full thread (`Thread/get` + `Email/get`) in place. Collapse state is per-view, in-memory. The chevron marks a thread **only when it has another member to render beneath it** — a single-message thread stays plain — and Enter expands **the row it is on**, never whatever the engine cursor points at (the unified cursor is app-side, FR-A5). A window holds one row per thread: a live reply supersedes its thread's existing row instead of appearing beside it. *(Chevron, target and one-row rules added 2026-09-26 after a wrong-row report; the `Thread/get` wording — briefly superseded by a non-standard `inThread` filter — is accurate again.)*
- **[FR-D3] [M1]** **Rolling window:** the query runs server-side with `limit` = chunk (default 50). Scrolling within 10 rows of a loaded edge prefetches the next chunk (`position`/`anchor` offsets) so the user only ever experiences smooth endless scroll. Window may grow to a configurable cap (default 2,000 rows); scrolling further re-anchors and trims the far edge, silently — to the user it is still endless.
- **[FR-D4] [M1]** Only **summaries** are fetched for the window (`Email/get` with small property set). Bodies load lazily on open.
- **[FR-D5] [M2]** While a live update rearranges the list, preserve the user's visual position by message id, not row index. *(`snapshotLocked` now re-anchors the engine cursor from its tracked id on every publish — extensions and live patches previously shifted a raw index and moved the selection; found via the keymap v2 pass, 2026-09-25.)*
- **[FR-D6] [M1]** List must render its first frame from data already in memory within **16 ms**; any fetch shows skeletons/spinner rows, never a freeze.
- **[FR-D7] [M7]** `J`/`K` jump the cursor to the next/previous unread message. The scan runs engine-side and force-extends the window while it looks (unlike the scrolling prefetch it ignores the cursor-proximity threshold), bounded to a fixed number of chunk fetches per press; nothing found ⇒ the non-fatal notice "no more unread". The unified view scans its loaded merged rows. *(Added post-M7 2026-09-25.)*
- **[FR-D8] [M7]** Server-side list sort (`s`/`o` picker): newest first (default), oldest first, by sender, by subject, by size. The window re-queries like a mailbox open (one outstanding query, cursor to top), the choice persists per account in `prefs.toml` (FR-J1), and a fresh client restores it. Thread and search queries keep their own order; the unified merge stays date-ordered. *(Added post-M7 2026-09-25.)*

## FR-E — Reading

- **[FR-E1] [M1]** Preview pane shows headers (from, to, date, subject, mailbox), body, attachments strip.
- **[FR-E2] [M1]** Body preference: `text/plain` part. If none, convert the `text/html` part with an in-repo converter (strip script/style, unwrap links, footnote URLs). No remote fetches, ever.
- **[FR-E3] [M1]** Viewport paging (j/k/d/Ctrl-f etc.) inside the preview; scroll position resets per message. `PgUp`/`PgDn` page the preview **from any pane** — focus stays where it is, and the preview's own keys are unchanged while it is focused. *(Global paging added post-M7 2026-09-25: reading a long message should not require leaving the list first.)*
- **[FR-E4] [M3]** Attachment list with name, size, type; `s` saves via Bubbles filepicker (default `~/Downloads`); downloads use the session `downloadUrl` (blob ids). *(Moved from M2 at implementation: M2 stayed sync-focused and attachment save belongs with the M3 action set; approved 2026-09-21.)*
- **[FR-E5] [M4]** Full-screen message view toggle (`v`) hiding sidebar/list.
- **[FR-E6] [FUTURE]** Image preview via terminal graphics protocols; external pager pipe (`|`).

## FR-F — Search

- **[FR-F1] [M4]** `/` opens the query bar; typing issues debounced server-side `Email/query` filters (`text`, `from`, `to`, `subject`, `after`/`before`, `hasKeyword`, `inMailbox`, `hasAttachment`). Results reuse the list component; `Esc` returns to the mailbox view with position preserved — `Esc` is the general back key (close search → exit full-screen → clear selection).
- **[FR-F2] [M4]** Advanced search modal (`ctrl-s`): fielded form generating filter operators. *(`/ /` was the original wording — dropped at the M7 docs gate 2026-09-25: `/` must stay typeable in queries, and `ctrl-s` reaches the form from the bar or from a closed search view.)*
- **[FR-F3] [M4]** Search scope defaults to current mailbox; toggle to all mailboxes. Unified-account mode searches across selected accounts (parallel queries, merged). *(The unified-account sentence rides M6 — it needs the multi-account Hub (FR-A4/A5); deferred at the M4 gate 2026-09-22.)*

## FR-G — Actions & triage

- **[FR-G1] [M3]** Toggle read/unread (`Email/set` `keywords` `$seen`), star/flag (`$flagged`), answered/replied indicators (`$answered`, `$draft`), custom keywords [FUTURE UI].
- **[FR-G2] [M3]** Move (`m` → mailbox picker) and copy between mailboxes; delete moves to role-`trash` mailbox; permanent delete inside Trash.
- **[FR-G3] [M3]** Multi-select (`x`) with all actions applying to the selection as one batched `/set`.
- **[FR-G4] [M3]** Archive (`y`) → role-`archive` mailbox; if the server has no archive role, fall back to a user-chosen mailbox with a one-time prompt, remembered per account in app-managed prefs (FR-J1).
- **[FR-G5] [M3]** Every destructive action shows a brief undo toast (5s default) — implemented as a second `/set` reversal, not a modal.

## FR-H — Compose & send

- **[FR-H1] [M5]** Compose buffer with To/Cc/Bcc/Subject/Body; header/body focus zones; `Identity/get` populates From (`ctrl+i` picks when an account has several). Address completion comes from the **JMAP contacts stores** (FR-L3): a suggestion popup under the focused address field plus `ctrl+g` quick search. *(Address completion from recent mail headers was deferred at the M5 gate 2026-09-25 and never shipped — the source question is now answered by FR-L.)*
- **[FR-H2] [M5]** Reply, reply-all, forward — quoting with attribution line (`On <date>, <name> <email> wrote:` then `> `-prefixed lines; forwards open with a `---------- Forwarded message ----------` block). Threading is carried by the immutable `inReplyTo` and `references` properties set on draft create (RFC 8621 §4.1.2.5). *(Was "handled by JMAP `inReplyToEmailId`" — no such property exists in RFC 8621; it appeared only in a 2017 draft of draft-ietf-jmap-mail and was dropped before the RFC was published. Corrected 2026-09-25 after verifying the replacement against the RFC text and live Stalwart.)*
- **[FR-H3] [M5]** Attachments via session `uploadUrl`; progress + cancel; multiple files.
- **[FR-H4] [M5]** Drafts: autosave (debounced `Email/set` into role-`drafts`) on blur/interval; discard confirmation (`y` discards and destroys the server draft, `n` keeps it). An edit writes a *new* Email and retires the old one: message content is immutable (RFC 8621 §4.1.2), so `Email/set update` may only patch keywords and mailbox membership — verified against live Stalwart 2026-09-25. The edit carries everything the draft holds — attachments included, loaded when the draft is opened — and every save re-adopts the server's message-scoped blob ids so the next save never references a replaced message's blob (found and fixed 2026-09-27 during the security-audit live verification; mockjmap models the same derive-at-write/free-with-message semantics).
- **[FR-H5] [M5]** Send via `EmailSubmission/set`; **undo send** = client holds submission for a configurable delay (default 5s, `[compose] undo_delay` in config.toml) with a cancel toast, then submits; after submit, the `EmailSubmission`'s server undo window is reported if available.
- **[FR-H6] [M5]** Sending updates the Sent mailbox view optimistically; server reconciliation corrects any drift.

## FR-I — UI/UX

- **[FR-I1] [M1]** Layout: three panes (sidebar 24–30 cols · list · preview), responsive: two-pane < 100 cols, single-pane with stack navigation < 60 cols.
- **[FR-I2] [M1]** **Minimal & elegant theme system:** Lipgloss v2 adaptive colors (dark default, light aware), one accent color, semantic colors only (unread, danger, success, muted). Border styles used sparingly; whitespace does the separating. No gradients/emoji chrome in v1.
- **[FR-I3] [M1]** Vim-style keys as listed in README, fully remappable in config. Unambiguous defaults; conflicts forbidden. *Defaults redesigned as keymap v2 (2026-09-25): motion letters are motion-only (`h` archive became `e`), `Space` pages, `d` deletes, `e` archives, sizes moved to `S` so the account switcher could take `A`, `esc` is a contextual back chain — action ids unchanged, existing `[keys]` remaps keep working.*
- **[FR-I4] [M1]** Help overlay (`?`) auto-generated from the keymap, context-sensitive per pane.
- **[FR-I5] [M2]** Status line: connection state per account, last-sync time, sync errors, current mailbox counts.
- **[FR-I6] [M5]** Toasts (bottom-right) for async results; errors always actionable ("Retry", "Show details").
- **[FR-I7] [M6]** Account switcher UI + unified view toggle.
- **[FR-I8] [M7]** First-run wizard: add account, test connection, store secret, pick initial mailbox. Re-runnable via `jmap-tui login` **or from the running TUI (`ctrl+a`)** — the TUI exits, runs the wizard, and relaunches with the change applied. When accounts already exist the wizard opens on an account picker: selecting one **edits that account in place** (re-test, rotate the secret, re-pick the opening mailbox) while preserving hand-written config fields (`session_url`, `default_identity`, comments) and the existing secret when the password field is left empty. *(`ctrl+a` and in-place editing added at the M7 gate 2026-09-25 after user testing: there was no way to reach the wizard, or to fix a mistake, from inside the app.)*

- **[FR-I9] [M7]** Column chrome: every visible column leads with a top rule — **heavy accent rule under the focused column, hairline under the others**, so focus is visible for all three panes (including the cursor-less preview) and survives terminals without colour. Sidebar rows are bracketed by **account headers** so chrome can never be mistaken for a mailbox row (FR-C5). *(Added post-M7 2026-09-25: the sidebar had no indication of whose folders it listed — most confusing in the unified view — and focus was only inferable from the selection wash, which the preview never has. Superseded at M8 2026-09-26: the single fixed top label became one header row per account — always emitted, tinted, and cursor-selectable — so the "tree is the active account's" clause above is retired with FR-C5.)*

- **[FR-I10] [M7]** Pane layout toggle (`z`): side-by-side (default) ↔ **stacked**, where the message list sits above the preview at a 50/50 height split (the preview never drops below its chrome plus one body row). The sidebar stays left at full height in both modes; at 60–99 cols stacked shows the list and preview together instead of swapping by focus (FR-I1's swap still applies side-by-side); below 60 cols the single-pane stack is unchanged, and full-screen view (`v`) is unaffected. The choice persists in `prefs.toml` (FR-J1) and is restored at startup. *(Added post-M7 2026-09-25: a top/bottom alternative for reading mail.)*

- **[FR-I11] [Release]** The help overlay (`?`) shows the build version — `jmap-tui <version>`, muted, right-aligned on the overlay's title row (left off when the terminal is too narrow to show both). Release binaries carry the git tag injected at build time (NFR-6, GoReleaser); local/dev builds report `0.0.0-dev`. Display only: **the app never checks for updates and never phones home.** *(Added 2026-09-27: there was no in-app way to see what version you're running; an update-check alert was explicitly rejected — indicator only. On the title row so it never costs a binding row or gets truncated on short terminals.)*

- **[FR-I12] [Release]** Paste into every text field: the wizard's four credential fields, the composer's headers and body, the search bar, the advanced-search fields, the contact form. Two sources, one destination — a terminal's bracketed paste (`ctrl+shift+v`, middle-click, whatever the terminal sends, delivered as `tea.PasteMsg`) and `ctrl+v` from the OS clipboard (the text widget's own bubbles command). Both reach the focused field as *messages*, the way the filepicker's directory reads already do, because the widget's clipboard reply is an unexported type no model can name: no binding, no keymap action, nothing to remap (FR-I3 unaffected). An overlay that owns the keyboard (switcher, pickers, help, the discard confirmation) keeps the paste to itself; a field that is not focused ignores it. Pasted text passes bubbles' rune sanitizer, which drops control bytes exactly as it does for typed input, so a paste can put nothing into a field a keystroke couldn't. The two raw type-to-filter strings (move/copy picker, contacts screen) are not text widgets and stay typed-only. *(Added 2026-09-28 for issue #4: paste keys reached no widget at all, so a password had to be typed.)*

## FR-J — Configuration & credentials

- **[FR-J1] [M0]** Config: TOML at `${XDG_CONFIG_HOME:-~/.config}/jmap-tui/config.toml`; flags and env override config; config is written by the wizard or the user — and by the app for **exactly one key**: `default_account`, which the app keeps pointing at the account at the top of the sidebar display order (FR-C5). That one key is replaced in place (an inline comment on the line survives) or spliced in before the first table header when absent, every other byte of the file is preserved, and the write is atomic at the file's existing mode; nothing else in `config.toml` is ever rewritten silently by the app. App-managed preferences (remembered in-app choices, e.g. the pane layout (FR-I10), whether the unified view was left on (FR-A5), the archive destination per account, the sidebar/switcher account order, FR-C5) live in `prefs.toml` next to the config file; the app owns that file plus the `default_account` key above. *(Amended 2026-09-28 for issue #3: until then the app never wrote `config.toml` at all.)*
- **[FR-J2] [M0]** **Secrets live in the OS keyring** (service `jmap-tui`, entry per account). Config may reference `password_keyring = true`. Explicit opt-in escape hatch: `password_file` (chmod 600, warned). The `JMAP_TUI_PASSWORD_<ACCOUNT>` env var exists **for automated testing only** (interviewed 2026-09-27): it must only ever hold test-account credentials, is documented nowhere as a deployment technique, and headless use is `password_file`. Plaintext-in-config is a config-error, not a fallback.
- **[FR-J3] [M0]** Config schema validated at startup with precise, actionable errors. Server and session URLs are part of that validation: absolute `https://` — `http://` only for loopback hosts (`127.0.0.1`, `::1`, `localhost`, for mockjmap/local dev), never `user:pass@` userinfo — and the same policy is enforced on the `--url`/`smoke --url` flag paths and in the wizard, so credentials can never travel cleartext or embed in a URL.

## FR-K — Reliability, safety, observability

- **[FR-K1] [M1]** All network ops context-cancellable; `ctrl-c` twice exits cleanly, cancelling in-flight work.
- **[FR-K2] [M1]** Structured debug log (`--log-file`, `--log-level`) with **secrets redacted** (Authorization headers, passwords, full tokens). Log file never written unless requested.
- **[FR-K3] [M1]** Panic recovery with a crash report path; the TUI always restores the terminal. The TUI and wizard run in the **alternate screen buffer**: quitting (or crashing) restores the pre-launch screen exactly — no leftover frame under the shell prompt. *(Alternate-screen clause added 2026-09-26 per user request.)*
- **[FR-K4] [M3]** Rate-limit courtesy: batch method calls per round-trip (JMAP's strength), debounce search/scroll fetches, single EventSource per account.

## FR-L — Contacts (JMAP for Contacts, RFC 9610)

- **[FR-L1] [M9]** Full-screen contacts view (`c`): an address-books column (per-account headers + books with counts), a name-sorted contact list with owner tint bars (FR-A5 wording: tint = account identity), and a detail column — responsive like FR-I1 (three columns ≥100 cols, two with a focus swap at 60–99, one focused column below 60), type-to-filter (`/`), esc-back chain (filter → clear → close). **Unified by default** when more than one account advertises the capability; rows merge across accounts and are account-qualified (JMAP ids are unique per account only). Gated on `urn:ietf:params:jmap:contacts`: without it the key shows a non-fatal message, nothing loads, never an error (FR-A6, FR-L6).
- **[FR-L2] [M9]** Contact CRUD: create/edit through one fielded modal (first/last name, emails, phones, organization, title, note; create also picks the target address book, defaulting to the account's `isDefault` book). The form round-trips the card: untouched collections and every property the form does not model are left alone (`/set update` patches only what changed, RFC 8620 §5.3 path patches keep sibling entries), and per-entry labels survive by value. Delete **holds** the `ContactCard/set destroy` behind a 5s undo toast cancelled with `ctrl+z` — destroy is irreversible, so the `/set` is not sent until the window closes (the mail-side prepareDestroy pattern). A card's address book is fixed at create; groups (`kind=group`) are hidden, never edited; address-book management itself (create/rename/delete book) and sharing (RFC 9670) are out of scope (§4).
- **[FR-L3] [M9]** Recipient completion in the composer (the source FR-H1 always needed): a suggestion popup under the focused To/Cc/Bcc field — top 5 matches, prefix hits ranked first, one row per email address, `up`/`down` move, `enter`/`tab` accept (inserts `Name <email>, `, focus stays), `esc` dismisses before any other esc meaning (the esc chain's newest front); `ctrl+g` opens a type-to-filter quick search over the same merged set. Sources: **every account with the capability**, deduplicated by email (case-insensitive), enrollment order wins. The store loads lazily on first compose or contacts-screen open and stays live via `ContactCard/changes` + push; an account whose user never touched contacts loads nothing — a cold store costs nothing (FR-L5, FR-K4).
- **[FR-L4] [M9]** `shift+n` opens the add-contact modal prefilled from the sender of the message under the cursor (the row's owner account in unified view, FR-A5); when a merged contact already holds that address the modal opens in **edit** mode instead of creating a duplicate. With no message under the cursor — or from the contacts screen, whose `n` starts blank — the form opens empty.
- **[FR-L5] [M9]** Contacts obey the storage rule: in-memory only, nothing derived from them is written to disk (NFR-4 applies to contact data too — it is PII).
- **[FR-L6] [M9]** Degradation: an account without the capability simply contributes no contacts to the merged views (suggestions come from the accounts that do have them); a server with no contacts support at all keeps the screen closed and toasts on `c`/`shift+n`. `ContactCard/changes` answering `cannotCalculateChanges` full-refetches the store (FR-B5 applied to contacts); contact errors surface on the contacts screen, never on the mail status line.

---

## 6. Non-functional requirements

| ID | Requirement |
|---|---|
| **NFR-1** | Keystroke→render latency ≤ 16 ms for purely local state changes. |
| **NFR-2** | Memory: message summaries for ≤ 2,000 rows + LRU of ≤ 100 fetched bodies + attachments metadata. Hard budget ~50 MB RSS with one account, large mailbox. |
| **NFR-3** | Cold start to interactive (config valid, server reachable): ≤ 1.5 s on broadband; mailbox tree renders before first query resolves. |
| **NFR-4** | Zero disk writes from mail data. Audit: no message content in config/log/cache unless user saves an attachment or export. |
| **NFR-5** | No secrets in memory dumps/logs/`%v` of config. Keyring accessed only at auth time. |
| **NFR-6** | Single static binary; `go install` and goreleaser artifacts. README documents the install trust model and how to verify a binary. |
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
- **M8** — every configured account's folder tree visible in one sidebar under its tinted header; Enter opens another account's folder (switching to it); `ctrl+up`/`ctrl+down` reorders an account block and a fresh client restores the order from `prefs.toml`; reordering never changes an account's tint.
- **M9** — contacts screen lists every supporting account's cards with owner tints and a detail column; create/edit round-trips through `ContactCard/set` with untouched properties intact; delete holds behind `ctrl+z`; the composer suggests recipients and `ctrl+g` picks one; `shift+n` prefills from the sender and edits an existing holder instead of duplicating; no-capability servers degrade to a toast. Verified against live Stalwart (probe + CRUD round-trip + push liveness).
- **Release v0.1** **[Release]** — every gate above passes on Fastmail as well as Stalwart (PLAN §7), NFR-6 artifacts built, tag v0.1.0. *(Split out of M7 at the M7 scoping 2026-09-25: Fastmail verification, goreleaser, and the tag ride their own milestone so M7 stays polish.)*

## 8. Open questions

1. **HTML conversion fidelity** — in-repo converter is v1; do we ever want an optional external renderer (glow/w3m/pandoc) pipe? *(lean no for v1, revisit)*

All earlier open questions are resolved and recorded on their requirements: address completion / address book → FR-L (JMAP contacts capability, no local store — NFR-4); default archive behaviour → FR-G4/FR-J1; Windows terminal depth → no blockers at M1.
