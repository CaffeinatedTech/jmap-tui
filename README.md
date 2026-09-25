# jmap-tui

> A beautiful, fast, JMAP-first terminal email client. Live-synced, zero local storage, built for the modern mail protocol.

**Status: pre-alpha — M7 landed (reader, live sync, triage, search, composer, multi-account, and now the first-run account wizard: `jmap-tui login` adds an account — test the connection, pick the mailbox that opens first, secret to the OS keyring — a bare `jmap-tui` with no config launches it for you, and `ctrl+a` reopens it any time to add or edit an account; verified against two live Stalwart accounts). Release packaging and the Fastmail verification pass are the next milestone.** See [REQUIREMENTS.md](REQUIREMENTS.md) for scope, [PLAN.md](PLAN.md) for the build plan, and [AGENTS.md](AGENTS.md) for AI-agent contribution rules.

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

Three panes (≥100 cols) — sidebar · list · preview, one accent. Every column leads with a top rule: heavy and accent-coloured under the focused column, a hairline under the others — the focus indicator, readable even without colour. The sidebar's first row names the account whose folders it shows, bracketed by accent-filled cells so it can't be mistaken for a mailbox:

```text
jmap-tui  Inbox  1432 messages · 3 unread
─────────────────────────│━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━│──────────────────────────────────────────
█ Work                  █│  ★   Dana Ops         Deploy pipeline is… 35m    │From: Eve Security <from@example.test>
Inbox                   3│   ↩  Eve Security     Quarterly audit re… 3h     │To: me@example.test
Sent Items               │      Bob Thread       ▾ Re: planning s…   1d     │Date: Mon, 21 Sep 2026 07:00
  agent-test             │ ●    Alice Root         └ planning sync   1d     │Subject: Quarterly audit report attached
Archive                  │↓ more                                            │──────────────────────────────────────────
                         │                                                  │Hello,
```

The unified inbox (`i`) interleaves every account's mail by date, each row led by a one-cell **colour bar** naming its owner (six tints, assigned in account order — shown here as `█` / `▓`), and the preview header spells the account out in text. The folder tree stays the active account's — the sidebar label says whose. Actions always route to the owning account:

```text
jmap-tui  unified inbox  4 messages
─────────────────────────│━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━│──────────────────────────────────────────
█ Work                  █│█  ★   Dana Ops         Deploy pipeline i… 35m    │Account: Personal
Inbox                   3│▓   ↩  Eve Security     Quarterly audit r… 3h     │From: Eve Security <from@example.test>
Sent Items               │█      Bob Thread       ▾ Re: planning …   1d     │To: me@example.test
  agent-test             │█ ●    Alice Root         └ planning sync  1d     │Date: Mon, 21 Sep 2026 07:00
Archive                  │↓ more                                            │Subject: Quarterly audit report attached
                         │                                                  │──────────────────────────────────────────
unified  live  synced 10:00:00                                                        Old laptop: connect: auth rejected
```

Two panes at 60–99 cols (preview swaps in via `Tab`); single pane below 60. Dark and light palettes are terminal-adaptive.

A footer status line reports the sync state: connection mode (`live` / `polling` / `connecting…`), last-sync time, retry count and errors, and the active mailbox's unread/total counts. With several accounts it leads with the active account's name (or `unified`), and any account in error shows a named, right-aligned error. New mail slides in at the top of the list with a brief highlight.

## Install

```sh
# v0.1 goal
go install github.com/CaffeinatedTech/jmap-tui/cmd/jmap-tui@latest
jmap-tui
```

Pre-built binaries for Linux, macOS, and Windows are planned for the v0.1 release (goreleaser config is a release-milestone item); until then build from source.

## Quick start

First run starts the account wizard (re-runnable at any time with `jmap-tui login`):

```text
Add an account                       1 of 4 · details
Server URL     https://mail.example.com    (or https://api.fastmail.com)
Username       you@example.com
Password       ••••••••                     → stored in your OS keyring, never on disk
Account name   agent-test1                     (defaults to your username)

2 of 4 · connection        tests the session and lists your mailboxes
3 of 4 · opening mailbox   picks what opens first (saved as initial_mailbox)
4 of 4 · save              keyring write + config — password_file is offered
                           if no OS keyring is available (SSH/headless)
```

The wizard writes `config.toml` (never `prefs.toml`), pins `default_account` on a first account, and leaves any existing config content — comments included — untouched. A password supplied via `JMAP_TUI_PASSWORD_<ACCOUNT>` is used as-is and not stored.

Once an account exists, `jmap-tui login` — or `ctrl+a` from the running TUI (the TUI exits, runs the wizard, and restarts) — opens an **account picker**: choose an account to edit it in place (re-test the connection, rotate the password, re-pick the opening mailbox) or pick `+ add a new account`. Edits keep everything the wizard didn't ask about — `session_url`, `default_identity`, your comments — and leaving the password field empty keeps the current secret.

Advanced config lives at `$XDG_CONFIG_HOME/jmap-tui/config.toml` (default `~/.config/jmap-tui/config.toml`) — see [docs/config](REQUIREMENTS.md#fr-j--configuration--credentials). Several `[accounts.*]` tables configure every account (`default_account` picks the one that opens first; `S` switches, `i` toggles the unified inbox):

```toml
default_account = "work"

[accounts.work]
display_name  = "Work"
url           = "https://mail.example.com"
username      = "you@work.example.com"
default_identity = "you@work.example.com"   # optional: the composer's From
initial_mailbox  = "mb-abc123"              # optional: wizard-chosen; defaults to Inbox

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
| Any | `ctrl+s` | Advanced search (fielded form — works from the query bar or anywhere else) |
| Any | `Esc` | Clear the search (while a search view is open; no-op otherwise) |
| Query bar | `Tab` | Toggle scope: current mailbox ↔ all mailboxes |
| Any | `ctrl+z` | Undo last action (while its toast shows) |
| Any | `Tab` / `Shift+Tab` | Cycle panes |
| Any | `S` | Switch account (instant — every account stays warm) |
| Any | `i` | Toggle the unified inbox (all accounts, interleaved by date) |
| Any | `ctrl+a` | Add or edit an account (opens the wizard; the TUI restarts) |
| Any | `[` | Show/hide sidebar |
| Any | `?` | Help overlay |
| Any | `q` | Quit (or `ctrl+c` twice — cancels in-flight work first) |

Every key above is remappable: `[keys]` maps an action id — the action's dotted name (`list.down`, `ui.quit`, `list.archive`, …; the `?` overlay shows what each action does) — to a keystroke. Conflicts are rejected at startup — a global key rebound onto a pane key is an error, not a silent shadow:

```toml
[keys]
"list.down" = "ctrl+n"
"ui.quit"   = "ctrl+d"
```

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

- **v0.1** — feature-complete (everything above landed); the release milestone finishes it: Fastmail verification pass, goreleaser artifacts, tag `v0.1.0`
- **v0.2+** — push subscriptions, Sieve script management (RFC 9291), vacation responder, quota display (RFC 9425), advanced theming
- **Later** — IMAP provider behind the same provider interface (will use a local cache; see [PLAN.md](PLAN.md#imap-future))

## License

[MIT](LICENSE)
