# jmap-tui

> A beautiful, fast, JMAP-first terminal email client. Live-synced, zero local storage, built for the modern mail protocol.

**Status: pre-alpha — M6 landed (reader, live sync, triage, search, composer, and now multi-account: an `S` switcher with per-account status, an `i` unified inbox that interleaves every account's mail by date with owner badges and routes every action back to the owning account, per-account failure isolation with connect retry, and search merged across accounts; verified against two live Stalwart accounts).** See [REQUIREMENTS.md](REQUIREMENTS.md) for scope, [PLAN.md](PLAN.md) for the build plan, and [AGENTS.md](AGENTS.md) for AI-agent contribution rules.

---

## Why another mail client?

Most terminal mail clients are built on IMAP — a protocol designed in the 1980s, retrofitted for decades. [JMAP](https://jmap.io) (RFC 8620/8621) is the modern replacement: JSON, batchable, state-synced, with server-side push.

**jmap-tui is built for JMAP first, not converted to it.** That means:

- **Live sync** — push events from the server keep the UI current in real time. New mail appears, flags change, mail moves — no manual refresh, no polling firehose.
- **Zero local mail storage** — the server is the database. State lives in memory, synchronised via JMAP's incremental `/changes`. Closing the client discards nothing important; reopening rebuilds in seconds.
- **Server-side everything** — search, sorting, threading, and unread counts run on the server. The client stays thin and fast.
- **Endless scroll that never chugs** — a rolling window over a server-side query. The list feels infinite; memory stays bounded no matter how big the mailbox.

IMAP support is a **future roadmap item**, designed for from day one via a provider interface — but v1 is JMAP only, using the full feature set.

## Features (v1 target)

- Multi-account — instant switcher, unified inbox, per-account status and failure isolation (JMAP servers: Stalwart, Fastmail, Cyrus, Apache James)
- Three-pane layout: mailbox sidebar · message list · reading pane
- Threaded view with collapse/expand
- Rolling-window message list — endless scroll, bounded memory
- Live sync via JMAP push (EventSource), with polling fallback
- Read/unread, star/flag, move, copy, delete (to trash), keywords
- Server-side search with a fast, keyboard-driven query bar
- Compose, reply, reply-all, forward — with drafts and identities
- Send with undo (configurable delay before submission)
- Attachment download and upload
- Credentials in the OS keyring (no plaintext secrets on disk)
- Minimal & elegant UI — restrained palette, clean typography, vim-style keys
- Full keybinding help (`?`) and first-run account wizard

## Screenshots

> Rendered output from the golden test suite (real UI frames, ANSI colours in the terminal):

Three panes (≥100 cols) — sidebar · list · preview, one accent, hairline rules:

```text
jmap-tui  Inbox  1432 messages · 3 unread
Inbox               3 │ ★   Dana Ops      Deploy pipeline is … 35m │ From: Eve Security <eve@example.test>
Sent Items            │   ↩   Eve Security  Quarterly audit r… 3h  │ To: me@example.test
  agent-test          │       Bob Thread    ▾ Re: planning sync 1d │ Date: Mon, 21 Sep 2026 07:00
Archive               │ ●     Alice Root     └ planning sync   1d  │ Subject: Quarterly audit report attached
                      │ ↓ more                                     │ ──────────────────────────────
                      │                                            │ Hello,
                      │                                            │ Find the quarterly audit report attached.
                      │                                            │ 1 attachment: audit-q3.pdf (242.5K)
```

The unified inbox (`i`) interleaves every account's mail by date, each row badged with its owner — actions always route to the owning account:

```text
jmap-tui  unified inbox  13 messages
Inbox               3 │ ●   Work      Dana Ops      Deploy pipeline is … 35m │ From: Eve Security <eve@example.test>
Sent Items            │     Personal  Eve Security  Quarterly audit r… 3h   │ Subject: Quarterly audit report attached
  agent-test          │     Work      Bob Thread    ▾ Re: planning sync  1d  │ Date: Mon, 21 Sep 2026 07:00
Archive               │ ●   Work      Alice Root     └ planning sync    1d  │ ↑ new mail
                      │ ↓ more                                              │ Hello,
unified  live  synced 10:00:00   Personal: push stream lost, reconnecting
```

Two panes at 60–99 cols (preview swaps in via `Tab`); single pane below 60. Dark and light palettes are terminal-adaptive.

A footer status line reports the sync state: connection mode (`live` / `polling` / `connecting…`), last-sync time, retry count and errors, and the active mailbox's unread/total counts. With several accounts it leads with the active account's name (or `unified`), and any account in error shows a named, right-aligned error. New mail slides in at the top of the list with a brief highlight.

## Install

```sh
# v0.1 goal
go install github.com/CaffeinatedTech/jmap-tui/cmd/jmap-tui@latest
jmap-tui
```

Pre-built binaries for Linux, macOS, and Windows will ship with releases.

## Quick start

First run starts an account wizard:

```text
Server URL:  https://mail.example.com      (or https://api.fastmail.com)
Username:    you@example.com
Password:    → stored in your OS keyring, never on disk
```

Advanced config lives at `$XDG_CONFIG_HOME/jmap-tui/config.toml` (default `~/.config/jmap-tui/config.toml`) — see [docs/config](REQUIREMENTS.md#fr-k-configuration--credentials). Several `[accounts.*]` tables configure every account (`default_account` picks the one that opens first; `S` switches, `i` toggles the unified inbox):

```toml
default_account = "work"

[accounts.work]
display_name  = "Work"
url           = "https://mail.example.com"
username      = "you@work.example.com"
default_identity = "you@work.example.com"   # optional: the composer's From

[accounts.personal]
display_name = "Personal"
url          = "https://mail.example.com"
username     = "you@example.com"

[compose]
undo_delay = "5s"   # 0s submits immediately
```

## Keys (default, remappable)

| Context | Key | Action |
|---|---|---|
| List | `j` / `k`, `↓` / `↑` | Next / previous message |
| List | `g` / `G` | Top / bottom |
| List | `Space` / `u` | Toggle read/unread |
| List | `*` | Toggle star/flag |
| List | `x` | Select (multi-select; batched actions) |
| List | `h` | Archive |
| List | `m` | Move to mailbox… |
| List | `y` | Copy to mailbox… |
| List | `#` | Delete (to Trash; permanent inside Trash, `ctrl+z` cancels) |
| Message | `s` | Save attachments… |
| Message | `v` | Full-screen message (hides sidebar + list) |
| List | `n` | Compose a new message |
| List | `r` | Reply |
| List | `a` | Reply to all |
| List | `f` | Forward |
| Drafts | `Enter` | Edit the draft in the composer |
| Any | `/` | Search — server-side query bar; `Enter` confirms and jumps into the results, `/` re-focuses the bar |
| Any | `ctrl+s` | Advanced search (fielded form; also `/` while the query bar is open) |
| Any | `Esc` | Clear the search (while a search view is open; no-op otherwise) |
| Query bar | `Tab` | Toggle scope: current mailbox ↔ all mailboxes |
| Any | `ctrl+z` | Undo last action (while its toast shows) |
| Any | `Tab` / `Shift+Tab` | Cycle panes |
| Any | `S` | Switch account (instant — every account stays warm) |
| Any | `i` | Toggle the unified inbox (all accounts, interleaved by date) |
| Any | `?` | Help overlay |
| Any | `q` | Quit |

Search runs server-side (`Email/query` filters) with a 300 ms keystroke debounce; results use the same rolling-window list, so huge result sets scroll like any mailbox. `Enter` confirms a search and moves the cursor into the filtered list — `j`/`k` navigate, `/` re-focuses the bar, `Esc` clears the search and restores the mailbox view with position preserved. The advanced modal (`ctrl+s`) composes fielded filters — text, from, to, subject, after/before dates, keyword, attachments — and every field is contains-style. Servers index whole words only, so when a search matches nothing server-side (e.g. a partial word like `0008`), the client automatically falls back to a fuzzy scan: it walks the scope newest-first and matches the chosen fields in memory (keyword, attachment and date filters still apply), streaming matches in with a `scanning n/N` indicator — `Esc` cancels.

Multi-selected rows show a `×` marker in the list; actions apply to the selection as one batched server call. Destructive actions show an undo toast for five seconds — `ctrl+z` reverses them (delete-inside-Trash is held for the same window before destroying).

The composer takes the whole screen: To/Cc/Bcc/Subject fields above a hairline, the body below it, `tab`/`shift+tab` moving between zones and `ctrl+i` choosing From when the account has more than one identity. Replies quote the original with an attribution line and `> `-prefixed lines; forwards carry a `---------- Forwarded message ----------` block. Drafts are written to the server on a two-second debounce, whenever you leave a field, and whenever you answer the discard prompt with `n` — so a draft survives a restart — editing one from the Drafts mailbox recreates it, because message content is immutable in JMAP (RFC 8621 §4.1.2). Attachments upload with a live percentage and cancel with `esc`. Sending holds the submission for `[compose] undo_delay` (default 5s) and shows a toast: `ctrl+z` cancels and puts you back in the composer with the draft intact. When the window closes the message is submitted once, and the server files it into Sent.

## Stack

- **Go** (≥ 1.24)
- [Bubble Tea v2](https://github.com/charmbracelet/bubbletea) + [Lipgloss v2](https://github.com/charmbracelet/lipgloss) + [Bubbles v2](https://github.com/charmbracelet/bubbles) — `charm.land/*` module paths
- [go-jmap](https://git.sr.ht/~rockorager/go-jmap) (RFC 8620 core + mail + push) with a thin in-repo client layer on top

## Docs

| Doc | Purpose |
|---|---|
| [REQUIREMENTS.md](REQUIREMENTS.md) | What we're building — functional & non-functional requirements |
| [PLAN.md](PLAN.md) | How we're building it — architecture, milestones, risks |
| [AGENTS.md](AGENTS.md) | Rules for AI agents (and humans) contributing to the repo |

## Roadmap

- **v0.1** — everything above against JMAP servers (Stalwart & Fastmail first)
- **v0.2+** — push subscriptions, Sieve script management (RFC 9291), vacation responder, quota display (RFC 9425), advanced theming
- **Later** — IMAP provider behind the same provider interface (will use a local cache; see [PLAN.md](PLAN.md#imap-future))

## License

[MIT](LICENSE)
