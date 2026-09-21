package sync

import (
	"context"
	"strings"
	"time"

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
// over the scope's message ids, matching the summary headers client-side.
// Stalwart-class servers match whole tokens only (PLAN §7), so text-ish
// fields degrade to substring semantics here; exact fields (keyword,
// attachment, dates) re-apply with their native semantics. Only match ids
// are held — memory stays bounded (NFR-2), nothing touches disk (NFR-4).
type scanState struct {
	scope   mail.ID  // mailbox scope (empty = all mailboxes)
	text    []string // lowercased substrings: any of subject/from/to
	subject []string // subject only
	from    []string // from only
	to      []string // to only
	keyword string   // exact keyword presence
	attach  *bool    // nil = any; true = has; false = lacks
	after   time.Time
	before  time.Time

	matches []mail.ID
	scanned int
	total   int // scope size; -1 until the first id page
}

// newScanState lowers the spec's text-ish fields for matching. scannable
// must have returned true first (at least one text-ish field set).
func newScanState(s SearchSpec) *scanState {
	return &scanState{
		scope:   s.ScopeMailbox,
		text:    lowerWords(s.Text),
		subject: lowerWords(s.Subject),
		from:    lowerWords(s.From),
		to:      lowerWords(s.To),
		keyword: s.HasKeyword,
		attach:  s.HasAttachment,
		after:   s.After,
		before:  s.Before,
	}
}

// scannable reports whether the spec has at least one text-ish field —
// the only case where client-side substring semantics can differ from
// the server's token semantics. Purely exact searches (keyword,
// attachment, dates, scope) never scan: the server's zero is final and
// the walk would only repeat it (FR-K4 courtesy).
func (s SearchSpec) scannable() bool {
	return len(lowerWords(s.Text)) > 0 ||
		len(lowerWords(s.From)) > 0 ||
		len(lowerWords(s.To)) > 0 ||
		len(lowerWords(s.Subject)) > 0
}

// match applies the scan predicates to one summary: every text word must
// appear (case-insensitively) in its field's haystack — the text field's
// haystack is subject+from+to combined — and the exact fields re-apply.
func (st *scanState) match(s mail.EmailSummary) bool {
	subj := strings.ToLower(s.Subject)
	from, to := addressHay(s.From), addressHay(s.To)
	containsAll := func(hay string, words []string) bool {
		for _, w := range words {
			if !strings.Contains(hay, w) {
				return false
			}
		}
		return true
	}
	for _, w := range st.text {
		if !strings.Contains(subj, w) && !strings.Contains(from, w) && !strings.Contains(to, w) {
			return false
		}
	}
	if !containsAll(subj, st.subject) || !containsAll(from, st.from) || !containsAll(to, st.to) {
		return false
	}
	if st.keyword != "" && !s.Keywords.Has(st.keyword) {
		return false
	}
	if st.attach != nil && s.HasAttachment != *st.attach {
		return false
	}
	if !st.after.IsZero() && !s.ReceivedAt.After(st.after) {
		return false
	}
	if !st.before.IsZero() && !s.ReceivedAt.Before(st.before) {
		return false
	}
	return true
}

// addressHay flattens addresses into one lowercased haystack.
func addressHay(addrs []mail.Address) string {
	var b strings.Builder
	for _, a := range addrs {
		b.WriteString(" ")
		b.WriteString(strings.ToLower(a.Name))
		b.WriteString(" ")
		b.WriteString(strings.ToLower(a.Email))
	}
	return b.String()
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
	st := newScanState(s)
	st.total = -1
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
			if st.match(s) {
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
