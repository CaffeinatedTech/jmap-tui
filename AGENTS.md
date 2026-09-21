# AGENTS.md — rules for AI agents working on jmap-tui

This repo is developed heavily with AI agents. These rules exist so agent-generated work doesn't erode the two properties that make this project worth existing: **a UI that never chugs** and **a server that is the only source of mail truth**. Humans: these apply to you too.

## Project in one paragraph

jmap-tui is a JMAP-first terminal email client (Go, Bubble Tea/Lipgloss/Bubbles v2). No local message storage for JMAP — live sync via push, everything server-side, rolling-window pagination so huge mailboxes scroll like butter. Multi-account from day one. IMAP is a future provider behind the `mail.Provider` interface — **never write IMAP code, it's out of scope until the plan says otherwise.**

Authoritative docs: **REQUIREMENTS.md** = scope (source of truth), **PLAN.md** = architecture/milestones/designs, README.md = user-facing. If code and docs disagree, flag it and fix the docs or the code — don't let them drift.

## Golden rules (violations = rejected work)

1. **No local mail persistence.** Never write message data to disk: no caches, no spools, no JSON dumps, no embedding mail in logs. Only config + keyring + user-saved attachments touch disk. (NFR-4)
2. **Never block the UI thread.** All network I/O lives in `sync/`/`jmapclient/` and reaches the UI as `tea.Msg`/`tea.Cmd`. No synchronous calls in `Update`, no HTTP in `Init` without a Cmd, no locks held during render. (NFR-1)
3. **UI never imports `go-jmap`.** Only `internal/jmapclient` may. UI works with `internal/mail` + `sync` snapshot types. This is the swap seam — protect it.
4. **Respect window invariants.** The rolling window (PLAN §4.1) is the heart. Cursor tracks ids, not row indexes. Prefetches are debounced and coalesced. One outstanding query per window; stale results discarded by request id. Don't "simplify" this into fetch-everything.
5. **JMAP state strings are the only sync truth.** No client-side timestamps for sync decisions, no polling loops that bypass `/changes`.
6. **Secrets discipline.** Never log, `fmt`-print, or commit: passwords, tokens, Authorization headers. Redact in the debug logger (FR-K2). Test creds come from env only (below).
7. **No scope drift.** Features not in REQUIREMENTS.md need a REQUIREMENTS edit in the same PR. Milestone ordering per PLAN §6; M1 scope is a reader, period.

## Stack & conventions

- Go ≥ 1.24. Module: `github.com/CaffeinatedTech/jmap-tui`.
- UI: `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2` (v2 module paths — do not import `github.com/charmbracelet/*` v1 paths).
- JMAP: `git.sr.ht/~rockorager/go-jmap` (core, mail, mail/emailsubmission, core/push), only inside `jmapclient`.
- Config: TOML (`github.com/BurntSushi/toml` or equivalent — pick one, don't mix). Keyring: `github.com/zalando/go-keyring`.
- Style: standard Go. `gofumpt` formatting, `golangci-lint` clean, errors wrapped with `%w` + context, `context.Context` on every network call, no global mutable state, no `init()` side effects.
- Comments: explain *why*, not *what*; every exported symbol has a doc comment.
- Aesthetic: minimal & elegant (REQUIREMENTS FR-I2). One accent color. No emoji in UI chrome, no decorative borders, no rainbow.

## Commands

```sh
go build ./...                    # build
go test ./...                     # unit + golden + mockjmap
go test ./internal/sync/ -run TestWindow -v   # window manager tests (the important ones)
golangci-lint run                 # lint
gofumpt -l -w .                   # format
go run ./cmd/jmap-tui --version   # smoke
docker compose -f deploy/docker-compose.yml up -d   # local Stalwart for integration
```

CI must pass: build, vet, lint, test (unit+golden), and integration against the dockerized Stalwart.

## Testing rules

- New protocol/sync behaviour ships with tests using `test/mockjmap` (in-process fake server) — no real-network unit tests, ever.
- UI changes ship with golden files at the three standard sizes × dark/light (PLAN §8). Regenerate deliberately (`-update` flag) and review the diff like code.
- The window manager (PLAN §4.1) is the most-tested code in the repo. Table-driven: extend, trim, re-anchor, destroyed-cursor, `cannotCalculateChanges`, live rearrangement preserving position.
- Live-server integration tests read creds from env: `JMAP_TUI_TEST_URL`, `JMAP_TUI_TEST_USER`, `JMAP_TUI_TEST_PASSWORD`. **Unset env ⇒ tests skip. Never hardcode, never commit, never echo.**
- Soak test (RSS bound) runs in CI on PRs touching `sync/`.

## Live Stalwart test account — rules of engagement

The user provides a real account on their live Stalwart server for agent testing. Treat it carefully:

- Work only inside the designated test mailboxes (e.g., `agent-test/…`); create them if absent.
- Allowed: reading anything, full CRUD inside test mailboxes.
- Not allowed without explicit user instruction: mutating/deleting anything in Inbox, Sent, Drafts, Archive, or other real mailboxes; bulk operations; changing account settings; Sieve edits.
- After test runs, clean up test messages you created.
- If a test needs to send mail: send only between test addresses, and never more than a handful per run.
- Rate courtesy: batch method calls (JMAP does this natively), don't hammer the live server in loops.

## Repo chores expected of every agent

- Update PLAN.md milestone tables and the degradation matrix (§7) when you land or verify capabilities.
- REQUIREMENTS.md changes accompany scope changes (same PR).
- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`), ≤ 50-char subject.
- Don't commit: `.env*`, binaries, golden-test `*-update` artifacts without review, editor config beyond `.editorconfig`.
- Don't amend/rebase shared history without instruction; don't force-push.

## When you're unsure

- Scope question → REQUIREMENTS.md; design question → PLAN.md; both silent → **ask the user**, don't improvise.
- The user interviews agents for critical decisions (auth, storage, protocol choices, UI paradigms). Err toward asking over assuming.
