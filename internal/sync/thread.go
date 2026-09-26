package sync

import (
	"context"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// maxThreadSizes bounds one Thread/get issued by the size refresh: the
// window itself grows in chunks (FR-D3), so counts arrive in batches as
// rows appear, and a mailbox full of long threads never turns a single
// refresh into a firehose of member ids.
const maxThreadSizes = 200

// RefreshThreadSizes fills in the member counts behind the list's
// expandable-row chevron (FR-D1) — the answer to "which of these rows can
// I press Enter on?" Every thread the window shows whose size is still
// unknown goes out in one batched Thread/get; once a view is fully sized
// this costs a mutex and a slice scan, no round trip.
//
// It runs off the UI thread from a Cmd (NFR-1) and publishes only what it
// learned. A server that cannot answer is not fatal (FR-A6): the threads
// are recorded as singletons so a persistent failure can never become a
// request per snapshot (FR-K4), and the real counts come back the next
// time the row set changes, because sizes are pruned with the window.
func (e *Engine) RefreshThreadSizes(ctx context.Context) {
	e.mu.Lock()
	if e.window == nil || e.scan != nil {
		// Scan results are flat rows with no thread to expand (FR-F1).
		e.mu.Unlock()
		return
	}
	want := make([]mail.ID, 0, 16)
	seen := make(map[mail.ID]bool, 16)
	for _, id := range e.window.IDs() {
		s, ok := e.summaries[id]
		if !ok || s.ThreadID == "" || seen[s.ThreadID] {
			continue
		}
		seen[s.ThreadID] = true
		if _, known := e.threadSizes[s.ThreadID]; known {
			continue
		}
		want = append(want, s.ThreadID)
		if len(want) >= maxThreadSizes {
			break
		}
	}
	e.mu.Unlock()
	if len(want) == 0 {
		return // a view is already fully sized
	}

	threads, err := e.p.Threads(ctx, want)

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, tid := range want {
		if _, known := e.threadSizes[tid]; known {
			continue // a concurrent expansion learned it first
		}
		if err != nil {
			// Cosmetic data: record "nothing beneath this row" rather than
			// surface a toast, and stop asking until the row set changes.
			e.threadSizes[tid] = 1
			continue
		}
		// A thread the server does not answer for is not expandable; a
		// thread with no members cannot exist, so both read as a singleton
		// rather than as "still unknown" — unknown would re-fetch forever.
		members := threads[tid]
		if len(members) == 0 {
			e.threadSizes[tid] = 1
			continue
		}
		e.threadSizes[tid] = len(members)
	}
	e.publishLocked()
}

// rememberThreadSizeLocked records a member count the server just reported
// (FR-D1): an expansion that finds only the row itself corrects a chevron
// that promised a reply beneath it. Caller holds mu; publishes only when
// the count actually changed.
func (e *Engine) rememberThreadSizeLocked(threadID mail.ID, size int) {
	if size < 1 {
		size = 1
	}
	if threadID == "" || e.threadSizes[threadID] == size {
		return
	}
	e.threadSizes[threadID] = size
	e.publishLocked()
}
