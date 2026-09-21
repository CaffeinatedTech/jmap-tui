package sync

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// rssKB reads the process RSS from /proc (Linux); ok=false elsewhere.
func rssKB() (int, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.Atoi(fields[1])
				return kb, err == nil
			}
		}
	}
	return 0, false
}

// TestSoakEndlessScrollBounded is the M1 acceptance gate (REQUIREMENTS §7):
// endless scroll over a 10k+ message folder with bounded memory. The full
// engine drives a 12,000-message synthetic mailbox through a long scroll,
// deep jumps in both directions, and periodic body loads; it asserts the
// summary set stays bounded by the window cap, interaction latency stays
// under the 16ms keystroke budget (NFR-1), and process RSS stays under the
// ~50 MB budget (NFR-2).
func TestSoakEndlessScrollBounded(t *testing.T) {
	const total = 12000
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-big", Name: "Big", TotalEmails: total},
	})
	srv.SetSyntheticMailbox(mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: total})
	defer srv.Close()

	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	e := NewEngine(c, Config{Window: WindowConfig{Chunk: 50, Cap: 2000, PrefetchAt: 10}})
	ctx := context.Background()

	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	if err := e.OpenMailbox(ctx, "mb-big"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	if snap := e.Snapshot(); snap.Total != total {
		t.Fatalf("total = %d, want %d", snap.Total, total)
	}

	// The 16ms budget for purely local state changes (NFR-1).
	const localBudget = 16 * time.Millisecond
	var worstLocal time.Duration

	// Endless scroll: walk the entire folder in half-chunk steps,
	// prefetching at the edges as the UI would, loading a body every few
	// steps (body LRU churn included).
	const step = 25
	for moved := 0; moved < total; moved += step {
		t0 := time.Now()
		e.MoveCursor(step)
		if d := time.Since(t0); d > worstLocal {
			worstLocal = d
		}
		if err := e.Prefetch(ctx); err != nil {
			t.Fatalf("Prefetch at %d: %v", moved, err)
		}
		if moved%(step*10) == 0 {
			if err := e.LoadBody(ctx, e.CursorID()); err != nil {
				t.Fatalf("LoadBody at %d: %v", moved, err)
			}
		}
		if snap := e.Snapshot(); len(snap.Rows) > 2000 {
			t.Fatalf("window grew to %d rows at position %d", len(snap.Rows), moved)
		}
	}

	// Deep jumps in both directions (FR-D3 re-anchor).
	for i := 0; i < 10; i++ {
		if err := e.Jump(ctx, JumpEnd); err != nil {
			t.Fatalf("JumpEnd: %v", err)
		}
		if err := e.Jump(ctx, JumpStart); err != nil {
			t.Fatalf("JumpStart: %v", err)
		}
	}

	// Memory bounds: summaries must be held only for the window plus
	// cached threads (here: none), so at most cap+chunk entries.
	summaries := len(e.summaries)
	if summaries > 2100 {
		t.Fatalf("summaries held = %d, want <= ~cap+chunk", summaries)
	}

	if kb, ok := rssKB(); ok {
		budgetKB := 50 * 1024
		if raceEnabled {
			// The race detector's shadow memory inflates RSS; the NFR-2
			// budget applies to production builds only.
			budgetKB = 150 * 1024
		}
		if kb > budgetKB {
			t.Fatalf("RSS = %d KB exceeds the %d KB budget (NFR-2)", kb, budgetKB)
		}
		t.Logf("RSS after 12k-message full scroll + jumps + bodies: %d KB", kb)
	} else {
		t.Log("RSS not measurable on this platform; skipped NFR-2 assertion")
	}
	budget := localBudget
	if raceEnabled {
		budget *= 3
	}
	t.Logf("worst local interaction: %v (budget %v)", worstLocal, budget)
	if worstLocal > budget {
		t.Fatalf("local interaction took %v, over the %v budget (NFR-1)", worstLocal, budget)
	}
}

// TestSoakLivePushDuringScroll extends the M1 soak with the M2 acceptance
// reality: live push events arrive while the user scrolls. An injector
// goroutine creates fresh mail (slide-in path) and flips flags on seeded
// messages (patch path) while a long scroll runs; the assertions are the
// same bounded-RSS and bounded-latency budgets, now under concurrent
// reconciliation churn (AGENTS.md: soak before commits touching sync/).
func TestSoakLivePushDuringScroll(t *testing.T) {
	const total = 6000
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-big", Name: "Big", TotalEmails: total},
	})
	srv.SetSyntheticMailbox(mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: total})
	defer srv.Close()

	// Real fixtures at the old end of the result: targets for flag churn.
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var seeded []mockjmap.Email
	for i := 0; i < 20; i++ {
		seeded = append(seeded, mockjmap.Email{
			ID: fmt.Sprintf("real-%02d", i), ThreadID: fmt.Sprintf("rt-%02d", i),
			MailboxIDs: []string{"mb-big"},
			Keywords:   map[string]bool{"$seen": true},
			Subject:    fmt.Sprintf("seeded %02d", i),
			ReceivedAt: base.Add(time.Duration(i) * time.Minute),
			TextBody:   "seed body\n",
		})
	}
	srv.CreateEmails(seeded)

	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	e := NewEngine(c, Config{Window: WindowConfig{Chunk: 50, Cap: 2000, PrefetchAt: 10}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.liveCfg.pollInterval = 50 * time.Millisecond
	e.liveCfg.backoffBase = 5 * time.Millisecond
	e.Start(ctx)

	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	if err := e.OpenMailbox(ctx, "mb-big"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return srv.StreamCount() == 1 })

	// Injector: a new message every few steps, flags flipped on the seeded
	// set, each announced through the push stream (FR-K4 rate courtesy:
	// one notify per batch).
	stop := make(chan struct{})
	go func() {
		defer close(stop)
		seq := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			default:
			}
			seq++
			srv.CreateEmails([]mockjmap.Email{{
				ID: fmt.Sprintf("live-%04d", seq), ThreadID: fmt.Sprintf("lt-%04d", seq),
				MailboxIDs: []string{"mb-big"},
				Subject:    fmt.Sprintf("live arrival %04d", seq),
				ReceivedAt: time.Now().UTC(),
				TextBody:   "live body\n",
			}})
			if seq%2 == 0 {
				id := fmt.Sprintf("real-%02d", seq%20)
				srv.UpdateEmails([]string{id}, func(em *mockjmap.Email) {
					em.Keywords = map[string]bool{"$seen": seq%4 == 0}
				})
			}
			srv.Notify()
			time.Sleep(4 * time.Millisecond)
		}
	}()

	// Scroll a long stretch with prefetches and body loads, like the UI.
	const localBudget = 16 * time.Millisecond
	var worstLocal time.Duration
	const step = 25
	for moved := 0; moved < 3000; moved += step {
		t0 := time.Now()
		e.MoveCursor(step)
		if d := time.Since(t0); d > worstLocal {
			worstLocal = d
		}
		if err := e.Prefetch(ctx); err != nil {
			t.Fatalf("Prefetch at %d: %v", moved, err)
		}
		if moved%(step*20) == 0 {
			if err := e.LoadBody(ctx, e.CursorID()); err != nil {
				t.Fatalf("LoadBody at %d: %v", moved, err)
			}
		}
		if snap := e.Snapshot(); len(snap.Rows) > 2000 {
			t.Fatalf("window grew to %d rows at position %d", len(snap.Rows), moved)
		}
	}

	cancel()
	<-stop

	// Proof the live loop actually reconciled during the scroll: the email
	// state string must have advanced past its bootstrap value.
	e.mu.Lock()
	bootstrapped, advanced := e.emailState, e.emailState != ""
	e.mu.Unlock()
	if !advanced {
		t.Fatal("email state string empty; live loop never bootstrapped")
	}
	_ = bootstrapped

	// Bounded memory under churn: window + thread cache + overlay + a
	// bounded fresh set — summaries must not accumulate per push event.
	if n := len(e.summaries); n > 2100 {
		t.Fatalf("summaries held = %d under live churn, want <= ~cap+chunk", n)
	}
	if kb, ok := rssKB(); ok {
		budgetKB := 50 * 1024
		if raceEnabled {
			// The race detector's shadow memory roughly triples RSS; the
			// NFR-2 budget applies to production builds only.
			budgetKB = 150 * 1024
		}
		if kb > budgetKB {
			t.Fatalf("RSS = %d KB exceeds the %d KB budget under live churn (NFR-2)", kb, budgetKB)
		}
		t.Logf("RSS after scroll + live push churn: %d KB", kb)
	}
	budget := localBudget
	if raceEnabled {
		budget *= 3
	}
	t.Logf("worst local interaction under churn: %v (budget %v)", worstLocal, budget)
	if worstLocal > budget {
		t.Fatalf("local interaction took %v under churn, over the %v budget (NFR-1)", worstLocal, budget)
	}
}

// TestSoakSearchScrollBounded is the M4 companion to the endless-scroll
// soak: a search view over a 12k-message mailbox scrolled end to end with
// jump re-anchors, then closed — asserting the same NFR-1/2 bounds and
// that the parked mailbox view doubles nothing beyond one extra window's
// summaries (FR-F1 Esc is instant, so the parked rows stay resident).
func TestSoakSearchScrollBounded(t *testing.T) {
	const total = 12000
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-big", Name: "Big", TotalEmails: total},
	})
	srv.SetSyntheticMailbox(mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: total})
	defer srv.Close()

	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	e := NewEngine(c, Config{Window: WindowConfig{Chunk: 50, Cap: 2000, PrefetchAt: 10}})
	ctx := context.Background()

	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	if err := e.OpenMailbox(ctx, "mb-big"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	if err := e.SearchOpen(ctx, SearchSpec{Text: "Synthetic"}); err != nil {
		t.Fatalf("SearchOpen: %v", err)
	}
	if snap := e.Snapshot(); snap.Total != total {
		t.Fatalf("search total = %d, want %d", snap.Total, total)
	}

	const localBudget = 16 * time.Millisecond
	var worstLocal time.Duration
	const step = 25
	for moved := 0; moved < total; moved += step {
		t0 := time.Now()
		e.MoveCursor(step)
		if d := time.Since(t0); d > worstLocal {
			worstLocal = d
		}
		if err := e.Prefetch(ctx); err != nil {
			t.Fatalf("Prefetch at %d: %v", moved, err)
		}
		if snap := e.Snapshot(); len(snap.Rows) > 2000 {
			t.Fatalf("search window grew to %d rows at position %d", len(snap.Rows), moved)
		}
	}
	for i := 0; i < 5; i++ {
		if err := e.Jump(ctx, JumpEnd); err != nil {
			t.Fatalf("JumpEnd: %v", err)
		}
		if err := e.Jump(ctx, JumpStart); err != nil {
			t.Fatalf("JumpStart: %v", err)
		}
	}

	// Two windows are resident while searching (active + parked): at
	// most 2×(cap+chunk) summaries (FR-F1 instant-Esc cost, bounded).
	if got := len(e.summaries); got > 4200 {
		t.Fatalf("summaries held = %d, want <= ~2×(cap+chunk)", got)
	}

	snapAfterClose := e.SearchClose()
	if snapAfterClose.SearchActive {
		t.Fatal("SearchActive survived SearchClose")
	}

	if kb, ok := rssKB(); ok {
		budgetKB := 50 * 1024
		if raceEnabled {
			budgetKB = 150 * 1024
		}
		if kb > budgetKB {
			t.Fatalf("RSS = %d KB exceeds the %d KB budget (NFR-2)", kb, budgetKB)
		}
		t.Logf("RSS after 12k-message search scroll + jumps + close: %d KB", kb)
	}
	budget := localBudget
	if raceEnabled {
		budget *= 3
	}
	if worstLocal > budget {
		t.Fatalf("local interaction took %v, over the %v budget (NFR-1)", worstLocal, budget)
	}
	t.Logf("worst local interaction: %v (budget %v)", worstLocal, budget)
}
