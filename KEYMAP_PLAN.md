# KEYMAP_PLAN — keymap redesign v2

Status: **implemented** 2026-09-25 (decisions finalised the same day).
All bindings, the esc chain, unread jumps, sidebar `g/G`, and sort (FR-D8) landed; §9 records the resolutions.
Scope: redesign default keybindings for consistency and muscle memory
targeting vim-literate users and habits from mutt/neomutt, newsboat,
ranger/lf, less, aerc, lazygit, Thunderbird. Remap action ids stay
stable (FR-I3); REQUIREMENTS/README/help overlay update in the same
change.

## 1. Inventory — current keys, by screen (as of fc64ad8)

### 1.1 Normal mode, List pane
| Key | Action id | Notes |
|---|---|---|
| `j` `k` / `↓` `↑` | list.down / list.up | |
| `g` `shift+g` | list.top / list.bottom | `g` pressed twice behaves as vim `gg` (re-jumps) |
| `ctrl+f` `ctrl+b` | list.page_down / list.page_up | page = viewport rows − 2 |
| `enter` `o` | list.toggle_thread | drafts: Enter edits instead |
| `space` `u` | list.toggle_read | **space reassigned (§5)** |
| `*` | list.toggle_star | |
| `x` | list.toggle_select | batch marker gutter |
| `m` `y` `h` `#` | list.move / copy / archive / delete | **h and # reassigned (§5)** |

### 1.2 Normal mode, Sidebar pane
| Key | Action id | Notes |
|---|---|---|
| `j` `k` / `↓` `↑` | sidebar.down / up | |
| `enter` `l` | sidebar.open | open also moves focus to list |
| `h` | sidebar.close | collapse (vim "left" ✓) |

### 1.3 Normal mode, Preview pane
| Key | Action id | Notes |
|---|---|---|
| `j` `k` / `↓` `↑` | preview.down / up | line scroll |
| `d` `u` | preview.half_down / half_up | vim half-page ✓ |
| `g` `shift+g` | preview.top / bottom | |
| `ctrl+f` `ctrl+b` | preview.down / up | **BUG: help says "page down", scrolls 1 line** |
| `s` | preview.save_attachment | |

### 1.4 Global (PaneAny)
| Key | Action id |
|---|---|
| `n` `r` `a` `f` | ui.compose, list.reply / reply_all / forward |
| `/` `ctrl+s` `esc` | ui.search / ui.search_advanced / ui.search_clear |
| `v` | preview.fullscreen |
| `i` `shift+s` `ctrl+a` | unified.toggle / account.switch / account.manage |
| `[` `z` | pane.toggle_sidebar / pane.toggle_layout |
| `tab` `shift+tab` | pane.cycle / cycle_reverse |
| `pgup` `pgdown` | preview.page_up / page_down |
| `ctrl+z` | ui.undo (toast window; cancels held send) |
| `?` `q` `ctrl+c` | ui.help / ui.quit (instant) / quit (2×) |

### 1.5 Modes & modals (own key loops, not the keymap)
- **Search bar:** typing · `enter` confirm→results · `tab` scope · `esc` close · `ctrl+s` advanced.
- **Advanced search modal:** `↑↓`/`tab`/`shift+tab` fields · `space` attach toggle · `enter` run · `esc` close.
- **Compose:** `tab`/`shift+tab` zones · `enter` next field (newline in body) · `ctrl+s` send · `ctrl+a` attach · `ctrl+i` identity · `ctrl+x` remove attach · `↑↓` attach zone · `esc` → discard confirm (`y`/`n`/`esc`/`q`).
- **Mailbox picker:** `j/k/↑↓` · type-to-filter · `backspace` · `enter` · `esc`.
- **File picker:** bubbles' own keys · `enter` save · `esc`.
- **Account switcher:** `j/k/↑↓` · `enter`/`l` · `esc`.
- **Help overlay:** `?`/`q`/`esc` close; all else swallowed.
- **Wizard:** `tab`/`shift+tab`/`enter`/`esc`; `j/k/g/G` in its pickers; `r` retry.

## 2. Design principles

1. **One motion alphabet everywhere.** `j/k`, `g/G`, `ctrl+f/b`,
   `ctrl+d/u`, `h/l` mean the same class of thing in every pane that
   scrolls or navigates (vim, ranger).
2. **Navigation letters vs semantic letters.** Vim-motion letters never
   perform semantic actions in the list (`h` is "left", not archive).
   Semantic actions use mnemonics: `d`elete, archiv`e`, `y`ank, `m`ove,
   `r`eply, `f`orward.
3. **Pane-scoped same-key is fine when the meaning is the same**
   (motion), forbidden when it isn't — enforced by `Validate()`.
4. **Destructive keys cluster and always produce a `ctrl+z` undo toast.**
5. **`esc` = back, `enter` = confirm/open, `tab` = cycle** — modal and
   normal mode agree.
6. **Discoverability:** every binding renders in `?` with a one-line
   description; README table regenerates from the same source.
7. **No key sequences** (no `dd`, no `gg` engine) — `g` twice already
   gives `gg`'s effect. Documented as a non-goal for v1 of this map.

## 3. Reference habits deliberately adopted

| From | Take |
|---|---|
| vim | `ctrl+d/u` half-page, esc-as-back, motion letters are motion-only |
| less/mutt/newsboat | `space` = page down |
| mutt/neomutt | `d` = delete |
| Thunderbird | `e` = archive |
| ranger/lf | `h/l` collapse/enter in the tree, type-to-filter pickers |
| aerc/lazygit | j/k + enter + esc modal grammar (already in place) |

## 4. Confirmed decisions (2026-09-25)

1. **`space` in the list = page down**; toggle-read stays on `u`.
2. **Triage:** `d` = delete (`#` kept as alias), `e` = archive
   (`h` freed — list-`h` removed, sidebar-`h` collapse stays).
3. **New actions:** `list.next_unread` / `list.prev_unread` on `J` / `K`
   (`shift+j`/`shift+k`), plus `list.half_down` / `list.half_up` on
   `ctrl+d` / `ctrl+u`.
4. **`esc` = contextual back chain:** close search → exit fullscreen →
   clear multi-select → no-op. One action, ordered behaviour.
5. **`q` stays single-press quit** — no confirm dialog (decision: the
   `ctrl+c` double-press remains the "careful exit" path).
6. **Sidebar gains `g` / `shift+g`** — tree top/bottom, new actions
   `sidebar.top` / `sidebar.bottom`; every pane now speaks `g/G`.
7. **`s` = sort** (opens a sort picker: Newest first (default), Oldest
   first, By sender, By subject, By size — mutt's `o` kept as a free
   alias). Server-side `Email/query` sort; the window re-queries like a
   mailbox open (cursor to top, one outstanding query invariant holds);
   the choice is **remembered per account in prefs.toml**
   (`AccountPrefs.Sort`).
8. **`S` = sizes** (was `s`); the account switcher moves `shift+s` →
   **`shift+a`** (`A`, mnemonic Account — `Validate()` forbids a pane
   key shadowing a global, so the switcher had to move).
9. **Drop `o` from thread toggle** — `enter` alone expands/collapses
   (and edits drafts); `o` is freed for the sort alias.

## 5. Proposed keymap v2

### 5.1 List pane
| Key | Action | Change |
|---|---|---|
| `j` `k` / arrows | list.down / up | — |
| `g` `shift+g` | list.top / bottom | — (`gg` works by double-press) |
| `ctrl+f` `ctrl+b` | list.page_down / up | — |
| **`ctrl+d` `ctrl+u`** | **list.half_down / up** | **new** |
| **`space`** | **list.page_down** | **was toggle_read** |
| **`J` `K`** | **list.next_unread / prev_unread** | **new** |
| `u` | list.toggle_read | kept as sole read key |
| `enter` | list.toggle_thread | **`o` dropped** (drafts: Enter still edits) |
| `*` | list.toggle_star | — |
| `x` | list.toggle_select | — (`esc` now clears) |
| **`d` `#`** | **list.delete** | **`d` new, `#` alias kept** |
| **`e`** | **list.archive** | **was `h`** |
| `m` `y` | list.move / copy | — |
| **`s` `o`** | **list.sort** | **new — opens the sort picker; `o` is the mutt alias** |
| **`S`** | **list.toggle_size** | **was `s`** |

### 5.2 Sidebar
| Key | Action | Change |
|---|---|---|
| `j` `k` / arrows | sidebar.down / up | — |
| `enter` `l` | sidebar.open | — |
| `h` | sidebar.close | — |
| **`g` `shift+g`** | **sidebar.top / bottom** | **new — every pane speaks `g/G`** |

### 5.3 Preview
| Key | Action | Change |
|---|---|---|
| `j/k`, `d/u`, `g/shift+g` | unchanged | |
| **`ctrl+f` `ctrl+b`** | **preview.page_down / up** | **was preview.down/up — fixes the mislabeled 1-line scroll** |
| `pgup/pgdown` (global) | preview.page_up / down | — |
| `s` | preview.save_attachment | kept (pane-local: list `s` is now sort — different meanings by pane, documented in help) |

### 5.4 Global
Unchanged except `esc`:
| Key | Action | Change |
|---|---|---|
| `n r a f` `/` `ctrl+s` `v` `i` `ctrl+a` `[` `z` `tab` `ctrl+z` `?` `q` | unchanged | |
| **`shift+a`** | **account.switch** | **was `shift+s` — the switcher moved so `S` could mean sizes in the list** |
| **`esc`** | **ui.search_clear → contextual back** | **behavior widens, id unchanged** |
| `pgup`/`pgdown` | preview paging | — |

### 5.5 Modes & modals
No changes. Search bar, advanced modal, compose, pickers, switcher,
help, wizard keep their current loops.

## 6. Change summary

**New action ids** (remappable): `list.half_down`, `list.half_up`,
`list.next_unread`, `list.prev_unread`, `list.sort`, `sidebar.top`,
`sidebar.bottom`.
**Reassigned defaults:** space(list), d/e/h/s/S(list), preview
ctrl+f/b, account.switch `shift+s`→`shift+a`, esc behavior; **dropped:**
`o` as thread toggle (freed as sort alias).
**Sort is a feature, not only a binding:** server-side
`Email/query` sort orders (new/old/from/subject/size), window re-query
on change (same one-query invariant as mailbox open), sort picker modal
(reuses `PickerView`), `AccountPrefs.Sort` persisted per account.
mockjmap gains `sort` handling for tests.

**No action id is removed or renamed** — existing `[keys]` remaps keep
working; a remap that collides under the new defaults still fails
startup via `Validate()` with the usual conflict error.

**Unread jump implementation note:** scan rendered rows from
cursor+1/−1 for `!$seen`; at a loaded-window edge, trigger prefetch and
re-scan (same coalescing as edge scrolling, PLAN §4.1). Unified view
scans merged rows. Behaviour when nothing unread remains: stay put
(show the existing non-fatal error line "no more unread").

## 7. Scope & docs impact (AGENTS.md rule 7)

- **REQUIREMENTS:** amend FR-I3 (v2 defaults summary); **new FR-D7**
  (unread navigation + half-page + space-page); **new FR-D8** (server-side
  sort: orders, picker, per-account memory); note esc chain where
  FR-F1 describes `Esc` returns (chain preserves it: search first).
- **README:** keys table rewrite (incl. `A` switcher row); layout/pane
  prose unaffected.
- **PLAN.md:** post-M7 clause in §6.
- **Help overlay:** regenerates from keymap; `help.golden` +
  any frame showing keys re-recorded.
- **Tests:** `keys_test` (Validate defaults, unbound probe — `w` still
  free), app routing tests updated where they press `space`/`h`
  (triage_test ×3, ×4, multiaccount_test ×2, live tests ×2 — those use
  `shift+s`/`S` for the switcher too), new tests for: esc chain
  (search→fullscreen→selection), unread jump (incl. prefetch edge),
  preview ctrl+f real page, sort picker (mockjmap sort orders + prefs
  round-trip + window re-query), sidebar g/G.
- **Gates:** build, full suite, lint, gofumpt; live feel-test against
  Stalwart before commit.

## 8. Implementation order

1. keys.go: actions + bindings + help strings (incl. switcher `shift+a`,
   sizes `S`, sidebar g/G, sort); Validate green.
2. app: esc chain in the ui.search_clear handler; unread jump helper;
   preview ctrl+f/b action swap (one-line); sort picker flow.
3. sync/mockjmap: sort orders in `Email/query` + re-query path;
   `AccountPrefs.Sort` persistence.
4. Tests: fix re-pointed keys, add chain/jump/page/sort tests.
5. Docs: REQUIREMENTS (FR-I3, FR-D7, FR-D8), README table, PLAN; regen
   `help.golden`.
6. Gates + live feel-test; commit `feat: keymap v2 …`.

## 9. Resolved open questions (2026-09-25)

| # | Question | Decision |
|---|---|---|
| 1 | `q` quit confirm? | No — single press quits; `ctrl+c`×2 stays the careful path |
| 2 | Sidebar `g/G`? | Yes — added (`sidebar.top`/`bottom`) |
| 3 | `s` sort / `S` sizes? | Yes — sort picker (remembered, `o` alias), sizes on `S`, switcher moved to `A` |
| 4 | Drop `o`? | Yes — `enter` alone toggles threads |

## 10. Addendum — sidebar fold keys (2026-09-26, FR-C6)

Folder folding landed the day after this map and reassigns the sidebar's
`h`/`l` (user decision, interviewed): the ranger/lf §3 intent — `h/l`
collapse/enter in the tree — completed as **`h`/`←` fold, `l`/`→`
unfold**, with `sidebar.collapse` / `sidebar.expand` as the new action
ids (aliases grouped in help as `h/left`, `l/right`).

| Key | Was | Now |
|---|---|---|
| sidebar `h` | `sidebar.close` (hide sidebar) | `sidebar.collapse` — fold, or climb to the parent row |
| sidebar `l` | `sidebar.open` (alias of Enter) | `sidebar.expand` — unfold only; **Enter alone opens** |
| `left` / `right` | unbound | aliases of collapse / expand (sidebar pane) |
| `[` | show/hide sidebar | unchanged — now the **sole** hide key |

Consequences, all landing in the same change:

- **`sidebar.close` ships unbound** — the action id survives for remap
  stability (§6) with an empty default key; `Help` skips it until a
  remap gives it one. Its focus-hand-off (hide ⇒ focus to list) moved
  into `pane.toggle_sidebar`.
- **FR-C2/FR-C5 amended**: "Enter/`l` opens" is now "Enter opens" — the
  `l`-opens clause in §1.2/§5.2 above is superseded.
- §4 decision 2's "sidebar-`h` collapse stays" is superseded too — it
  now collapses the *tree*, which is what the help string always said.
- `Validate()` unchanged: `left`/`right` were free everywhere, `h`/`l`
  stay pane-scoped, and an unbound id still validates its remaps.

## 11. Addendum — contacts actions (2026-09-26, M9)

Two global actions joined the map with the contacts feature (FR-L),
interviewed and `Validate()`-clean — neither `c` nor `shift+n` was bound
anywhere (pane or global):

| Key | Action id | Help |
|---|---|---|
| `c` | `ui.contacts` | contacts |
| `shift+n` | `contacts.new` | add contact |

Both stay outside the contacts screen's own key loop: `ui.contacts` is a
toggle (`c` closes the screen too — implemented in `contactsKey`, since the
screen's keys never reach the keymap), and `contacts.new` prefills from the
message under the cursor (FR-L4). The screen's inner keys (`n`/`e`/`d`,
`/`, `tab`, motion) and the composer's `ctrl+g` quick search are modal-loop
keys, not keymap actions — §5.5's rule that modes own their loops.
