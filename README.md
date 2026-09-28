# jmap-tui

> **Your email, in the terminal.** Fast, live, and easy on the eyes — a modern
> mail client for people who'd rather type than click.

jmap-tui is a full email client that lives in your terminal. Read, reply,
search, archive, and send — across as many accounts as you like — with a
keyboard-first interface that stays out of your way. It talks **JMAP**, the
modern email protocol, which means mail updates arrive *live* and nothing is
ever copied to your hard drive.

- **Instant.** Every keystroke draws immediately. No spinners, no lag, no jank — even with 100,000 messages in a folder.
- **Live.** New mail, flag changes, and moves appear within a second. You never press refresh.
- **Private.** Your mail is never written to disk. Your password lives in your OS keyring. Quit, and not a trace of your mail is left behind.
- **Multi-account.** One sidebar for all your accounts, an instant switcher, and a unified inbox that merges everything by date.
- **Beautiful.** Clean typography, one accent colour, dark and light themes that follow your terminal.

```text
jmap-tui  Inbox  1432 messages · 3 unread
─────────────────────────│━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━│──────────────────────────────────────────
█ ▾ Work                █│  ★   Dana Ops         ▸ Deploy pipeline…  35m    │From: Eve Security <from@example.test>
  Inbox                 3│   ↩  Eve Security       Quarterly audit…  3h     │To: me@example.test
  Sent Items             │      Bob Thread       ▾ Re: planning sync 1d     │Date: Mon, 21 Sep 2026 07:00
▾ Archive                │ ●    Alice Root         └ planning sync   1d     │Subject: Quarterly audit report attached
    agent-test           │↓ more                                            │──────────────────────────────────────────
                         │                                                  │Hello,
```

Folders on the left, messages in the middle, the message itself on the right.
The heavy line under a column shows where your keyboard is.

**Works with** any JMAP server — [Stalwart](https://stalwartlabs.xyz/) and
[Fastmail](https://fastmail.com) are verified today, and Cyrus, Apache
James, and friends speak the same protocol.

**JMAP only.** One protocol, done properly — no other mail protocols, now
or on the roadmap.

**Everything you'd expect from a mail client, and then some:**

- Read threads, expand and collapse replies
- Archive, delete, move, copy, star, mark unread — with a 5-second undo on anything destructive
- Server-side search across one folder or all of them, plus a filter form
- Compose, reply, reply-all, forward — with attachments, drafts that live on the server, and a send delay you can cancel
- Contacts, unified across accounts, with autocomplete in the composer
- Multi-account with an instant switcher and a unified inbox
- Add an account from your email address alone — the JMAP server is discovered for you
- Credentials in your OS keyring, never on disk
- Rebind any key, pick your layout, dark or light

---

## Install

One command, on Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/CaffeinatedTech/jmap-tui/master/install.sh | bash
```

It downloads the latest binary for your machine into `~/.local/bin` and tells
you if that folder isn't on your `PATH` yet.

<details>
<summary>Other ways to install</summary>

**Prefer Go?**

```sh
go install github.com/CaffeinatedTech/jmap-tui/cmd/jmap-tui@latest
```

**Build from source**

```sh
git clone https://github.com/CaffeinatedTech/jmap-tui
cd jmap-tui && go build ./cmd/jmap-tui
```

**Manual download** — grab the archive for your OS from the
[releases page](https://github.com/CaffeinatedTech/jmap-tui/releases), unpack
it, and put `jmap-tui` somewhere on your `PATH`.

**Uninstall**

```sh
rm ~/.local/bin/jmap-tui            # wherever you installed it
rm -rf ~/.config/jmap-tui           # optional: config + preferences
```

</details>

### Verifying your binary

The one-liner trusts this repository and GitHub: `install.sh` picks the
release asset matching your machine and checks no signature or checksum —
a checksum shipped next to the binary would prove download integrity, not
origin, so none is offered. If "HTTPS from GitHub" isn't enough assurance:

- **Build from source** — the `git clone` + `go build` steps above; the Go
  toolchain verifies every dependency against `go.sum`.
- **Download directly** — fetch the archive yourself from the
  [releases page](https://github.com/CaffeinatedTech/jmap-tui/releases)
  instead of piping a script to your shell.
- **Compare out of band** — if you obtain a SHA-256 from a trusted channel
  (signed tag, separately verified announcement), compare it with
  `sha256sum jmap-tui` on the file you downloaded.

> **Status:** pre-release and under active development. It already does
> everything described on this page, verified against Stalwart and Fastmail;
> the `v0.1.0` tag is what's left before the v0.1 release.

## Getting started

**1. Install it** with the one-liner above, then run:

```sh
jmap-tui
```

**2. The account wizard opens.** Nothing to configure by hand — it walks you
through four short screens:

```text
Add an account                       1 of 4 · details
Email          you@example.com       the server is found from this address
Password       ••••••••••••••        → saved to your OS keyring, never to disk
Account name   Work                  (defaults to your username)

2 of 4 · finding server     DNS _jmap._tcp, then https://<domain>
2 of 4 · connection         tests the login and lists your mailboxes
3 of 4 · opening mailbox    picks the folder that opens first (Inbox is fine)
4 of 4 · save               writes the config — you're done
```

There is no Server URL to type: from your **email address** the wizard
discovers the JMAP server (the `_jmap._tcp` DNS record first, then the
domain itself — RFC 8620), verifies it with one anonymous request, and
fills the field in. When it can't find one, the **Server URL** field
appears for you to fill in; <kbd>ctrl+u</kbd> reveals it at any time.

Use an **app password** (Stalwart: any credential your admin gives you; the
usual rule) — not your real login password. **Fastmail is the exception:** its
JMAP API only accepts an **API token** (*Settings → Privacy & Security →
Manage API tokens*, scopes: mail, submission, contacts). The wizard probes the
server, works it out, and records it as `auth = "bearer"` in your config.

**3. Mail.** The wizard closes and your inbox appears. That's it.

Anything you can do in the wizard you can do again any time: run
`jmap-tui login`, or press <kbd>ctrl+a</kbd> inside the app to add another
account or edit this one.

## Guides

### Read your mail

1. Move around the folder list with <kbd>j</kbd>/<kbd>k</kbd>, press <kbd>Enter</kbd> to open a folder. Focus jumps to the message list automatically.
2. <kbd>j</kbd>/<kbd>k</kbd> walks the list; the preview pane follows along as you go.
3. <kbd>Tab</kbd> moves focus into the preview to scroll it, or press <kbd>v</kbd> to read full-screen.
4. Threads show a `▸` in front of the subject — <kbd>Enter</kbd> expands the replies, <kbd>Enter</kbd> again collapses them.
5. <kbd>PgUp</kbd>/<kbd>PgDn</kbd> pages the message from *anywhere* — focus never moves.

### Triage your inbox

1. <kbd>J</kbd> / <kbd>K</kbd> jumps to the next / previous **unread** message.
2. <kbd>e</kbd> archives, <kbd>d</kbd> deletes (to Trash), <kbd>*</kbd> stars, <kbd>u</kbd> marks read/unread. The list closes the gap, so you're straight onto the next message.
3. Made a mistake? A toast appears for five seconds — <kbd>ctrl+z</kbd> undoes it.
4. Doing several at once? <kbd>x</kbd> selects messages (they show a `×`), then hit <kbd>e</kbd> or <kbd>d</kbd> to act on the whole selection in one go.
5. <kbd>m</kbd> moves to another folder, <kbd>y</kbd> copies. Start typing to filter the folder picker.

### Find anything

1. Press <kbd>/</kbd> and type. Results stream in as you type — the search runs on the server, so it's fast even across 50,000 messages.
2. <kbd>Enter</kbd> confirms the search and drops you into the results; <kbd>Tab</kbd> widens the scope from this folder to *all* folders.
3. Need filters? <kbd>ctrl+s</kbd> opens a form: from, to, subject, dates, keywords, has-attachments.
4. <kbd>Esc</kbd> backs out — close the search, exit full-screen, clear your selection — one key, in that order.

### Write, reply, and send

1. Press <kbd>n</kbd> for a new message, <kbd>r</kbd> to reply, <kbd>a</kbd> reply-all, <kbd>f</kbd> forward. Replies quote the original for you.
2. <kbd>Tab</kbd> moves between To / Cc / Bcc / Subject / body. In an address field, contacts are suggested as you type — <kbd>Enter</kbd> accepts one. <kbd>ctrl+g</kbd> opens a full contact search.
3. <kbd>ctrl+a</kbd> attaches a file (a directory browser: <kbd>j</kbd>/<kbd>k</kbd> move, <kbd>Enter</kbd> attaches), <kbd>ctrl+i</kbd> picks the From address if your account has several.
4. <kbd>ctrl+s</kbd> sends — and then you have **five seconds to change your mind**: <kbd>ctrl+z</kbd> cancels the send and puts you back in the composer with everything intact.
5. Drafts save themselves to the *server* every couple of seconds, so you can quit and pick the draft up later (it's in your Drafts folder).

### Add more accounts

1. Press <kbd>ctrl+a</kbd> (the app restarts into the wizard) or run `jmap-tui login` — choose **+ add a new account** and go through the same four steps.
2. <kbd>A</kbd> switches accounts instantly — everything stays warm in the background, so there's no reload.
3. <kbd>i</kbd> toggles the **unified inbox**: every account's mail, interleaved by date, each row tagged with a colour for its account. Actions always go to the account that owns the message. The choice is remembered — quit while it's on and the next start opens there.
4. Every account's folder tree sits in one sidebar under its own tinted header. With a folder selected, <kbd>ctrl+↑</kbd>/<kbd>ctrl+↓</kbd> moves that whole account block up or down — the order is remembered, and the account that ends up on top becomes the default: the one that opens first next time.
5. One account offline? It shows a quiet error in the status bar and the others keep working.

### Contacts

1. <kbd>c</kbd> opens the contacts screen — address books, contact list, and details, merged across every account that supports contacts.
2. <kbd>n</kbd> new, <kbd>e</kbd> edit, <kbd>d</kbd> delete (held five seconds behind an undo toast), <kbd>/</kbd> filters the list.
3. Reading a message from someone new? <kbd>Shift+N</kbd> adds them as a contact, already filled in.
4. The composer's address fields suggest from these same contacts automatically.

### Make it yours

- <kbd>[</kbd> hides the sidebar for a distraction-free view; <kbd>z</kbd> flips between side-by-side and stacked panes.
- <kbd>s</kbd> sorts (newest, oldest, sender, subject, size) — remembered per account. <kbd>S</kbd> shows message sizes.
- `--theme dark`, `--theme light`, or the default, which follows your terminal.
- Every key can be rebound in config (see [Configuration](#configuration)).
- <kbd>?</kbd> shows the full key help, always contextual to where your cursor is.

## Keys

Everything is keyboard-driven. Keys are grouped by **where your focus is** —
press <kbd>?</kbd> at any time for the same list inside the app.

### Sidebar (folders)

| Key | What it does |
|---|---|
| <kbd>j</kbd> / <kbd>k</kbd> · <kbd>↑</kbd> / <kbd>↓</kbd> | Next / previous folder |
| <kbd>g</kbd> / <kbd>G</kbd> | Top / bottom of the tree |
| <kbd>Enter</kbd> | Open the folder (focus moves to the message list) |
| <kbd>h</kbd> / <kbd>←</kbd> | Fold a folder's subfolders — or the whole account |
| <kbd>l</kbd> / <kbd>→</kbd> | Unfold (never opens — that's <kbd>Enter</kbd>) |
| <kbd>ctrl+↑</kbd> / <kbd>ctrl+↓</kbd> | Move the account's block up / down |

### Message list

| Key | What it does |
|---|---|
| <kbd>j</kbd> / <kbd>k</kbd> · <kbd>↑</kbd> / <kbd>↓</kbd> | Next / previous message |
| <kbd>g</kbd> / <kbd>G</kbd> | First / last message |
| <kbd>Space</kbd> or <kbd>ctrl+f</kbd> / <kbd>ctrl+b</kbd> | Page down / page up |
| <kbd>ctrl+d</kbd> / <kbd>ctrl+u</kbd> | Half page down / up |
| <kbd>J</kbd> / <kbd>K</kbd> | Next / previous **unread** |
| <kbd>Enter</kbd> | Expand / collapse the thread |
| <kbd>u</kbd> | Toggle read / unread |
| <kbd>\*</kbd> | Toggle star |
| <kbd>x</kbd> | Select for a batch action |
| <kbd>e</kbd> | Archive |
| <kbd>d</kbd> (or <kbd>#</kbd>) | Delete — to Trash |
| <kbd>m</kbd> / <kbd>y</kbd> | Move / copy to another folder |
| <kbd>s</kbd> (or <kbd>o</kbd>) | Sort by… (remembered per account) |
| <kbd>S</kbd> | Show / hide message sizes |
| <kbd>n</kbd> / <kbd>r</kbd> / <kbd>a</kbd> / <kbd>f</kbd> | Compose / reply / reply-all / forward |

### Reading pane

| Key | What it does |
|---|---|
| <kbd>j</kbd> / <kbd>k</kbd> | Scroll a line |
| <kbd>d</kbd> / <kbd>u</kbd> | Half page down / up |
| <kbd>ctrl+f</kbd> / <kbd>ctrl+b</kbd> | Page down / page up |
| <kbd>g</kbd> / <kbd>G</kbd> | Top / bottom of the message |
| <kbd>PgUp</kbd> / <kbd>PgDn</kbd> | Page the message — works from *any* pane |
| <kbd>s</kbd> | Save attachments… |
| <kbd>v</kbd> | Full-screen message (press <kbd>Esc</kbd> to leave) |

### Composer

| Key | What it does |
|---|---|
| <kbd>Tab</kbd> / <kbd>Shift+Tab</kbd> | Next / previous field (To → Cc → Bcc → Subject → body → attachments) |
| <kbd>Enter</kbd> | Next field (in the body: new line) |
| type | Contact suggestions pop up under address fields — <kbd>↑</kbd>/<kbd>↓</kbd> choose, <kbd>Enter</kbd> inserts |
| <kbd>ctrl+g</kbd> | Full contact search inside an address field |
| <kbd>ctrl+i</kbd> | Choose the From identity (if the account has more than one) |
| <kbd>ctrl+a</kbd> | Attach a file — <kbd>j</kbd>/<kbd>k</kbd> move, <kbd>l</kbd> open, <kbd>h</kbd> back, <kbd>Enter</kbd> attach, <kbd>Esc</kbd> cancel |
| <kbd>ctrl+x</kbd> | Remove the highlighted attachment |
| <kbd>ctrl+s</kbd> | Send — then <kbd>ctrl+z</kbd> within 5s to cancel |
| <kbd>Esc</kbd> | Close (asks before discarding; <kbd>n</kbd> keeps the draft) |

### Contacts screen

| Key | What it does |
|---|---|
| <kbd>j</kbd> / <kbd>k</kbd> · <kbd>g</kbd> / <kbd>G</kbd> | Move through the list |
| <kbd>Tab</kbd> / <kbd>Shift+Tab</kbd> | Cycle columns (books · contacts · details) |
| <kbd>/</kbd> | Type-to-filter (<kbd>Esc</kbd> clears, <kbd>Esc</kbd> again closes) |
| <kbd>n</kbd> / <kbd>e</kbd> / <kbd>d</kbd> | New / edit / delete (delete waits 5s — <kbd>ctrl+z</kbd> cancels) |
| <kbd>Enter</kbd> | Show the selected contact |
| <kbd>c</kbd> | Close |

### Search bar, pickers & dialogs

| Where | Key | What it does |
|---|---|---|
| Search bar | type | Search runs as you type |
| | <kbd>Enter</kbd> | Confirm and jump into the results |
| | <kbd>Tab</kbd> | Scope: this folder ↔ all folders |
| | <kbd>Esc</kbd> | Close (position restored) |
| | <kbd>ctrl+s</kbd> | Advanced search form |
| Folder / mailbox picker | type | Type-to-filter |
| | <kbd>j</kbd> / <kbd>k</kbd> · <kbd>Enter</kbd> · <kbd>Esc</kbd> | Choose / confirm / cancel |
| Account switcher (<kbd>A</kbd>) | <kbd>j</kbd> / <kbd>k</kbd> · <kbd>Enter</kbd> · <kbd>Esc</kbd> | Pick an account |
| Help (<kbd>?</kbd>) | <kbd>?</kbd> / <kbd>q</kbd> / <kbd>Esc</kbd> | Close |

### Anywhere (global)

| Key | What it does |
|---|---|
| <kbd>/</kbd> · <kbd>ctrl+s</kbd> | Search · advanced search |
| <kbd>Esc</kbd> | Back — closes search, then full-screen, then clears selection |
| <kbd>A</kbd> | Switch account |
| <kbd>i</kbd> | Toggle the unified inbox |
| <kbd>c</kbd> · <kbd>Shift+N</kbd> | Contacts · add the sender as a contact |
| <kbd>Tab</kbd> / <kbd>Shift+Tab</kbd> | Cycle panes |
| <kbd>[</kbd> · <kbd>z</kbd> | Show/hide sidebar · switch pane layout |
| <kbd>ctrl+a</kbd> | Add or edit an account |
| <kbd>ctrl+z</kbd> | Undo the last action (while its toast shows) |
| <kbd>PgUp</kbd> / <kbd>PgDn</kbd> | Page the reading pane |
| <kbd>?</kbd> | Key help |
| <kbd>q</kbd> | Quit (<kbd>ctrl+c</kbd> twice cancels in-flight work first) |

Paste works in every field you can type in — the composer, the search bar,
the contact form, and the account wizard: <kbd>ctrl+v</kbd> reads your system
clipboard, and your terminal's own paste (<kbd>ctrl+shift+v</kbd> in most)
arrives as bracketed paste. Neither is a keymap action, so neither is
rebindable.

Rebinding a key is one line in your config — the action name is what you see in
<kbd>?</kbd>:

```toml
[keys]
"list.down" = "ctrl+n"
"ui.quit"   = "ctrl+d"
```

Conflicts are rejected at startup with a clear error rather than silently
shadowing each other.

## Configuration

Two small files live in `~/.config/jmap-tui/` (`$XDG_CONFIG_HOME` honoured).

**`config.toml` — your accounts and settings.** The wizard writes the account
bits for you; the parts you might edit yourself:

- `default_account` — which account opens first (the app keeps it on the
  account at the top of your sidebar order)
- `[accounts.<id>]` — server URL, username, display name, optional
  `initial_mailbox` (the folder that opens at startup), `default_identity`
  (the From address the composer defaults to), and `auth` (`"bearer"` for
  API-token servers like Fastmail; default is HTTP Basic)
- `[keys]` — rebind any key to an action id (see [Keys](#keys))
- `theme` — `dark`, `light`, or `auto` (follow your terminal)
- `[compose] undo_delay` — how long you get to cancel a send
- `[window]` — list chunk/prefetch sizes; rarely worth touching

```toml
default_account = "work"

[accounts.work]
display_name = "Work"
url          = "https://mail.example.com"
username     = "you@work.example.com"

[accounts.personal]
display_name = "Personal"
url          = "https://mail.example.com"
username     = "you@example.com"
# auth = "bearer"   # API-token servers (Fastmail); default is HTTP Basic

[compose]
undo_delay = "5s"   # how long you get to cancel a send; "0s" sends instantly
```

Passwords never land in this file — the wizard stores them in your OS keyring
or, on headless machines, in a `password_file` you pick. And if you'd rather
not edit TOML at all,
`jmap-tui login` edits an account through the wizard instead.

**`prefs.toml` — your in-app choices, saved as you make them.** Pane layout,
whether the unified inbox was left on, the order of account blocks in the
sidebar, which folders are folded shut, and per-account sort order and archive
destination. You shouldn't need to edit it; delete it if you ever want those
choices back to defaults.

## Troubleshooting

| Symptom | Fix |
|---|---|
| "couldn't find a JMAP server" in the wizard | Your address's domain published nothing to discover: publish a `_jmap._tcp.<domain>` SRV record pointing at your JMAP host, or just fill in the **Server URL** field the wizard reveals (<kbd>ctrl+u</kbd> shows it any time). |
| "connection failed" in the wizard | Check the server URL (it should be the JMAP URL — `https://api.fastmail.com` for Fastmail) and that you're using an **app password** — or on Fastmail an **API token** (the wizard probes and saves `auth = "bearer"`) — not your login password. |
| No OS keyring (SSH session, minimal container) | The wizard offers a `password_file` fallback — a chmod-600 file holding the app password. |
| Folders look stale | The status bar shows `live`, `polling`, or `connecting…`. `polling` means push dropped and it's falling back — it recovers on its own. |
| A search finds nothing | Server search matches whole words; jmap-tui then falls back to a fuzzy scan with a `scanning n/N` indicator. <kbd>Esc</kbd> cancels it. |
| Need details for a bug report | `jmap-tui --log-file /tmp/jmap-tui.log --log-level debug` writes a **redacted** log (secrets never appear in it). |

## Security

A hostile-server / hostile-mail / hostile-filesystem audit (2026-09-27 —
17 findings, 4 High / 9 Medium / 4 Low) probed the client end to end.
Every vector below is mitigated in code with regression tests.

**Credentials & transport**

- [x] `Authorization` following cross-origin redirects (C-1)
- [x] Credentials sent to session-supplied cross-origin URLs — now
      scoped to the authenticated session's own `https` endpoints
      (regional API/CDN hosts); cleartext, IP-literal and unadvertised
      origins — and every cross-origin redirect — still get nothing
      (C-2 revised, S-2); hostile blob names in the authenticated
      download path stay validated (S-2)
- [x] Cleartext `http://` to non-loopback hosts (C-3)
- [x] `user:pass@` userinfo accepted by config/flags or written to the
      debug log (C-3b, C-4)
- [x] Redirect loops; untrusted TLS failing open (S-1, S-8)
- [x] Unbounded JSON responses and attachment downloads — capped at
      32 MiB / 100 MiB (S-3, S-4)

**Mail → terminal**

- [x] Terminal escape/control injection via subject, preview, sender and
      mailbox names, body, error lines (T-1, T-3, T-5, T-6)
- [x] HTML entities decoding to controls after conversion (T-2)
- [x] UTF-8-splitting truncation in status/error lines (T-7)

**Files on disk**

- [x] Attachment path traversal and filename-driven save hangs (W-1, W-2)
- [x] Symlink clobber via crash reports and config/prefs writes —
      `O_EXCL` random temp names (W-4, W-5)
- [x] Pre-existing world-writable debug log (W-7)
- [x] `password_file` FIFOs, directories, and loose symlink targets (W-6)

**Config & identity**

- [x] Plaintext passwords smuggled into config (I-1)
- [x] CR/NUL carried through parsed address fields (I-6)
- [x] Env-var name collisions (C-7) — env vars are test-only (FR-J2)

**Supply chain**

- [x] `install.sh` `curl | bash` trust model — documented residual;
      verification options under [Install](#verifying-your-binary)
- [x] Dependency vulnerabilities — `govulncheck ./...` clean

Also probed and holding: SSE overlong-line recovery, HTML conversion
termination, goroutine hygiene under churn, config fuzzing, and a
race-free test suite.

## Roadmap

- **v0.1** — release binaries, `v0.1.0` (Fastmail verification done 2026-09-28)
- **v0.2+** — push subscriptions, Sieve script management, vacation responder, quota display, richer theming
- **Never** — a second mail protocol. jmap-tui is JMAP only; the `mail.Provider` seam is an internal boundary, not a roadmap.

## Development

jmap-tui is written in Go with [Bubble Tea](https://github.com/charmbracelet/bubbletea),
[Lipgloss](https://github.com/charmbracelet/lipgloss), and
[go-jmap](https://git.sr.ht/~rockorager/go-jmap). The repo is built for
AI-assisted development — read the rules before contributing:

| Doc | Purpose |
|---|---|
| [REQUIREMENTS.md](REQUIREMENTS.md) | What we're building — scope & requirements |
| [PLAN.md](PLAN.md) | How we're building it — architecture & milestones |
| [AGENTS.md](AGENTS.md) | Contribution rules (for agents and humans) |

## License

[MIT](LICENSE)
