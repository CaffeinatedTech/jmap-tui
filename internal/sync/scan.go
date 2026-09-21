package sync

import (
	"context"
	"strings"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// scanChunk bounds one id page and one summary batch of the LIKE scan:
// big enough to keep round-trips down on large scopes, small enough that
// matches stream in promptly (FR-K4).
const scanChunk = 500

// ScanProgress reports an active LIKE scan for the status display (the
// "scanning… n/N" line, PLAN §4.4).
type ScanProgress struct {
	Active  bool
	Scanned int
	Total   int // scope size; -1 until the first id page lands
}

// scanState is one fuzzy scan (FR-F1 auto fallback): a newest-first walk
// over the scope's message ids, substring-matching the summary headers
// client-side. Stalwart-class servers match whole tokens only (PLAN §7),
// so partial-word queries fall back to this scan when the server search
// returns nothing. Only match ids are held — memory stays bounded (NFR-2)
// and nothing touches disk (NFR-4).
type scanState struct {
	scope   mail.ID  // mailbox scope (empty = all mailboxes)
	words   []string // lowercased substrings, AND semantics across words
	matches []mail.ID
	scanned int
	total   int // scope size; -1 until the first id page
}

// scanMatch reports whether every word appears (case-insensitively) in
// the summary's subject or addresses — per-word any-field, AND across
// words. Bodies are not scanned: summaries never carry them, and the
// server token path covers full-word body search.
func scanMatch(s mail.EmailSummary, words []string) bool {
	var hay strings.Builder
	hay.WriteString(strings.ToLower(s.Subject))
	for _, a := range s.From {
		hay.WriteString(" ")
		hay.WriteString(strings.ToLower(a.Name))
		hay.WriteString(" ")
		hay.WriteString(strings.ToLower(a.Email))
	}
	for _, a := range s.To {
		hay.WriteString(" ")
		hay.WriteString(strings.ToLower(a.Name))
		hay.WriteString(" ")
		hay.WriteString(strings.ToLower(a.Email))
	}
	h := hay.String()
	for _, w := range words {
		if !strings.Contains(h, w) {
			return false
		}
	}
	return true
}

// lowerWords splits the raw query into lowercased per-word substrings.
func lowerWords(raw string) []string {
	return strings.Fields(strings.ToLower(raw))
}

// startScan cancels any running scan and launches a fresh one for the
// given spec. The caller holds mu. The scan goroutine publishes snapshots
// as match batches land; SearchClose and later SearchOpens supersede it
// via the generation counter.
func (e *Engine) startScanLocked(ctx context.Context, s SearchSpec) {
	e.scanGen++
	st := &scanState{scope: s.ScopeMailbox, words: lowerWords(s.Text), total: -1}
	e.scan = st
	gen := e.scanGen
	go e.runScan(ctx, st, gen)
}

// cancelScanLocked drops the active scan, if any. Caller holds mu.
func (e *Engine) cancelScanLocked() {
	e.scan = nil
	e.scanGen++
}

// runScan walks the scope newest-first: one id page, one summary batch,
// one publish per round-trip pair. Superseded scans (generation bump) and
// cancelled contexts exit quietly — the view they served is gone.
func (e *Engine) runScan(ctx context.Context, st *scanState, gen uint64) {
	superseded := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.scanGen != gen || e.scan != st
	}

	for pos := 0; ; pos += scanChunk {
		if ctx.Err() != nil || superseded() {
			return
		}
		// The scope page: the same filter the token search used, minus
		// the text condition — every message in scope, newest first.
		spec := mail.QuerySpec{
			MailboxID: st.scope,
			Sort:      []mail.SortCriterion{{Property: "receivedAt", IsDescending: true}},
			Position:  pos,
			Limit:     scanChunk,
		}
		handle, _, err := e.p.OpenQuery(ctx, spec)
		if err != nil {
			e.finishScan(st, gen)
			return
		}
		ids := handle.IDs()
		if len(ids) == 0 {
			break
		}
		sums, err := e.p.FetchSummaries(ctx, ids)
		if err != nil {
			e.finishScan(st, gen)
			return
		}

		e.mu.Lock()
		if e.scanGen != gen || e.scan != st {
			e.mu.Unlock()
			return
		}
		st.scanned += len(ids)
		if st.total < 0 {
			st.total = handle.Total()
		}
		for _, s := range sums {
			if scanMatch(s, st.words) {
				st.matches = append(st.matches, s.ID)
				e.summaries[s.ID] = s
			}
		}
		e.publishLocked()
		e.mu.Unlock()
	}

	e.mu.Lock()
	if e.scanGen == gen && e.scan == st {
		e.publishLocked()
	}
	e.mu.Unlock()
}

// finishScan marks a failed scan complete so the progress line settles
// instead of spinning forever. Caller context: none held.
func (e *Engine) finishScan(st *scanState, gen uint64) {
	e.mu.Lock()
	if e.scanGen == gen && e.scan == st {
		e.publishLocked()
	}
	e.mu.Unlock()
}
