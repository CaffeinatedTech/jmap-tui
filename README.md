# jmap-tui

> A beautiful, fast, JMAP-first terminal email client. Live-synced, zero local storage, built for the modern mail protocol.

**Status: pre-alpha — M1 landed (daily-drivable reader: mailbox browsing, rolling-window list, preview with HTML→text, thread expansion, live-verified against Stalwart).** See [REQUIREMENTS.md](REQUIREMENTS.md) for scope, [PLAN.md](PLAN.md) for the build plan, and [AGENTS.md](AGENTS.md) for AI-agent contribution rules.

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

- Multi-account (JMAP servers: Stalwart, Fastmail, Cyrus, Apache James)
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

Two panes at 60–99 cols (preview swaps in via `Tab`); single pane below 60. Dark and light palettes are terminal-adaptive.

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

Advanced config lives at `$XDG_CONFIG_HOME/jmap-tui/config.toml` (default `~/.config/jmap-tui/config.toml`) — see [docs/config](REQUIREMENTS.md#fr-k-configuration--credentials).

## Keys (default, remappable)

| Context | Key | Action |
|---|---|---|
| List | `j` / `k`, `↓` / `↑` | Next / previous message |
| List | `g` / `G` | Top / bottom |
| List | `Space` / `u` | Toggle read/unread |
| List | `*` | Toggle star/flag |
| List | `y` | Archive |
| List | `m` | Move to mailbox… |
| List | `#` | Delete (to Trash) |
| List | `x` | Select (multi-select) |
| List | `/` | Search |
| List | `R` | Force resync |
| Any | `c` | Compose |
| Any | `Tab` / `Shift+Tab` | Cycle panes |
| Any | `S` | Switch account |
| Any | `?` | Help overlay |
| Any | `q` | Quit |

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
