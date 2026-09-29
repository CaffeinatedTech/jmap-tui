# Plan — Styled HTML email rendering (markdown → glamour)

**Status:** ✅ complete (2026-09-29) — all phases landed, gates green.
**Scope:** improve HTML body rendering in the preview pane. `internal/mailtext`
already converted HTML through a real DOM walk, but the output had no visual
hierarchy — headings, bold, tables and quotes were indistinguishable from body text.

**Decision (user-approved):** direction **B** — keep the in-repo HTML walker, teach it to
emit *markdown* for display, render ANSI at the UI layer with `charm.land/glamour/v2`
(Glow's engine) using a custom one-accent style derived from `ui.Palette`.

- `text/plain` bodies: **untouched** (raw text, as today).
- Reply/forward quoting: **untouched** (plain text via `HTMLToText`).
- `REQUIREMENTS.md` edit included (golden rule 7): FR-E2 rewritten, §4 non-goal
  reworded, §8.1 open question closed.

```
HTML ──HTMLToMarkdown──▶ sanitized markdown (engine cache, width-independent)
Plain ──────────────────▶ raw text (unchanged path)
                              │
              app.setBody — ONE render, width-independent
                              │
        styled? ──yes──▶ ui.RenderBody (glamour, Palette style) ──▶ bodyRaw (ANSI)
              └──no────▶ raw text ──────────────────────────────▶ bodyRaw
                              │
              app.applyBody — ansi.Wrap(bodyRaw, pane width)   ← plain & styled alike
```

## Why not the alternatives

- **Glow** renders markdown only, not HTML. Its *engine* (`glamour`) is a library → use it.
- **External pipe (w3m/lynx)**: REQUIREMENTS §8.1 leaned no; now closed — no pipe.
- **client9/htmlterm**: full HTML+CSS renderer, but brand-new (0 stars) and CSS colors
  fight the one-accent rule.

## Phase 1 — converter (`internal/mailtext`) ✅

- [x] 1.1 `mailtext.go`: walker gained a `md` mode; new `HTMLToMarkdown(src) string`.
  Emission: `h1-h6`→`#`, `b/strong`→`**`, `i/em`→`*`, `blockquote`→`> `,
  `pre`→fenced code, `ol`→`1. `, `hr`→`---`, `<br>`→ two-space hard break,
  text nodes **markdown-escaped** (`#`/`-`/`+`/`=` and leading `1.` only at a
  segment start; `&` round-trips as `&amp;` because the renderer runs
  `html.UnescapeString` over text nodes — a backslash-escape cannot reach it).
- [x] 1.2 Links keep the proven footnote UX (`[1]` markers + URL footer, one URL per
  line in markdown since paragraphs otherwise join them).
- [x] 1.3 Tables: pipe tables only when tabular (`<th>` or consistent >1 columns, no
  colspan/rowspan, no nested table, no empty spacer cell); layout tables degrade to
  block flow.
- [x] 1.4 `sanitize.go`: `SanitizeStyled(s)` — passes only well-formed `CSI … m`,
  drops every other escape **whole** (CSI non-SGR, OSC hyperlinks/titles, bare ESC).
  `HTMLToMarkdown` output runs through plain `Sanitize` → markdown source is ESC-free
  before glamour sees it. Also dropped inline-hidden elements (`display:none`,
  `visibility:hidden`, `opacity:0`, `height:0`+clip) — email preheaders/tracking pixels.
- [x] 1.5 Tests: `markdown_test.go` table-driven (escaping, tables, breaks, hidden
  elements, plain path unchanged), `sanitize_styled_test.go` adversarial, audit tests
  extended to both converters, `FuzzHTMLToMarkdown` (343k execs clean).

## Phase 2 — sync plumbing (`internal/sync`) ✅

- [x] 2.1 `BodyView` gained `Styled bool`.
- [x] 2.2/2.3 `convertBody` produces both texts: `EmailBody.Text` stays the plain
  quote, `BodyView.Text` is the markdown display; cache entry stores both
  (`bodyEntry{text, styled, body}`). `fetchBody`, `cachedBody`, `setBodyLocked`
  (now takes `*BodyView`), `ReplyContext` all updated.
- [x] 2.4 Sync tests updated: display is markdown + styled, quote is plain, plain
  bodies stay unstyled.

## Phase 3 — UI render (`internal/ui`, `internal/app`) ✅

- [x] 3.1 `internal/ui/mailrender.go`: `RenderBody(md, Palette)`.
  `WithWordWrap(0)` (see perf note below), `WithPreservedNewLines`, `WithTableWrap`,
  style from `Palette`: accent headings/links/code, muted quotes/footnotes/code blocks,
  body text, rule-colored `---`, **no margins**, no borders/backgrounds. Output goes
  through `SanitizeStyled` then `trimTrailingPad`.
- [x] 3.2 `app.setBody(text, styled)` renders once (styled) into `bodyRaw`;
  `app.applyBody` is the single `ansi.Wrap` path for both — resize never renders.
  Unified view (`uBody`) rides the same call.
- [x] 3.3 `ui/sanitize.go`: `st.VpView` → `SanitizeStyled` (frame boundary keeps
  pane SGR, still drops everything else).

## Phase 4 — tests, goldens, docs ✅

- [x] 4.1 Goldens: 6 new `styled-body*` frames (120×40 / 99×35 / 59×25 × dark/light)
  built through the real converter + renderer + wrap; regenerated and reviewed.
  Dark golden carries `#82aaff`, light carries `#2a5db0`, preheader absent.
- [x] 4.2 REQUIREMENTS: FR-E2 rewritten, §4 non-goal reworded ("browser-faithful
  HTML/CSS rendering" stays a non-goal), §8 closed with the FR-E2 resolution.
- [x] 4.3 PLAN: §6.1 open-question item removed, §6.3 design record added, §7 gained
  the client-side degradation note, M1 milestone row notes the follow-up.
- [x] 4.4 Gates: `go build`, `go vet`, `golangci-lint` (0 issues), `gofumpt`, `go test ./...`.

## Performance finding (changed the design mid-implementation)

Glamour's own wrapping pads **every line to the wrap width with one styled space per
column**, and its padding writer re-parses ANSI for each — profiled at **~10 ms/KB,
44% of total runtime**, i.e. most of a 427 ms render of a 60 KB body, produced only to
be trimmed off afterwards. Fix: `WithWordWrap(0)`.

- Output becomes **width-independent** → render once in `setBody`, resize re-wraps with
  `ansi.Wrap` (µs), the same path plain bodies use.
- Whole pipeline dropped to **~1.6 ms/KB** (`BenchmarkStyledBody`: 8 KB → 10.7 ms,
  32 KB → 52 ms), paid once per body.
- Size guard lowered 2 MB → **64 KB** (≈ a 100 ms frame worst case); above it the
  markdown source shows unstyled. Trade-off: tables size to content, so a table wider
  than the pane is word-wrapped like any long line (the old plain-text behaviour).
- Escape hatch if it ever janks: a stale-discarded `tea.Cmd`, same shape as the body
  fetch. Not needed — one-time cost, not on the resize path.

## Verification

- `go build ./...`, `go vet ./...`, `golangci-lint run` (0 issues), `gofumpt -l` (clean)
- `go test ./...` — all packages green
- `FuzzHTMLToMarkdown` 15s / 343k execs, no failures
- Golden diff reviewed: heading accent per palette, `<br>` signature on two lines,
  `&` not `\&`, layout table not gridded, preheader dropped
