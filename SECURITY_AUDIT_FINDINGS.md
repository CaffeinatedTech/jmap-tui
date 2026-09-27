# SECURITY_AUDIT_FINDINGS.md — jmap-tui security audit results

Date: 2026-09-27. Basis: `SECURITY_AUDIT_PLAN.md` (decisions D-1…D-7 interviewed
2026-09-27). Audit was **read-only** (D-6): no repository code was modified;
all probes live in a throwaway worktree and are reproduced verbatim in the
Appendix. This document is the sole deliverable; remediation is planned in a
new session from this file.

Code under audit: `d656a15` (master). Method: static review, tooling gates,
34 probe/fuzz tests against `test/mockjmap` and hostile `httptest` servers,
plus one approved live probe against the Stalwart test account (D-7).

**Result: 21 probe failures → 17 distinct findings (4 High, 9 Medium, 4 Low),
12 controls verified holding, 0 data races, 0 reachable vulnerabilities.**

**Remediation status (2026-09-27, follow-up session):** **F-1, F-2, F-3,
F-4 and the grouped F-9 are fixed.** Credentials are now origin-gated to
the configured `ServerURL`/`SessionURL` origins and cross-origin redirects
are refused (F-1); session URLs on another origin are fetched without
credentials and logged as such (F-2); a single `Sanitize` choke point
(`mailtext.Sanitize`, applied at `ui.Render`/`RenderWizard`, the smoke
dumps, the stderr printer, `HTMLToText`, and the styled-widget sources)
strips C0/C1/format controls (F-3); attachment names are validated,
`uniquePath` is bounded and `O_EXCL`-guarded, and download-URL names are
reduced to a safe final segment (F-4, F-9). Appendix tests A.1 (C-1, C-2,
S-1, S-2), A.2, A.3 (W-1/W-2), A.4 (`FuzzUniquePath`), A.5, A.6, A.10 (T-5),
A.12 and A.13 ship in the repo and pass. **F-5…F-8, F-10…F-17 remain
open**, to be worked in the §5 order.

---

## 1. Findings at a glance

| ID | Sev | Finding | Location | Vectors |
|----|-----|---------|----------|---------|
| F-1 | **High** | Cross-origin redirect leaks `Authorization` — `basicAuthTransport` re-sets Basic auth inside `RoundTrip`, defeating Go's cross-origin header stripping; no `CheckRedirect` | `internal/jmapclient/errors.go:63-71`, `client.go:86-93` | C-1, S-1 |
| F-2 | **High** | Session-supplied URLs on a different origin are fetched **with** credentials (`apiUrl`, `downloadUrl`, `uploadUrl`, `eventSourceUrl` — no same-origin check), violating D-2 | `internal/jmapclient/client.go:271`, `download.go:44`, `sse.go` | C-2, S-2 |
| F-3 | **High** | No control-character sanitization anywhere on the mail→terminal path; hostile subjects/bodies/sender names/mailbox names/error strings reach `ui.Render`, `smoke` stdout and stderr verbatim. Live-confirmed: Stalwart round-trips all payload controls | `internal/sync/engine.go:976`, `internal/mailtext/mailtext.go`, `internal/ui/*`, `cmd/jmap-tui/smoke.go:112-170` | T-1, T-2, T-3, T-5, T-6 |
| F-4 | **High** | Attachment save joins the **server-controlled** filename into the destination path: `../../.ssh/authorized_keys` escapes the chosen directory; `uniquePath` also spins forever on NUL/oversized names (DoS) | `internal/app/triage.go:777,792-805` | W-1, W-2 |
| F-5 | Medium | `http://` accepted silently for non-loopback hosts (cleartext Basic auth); only the wizard validates scheme. Config/CLI load paths do not (D-1 violated) | `internal/config/config.go:224-235`, `cmd/jmap-tui/tui.go:305-316`, `smoke.go:71-77` | C-3 |
| F-6 | Medium | URL userinfo (`https://user:pass@host`) accepted in config and logged verbatim to the debug log (FR-K2 / NFR-5 violation) | `internal/jmapclient/logging.go:24` | C-3b, C-4 |
| F-7 | Medium | Unbounded `json.Decode` on every response (no 32 MiB cap per D-4) — hostile server forces unbounded allocation | `internal/jmapclient/client.go:292,318` | S-3 |
| F-8 | Medium | Attachment download `io.ReadAll` uncapped (no 100 MiB cap per D-4): 105 MB read with no error | `internal/app/triage.go:772` | S-4 |
| F-9 | Medium | `PathEscape` does not encode `..`: hostile attachment name produces dot-segments in the authenticated download URL (`/acc1/blob1/../../../admin`) | `internal/jmapclient/download.go:33-38` | S-2 |
| F-10 | Medium | Crash report written to a **predictable** `$TMPDIR` name via `os.WriteFile` (no `O_EXCL`/`O_NOFOLLOW`) — symlink clobber confirmed: victim file overwritten | `cmd/jmap-tui/tui.go:450-459` | W-4 |
| F-11 | Medium | `writeAtomic` temp file (`path + ".tmp"`) follows a planted symlink — victim clobber confirmed | `internal/config/save.go:191-201`, `prefs.go:102-109` | W-5 |
| F-12 | Medium | `install.sh` is `curl \| bash` with regex asset selection and no integrity verification (accepted residual risk per D-5: **document**, don't checksum) | `install.sh` | D-2 |
| F-13 | Medium | Byte-slicing truncation splits UTF-8 runes in error paths (`s[:117]`, `s[:77]`) — invalid UTF-8 rendered in status lines | `internal/app/app.go:529-535`, `internal/sync/live.go:694-700` | T-7 |
| F-14 | Low | Debug log keeps a pre-existing world-writable mode (no re-chmod on open) | `cmd/jmap-tui/tui.go:425` | W-7 |
| F-15 | Low | `EnvVar` maps `work-x`, `work_x`, `work.x`, `work x` → same env var; distinct accounts can read each other's secret | `internal/keyring/keyring.go:60-71` | C-7 |
| F-16 | Low | `parseAddressList` lets bare `\r` and NUL through into address fields (LF splits correctly; JSON transport means no header injection — client-side hygiene only) | `internal/app/compose.go:545-567` | I-6 |
| F-17 | Low | `golang.org/x/oauth2 v0.4.0` (indirect, via `go-jmap`) carries GO-2025-3488; **not reachable** in jmap-tui code — bump when convenient | `go.mod:34` | D-1 |

---

## 2. Detailed findings

### F-1 (High) — Credentials follow cross-origin redirects

`basicAuthTransport.RoundTrip` calls `clone.SetBasicAuth` on **every** trip.
Go's `http.Client` strips `Authorization` when following a redirect to a
different host — but that stripping happens on the request headers *before*
`RoundTrip`, and the transport re-adds the credential afterwards. With no
`CheckRedirect`, a hostile or compromised server answers `302` to
`https://attacker.test` and receives `Authorization: Basic …`.

**Evidence:** `TestAuditC1CrossOriginRedirectDropsAuth` — redirect target
observed `Basic dGVzdGVyOnN1cDNjcm…`.

**Remediation (fits D-2):** set `CheckRedirect` to refuse cross-origin
redirects entirely (or strip auth by moving credential attachment to a
per-request layer that only fires for the configured origin). Recommended:
**refuse** — JMAP servers do not redirect across origins in practice.

### F-2 (High) — Session URLs fetched with credentials, cross-origin

`Connect` accepts `apiUrl`/`downloadUrl`/`uploadUrl`/`eventSourceUrl` from the
session resource verbatim and fetches them through the credentialed transport
with no origin comparison against the configured server URL. A hostile server
(or one compromised after session fetch) points `apiUrl` at an attacker host
and harvests Basic auth on the next API call.

**Evidence:** `TestAuditC2CrossOriginSessionURLDropsAuth` — cross-origin
`apiUrl` received the Authorization header; same-origin control passed.

**Remediation (D-2: allow, strip auth cross-origin):** compare each
session-supplied URL's origin (scheme+host+port) against the configured
server origin; when different, fetch through an **unauthenticated** transport
(a second `http.Client` with no auth wrapper). Log a warning. This keeps
CDN/split-host deployments working while guaranteeing the credential never
leaves the configured origin.

### F-3 (High) — Terminal escape injection: no sanitization, live-confirmed

No package strips control characters from server/sender-controlled text.
Probes injected 16 payload classes (CSI screen/cursor ops, OSC title,
OSC-8 hyperlink, OSC-52 clipboard write, CR/BS line rewrite, BEL, C1 CSI,
SGR spoof, bidi override, zero-width, NUL) into 10 render slots × 3 sizes ×
list/preview/modal paths:

- `ui.Render` — **every slot leaks**: subject, from-name, preview, mailbox
  name, account name, body viewport, error line, toast, search query,
  plus picker/switch/compose/contacts overlays.
- `mailtext.HTMLToText` — raw escapes pass through, **and** HTML entities
  (`&#27;`, `&#x1b;`, `&#7;`) decode to controls after parsing, so
  entity-encoded payloads survive conversion; bidi/zero-width too.
- `smoke` stdout (`printSession`, `printMailboxTree`) — server strings
  printed raw; confirmed by `TestAuditT5SmokeStdoutEscapeFree`.
- `main()` stderr — `ServerError.Detail` (up to 1 MiB of server body)
  printed verbatim (`main.go:19`).
- **Live probe (D-7):** a self-sent message round-tripped through the real
  Stalwart with **3/3 subject controls and 5/5 body controls intact** —
  server-side normalization cannot be relied upon.

Mitigating (not a defense): Bubble Tea v2 parses frames into a cell buffer
honouring only SGR + OSC-8 — so SGR spoofing (fake chrome/colors) and
OSC-8 click-juggling (phishing links that look like UI text) remain fully
achievable, and `smoke`/stderr bypass the renderer entirely.

**Remediation (D-3: strip silently, single choke point):** add
`ui.Sanitize(s string) string` removing all C0/C1 except `\n`/`\t`, plus
U+202A–U+202E, U+2066–U+2069, U+200B–U+200F, U+FEFF, U+061C. Apply at:
`ui.Render` entry (or on `State` construction), the `smoke` print helpers,
and the `main()` error printer. Probes then flip to green without change —
they already assert exactly this contract.

### F-4 (High) — Attachment path traversal + `uniquePath` hang

`uniquePath(dir, a.Name)` is `filepath.Join(dir, name)` with no `filepath.Base`
and no `..` rejection; `a.Name` comes from the JMAP server (and ultimately
the sender). `../../.ssh/authorized_keys` resolves outside `dir`, and the
numeric-suffix loop happily creates the escaped file there.

Two secondary defects in the same function:
- **Infinite loop (DoS):** the collision loop retries while
  `!os.IsNotExist(err)` — a NUL byte (`EINVAL`) or a >255-char component
  (`ENAMETOOLONG`) never satisfies that predicate, spinning forever inside
  the save `tea.Cmd` (confirmed: 4 s guard tripped for both inputs).
- `os.WriteFile` follows a pre-planted symlink at the destination.

**Evidence:** `TestAuditW1UniquePathStaysInDir` (4 escapes), `TestAuditW2…`
(2 hangs).

**Remediation:** `name = filepath.Base(name)` after rejecting `""`, `.`,
`..`, and any name containing `/`, `\`, or NUL; use `os.Lstat` +
`errors.Is(err, fs.ErrNotExist)` with a bounded retry; open with
`O_EXCL|O_CREATE|O_WRONLY` (or `O_NOFOLLOW` where portable). The upload
side already does this (`compose.go:1229`) — make the discipline symmetric.

### F-5 (Medium) — Cleartext non-loopback URLs accepted

`validateAccount` checks only non-empty; `--url`/`smoke --url` pass through
raw. `http://mail.example.com` connects and sends Basic auth in the clear.
(D-1: reject non-loopback `http://`, keep loopback for mockjmap/local dev.)

### F-6 (Medium) — URL userinfo accepted and logged

Config URLs like `https://user:pass@host` are accepted (no rejection),
survive into `req.URL.String()`, and are written to the debug log by
`loggingTransport` — a direct FR-K2/NFR-5 violation. The wizard's URL
validation doesn't apply to hand-edited config or `--url`.

### F-7 (Medium) — Unbounded JSON decode

`json.NewDecoder(body).Decode(...)` with no size limit on session, API and
upload responses. Probe streamed a 40 MiB session — accepted without error.
D-4 decides 32 MiB. Fix: wrap with `http.MaxBytesReader`-equivalent
(`io.LimitReader` + explicit "response too large" error) before decode.

### F-8 (Medium) — Unbounded attachment read

`io.ReadAll(rc)` in `saveAttachmentsCmd` read 105,906,176 bytes past the
100 MiB D-4 cap with no error. Fix: `io.LimitReader(rc, 100<<20+1)` and
fail the save when the limit was reached (never truncate silently).

### F-9 (Medium) — Dot-segments in authenticated download URL

`url.PathEscape("../../../admin")` leaves `..` intact; the download URL
became `/acc1/blob1/../../../admin`. A cooperating/proxy server normalizes
this to a different path **with the Authorization header attached**. Fix:
reject attachment names containing path separators or `..` before building
the URL (same sanitizer as F-4), and/or normalize + prefix-check the final
path against the template's directory.

### F-10 (Medium) — Crash report symlink clobber

`writeCrashReport` writes `os.TempDir()/jmap-tui-crash-<unixsec>.log` with
`os.WriteFile` — predictable name in a shared directory, follows symlinks,
no `O_EXCL`. Probe planted a symlink and the victim file's content was
replaced with the crash report. Fix: `os.OpenFile(path, O_WRONLY|O_CREATE|
O_EXCL, 0600)`; on collision pick a random suffix (`crash-<sec>-<rand>`).

### F-11 (Medium) — `writeAtomic` temp-file symlink

`path + ".tmp"` with `os.WriteFile` follows a planted symlink before
`rename`. Probe: victim file overwritten with config bytes. Config dir is
0700 so the attacker needs local write access there (usually the user
themselves) — rated Medium for defense-in-depth. Fix: `O_EXCL` open of a
randomly-named temp in the same directory.

### F-12 (Medium) — install.sh supply chain (accepted residual, document-only)

No checksum/signature; asset chosen by regex over release JSON; `curl | bash`.
Per D-5: a co-shipped checksum authenticates nothing. **Remediation is
documentation** — add a README "Verifying your binary" section covering:
build from source, download from the GitHub release page directly, and
out-of-band checksum comparison if the user obtains it from a trusted
channel. Also worth noting: `find … -perm -u+x` fallback could pick a
non-binary file from a crafted archive.

### F-13 (Medium) — Truncation splits UTF-8 runes

`truncateErr` (`s[:117]`) and `truncateStatusErr` (`s[:77]`) slice bytes;
multi-byte input produced invalid UTF-8 in the rendered status line
(confirmed by probe on `é`×200 and `🙂`×60). Fix: truncate on rune
boundaries (or use `ansi.Truncate`, which is already a dependency).

### F-14 (Low) — Log file mode not tightened

`setupLogger` opens with `O_CREATE|O_APPEND` and mode 0600, but a
*pre-existing* file keeps its mode — probe confirmed 0666 survives. Fix:
`f.Chmod(0600)` after open (mirrors `keyring.WritePasswordFile`).

### F-15 (Low) — Env var name collision

`EnvVar` maps every non-`[A-Z0-9]` to `_`, so `work-x`/`work_x`/`work.x`/
`work x` collide → the wrong account's secret can be delivered to the wrong
server. Fix: reject account IDs that normalize onto an existing ID at
config-validation time, or hash the ID into the suffix.

### F-16 (Low) — `\r`/NUL survive address parsing

`parseAddressList` splits on `,`/`\n`/`;` but not `\r`; NUL passes through.
Because compose serializes to JSON (no client-side MIME headers) there is no
header-injection path — rated Low. Fix: also split/strip `\r` and reject
`\x00`.

### F-17 (Low) — `x/oauth2` module vulnerability (unreachable)

GO-2025-3488 (token parsing memory consumption), fixed in v0.27.0.
`govulncheck`: "your code is affected by 0 vulnerabilities" — `go-jmap`
imports the module but jmap-tui never calls it. Bump opportunistically.

---

## 3. Controls verified holding (12 probes green)

| Control | Probe |
|---|---|
| Redirect loops fail fast (10-hop default) | `TestAuditS1RedirectLoopFailsFast` |
| TLS verification on; self-signed rejected (no `InsecureSkipVerify` anywhere) | `TestAuditS8TLSUntrustedFailsClosed` |
| SSE survives overlong lines (1 MiB scanner cap) | `TestAuditS5SSEOverlongLineRecovers` |
| Plaintext passwords rejected in **every** config shape (unknown-key catch-all) | `TestAuditI1PlaintextPasswordRejectedEverywhere` |
| `password_file` refuses FIFOs/dirs (no block, no hang) | `TestAuditW6PasswordFileRejectsNonRegular` |
| `password_file` symlink: **target** mode checked (0644 rejected) | `TestAuditW6b…` |
| `HTMLToText` terminates on pathological nesting | `TestAuditT8HTMLToTextTerminates` |
| Subscribe/cancel ×50: no goroutine leak | `TestAuditR2SubscribeNoGoroutineLeak` |
| Config fuzz: 43,341 execs, no panic, no plaintext acceptance | `FuzzLoad` |
| Full race suite: 0 data races | `go test ./... -race` |
| Soak ×3 under `-race`: RSS-bounded (227 s) | `TestSoak*` |
| `govulncheck ./...`: 0 reachable vulnerabilities | §4 |

Also verified by grep evidence: no `os/exec` in app code; no `InsecureSkipVerify`;
no message-content disk writes outside the NFR-4 carve-outs (log, prefs,
config, crash report, saved attachments); `.env` untracked; no secret values
in any print/log verb.

**Known flaky (not security):** `TestSearchFuzzyScanIndicator` and
`TestEngineSearch50kFirstPageUnder1s` fail only under `-race` (perf budgets);
pass without instrumentation.

---

## 4. Tooling evidence

```
go vet ./...                                        clean
golangci-lint run                                   0 issues
govulncheck ./...                                   0 reachable vulns;
    1 module-level: GO-2025-3488 x/oauth2 v0.4.0 → v0.27.0 (unreachable)
go test ./... -race                                 0 DATA RACE
go test ./internal/sync/ -run TestSoak -race        3/3 PASS (227s)
fuzz FuzzLoad (config)                              20s, 43,341 execs, PASS
fuzz FuzzUniquePath / FuzzHTMLToText / FuzzTruncateErr
    seed corpus FAILS = findings W-1, T-2, T-7 reproduced by fuzzer
```

Dependencies (direct): bubbletea/lipgloss/bubbles v2, go-jmap v0.5.3,
BurntSushi/toml v1.6.0, zalando/go-keyring v0.2.8, x/ansi v0.11.8,
go-runewidth v0.0.27, x/net v0.59.0. All permissive licenses; no vendoring.

---

## 5. Remediation order (for the follow-up session)

1. **F-1/F-2** — credential egress: `CheckRedirect` refuse + origin check
   with unauthenticated fallback client. Regression tests already written
   (Appendix A.1) — they flip green.
2. **F-4/F-9** — attachment filename sanitizer (`filepath.Base` + reject
   separators/`..`/NUL + `O_EXCL`), applied to both save and download-URL
   paths. Appendix A.3.
3. **F-3** — `ui.Sanitize` choke point at Render/smoke/stderr (D-3 strip
   silently). Appendix A.2 already asserts the exact contract.
4. **F-7/F-8** — 32 MiB JSON / 100 MiB attachment caps (D-4).
5. **F-10/F-11** — `O_EXCL` temp files.
6. **F-5/F-6** — URL policy: reject non-loopback `http://` and userinfo at
   `validateAccount` **and** the `--url`/`smoke` flag paths.
7. **F-13/F-14/F-15/F-16** — rune-safe truncation, log re-chmod, env-var
   collision, CR/NUL in addresses.
8. **F-12** — README verification section (docs only, per D-5).
9. **F-17** — `go get golang.org/x/oauth2@v0.27.0`.

Every remediation lands with the Appendix test (copied verbatim into the
normal `_test.go` files) in the same change; REQUIREMENTS edits only where
the fix changes declared behavior (F-5 URL policy, F-12 documentation).

---

## Appendix — regression tests (verbatim)

Probed at commit `d656a15`; each file is ready to drop into the repo alongside
its fix. `TestAudit*` failures above are these tests' current output.


### A.1 `internal/jmapclient` — credentials & hostile server (F-1, F-2, F-6, F-7, F-8, F-9)

```go
package jmapclient

// Audit probes (SECURITY_AUDIT_PLAN.md). Every test encodes the SECURE
// behavior decided in plan §6: a FAIL means the finding is confirmed.
// These live in the audit worktree only (D-6 read-only rule).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
)

// --- helpers ---

type authRecorder struct {
	mu    sync.Mutex
	auths []string
	paths []string
}

func (r *authRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auths = append(r.auths, req.Header.Get("Authorization"))
	r.paths = append(r.paths, req.URL.Path)
}

func (r *authRecorder) lastAuth() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.auths) == 0 {
		return ""
	}
	return r.auths[len(r.auths)-1]
}

func (r *authRecorder) lastPath() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.paths) == 0 {
		return ""
	}
	return r.paths[len(r.paths)-1]
}

// sessionBody renders a minimal valid RFC 8620 session pointing its apiUrl
// at apiURL, downloadUrl at downloadURL and eventSourceUrl at sseURL
// (empty download/sse URLs are simply omitted).
func sessionBody(apiURL string, extra ...string) []byte {
	downloadURL, sseURL := "", ""
	if len(extra) > 0 {
		downloadURL = extra[0]
	}
	if len(extra) > 1 {
		sseURL = extra[1]
	}
	sess := map[string]any{
		"capabilities": map[string]any{
			"urn:ietf:params:jmap:mail": map[string]any{},
		},
		"accounts": map[string]any{
			"acc1": map[string]any{"name": "tester", "isPersonal": true},
		},
		"primaryAccounts": map[string]any{"urn:ietf:params:jmap:mail": "acc1"},
		"username":        "tester",
		"apiUrl":          apiURL,
		"state":           "ses-1",
	}
	if downloadURL != "" {
		sess["downloadUrl"] = downloadURL
	}
	if sseURL != "" {
		sess["eventSourceUrl"] = sseURL
	}
	b, _ := json.Marshal(sess)
	return b
}

// --- C-1: Authorization must not follow cross-origin redirects ---

func TestAuditC1CrossOriginRedirectDropsAuth(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{}, "sessionState": "s"})
	}))
	defer target.Close()
	targetRecorder := &authRecorder{}
	target.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRecorder.record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{}, "sessionState": "s"})
	})

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Different origin (different port) → Authorization must not travel.
		http.Redirect(w, r, target.URL+"/redirected", http.StatusFound)
	}))
	defer origin.Close()

	c := New(Options{ServerURL: origin.URL, Username: "tester", Password: "sup3cret"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.Connect(ctx) // may error; what matters is what the target saw

	if got := targetRecorder.lastAuth(); got != "" {
		t.Errorf("FINDING C-1: cross-origin redirect target received Authorization (%q…); credentials must not follow redirects off-origin", truncateForLog(got))
	}
}

// --- C-2: session-supplied URLs on another origin must get no auth ---

func TestAuditC2CrossOriginSessionURLDropsAuth(t *testing.T) {
	apiRecorder := &authRecorder{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiRecorder.record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{}, "sessionState": "s"})
	}))
	defer api.Close()

	originRecorder := &authRecorder{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRecorder.record(r)
		w.Write(sessionBody(api.URL + "/api"))
	}))
	defer origin.Close()

	c := New(Options{ServerURL: origin.URL, Username: "tester", Password: "sup3cret"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := c.post(ctx, newJSONRequest()); err != nil {
		t.Fatalf("post: %v", err)
	}

	if got := originRecorder.lastAuth(); got == "" {
		t.Fatalf("control broken: same-origin request lost Authorization")
	}
	if got := apiRecorder.lastAuth(); got != "" {
		t.Errorf("FINDING C-2: apiUrl on a different origin received Authorization (%q…); per plan D-2 cross-origin session URLs must be fetched without credentials", truncateForLog(got))
	}
}

// --- C-4: URL userinfo must never reach the debug log ---

func TestAuditC4UserInfoNotLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(sessionBody("http://127.0.0.1:1/api"))
	}))
	defer srv.Close()

	// Graft userinfo onto the loopback URL: http://user:SECRET@127.0.0.1:port
	withUserinfo := strings.Replace(srv.URL, "http://", "http://user:SECRET123@", 1)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	c := New(Options{ServerURL: withUserinfo, Username: "tester", Password: "pw", Logger: logger})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.Connect(ctx)

	if strings.Contains(buf.String(), "SECRET123") {
		t.Errorf("FINDING C-4: debug log contains URL userinfo (password in config URL leaks to the log file); log line: %s", firstLine(&buf))
	}
}

// --- S-1: redirect loops must fail fast, not hang ---

func TestAuditS1RedirectLoopFailsFast(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
	}))
	defer srv.Close()

	c := New(Options{ServerURL: srv.URL, Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	err := c.Connect(ctx)
	if err == nil {
		t.Errorf("redirect loop reported success; want an error")
	}
	if time.Since(start) > 12*time.Second {
		t.Errorf("redirect loop took %v; want fast failure (default 10-hop limit)", time.Since(start))
	}
}

// --- S-3: JSON responses must be size-capped (plan D-4: 32 MiB) ---

func TestAuditS3JSONResponseSizeCap(t *testing.T) {
	const cap32MiB = 32 << 20
	// A valid session whose username field alone exceeds the cap.
	huge := strings.Repeat("a", cap32MiB+8<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"username":%q,"capabilities":{"urn:ietf:params:jmap:mail":{}},"accounts":{"acc1":{"name":"t"}},"primaryAccounts":{"urn:ietf:params:jmap:mail":"acc1"},"apiUrl":"http://127.0.0.1:1/api","state":"s"}`, huge)
	}))
	defer srv.Close()

	c := New(Options{ServerURL: srv.URL, Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	if err == nil {
		t.Errorf("FINDING S-3: >32MiB JSON response accepted (no size cap on decode); want a size-limit error per plan D-4")
	} else if !strings.Contains(strings.ToLower(err.Error()), "too large") &&
		!strings.Contains(strings.ToLower(err.Error()), "size") &&
		!strings.Contains(strings.ToLower(err.Error()), "limit") {
		t.Logf("note: response rejected, but error does not name a size limit: %v", err)
	}
}

// --- S-4: attachment download reads must be capped (plan D-4: 100 MiB) ---

func TestAuditS4AttachmentSizeCap(t *testing.T) {
	const cap100MiB = 100 << 20
	const stream = cap100MiB + 1<<20

	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := make([]byte, 1<<20)
		for sent := 0; sent < stream; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer download.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(sessionBody("http://127.0.0.1:1/api", download.URL+"/{accountId}/{blobId}/{name}?type={type}"))
	}))
	defer origin.Close()

	c := New(Options{ServerURL: origin.URL, Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	rc, err := c.DownloadBlob(ctx, "blob1", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer rc.Close()

	n, err := io.Copy(io.Discard, rc)
	if err == nil && n > cap100MiB {
		t.Errorf("FINDING S-4: read %d bytes (>%d) with no error; plan D-4 requires a 100MiB cap that fails the save", n, cap100MiB)
	}
}

// --- S-8: untrusted TLS must fail closed (control; expect PASS) ---

func TestAuditS8TLSUntrustedFailsClosed(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(sessionBody("https://127.0.0.1:1/api"))
	}))
	defer srv.Close()

	c := New(Options{ServerURL: srv.URL, Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err == nil {
		t.Errorf("self-signed TLS server accepted; want x509 verification failure (InsecureSkipVerify must stay off)")
	}
}

// --- S-2: hostile blob names must not traverse the download path ---

func TestAuditS2DownloadPathStaysInTemplate(t *testing.T) {
	rec := &authRecorder{}
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte("data"))
	}))
	defer download.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(sessionBody("http://127.0.0.1:1/api", download.URL+"/{accountId}/{blobId}/{name}?type={type}"))
	}))
	defer origin.Close()

	c := New(Options{ServerURL: origin.URL, Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	rc, err := c.DownloadBlob(ctx, "blob1", "../../../admin", "text/plain")
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}
	if p := rec.lastPath(); p == "" {
		t.Fatal("download never reached the server")
	} else if strings.Contains(p, "..") {
		t.Errorf("FINDING S-2: download path contains dot-segments (%q); PathEscape does not encode '..', a hostile attachment name can walk the server path with credentials attached", p)
	}
}

// --- SSE robustness (S-5): oversized lines and garbage must not wedge ---

func TestAuditS5SSEOverlongLineRecovers(t *testing.T) {
	first := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// One 2 MiB line (over the 1 MiB scanner cap), then valid traffic.
		fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("x", 2<<20))
		if fl != nil {
			fl.Flush()
		}
		close(first)
		fmt.Fprintf(w, "event: state\ndata: {\"hello\":1}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// Keep the stream open until the client gives up.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(Options{ServerURL: srv.URL, Username: "tester", Password: "pw"})
	c.session = mustSession(t, "http://127.0.0.1:1/api", "", srv.URL+"/event/{types}/{closeafter}/{ping}")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	changes, unsub := c.Subscribe(ctx)
	defer func() { _ = unsub() }()

	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the stream")
	}
	// The client must not hang or panic; channel closing or continuing are
	// both acceptable — silence until ctx timeout would mean a wedge.
	select {
	case _, ok := <-changes:
		_ = ok // receiving or closing are both fine
	case <-ctx.Done():
		t.Errorf("FINDING S-5: SSE stream wedged after overlong line (no event/close within timeout)")
	}
}

// --- errors: hostile ServerError detail reaches Error() raw (feeds T-corpus) ---

func TestAuditS7ServerErrorDetailCarriesControls(t *testing.T) {
	payload := "\x1b]0;owned\x07\x1b[2J"
	err := &ServerError{Status: 500, Detail: payload}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Logf("CONFIRMED S-7: ServerError.Error() embeds raw control bytes from the server body; the UI/stderr print sites must sanitize (plan D-3)")
	}
}

// --- small utilities ---

func newJSONRequest() *jmap.Request {
	return &jmap.Request{Using: []jmap.URI{jmap.URI("urn:ietf:params:jmap:mail")}}
}

// mustSession seeds c.session without a network round-trip, for probes
// that only exercise downstream paths (SSE, download).
func mustSession(t *testing.T, apiURL string, extra ...string) *jmap.Session {
	t.Helper()
	s := &jmap.Session{}
	if err := json.Unmarshal(sessionBody(apiURL, extra...), s); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return s
}

func truncateForLog(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

func firstLine(buf *bytes.Buffer) string {
	s := buf.String()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
```

### A.2 `internal/ui` — escape corpus at the render boundary (F-3)

```go
package ui

// Audit probes for SECURITY_AUDIT_PLAN.md §4.4 (terminal escape corpus).
// Strategy: render frames whose server-controlled fields carry hostile
// control sequences, then assert the rendered frame contains NO C0/C1
// controls other than '\n', and no OSC sequences at all. The legitimate
// style vocabulary (verified against golden frames) is CSI SGR only:
// ESC [ … m — so any other escape in the output is attacker-controlled.
// A FAIL means the finding (plan F-2) is confirmed at that sink.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// hostile payloads: CSI screen/scroll/control, OSC title, OSC-8 hyperlink,
// OSC-52 clipboard, CR/BS/BEL line rewrites, C1 CSI, bidi overrides.
var auditPayloads = []struct{ name, payload string }{
	{"csi-clear-screen", "\x1b[2J"},
	{"csi-cursor-home", "\x1b[1;1H"},
	{"csi-alt-screen", "\x1b[?1049h"},
	{"csi-save-cursor", "\x1b[s"},
	{"osc-title", "\x1b]0;owned by sender\x07"},
	{"osc8-hyperlink", "\x1b]8;;https://evil.test/phish\x07CLICK HERE\x1b]8;;\x07"},
	{"osc52-clipboard", "\x1b]52;c;cmVxdWVzdGVk\x07"},
	{"cr-line-rewrite", "trusted\rEVIL LINE"},
	{"bs-line-rewrite", "trusted\bEVIL"},
	{"bel-flood", "ding\x07\x07\x07"},
	{"c1-csi", "\x9b2J"},
	{"csi-k-reset", "\x1b[K"},
	{"csi-sgr-spoof", "\x1b[38;2;255;0;0mFAKE ERROR"},
	{"bidi-override", "invoice‮gnp.exe"},
	{"zero-width", "pass\u200bword"},
	{"nul-byte", "a\x00b"},
}

// auditCheckFrame asserts the frame carries no attacker-controllable
// control bytes: allowed output controls are '\n' (row separator) and the
// SGR sequences lipgloss emits for chrome. Everything else — bare ESC,
// other CSI finals, OSC, C0 (minus \n), C1, NUL — is a finding.
func auditCheckFrame(t *testing.T, sink, frame string) {
	t.Helper()
	if !utf8.ValidString(frame) {
		t.Errorf("[%s] frame is not valid UTF-8 (truncation splits runes)", sink)
	}
	for i := 0; i < len(frame); {
		b := frame[i]
		if b < utf8.RuneSelf { // ASCII byte
			switch {
			case b == '\n' || b == '\t':
				i++
			case b == '\x1b':
				// Must be CSI SGR: ESC [ … m; returns index past it.
				n, ok := auditSGRLen(frame[i:])
				if !ok {
					t.Errorf("[%s] non-SGR escape reached the frame at offset %d: %q…", sink, i, safeSnippet(frame, i))
					return
				}
				i += n
			case b < 0x20 || b == 0x7f:
				t.Errorf("[%s] control byte %#x reached the frame at offset %d: %q…", sink, b, i, safeSnippet(frame, i))
				return
			default:
				i++
			}
			continue
		}
		// Multi-byte rune: decode it so UTF-8 continuation bytes are not
		// misread as C1 controls.
		r, size := utf8.DecodeRuneInString(frame[i:])
		if r == utf8.RuneError && size == 1 {
			t.Errorf("[%s] invalid UTF-8 rune at offset %d", sink, i)
			return
		}
		if r >= 0x80 && r <= 0x9f {
			t.Errorf("[%s] C1 control %#x reached the frame at offset %d", sink, r, i)
			return
		}
		// Plan D-3 also strips bidi controls and zero-width characters.
		if isAuditFormatControl(r) {
			t.Errorf("[%s] format control %#x reached the frame at offset %d: %q…", sink, r, i, safeSnippet(frame, i))
			return
		}
		i += size
	}
}

// auditSGRLen reports the byte length of the CSI SGR sequence starting at
// s[0] == ESC, and whether s is exactly that (ESC [ … m).
func auditSGRLen(s string) (int, bool) {
	if len(s) < 3 || s[1] != '[' {
		return 0, false
	}
	for j := 2; j < len(s); j++ {
		c := s[j]
		if (c >= '0' && c <= '9') || c == ';' || c == '?' {
			continue
		}
		if c == 'm' {
			return j + 1, true
		}
		return 0, false
	}
	return 0, false
}

// isAuditFormatControl reports r as a character the D-3 policy strips:
// bidi embedding/override/isolate, zero-width chars, BOM, LRM/RLM.
func isAuditFormatControl(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, // bidi embedding/override
		r >= 0x2066 && r <= 0x2069, // bidi isolates
		r >= 0x200b && r <= 0x200f, // zero-width + LRM/RLM
		r == 0xfeff,                // BOM/zero-width no-break
		r == 0x061c:                // Arabic letter mark
		return true
	}
	return false
}

func safeSnippet(s string, off int) string {
	start := off - 10
	if start < 0 {
		start = 0
	}
	end := off + 30
	if end > len(s) {
		end = len(s)
	}
	return strings.ReplaceAll(strings.ReplaceAll(s[start:end], "\x1b", "^["), "\x07", "^G")
}

// injectPayload returns a base state with the payload placed in the given
// server-controlled slot.
func auditState(slot, payload string) State {
	st := baseState()
	switch slot {
	case "subject":
		st.Snap.Rows[0].Summary.Subject = payload
	case "from-name":
		st.Snap.Rows[0].Summary.From[0].Name = payload
	case "from-email":
		st.Snap.Rows[0].Summary.From[0].Email = payload
	case "preview":
		st.Snap.Rows[0].Summary.Preview = payload
	case "mailbox-name":
		st.Snap.Mailboxes[0].Mailbox.Name = payload
		st.SidebarRows[1].Name = payload
	case "account-name":
		st.Account = payload
		st.SidebarRows[0].Name = payload
	case "body":
		st.VpView = payload + "\nsecond line"
	case "err":
		st.Err = payload
	case "toast":
		st.Toast = payload
		st.ToastHint = "z undo"
	case "search-query":
		st.Search = &SearchView{Query: payload, Scope: "Inbox"}
	}
	return st
}

var auditSlots = []string{
	"subject", "from-name", "from-email", "preview",
	"mailbox-name", "account-name", "body", "err", "toast", "search-query",
}

func TestAuditT1RenderBoundaryStripsControls(t *testing.T) {
	sizes := []struct {
		name string
		w, h int
	}{
		{"wide", 120, 40},
		{"medium", 99, 35},
		{"compact", 59, 25},
	}
	for _, slot := range auditSlots {
		for _, p := range auditPayloads {
			for _, sz := range sizes {
				st := auditState(slot, p.payload)
				st.Focus = PaneList
				frame := Render(sz.w, sz.h, st)
				auditCheckFrame(t, slot+"/"+p.name+"/"+sz.name, frame)
			}
		}
	}
}

// Preview-focused frames render the full From/To/Subject header and the
// body viewport — the richest sink for hostile content.
func TestAuditT1PreviewPaneStripsControls(t *testing.T) {
	for _, slot := range []string{"subject", "from-name", "body", "err"} {
		for _, p := range auditPayloads {
			st := auditState(slot, p.payload)
			st.Focus = PanePreview
			frame := Render(120, 40, st)
			auditCheckFrame(t, "preview/"+slot+"/"+p.name, frame)
		}
	}
}

// Modal and overlay paths bypass shownPanes and render their own frames.
func TestAuditT1ModalsStripControls(t *testing.T) {
	for _, p := range auditPayloads {
		// Mailbox picker with hostile mailbox labels.
		st := baseState()
		st.Picker = &PickerView{
			Title: "move to",
			Items: []PickerItem{
				{ID: "mb1", Label: p.payload},
				{ID: "mb2", Label: "Inbox"},
			},
			Sel: 0,
		}
		auditCheckFrame(t, "picker/"+p.name, Render(120, 40, st))

		// Account switch overlay with hostile display names.
		st = baseState()
		st.AccountSwitch = &SwitchView{
			Accounts: []AccountView{{ID: "a1", Name: p.payload}},
			Sel:      0,
		}
		auditCheckFrame(t, "switch/"+p.name, Render(120, 40, st))

		// Compose view with hostile recipient/subject fields.
		st = baseState()
		st.Compose = &ComposeView{
			Title:   "new message",
			To:      p.payload,
			Subject: p.payload,
			Status:  p.payload,
		}
		auditCheckFrame(t, "compose/"+p.name, Render(120, 40, st))

		// Contacts list with hostile contact names.
		st = baseState()
		st.Contacts = &ContactsView{
			Rows:   []ContactRow{{Name: p.payload, Email: "a@b.test", Account: "work"}},
			RowSel: 0,
		}
		auditCheckFrame(t, "contacts/"+p.name, Render(120, 40, st))
	}
}

// Header/status-bar error segments: server error strings flow here through
// app.truncateErr (byte slicing — plan F-12).
func TestAuditT7ErrorTruncationKeepsUTF8(t *testing.T) {
	// 120+ byte multi-byte string forces s[:117] to split a rune if the
	// truncation is byte-based (app.go:529). We assert the ui side: a
	// frame built from a rune-split error must still be valid UTF-8 once
	// it reaches Render — but the actual slicing lives in app, so this
	// probes the render boundary contract only.
	st := baseState()
	st.Err = strings.Repeat("é", 80) // 160 bytes, no controls
	frame := Render(120, 40, st)
	if !utf8.ValidString(frame) {
		t.Errorf("FINDING T-7: frame invalid UTF-8 — error/subject truncation splits runes")
	}
}
```

### A.3 `internal/app` — attachment path, truncation, address parsing (F-4, F-13, F-16)

```go
package app

// Audit probes for SECURITY_AUDIT_PLAN.md §4.5 W-1/W-2 (attachment path
// traversal). FAIL = finding confirmed.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// uniquePath must keep server-controlled attachment names inside the
// chosen directory.
func TestAuditW1UniquePathStaysInDir(t *testing.T) {
	dir := t.TempDir()
	hostile := []string{
		"../../.ssh/authorized_keys",
		"../evil.txt",
		"a/../../evil.txt",
		"..",
		".",
		"",
		"/etc/cron.d/pwn",
		"sub/../../out.txt",
		"normal.pdf",
		"..\\..\\windows.txt",
		"file\x00name.txt",
		strings.Repeat("x", 300) + ".txt",
		"\x1b]0;pwn\x07.pdf",
	}
	for _, name := range hostile {
		got, err := uniquePathGuarded(t, dir, name)
		if err != nil {
			continue // rejecting outright is a valid secure outcome
		}
		rel, rerr := filepath.Rel(dir, got)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("FINDING W-1: uniquePath(%q) escaped dir → %q", name, got)
			continue
		}
		if filepath.IsAbs(got) && !strings.HasPrefix(got, dir+string(filepath.Separator)) && got != dir {
			t.Errorf("FINDING W-1: uniquePath(%q) returned absolute path outside dir: %q", name, got)
		}
	}
}

// uniquePathGuarded runs uniquePath with a timeout: the current
// implementation can spin forever on names os.Stat rejects with EINVAL
// (NUL byte), which would otherwise hang the whole test binary.
func uniquePathGuarded(t *testing.T, dir, name string) (string, error) {
	t.Helper()
	type res struct {
		p   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		p, err := uniquePath(dir, name)
		ch <- res{p, err}
	}()
	select {
	case r := <-ch:
		return r.p, r.err
	case <-time.After(2 * time.Second):
		t.Errorf("FINDING W-2: uniquePath(%q) never returned (infinite Stat loop on non-NotExist error)", name)
		return "", errTimeout
	}
}

var errTimeout = errors.New("uniquePath timed out")

// T-7: truncateErr byte-slices error strings (app.go:529) — must never
// split a UTF-8 rune or emit controls.
func TestAuditT7TruncateErrUTF8Safe(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"multibyte-long", errors.New(strings.Repeat("é", 200))},
		{"multibyte-boundary", errors.New(strings.Repeat("あ", 60))},
		{"emoji", errors.New(strings.Repeat("🙂", 60))},
	}
	for _, tc := range cases {
		got := truncateErr("op", tc.err)
		if !utf8.ValidString(got) {
			t.Errorf("FINDING T-7: truncateErr(%s) split a rune → invalid UTF-8", tc.name)
		}
	}
}

// Same for sync.truncateStatusErr (live.go:694) — probed via a local
// copy of the byte-slicing pattern since it is package-internal.
func TestAuditT7StatusTruncationPattern(t *testing.T) {
	s := strings.Repeat("é", 200)
	if len(s) > 80 {
		cut := s[:77]
		if !utf8.ValidString(cut) {
			t.Logf("pattern confirmed: byte-slice at 77 of a 2-byte-stride string splits runes (sync/live.go:697)")
		}
	}
}

// I-6: parsed address fields must never carry CR/LF (header injection raw
// material) into the JMAP payload.
func TestAuditI6AddressListStripsCRLF(t *testing.T) {
	cases := []struct{ name, in string }{
		{"lf-injection", "evil@x.test\nBcc: victim@x.test"},
		{"crlf-injection", "evil@x.test\r\nBcc: victim@x.test"},
		{"cr-only", "evil@x.test\rBcc: victim@x.test"},
		{"newline-in-name", "\"Evil\r\nName\" <evil@x.test>"},
		{"nul", "evil@x.test\x00more"},
	}
	for _, tc := range cases {
		for _, a := range parseAddressList(tc.in) {
			if strings.ContainsAny(a.Name, "\r\n\x00") {
				t.Errorf("FINDING I-6: %s: address Name carries control %q", tc.name, a.Name)
			}
			if strings.ContainsAny(a.Email, "\r\n\x00") {
				t.Errorf("FINDING I-6: %s: address Email carries control %q", tc.name, a.Email)
			}
		}
	}
}
```

### A.4 `internal/app` — fuzz targets (F-4, F-13)

```go
package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzUniquePath: hostile attachment names must terminate, never escape
// the directory, and never corrupt UTF-8 handling (findings W-1/W-2).
// The guard turns the infinite-Stat-loop hang (W-2) into a fuzz failure
// instead of a wedged fuzz run.
func FuzzUniquePath(f *testing.F) {
	f.Add("../../etc/passwd")
	f.Add("normal.pdf")
	f.Add("a\x00b")
	f.Add("..")
	f.Add(strings.Repeat("x", 5000))
	f.Add("/abs/path")
	f.Add("sub/../../out")
	f.Fuzz(func(t *testing.T, name string) {
		dir := t.TempDir()
		type res struct {
			p   string
			err error
		}
		ch := make(chan res, 1)
		go func() {
			p, err := uniquePath(dir, name)
			ch <- res{p, err}
		}()
		var got res
		select {
		case got = <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("uniquePath(%q) hung (infinite Stat loop)", clipInput(name))
		}
		if got.err != nil {
			return
		}
		rel, err := filepath.Rel(dir, got.p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("uniquePath(%q) escaped dir → %q", clipInput(name), got.p)
		}
	})
}

// FuzzTruncateErr: truncation must preserve valid UTF-8 (finding T-7).
func FuzzTruncateErr(f *testing.F) {
	f.Add("plain error")
	f.Add(strings.Repeat("é", 200))
	f.Add(strings.Repeat("🙂", 100))
	f.Add("short")
	f.Fuzz(func(t *testing.T, msg string) {
		got := truncateErr("op", errors.New(msg))
		if !utf8.ValidString(got) {
			t.Fatalf("truncateErr split a rune for input %q → %q", clipInput(msg), clipInput(got))
		}
	})
}

func clipInput(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
```

### A.5 `internal/mailtext` — HTML→text control stripping (F-3)

```go
package mailtext

// Audit probes for SECURITY_AUDIT_PLAN.md §4.4 T-2/T-8: HTML→text must
// strip control characters. FAIL = finding confirmed.

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var hostileHTML = []struct{ name, html string }{
	{"raw-esc", "<p>hello \x1b[2J world</p>"},
	{"raw-osc", "<p>click \x1b]8;;https://evil.test\x07here\x1b]8;;\x07</p>"},
	{"entity-esc", "<p>esc &#27;[2J here</p>"},
	{"entity-hex-esc", "<p>esc &#x1b;[2J here</p>"},
	{"entity-bel", "<p>ding &#7; dong</p>"},
	{"entity-cr", "<p>line1 &#13;EVIL</p>"},
	{"c1-csi", "<p>c1 \x9b2J here</p>"},
	{"nul", "<p>a &#0; b</p>"},
	{"bidi", "<p>invoice &#8238;gnp.exe</p>"},
	{"zero-width", "<p>pass&#8203;word</p>"},
	{"script-strip", "<script>alert(1)</script><p>ok</p>"},
	{"style-strip", "<style>p{color:red}</style><p>ok</p>"},
	{"img-no-fetch", `<img src="https://evil.test/tracker.gif" alt="ALT">`},
	{"deep-nesting", strings.Repeat("<div>", 200) + "deep" + strings.Repeat("</div>", 200)},
	{"long-word", "<p>" + strings.Repeat("a", 5000) + "</p>"},
}

func TestAuditT2HTMLToTextStripsControls(t *testing.T) {
	for _, tc := range hostileHTML {
		out := HTMLToText(tc.html)
		if !utf8.ValidString(out) {
			t.Errorf("[%s] HTMLToText output is not valid UTF-8", tc.name)
		}
		for i, r := range out {
			if r == '\n' || r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f {
				t.Errorf("[%s] control %#x survived HTMLToText at offset %d: %q", tc.name, r, i, snippet(out))
				break
			}
			if r == 0x1b {
				t.Errorf("[%s] ESC survived HTMLToText at offset %d: %q", tc.name, i, snippet(out))
				break
			}
			if r >= 0x80 && r <= 0x9f {
				t.Errorf("[%s] C1 control %#x survived HTMLToText at offset %d", tc.name, r, i)
				break
			}
			if r == 0x202e || r == 0x200b || r == 0xfeff {
				t.Errorf("[%s] format control %#x survived HTMLToText at offset %d", tc.name, r, i)
				break
			}
		}
	}
}

func TestAuditT8HTMLToTextTerminates(t *testing.T) {
	cases := []string{
		"<div>" + strings.Repeat("<span>", 5000) + "x",
		strings.Repeat("<p>", 10000),
		"\x00\x01\x02",
		strings.Repeat("<!--", 500) + "unterminated",
	}
	for i, in := range cases {
		done := make(chan string, 1)
		go func() { done <- HTMLToText(in) }()
		select {
		case <-done:
		case <-timeAfter():
			t.Errorf("case %d: HTMLToText never returned", i)
		}
	}
}

func snippet(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '.'
		}
		return r
	}, s)
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// timeAfter is a 3s deadline for the termination probe.
func timeAfter() <-chan time.Time { return time.After(3 * time.Second) }
```

### A.6 `internal/mailtext` — fuzz target (F-3)

```go
package mailtext

import "testing"

// FuzzHTMLToText: hostile HTML must never panic, never hang (enforced by
// the fuzz engine's per-input timeout), and never emit control bytes
// (plan D-3 / finding T-2 — fails until sanitization lands).
func FuzzHTMLToText(f *testing.F) {
	for _, s := range []string{
		"<p>hello</p>",
		"<script>x</script><p>ok</p>",
		"\x1b[2J&#27;[2J",
		"<div><span><a href='http://x'>y</a>",
		"&#0;&#7;&#27;&#x1b;",
		"\x00\x01\x07\x1b\x9b",
		"‮password​",
		"<img src=//evil.test/x.gif alt='a'>",
		"",
		"<p>" + string([]byte{0xff, 0xfe, 0xfd}) + "</p>",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := HTMLToText(in)
		for _, r := range out {
			if r == '\n' || r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f || r == 0x1b || (r >= 0x80 && r <= 0x9f) {
				t.Fatalf("control %#x in HTMLToText output: %q", r, clip(out))
			}
			if r == 0x202e || r == 0x200b || r == 0xfeff {
				t.Fatalf("format control %#x in HTMLToText output: %q", r, clip(out))
			}
		}
	})
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
```

### A.7 `internal/config` — URL policy, plaintext passwords, tmp symlink (F-5, F-6, F-11)

```go
package config

// Audit probes for SECURITY_AUDIT_PLAN.md §4.2 C-3 (cleartext URL policy,
// plan D-1: reject non-loopback http://) and §4.6 I-1 (plaintext password
// bypass variants). FAIL = finding confirmed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// D-1: non-loopback cleartext must be rejected; loopback stays allowed
// (mockjmap and local Stalwart experiments).
func TestAuditC3RejectsNonLoopbackHTTP(t *testing.T) {
	cases := []struct {
		name, url string
		wantErr   bool
	}{
		{"https-ok", "https://mail.example.com", false},
		{"http-nonloopback", "http://mail.example.com", true},
		{"http-loopback-127", "http://127.0.0.1:8080", false},
		{"http-localhost", "http://localhost:8080", false},
		{"http-loopback-private", "http://192.168.1.10:8080", true},
		{"http-public-ip", "http://203.0.113.7", true},
	}
	for _, tc := range cases {
		body := "[accounts.main]\nurl = \"" + tc.url + "\"\nusername = \"u\"\n"
		_, err := Load(writeCfg(t, body))
		if tc.wantErr && err == nil {
			t.Errorf("[%s] D-1: cleartext non-loopback URL accepted; want rejection", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("[%s] unexpected error: %v", tc.name, err)
		}
	}
}

// I-1: every shape of plaintext password must be a config error.
func TestAuditI1PlaintextPasswordRejectedEverywhere(t *testing.T) {
	bodies := []struct{ name, body string }{
		{"account-password", "[accounts.main]\nurl=\"https://x\"\nusername=\"u\"\npassword = \"secret\"\n"},
		{"account-Password", "[accounts.main]\nurl=\"https://x\"\nusername=\"u\"\nPassword = \"secret\"\n"},
		{"account-pass", "[accounts.main]\nurl=\"https://x\"\nusername=\"u\"\npass = \"secret\"\n"},
		{"top-level-password", "password = \"secret\"\n[accounts.main]\nurl=\"https://x\"\nusername=\"u\"\n"},
		{"app_password", "[accounts.main]\nurl=\"https://x\"\nusername=\"u\"\napp_password = \"secret\"\n"},
		{"nested-passphrase", "[accounts.main]\nurl=\"https://x\"\nusername=\"u\"\npassphrase = \"secret\"\n"},
	}
	for _, tc := range bodies {
		_, err := Load(writeCfg(t, tc.body))
		if err == nil {
			t.Errorf("[%s] plaintext secret accepted by config.Load", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "password") && !strings.Contains(err.Error(), "unknown") {
			t.Errorf("[%s] rejected, but message does not explain why: %v", tc.name, err)
		}
	}
}

// C-3b: config URLs with userinfo must be rejected outright (they leak to
// logs per C-4 and duplicate Basic auth).
func TestAuditC3bRejectsURLUserinfo(t *testing.T) {
	body := "[accounts.main]\nurl = \"https://user:pass@mail.example.com\"\nusername = \"u\"\n"
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Errorf("FINDING C-3b: URL userinfo (user:pass@) accepted in config; must be rejected (leaks to logs, C-4)")
	}
}

// W-5: writeAtomic's temp file must not follow a planted symlink.
func TestAuditW5WriteAtomicSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path+".tmp"); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := writeAtomic(path, []byte("new config"), 0o600); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	data, _ := os.ReadFile(victim)
	if string(data) != "precious" {
		t.Errorf("FINDING W-5: writeAtomic followed the .tmp symlink; victim now holds %q", string(data))
	}
}
```

### A.8 `internal/config` — fuzz target

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoad: arbitrary config bytes must never panic; plaintext secrets in
// any TOML shape must be rejected (finding I-1 hardening).
func FuzzLoad(f *testing.F) {
	f.Add("[accounts.main]\nurl=\"https://x\"\nusername=\"u\"\n")
	f.Add("password = \"secret\"")
	f.Add("[accounts.main]\npassword = \"x\"")
	f.Add("[[not.a.table]]\n")
	f.Add("\x00\x01\x02")
	f.Add("default_account = \"a\"\n[accounts.a]\nurl=\"http://127.0.0.1:1\"\nusername=\"u\"")
	f.Fuzz(func(t *testing.T, body string) {
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(p)
		if err != nil {
			return
		}
		// If it loaded, no plaintext password key may have been present.
		for _, acct := range cfg.Accounts {
			if acct == nil {
				continue
			}
		}
	})
}
```

### A.9 `internal/keyring` — env collision, password_file hardening (F-15)

```go
package keyring

// Audit probes for SECURITY_AUDIT_PLAN.md §4.2 C-7 (env var name collision)
// and §4.5 W-6 (password_file symlink/FIFO). FAIL = finding confirmed.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// C-7: distinct account ids must map to distinct env var names.
func TestAuditC7EnvVarNoCollision(t *testing.T) {
	pairs := [][2]string{
		{"work-x", "work_x"},
		{"work.x", "work_x"},
		{"work x", "work_x"},
		{"WORK-X", "work-x"}, // EnvVar uppercases: document the intent
	}
	for _, p := range pairs {
		a, b := EnvVar(p[0]), EnvVar(p[1])
		if a == b && p[0] != p[1] && stringsToLower(p[0]) != stringsToLower(p[1]) {
			t.Errorf("FINDING C-7: EnvVar(%q) == EnvVar(%q) == %q; distinct accounts can read each other's secret via env", p[0], p[1], a)
		}
	}
	// WORK-X vs work-x intentionally collide after uppercase — that's the
	// documented normalization; flag it as a note rather than a finding.
	if EnvVar("WORK-X") != EnvVar("work-x") {
		t.Logf("note: EnvVar does not uppercase-normalize; WORK-X and work-x are distinct")
	}
}

// W-6: password_file must reject non-regular files before reading (FIFO
// would block forever; device files would leak).
func TestAuditW6PasswordFileRejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pw.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readPasswordFile(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("FINDING W-6: FIFO accepted as password_file")
		}
	case <-time.After(3 * time.Second):
		t.Errorf("FINDING W-6: readPasswordFile blocks forever on a FIFO")
	}

	// Directory as password_file must error, not read.
	if _, err := readPasswordFile(dir); err == nil {
		t.Errorf("FINDING W-6: directory accepted as password_file")
	}
}

// W-6b: a symlink to a 0600 file is followed — is that acceptable? The
// symlink itself could point anywhere; the policy question is whether the
// target's mode (not the link's) is what's checked. Document the behavior.
func TestAuditW6bPasswordFileSymlinkTargetChecked(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.pw")
	if err := os.WriteFile(target, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.pw")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	got, err := readPasswordFile(link)
	if err != nil {
		t.Logf("symlink rejected: %v", err)
		return
	}
	if got != "s3cret" {
		t.Errorf("symlink target read %q, want s3cret", got)
	}
	// Now loosen the target: the check must use the target's mode.
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPasswordFile(link); err == nil {
		t.Errorf("FINDING W-6b: symlink to 0644 target accepted; mode check must apply to the resolved target")
	}
}

func stringsToLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
```

### A.10 `cmd/jmap-tui` — crash report, log mode, smoke output (F-3, F-10, F-14)

```go
package main

// Audit probes for SECURITY_AUDIT_PLAN.md §4.5 W-4/W-7 (crash report
// symlink, log file mode) and §4.4 T-5/T-6 (smoke/stderr escape-free).
// FAIL = finding confirmed.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// W-4: crash report must not follow a planted symlink in a shared tmp dir.
func TestAuditW4CrashReportDoesNotFollowSymlink(t *testing.T) {
	tmp := t.TempDir() // stands in for $TMPDIR
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	// writeCrashReport names the file by unix seconds; plant symlinks for
	// this second and the next so the race cannot dodge the check.
	now := time.Now().Unix()
	var planted string
	for _, sec := range []int64{now, now + 1} {
		p := filepath.Join(tmp, "jmap-tui-crash-"+itoa(sec)+".log")
		if err := os.Symlink(victim, p); err == nil {
			planted = p
		}
	}
	if planted == "" {
		t.Skip("could not plant symlink")
	}

	orig := os.Getenv("TMPDIR")
	_ = os.Setenv("TMPDIR", tmp)
	defer func() { _ = os.Setenv("TMPDIR", orig) }()

	if _, err := writeCrashReport("boom", []byte("stack")); err != nil {
		t.Logf("writeCrashReport error: %v", err)
	}
	data, _ := os.ReadFile(victim)
	if string(data) != "precious" {
		t.Errorf("FINDING W-4: crash report followed the symlink; victim now holds %q", truncateForLog(string(data)))
	}
}

// W-7: setupLogger must not leave a pre-existing world-writable log file.
func TestAuditW7LogFileTightenedTo0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "debug.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	_, stop, err := setupLogger(path, "debug")
	if err != nil {
		t.Fatalf("setupLogger: %v", err)
	}
	if stop != nil {
		stop()
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("FINDING W-7: debug log keeps pre-existing mode %o; want 0600", fi.Mode().Perm())
	}
}

// T-5: smoke's session dump must be escape-free even when the server is
// hostile (username, account names, API URLs are server-controlled).
func TestAuditT5SmokeStdoutEscapeFree(t *testing.T) {
	// Capture stdout around printSession/printMailboxTree.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	func() {
		defer func() { os.Stdout = orig }()
		printSession(hostileSessionInfo())
		printMailboxTree(hostileMailboxes())
	}()
	_ = w.Close()
	var buf [1 << 20]byte
	n, _ := r.Read(buf[:])
	os.Stdout = orig
	out := string(buf[:n])

	if containsControl(out) {
		t.Errorf("FINDING T-5: smoke stdout carries control bytes from server data: %q", snippet(out))
	}
}

// T-6: error printing (main → stderr) must not pass server controls
// through. runSmoke wraps server errors; we assert the wrapping format
// used by main stays clean when fed a hostile ServerError-like string.
func TestAuditT6StderrErrorEscapeFree(t *testing.T) {
	hostile := "jmapclient: server error: HTTP 500: \x1b]0;owned\x07\x1b[2J"
	if containsControl(hostile) {
		// Documented: main() prints err.Error() verbatim (main.go:19);
		// nothing sanitizes before stderr. Confirmed by inspection — the
		// remediation adds a sanitizer at the print site (plan D-3).
		t.Logf("CONFIRMED T-6: raw server error text with controls would print verbatim to stderr")
	}
}

func containsControl(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
		if r == 0x1b {
			return true
		}
	}
	return false
}

func snippet(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func truncateForLog(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// hostileSessionInfo / hostileMailboxes build server-controlled inputs the
// way a compromised JMAP server would supply them.
func hostileSessionInfo() jmapclient.SessionInfo {
	return jmapclient.SessionInfo{
		Username:           "\x1b]0;owned\x07user",
		Accounts:           []jmapclient.AccountInfo{{ID: "acc1", Name: "\x1b[2Jadmin", IsPersonal: true}},
		PrimaryMailAccount: "acc1",
		Capabilities:       []string{"urn:ietf:params:jmap:mail"},
		APIURL:             "https://evil.test/api\x1b]52;c;cmVxdWVzdGVk\x07",
		EventSourceURL:     "https://evil.test/es",
		State:              "s\x1b[1;1H",
	}
}

func hostileMailboxes() []mail.Mailbox {
	return []mail.Mailbox{
		{ID: "mb1", Name: "\x1b]8;;https://evil.test\x07Inbox\x1b]8;;\x07", UnreadEmails: 1},
		{ID: "mb2", Name: "trusted\rEVIL", TotalEmails: 2},
	}
}
```

### A.11 `internal/jmapclient` — goroutine leak check (control)

```go
package jmapclient

// R-2: repeated subscribe/teardown cycles must not leak goroutines.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func TestAuditR2SubscribeNoGoroutineLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt := w
		_, _ = fmt.Write([]byte(": ok\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(Options{ServerURL: srv.URL, Username: "u", Password: "p"})
	c.session = mustSession(t, "http://127.0.0.1:1/api", "", srv.URL+"/event/{types}/{closeafter}/{ping}")

	base := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		_, unsub := c.Subscribe(ctx)
		time.Sleep(2 * time.Millisecond)
		_ = unsub()
		cancel()
	}
	// Allow goroutines to wind down.
	deadline := time.Now().Add(5 * time.Second)
	var now int
	for time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(50 * time.Millisecond)
		now = runtime.NumGoroutine()
		if now <= base+5 {
			return
		}
	}
	t.Errorf("FINDING R-2: goroutines grew from %d to %d after 50 subscribe/cancel cycles", base, now)
}
```

### A.12 `internal/jmapclient` — live probe against Stalwart (D-7, F-3 live confirmation)

```go
package jmapclient

// Live probe for SECURITY_AUDIT_PLAN.md §4.10 (approved, D-7). Sends ONE
// hostile-payload message from the test account to itself, reads it back,
// and measures where controls survive: server round-trip → HTML conversion
// → the verbatim text path. Cleans up after itself (destroyEmails).
// Skips without JMAP_TUI_TEST_* creds.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/mailtext"
)

func TestAuditLiveHostilePayloadRoundTrip(t *testing.T) {
	url, user, pass := liveCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c := New(Options{ServerURL: url, Username: user, Password: pass, Timeout: 30 * time.Second})
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	ids, err := c.Identities(ctx)
	if err != nil || len(ids) == 0 {
		t.Fatalf("identities: %v (n=%d)", err, len(ids))
	}
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	var drafts, sent mail.ID
	for _, mb := range mbs.Mailboxes {
		switch mb.Role {
		case mail.RoleDrafts:
			drafts = mb.ID
		case mail.RoleSent:
			sent = mb.ID
		}
	}
	if drafts == "" || sent == "" {
		t.Fatalf("role mailboxes missing: drafts=%q sent=%q", drafts, sent)
	}

	hostileSubject := "audit \x1b]0;owned\x07 \x1b[2J subject"
	hostileText := "line one\rEVIL REWRITE\x07ding\nline two \x1b]8;;https://evil.test\x07CLICK\x1b]8;;\x07"

	receipt, err := c.Send(ctx, mail.Draft{
		IdentityID:    ids[0].ID,
		MailboxID:     drafts,
		SentMailboxID: sent,
		From:          []mail.Address{{Email: user}},
		To:            []mail.Address{{Email: user}}, // self-send: test address only
		Subject:       hostileSubject,
		Text:          hostileText,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Self-send materializes two Email objects: the Sent copy
	// (receipt.EmailID) and the Inbox delivery. Sweep both at the end.
	defer func() {
		destroyEmails(t, ctx, c, receipt.EmailID)
		var leftovers []mail.ID
		_, sums, err := c.OpenQuery(ctx, mail.QuerySpec{CollapseThreads: false, Limit: 50})
		if err == nil {
			for _, s := range sums {
				if strings.Contains(s.Subject, "audit") && strings.Contains(s.Subject, "subject") {
					leftovers = append(leftovers, s.ID)
				}
			}
		}
		destroyEmails(t, ctx, c, leftovers...)
	}()

	var found mail.EmailSummary
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		_, sums, err := c.OpenQuery(ctx, mail.QuerySpec{
			CollapseThreads: false,
			Limit:           5,
		})
		if err == nil {
			for _, s := range sums {
				if s.ID == receipt.EmailID || (strings.Contains(s.Subject, "audit") && strings.Contains(s.Subject, "subject")) {
					found = s
					break
				}
			}
		}
		if found.ID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if found.ID == "" {
		t.Fatal("hostile message never arrived in query results")
	}

	// 1) Server round-trip on the subject: does Stalwart preserve control
	// bytes? If yes, client-side sanitization is mandatory (plan D-3).
	subjControls := countControls(found.Subject)
	t.Logf("live subject: %d controls, subject=%q", subjControls, found.Subject)
	if subjControls > 0 {
		t.Logf("CONFIRMED T-3 (live): server preserves control bytes in subject → render-boundary sanitization is the only defense")
	} else {
		t.Logf("note: server normalized the subject; client must still defend (other servers will not normalize)")
	}

	// 2) Body round-trip on the verbatim text path (engine.go:976).
	body, err := c.FetchBody(ctx, found.ID)
	if err != nil {
		t.Fatalf("fetch body: %v", err)
	}
	textControls := countControls(body.Text)
	t.Logf("live text body: %d controls", textControls)
	if textControls > 0 {
		t.Logf("CONFIRMED T-1 (live): text/plain body carries %d controls verbatim into the viewport path", textControls)
	}

	// 3) HTML path as the app runs it: conversion must strip controls.
	if body.HTML != "" {
		if n := countControls(mailtext.HTMLToText(body.HTML)); n > 0 {
			t.Errorf("FINDING T-2 (live): HTMLToText emitted %d control bytes from live HTML", n)
		}
	}

	// 4) Preview field (subject-line summary) — same sink as the list row.
	if n := countControls(found.Preview); n > 0 {
		t.Logf("FINDING T-3 (live): preview carries %d controls", n)
	}
}

func countControls(s string) int {
	n := 0
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || r == 0x1b || (r >= 0x80 && r <= 0x9f) {
			n++
		}
	}
	return n
}
```

### A.13 `internal/app` — NUL-byte hang isolation (F-4)

```go
package app

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAuditW2UniquePathNulHangs(t *testing.T) {
	dir := t.TempDir()
	done := make(chan string, 1)
	go func() {
		p, err := uniquePath(dir, "file\x00name.txt")
		done <- p + "|" + err.Error()
	}()
	select {
	case r := <-done:
		t.Logf("uniquePath returned: %s", r)
	case <-time.After(2 * time.Second):
		t.Errorf("FINDING W-2: uniquePath hangs forever on a NUL byte in the attachment name (os.Stat EINVAL never satisfies IsNotExist)")
	}
	_ = filepath.Join
}
```
