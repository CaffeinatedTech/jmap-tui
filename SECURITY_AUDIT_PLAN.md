# SECURITY_AUDIT_PLAN.md — Full Security Audit Plan for jmap-tui

Status: plan (not yet executed). Companion to REQUIREMENTS.md (scope) and PLAN.md
(architecture). Findings from the audit will be recorded in a separate
`SECURITY_AUDIT_FINDINGS.md`.

All 7 open decisions (§6) were resolved by interview on 2026-09-27. Two of
them change the shape of this plan:

- **The audit is strictly read-only** (D-6): it produces
  `SECURITY_AUDIT_FINDINGS.md` only. No fixes, no repo modifications, no
  committed tests. Remediation happens in a **new session** that plans from
  the findings.
- **install.sh gets documentation, not checksum verification** (D-5): a
  checksum shipped alongside the binary authenticates nothing (an attacker
  who controls the download controls both). Document the trust model and
  give users verification options instead.

---

## 1. What we are auditing

jmap-tui is a JMAP-first terminal email client (Go, Bubble Tea v2). Relevant
security properties, distilled from AGENTS.md and REQUIREMENTS.md:

| Ref | Property |
|---|---|
| NFR-4 | **No local mail persistence** — no message data on disk except user-saved attachments |
| NFR-1 | UI thread never blocks; all network I/O behind `tea.Cmd` |
| NFR-5 | **Secrets never in logs / `%v` / crash reports** |
| FR-A2 | Auth is HTTP Basic app-password only (no OAuth in this codebase) |
| FR-K2 | Debug logger redacts secrets; no Authorization headers logged |
| FR-K3 | Crash report must be safe (no secrets, user-invoked) |
| FR-J2 | Secrets come from env → password file → OS keyring, in that order |
| Golden rule 6 | Never log/print/commit passwords, tokens, Authorization headers |

### Trust boundaries (the model for everything below)

```
 [untrusted]  JMAP server ──HTTPS──▶ jmapclient ──▶ sync/ ──▶ ui/ ──▶ terminal
 [untrusted]  email content (subject/from/body/attachments/mailbox names,
              server error text) — attacker-controllable via a hostile sender
              OR a hostile/compromised server
 [untrusted]  SSE events, session JSON (API/upload/download/EventSource URLs)
 [semi-trust] config.toml, prefs.toml, password file, env vars, CLI flags
 [trusted]    OS keyring, the user's own terminal emulator
 [untrusted]  install.sh supply chain (release artifacts)
```

Two primary adversaries:

- **A1 — Hostile sender:** can put arbitrary bytes in subject, display names,
  bodies (text/plain and text/html), attachment filenames, mailbox names (if
  shared), and anything else that round-trips through JMAP.
- **A2 — Hostile or compromised JMAP server:** controls session JSON URLs,
  all response bodies, HTTP status/detail text, SSE payloads, redirect
  targets, and blob contents. This is the stronger adversary and the one most
  often forgotten: the client *trusts the server for correctness*, but must not
  trust it for **confidentiality of the credential** or **integrity of the
  local filesystem**.

---

## 2. Preliminary findings (scouting pass, to be confirmed by the audit)

These were identified during reconnaissance. The audit's job is to confirm
each, assess exploitability, and attach a ready-to-land regression test (in
the findings appendix — the repo itself stays untouched, per D-6).

| ID | Finding | Location | Est. severity |
|---|---|---|---|
| F-1 | Attachment save joins server-controlled filename into the destination path with no `filepath.Base`/`..` rejection → path-traversal write (`../../.bashrc`) | `internal/app/triage.go:777`, `:792-805` (`uniquePath`) | High |
| F-2 | No control-character/ANSI sanitization anywhere on mail→terminal path (bodies, subjects, sender names, mailbox names, server error strings) | `internal/sync/engine.go:976-981`, `internal/mailtext/mailtext.go:293-309`, `internal/ui/panes.go:20-28`, `cmd/jmap-tui/smoke.go:112-170` | High |
| F-3 | Basic auth attached to **every** request inside `RoundTrip`, defeating Go's cross-origin header stripping on redirects; no `CheckRedirect`, no same-origin check on session-supplied URLs | `internal/jmapclient/errors.go:63-71`, `client.go:80-93` | High |
| F-4 | Crash report written to predictable `$TMPDIR` name via `os.WriteFile` (no `O_EXCL`) → symlink clobber | `cmd/jmap-tui/tui.go:450-459` | Medium |
| F-5 | `http://` accepted with no warning (cleartext Basic auth); only the wizard validates scheme | `internal/config/config.go:224-235`, `cmd/jmap-tui/tui.go:305-316`, `smoke.go:71-77` | Medium |
| F-6 | Unbounded reads: attachment `io.ReadAll`, JSON decode with no size cap (1 MiB `ServerError.Detail` embedded in error strings) | `internal/app/triage.go:772`, `internal/jmapclient/client.go:284,292,313,318` | Medium |
| F-7 | `install.sh` is `curl \| bash` with no checksum/signature verification | `install.sh` | Medium (distribution) |
| F-8 | Log records full `req.URL.String()`; URL userinfo (`user:pass@host`) would leak into the debug log → FR-K2/NFR-5 risk | `internal/jmapclient/logging.go:24` | Low–Med |
| F-9 | `keyring.EnvVar` maps `a-b` and `a_b` to the same env var → cross-account secret confusion | `internal/keyring/keyring.go:60-71` | Low |
| F-10 | Composer textarea pulls in `atotto/clipboard` (PATH-based `xclip`/`wl-copy` exec on copy/paste) | `internal/app/compose.go:693` + bubbles/textarea | Low |
| F-11 | Debug log file pre-existing with loose mode is not re-chmod'd | `cmd/jmap-tui/tui.go:418-445` | Low |
| F-12 | `app.truncateErr` byte-slices error strings at 117 bytes — can split UTF-8 runes (correctness/rendering issue in server-error path) | `internal/app/app.go:529-535` | Low |

---

## 3. Attack vectors and failure points

Organized by domain. Each vector lists: source → sink, why it fails, and the
test that proves/disproves it. Vector IDs are used by the findings template.

### 3.1 Credential handling & auth (A2: credential theft)

| ID | Vector / failure point | Source → sink | Notes |
|---|---|---|---|
| C-1 | **Cross-origin credential leak on redirect.** `basicAuthTransport.RoundTrip` re-sets `Authorization` on every hop, defeating Go's built-in cross-origin `Authorization` stripping. A 302 to `https://evil.example` exfiltrates the app password. | hostile server 30x → `errors.go:65` | No `CheckRedirect` set (`client.go:86-93`) |
| C-2 | **Credential sent to server-supplied URLs.** Session JSON supplies `apiUrl`, `uploadUrl`, `downloadUrl`, `eventSourceUrl`; all are fetched through the credentialed transport with no same-origin check against the configured host. | session JSON → `client.go:271`, `upload.go:33`, `download.go:33-38`, `sse.go:150-165` | Subdomain/attacker-host confusion |
| C-3 | **Cleartext transport.** `http://` accepted silently for `--url`, config, smoke → Basic password on the wire. | config/CLI → `config.go:224-235` | Wizard validates; load path does not |
| C-4 | **URL userinfo in logs.** `https://user:pass@host/` in config reaches `loggingTransport` via `req.URL.String()`. | config → `logging.go:24` | Violates FR-K2/NFR-5 |
| C-5 | **Secret in crash report / panic output.** Crash report captures logs + error text; if a secret ever reaches a log line or error string it lands in `$TMPDIR`. | panic → `tui.go:450-459` | Also: predictable filename (F-4) |
| C-6 | **Secret on disk.** Plaintext password in `config.toml` (rejected at `config.go:163-175` — verify bypasses), password file perms, wizard write path, `password_file` symlink to a world-readable file. | config/keyring → `keyring.go:106-145` | Existing tests cover happy paths |
| C-7 | **Env var collision.** `JMAP_TUI_PASSWORD_WORK_X` serves accounts `work-x` and `work_x` → wrong secret sent to wrong server. | env → `keyring.go:60-71` | F-9 |
| C-8 | **Process listing / env inheritance.** Password passed via env is visible in `/proc/<pid>/environ` to same-UID processes; children inherit. Documented tradeoff — verify warning is emitted (`tui.go:323-325`). | env | Acceptable if warned |

### 3.2 Hostile server / network (A2: integrity & availability)

| ID | Vector / failure point | Source → sink | Notes |
|---|---|---|---|
| S-1 | **Redirect chain abuse** (see C-1): open redirect, redirect loop, downgrade `https→http` on a hop. | server 30x | Go default 10 hops, no scheme pinning |
| S-2 | **Session URL confusion:** `apiUrl` on a different origin than `/.well-known/jmap`; API URL with embedded userinfo; `downloadUrl` template escaping (currently correct — verify). | session JSON | |
| S-3 | **Response-size DoS:** unbounded `json.NewDecoder(...).Decode(...)`; 1 MiB `ServerError.Detail` embedded in error strings then rendered/truncated (F-12 rune-split). | server body → `client.go:292,318` | |
| S-4 | **Attachment-size DoS:** `io.ReadAll` of blob with no cap → OOM before the save even happens. | blob → `triage.go:772` | |
| S-5 | **SSE abuse:** oversized lines (capped 1 MiB — verify), event floods, malformed JSON (dropped — verify), state-change storm causing reconcile thrash. | SSE → `sse.go:99-143` | |
| S-6 | **Malformed/deeply-nested JSON** in method responses → decoder resource use; typed structs limit blast radius — verify no `json.RawMessage` recursion. | server body | |
| S-7 | **Error-string injection into UI/status bar:** `MethodCallError.Description` and `ServerError.Detail` are server text rendered in toasts/status (`app.go:529`, `sync/live.go:694`) → terminal escape injection via error path. | server → UI | Overlaps T-1 |
| S-8 | **TLS config:** no custom `tls.Config`, no `InsecureSkipVerify` (grep-clean — verify); system roots only; confirm no fallback to plaintext after TLS failure. | — | Verify only |

### 3.3 Terminal rendering / untrusted content (A1 + A2: UI spoofing, terminal escape injection)

The app performs **no sanitization of control characters** anywhere. Bubble Tea
v2's cell renderer incidentally parses most sequences, but honours **SGR
styling and OSC-8 hyperlinks**, so spoofing and clickable-URL injection remain
achievable; `smoke` and stderr paths bypass the renderer entirely.

| ID | Vector / failure point | Source → sink | Payload examples |
|---|---|---|---|
| T-1 | **Escape injection via plain-text body** — `text/plain` used verbatim, no conversion. | sender → `engine.go:976` → `app.go:645` | `ESC[2J`, `ESC]0;pwnedBEL`, `ESC]8;;https://evilBEL…` |
| T-2 | **Escape injection via HTML body** — `mailtext` strips script/style but not C0/C1 controls (not whitespace). | sender → `mailtext.go:293-309` | same as T-1 embedded in `<p>` |
| T-3 | **Header/metadata injection:** subject, From display name, preview, mailbox names, account display name. | sender/server → `panes.go:339,443` | fake `ESC[K` + fake chrome, hidden text |
| T-4 | **Renderer-passthrough vectors:** SGR color injection (fake "verified" badge/colored chrome), OSC-8 hyperlink (`clickable link to attacker site`), `\r`/`\b` line rewriting inside a parsed line. | same | Renderer is incidental defence, not a contract |
| T-5 | **`smoke` stdout injection** — `printSession`/`printMailboxTree` print server strings raw to a real tty. | server → `smoke.go:112-170` | `ESC[?1049h`, OSC-52 clipboard write |
| T-6 | **stderr injection** — `main()` prints `ServerError.Detail` raw. | server → `main.go:18-21` | |
| T-7 | **Truncation bugs:** byte-slicing UTF-8 (`app.go:529`, `sync/live.go:694`) → mojibake/garbage render; `ansi.Truncate` preserves embedded escapes by design (`ui/panes.go:20-28`). | | |
| T-8 | **HTML→text residual risks:** entity decoding of `&nbsp;`/control refs, extremely long words (no wrap → column overflow), deeply nested tags (parser depth), `img alt` text as injection vector. | `mailtext.go` | |

### 3.4 Filesystem (A1 + A2: arbitrary write, clobber)

| ID | Vector / failure point | Source → sink | Notes |
|---|---|---|---|
| W-1 | **Attachment path traversal** — `filepath.Join(dir, a.Name)` with server-controlled `a.Name`, no `filepath.Base`, no `..`/absolute-path rejection. `uniquePath` even suffixes inside the escaped dir. | attachment name → `triage.go:777,792-805` | **F-1, High.** Upload side correctly uses `Base` (`compose.go:1229`) — inconsistent discipline |
| W-2 | **NUL byte / control chars / absolute path / Windows-ish `C:\` in filename**, empty name, name = `.`, name that is all dots, very long name (ENAMETOOLONG), reserved names. | attachment name | |
| W-3 | **Symlink escape:** destination is a symlink → write follows it; also `dir` itself resolved through symlinks. | filesystem | Combine with W-1 |
| W-4 | **Crash-report symlink race** — predictable `$TMPDIR/jmap-tui-crash-<unixsec>.log`, `os.WriteFile` no `O_EXCL` → attacker pre-creates symlink to victim file. | shared tmp → `tui.go:450-459` | F-4 |
| W-5 | **Config/prefs atomic write:** `path+".tmp"` not `O_EXCL`, mode preserved from existing file (`save.go:70-74`) — a 0644 config stays 0644. | config | |
| W-6 | **Password file symlink** at read time (`keyring.go:106-124` checks mode of target — verify symlink resolution) and at write time (`WritePasswordFile`). | keyring | |
| W-7 | **Debug log file loose pre-existing mode** not re-chmod'd (`tui.go:418-445`). | F-11 | |
| W-8 | **NFR-4 violations:** grep the tree for any message body/subject/contact write to disk (cache, spool, JSON dump, log body). Current recon says clean — prove it. | golden rule 1 | |

### 3.5 Input parsing & logic (config, CLI, JMAP semantics)

| ID | Vector / failure point | Notes |
|---|---|---|
| I-1 | Config TOML: unknown keys rejected (good), plaintext password rejected (good) — test bypasses: `Password` vs `password` key case, nested tables, multi-doc TOML, huge files. | `config.go:163-175` |
| I-2 | `password_file` pointing at `/etc/shadow`-style paths, FIFOs (blocking read), devices, directories. | `keyring.go:106-124` |
| I-3 | CLI flag injection: `--url` with userinfo/scheme tricks, `--log-file` to sensitive path (overwrite?), unexpected positional args. | `tui.go:53-68` |
| I-4 | Search: JSON filter built structurally (no concat — verify); client-side fuzzy scan is `strings.Contains` (no regex/ReDoS — verify). | `query.go:242-264`, `scan.go:77-109` |
| I-5 | JMAP semantics: keyword/flag spoofing, mailbox `sortOrder` tampering, `cannotCalculateChanges` handling, id confusion across accounts (multi-account id namespace collision). | window invariants |
| I-6 | Compose/send: header injection via address fields (`parseAddressList` strips `\n` — verify all fields), attachment upload path (`filepath.Base` used — verify), draft content never written to disk (NFR-4). | `compose.go:545-567`, `:1213-1266` |
| I-7 | Contacts: unbounded contact list render, control chars in contact names. | `contacts.go` |

### 3.6 Supply chain & dependencies

| ID | Vector / failure point | Notes |
|---|---|---|
| D-1 | `govulncheck ./...` on all direct + indirect deps (bubbletea v2, lipgloss v2, go-jmap, BurntSushi/toml, zalando/go-keyring, x/net, x/ansi, go-runewidth). | No CI — run locally |
| D-2 | `install.sh`: `curl \| bash`, no checksum/signature, regex asset selection, MITM on release JSON (HTTPS-only mitigation), installs 0755 to `~/.local/bin`. | F-7 |
| D-3 | `go.sum` integrity, no vendoring, `GONOSUMDB`/`GOFLAGS` tampering, typosquatted module paths. | |
| D-4 | Transitive surprise deps: `atotto/clipboard` (PATH exec), `godbus/dbus`, `protobuf`. License review / SBOM. | F-10 |
| D-5 | Clipboard exec: `xclip`/`wl-copy` resolved via `exec.LookPath` (PATH-dependent → PATH hijack if PATH is untrusted). Verify paste path doesn't execute anything. | Low |

### 3.7 Concurrency & memory safety

| ID | Vector / failure point | Notes |
|---|---|---|
| R-1 | Data races on shared engine/hub state under `-race` during push storms, multi-account pre-flight, search supersession. | Run full suite + soak with `-race` |
| R-2 | Goroutine leaks on SSE/body-close paths (leak = slow resource exhaustion). | `sse.go:52-71` |
| R-3 | Channel close/send races: `updates` cap-1 latest-wins, `changeBuffer` 64, per-account start loops. | `engine.go:1182-1193` |
| R-4 | `tea.Cmd` correctness: no network in `Update`, no locks held during `View` (NFR-1) — audit as availability property. | |
| R-5 | Unbounded LRU body cache growth (memory DoS via many large bodies). | `engine.go:954-997` |

### 3.8 Out of scope (record explicitly)

- **IMAP provider** — out of scope per AGENTS.md until the plan says otherwise.
- **JMAP server itself** (Stalwart) — we audit the client only.
- **OAuth** — no code path exists (only an unused indirect `x/oauth2` dep).
- **Terminal emulator vulnerabilities** — we assume the user's tty is trusted;
  we only guarantee we don't feed it attacker bytes.
- **Physical access, evil-maid, full-disk-encryption.**
- **DoS by the user's own server being down** (degradation matrix covers it).

---

## 4. Test plan

Method: static review → automated tests (unit/fuzz/golden) → dynamic probing
against `test/mockjmap` → optional live probing against the designated test
account (rules of engagement in AGENTS.md apply).

**Read-only rule (D-6):** the audit never modifies this repo. Verification
exploits and probe tests are developed in a throwaway `git worktree`
(`git worktree add /tmp/jmap-tui-audit HEAD` — deleted afterward) or as
standalone programs in `/tmp/opencode`, so the working tree and history stay
untouched. The exact regression-test code for each confirmed finding is
included **verbatim as an appendix** in `SECURITY_AUDIT_FINDINGS.md`, ready
for the remediation session to land alongside its fix. Findings are recorded,
not fixed.

### 4.1 Tooling gates (run first, record output)

```sh
govulncheck ./...            # D-1
golangci-lint run            # baseline (govet, etc.)
go vet ./...
go test ./... -race          # R-1..R-3
go test ./... -race -count=1 # dedupe cache effects
staticcheck ./...            # if not covered by golangci config
```

Manual greps (evidence for negatives):

```sh
# F-8/secret leakage
rg -n 'Password|password|token|Authorization' --glob '!*_test.go' | rg 'fmt\.(Print|Sprintf)|log\.|%[v+#w]'
# F-5 TLS
rg -n 'InsecureSkipVerify|tls\.Config|http://' --glob '!*_test.go'
# NFR-4
rg -n 'os\.(WriteFile|Create|OpenFile)|ioutil' --glob '!*_test.go'
# escape handling
rg -n '\\x1b|\\033|ESC\[' --glob '!*_test.go'
# process exec
rg -n 'os/exec|exec\.(Command|LookPath)'
```

### 4.2 Phase 1 — Credential & transport (C-*, S-1, S-2, S-8)

| Test | Vector | Design |
|---|---|---|
| `TestRedirectDropsAuthorization` | C-1 | mockjmap endpoint returns 302 to a second `httptest` server on a different origin; assert second server receives **no** `Authorization` header. Fails today. |
| `TestCrossOriginSessionURLRejected` | C-2 | session JSON with `apiUrl` on attacker origin; assert request is refused or sent unauthenticated. Define policy first (see §6 decision D-2). |
| `TestHTTPSchemeRejected` | C-1/C-3 | config/CLI with `http://example.com` → load fails closed; `http://127.0.0.1:…` (mockjmap) → accepted. D-1. |
| `TestLogRedactsUserInfo` | C-4 | client with `https://user:pass@host`; capture log lines; assert `pass` absent. |
| `TestCrashReportHasNoSecrets` | C-5 | force a panic with a known sentinel secret in scope; assert crash file lacks it. |
| `TestPlaintextConfigVariants` | C-6 | extend `config_test.go:70`: `Password`, nested tables, TOML multi-doc, comment-embedded. |
| `TestEnvVarCollision` | C-7 | accounts `work-x` / `work_x` resolve distinctly or error. |
| `TestPasswordFileSymlink` | W-6 | `password_file` → symlink to 0600 file, to 0644 file, to FIFO (bounded read), to dir. |
| `TestTLSDefaults` | S-8 | assert no `InsecureSkipVerify` reachable; httptest TLS server with untrusted cert must fail closed. |

### 4.3 Phase 2 — Hostile server (S-3..S-7)

| Test | Vector | Design |
|---|---|---|
| `TestHugeJSONResponseBounded` | S-3 | mockjmap returns 512 MB JSON stream; assert decode errors out under a size cap **and** RSS stays bounded. Fails today (no cap). |
| `TestServerErrorDetailBounded` | S-3 | 2 MB error body → assert `ServerError.Detail` capped and truncation is UTF-8-safe (T-7). |
| `TestAttachmentSizeCap` | S-4 | blob endpoint streams unbounded; assert a cap is enforced before `ReadAll` completes. |
| `TestSSEStorm` | S-5 | 10k events/sec + one 1 MiB line + malformed JSON; assert no goroutine growth, no crash, UI stays responsive (soak harness). |
| `TestServerErrorStringRendered` | S-7 | `ServerError.Detail` containing `ESC]8;;…` reaches status bar → assert sanitized at render. |

### 4.4 Phase 3 — Terminal escape corpus (T-1..T-8) — highest-volume phase

Build a **payload corpus** under `testdata/escapes/` (one payload per file or
table-driven): `ESC[2J`, `ESC[?1049h`, `ESC]0;titleBEL`, `OSC-8 hyperlink`,
`OSC-52 clipboard write`, `CSI 5 t` (window ops), `\r` line rewrite, `\b`,
`BEL` spam, `C1` controls (`0x9B`), `DEL`, RTL-override / bidi chars
(U+202E), zero-width chars, `CSI ? 2004 h` bracketed-paste confusion, NUL,
lone surrogates / invalid UTF-8.

| Test | Vector | Design |
|---|---|---|
| `TestBodySanitizedAtRenderBoundary` | T-1, T-2, T-4 | Inject payload as text/plain and text/html body; render at `ui.Render`/`View()`; assert output contains **no C0/C1 controls except `\n`/`\t`** and no OSC sequences. Table-driven over the whole corpus. |
| `TestSubjectFromMailboxSanitized` | T-3 | Same corpus through subject, From display name, preview, mailbox name, account display name. |
| `TestSmokeOutputEscapeFree` | T-5 | mockjmap with hostile session/mailbox names; run `smoke` capturing stdout; assert byte-clean. |
| `TestStderrEscapeFree` | T-6 | hostile `ServerError.Detail` through `main()` error path. |
| `TestHTMLTextControlStripped` | T-8 | `mailtext` corpus: entities → controls, `img alt`, long words, deep nesting, NUL. |
| `TestTruncateUTF8Safe` | T-7 | `truncateErr` over multi-byte strings at every boundary length 0..200. |

Silent-strip policy per D-3. Sanitize at a single choke point (render boundary
in `ui/` + explicit sanitize for `smoke`/stderr), not at every producer.

### 4.5 Phase 4 — Filesystem (W-1..W-8)

| Test | Vector | Design |
|---|---|---|
| `TestUniquePathTraversal` | W-1, W-2 | Table: `../../.ssh/authorized_keys`, `/etc/cron.d/x`, `..\\..\\x` (won't traverse on Linux but should still reject), `a/../../b`, `""`, `.`, `..`, `.hidden`, NUL, 300-char name, name with `\n`/`ESC`. Assert result stays inside `dir` (`filepath.Rel` + `..` check) or errors. **Fails today.** |
| `TestSaveAttachmentSymlinkDest` | W-3 | Pre-create destination as symlink to victim file; assert no clobber (`O_NOFOLLOW`/`O_EXCL` semantics or re-check after open). |
| `TestCrashReportNoSymlinkClobber` | W-4 | Shared temp dir; pre-create symlink at predicted crash path; trigger crash report; assert victim untouched. |
| `TestWriteAtomicModeAndExcl` | W-5 | Existing 0644 config → save → assert mode tightened or documented; `.tmp` pre-created as symlink. |
| `TestNoMailPersisted` | W-8 | Golden NFR-4 test: run a full session (read, search, compose, save one attachment) under a wrapped FS spy (or `strace`-style `os` shim) and assert the **only** writes are config/prefs/log/crash/attachment. |
| `TestLogFileMode` | W-7 | Pre-create log file 0666; assert re-chmod or refusal. |

### 4.6 Phase 5 — Input parsing & logic (I-*)

- Config: extend `config_test.go` with the bypass variants (I-1); FIFO and
  directory `password_file` (I-2); `--log-file` overwrite semantics (I-3).
- Search: property test that arbitrary query strings never panic and never
  build non-JSON filter strings (I-4).
- Compose: fuzz `parseAddressList` for `\n`/`\r`/NUL in every header field
  (I-6); assert upload path uses `Base`.
- Multi-account: id-collision test across two accounts with same mailbox ids
  (I-5).
- Contacts: 10k-contact list with hostile names renders bounded (I-7).

### 4.7 Phase 6 — Fuzzing (no fuzz targets exist today)

Develop the `FuzzXxx` targets in the throwaway worktree (their code ships
verbatim in the findings appendix, D-6), run each ≥30s
(`go test -fuzz=FuzzX -fuzztime=30s`), seed the corpus from real data:

1. `FuzzHTMLToText` — `internal/mailtext`: arbitrary bytes must never panic,
   never emit C0/C1 controls (except `\n\t`), terminate on deep nesting.
2. `FuzzConfigLoad` — TOML bytes → no panic, no secret echo in errors.
3. `FuzzSSELine` — SSE frame bytes.
4. `FuzzJMAPResponse` — JSON bytes into each response struct.
5. `FuzzAttachmentName` → `uniquePath` — result always within `dir`.
6. `FuzzParseAddressList` — header injection.
7. `FuzzTruncate` / `FuzzRenderLine` — UTF-8 safety + no escape emission.

### 4.8 Phase 7 — Concurrency & availability (R-*)

```sh
go test ./... -race
go test ./internal/sync/ -run TestSoak -race   # RSS-bounded soak
```

- Targeted: SSE drop/reconnect races, multi-account pre-flight, search
  generation supersession, engine body-LRU under concurrent push+render.
- Goroutine-leak check: capture `runtime.NumGoroutine` before/after 100
  connect/disconnect cycles in mockjmap.

### 4.9 Phase 8 — Supply chain (D-*)

- `govulncheck ./...` (record JSON output).
- SBOM: `go mod download && go list -m -json all` → CycloneDX/SPDX via
  `cyclonedx-gomod` if available; else a checked-in dependency inventory.
- License review of direct deps (all permissive today — confirm).
- `install.sh` review (**D-5: documentation only, no checksum work**):
  document the trust model in the findings — a checksum shipped alongside the
  binary authenticates nothing, since whoever controls the download controls
  both. Assess and describe user verification options (build from source,
  verify the GitHub release/attestation out-of-band, package managers). Flag
  regex asset selection and `| bash` flow as accepted residual risk unless
  the user later opts into signing infrastructure.
- Verify `.env*` untracked (`git ls-files` empty — already confirmed clean).

### 4.10 Phase 9 — Live probing (**approved**, D-7; rules of engagement apply)

Against the designated live Stalwart test account, **only** inside
`agent-test/…` mailboxes, batched, cleaned up after:

- Send one test message per high-value payload (subject + text/plain +
  text/html + attachment named `../../audit-traversal.txt`) from a test
  address to the test account; drive the TUI against it and observe the
  render boundary and save path.
- Never exceed a handful of sends per run; no mutations outside test
  mailboxes; no config/Sieve changes.

---

## 5. Deliverables & schedule

| Phase | Content | Est. effort |
|---|---|---|
| 0 | Scouting (done — this document) | — |
| 1 | Tooling gates + grep evidence report | small |
| 2 | Credential/transport tests (§4.2) → findings | medium |
| 3 | Hostile-server tests (§4.3) → findings | medium |
| 4 | Escape corpus + render-boundary sanitization tests (§4.4) | **large** |
| 5 | Filesystem tests (§4.5) | medium |
| 6 | Input-logic tests (§4.6) | small |
| 7 | Fuzz targets + first runs (§4.7) | medium |
| 8 | Race/soak (§4.8) | small |
| 9 | Supply chain (§4.9) | small |
| 10 | Live probe (approved, §4.10) | small |
| 11 | `SECURITY_AUDIT_FINDINGS.md`: severity ratings (CVSS-lite), exploit narratives, verbatim regression-test appendix, remediation recommendations | medium |

Ordering rationale: credential loss (Phase 2) and arbitrary file write
(Phase 5) are the two highest-severity classes; escape injection (Phase 4) is
the largest surface and needs the corpus designed first.

**Recording findings (D-6, read-only):** each finding gets ID, vector ref,
location (`file:line`), severity, exploit narrative, a **ready-to-land
regression test** (code verbatim in the findings appendix), and a recommended
remediation. The audit session changes **nothing** in this repo — no fixes, no
committed tests, no REQUIREMENTS edits. The findings document is the sole
deliverable; a **new session** will consume it to produce a remediation plan
(conventional commits, `fix:` with tests, REQUIREMENTS edits alongside any
scope change per AGENTS.md golden rule 7).

**Ongoing gates to recommend** in the findings (not implement now; no CI
exists): a lightest-possible local `scripts/security-gate.sh` running
`govulncheck`, `golangci-lint`, `go test -race ./...`, fuzz smoke
(`-fuzztime=10s` each), and the NFR-4 write-spy test.

---

## 6. Decisions (resolved — interviewed 2026-09-27)

| # | Question | Decision | Effect on the plan |
|---|---|---|---|
| D-1 | `http://` policy | **Reject non-loopback cleartext.** `http://` allowed only for loopback hosts (`127.0.0.1`, `localhost`, `[::1]`) so mockjmap tests keep working; anything else fails closed at config/CLI load. | §4.2 `TestHTTPSchemeWarned` becomes `TestHTTPSchemeRejected`; loopback carve-out is part of the contract. |
| D-2 | Cross-origin session URLs | **Allow but strip auth cross-origin.** Requests to session-supplied URLs with a different origin go out **without** `Authorization`. Worst case is a 401, never a credential leak; split-host/CDN deployments keep working. | C-1/C-2 tests assert absence of the header cross-origin and its presence same-origin. |
| D-3 | Control-character handling | **Strip silently**, at a single choke point: the `ui/` render boundary plus the `smoke`/stderr print paths. Remove all C0/C1 controls except `\n`/`\t`; bidi overrides (U+202E etc.) and zero-width chars stripped too. | §4.4 tests assert absence (not replacement glyphs) across the corpus. |
| D-4 | Response/attachment caps | **Hard-coded: 32 MiB JSON responses, 100 MiB attachments.** Over-limit fails with a clean error — never a truncated decode or a truncated file. Not configurable (YAGNI). | §4.3 tests assert the cap and the error path. |
| D-5 | `install.sh` integrity | **Documentation only.** A checksum shipped with the binary proves download integrity, not origin — whoever controls the download controls both. Document the trust model and user verification options (build from source, out-of-band release verification, package managers). | §4.9 is an assessment + README/findings text, no signing/checksum work. |
| D-6 | Findings disposition | **Strictly read-only audit.** Produce `SECURITY_AUDIT_FINDINGS.md` only; fix nothing in this session. A new session generates the remediation plan from the findings. | See the read-only rule in §4: PoCs run in throwaway worktrees/`/tmp`; regression tests ship as verbatim appendix, not commits. |
| D-7 | Live probe | **Approved: mockjmap first, then live.** Handful of payload sends into `agent-test/…` mailboxes to confirm real-world rendering against actual Stalwart; full AGENTS.md rules of engagement; clean up after. | §4.10 promoted from optional to approved. |
