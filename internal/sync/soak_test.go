package sync

import (
	"context"
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
		const budgetKB = 50 * 1024
		if kb > budgetKB {
			t.Fatalf("RSS = %d KB exceeds the %d KB budget (NFR-2)", kb, budgetKB)
		}
		t.Logf("RSS after 12k-message full scroll + jumps + bodies: %d KB", kb)
	} else {
		t.Log("RSS not measurable on this platform; skipped NFR-2 assertion")
	}
	t.Logf("worst local interaction: %v (budget %v)", worstLocal, localBudget)
	if worstLocal > localBudget {
		t.Fatalf("local interaction took %v, over the %v budget (NFR-1)", worstLocal, localBudget)
	}
}
