package sync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// AccountInfo identifies one enrolled account for the UI (FR-A1, FR-A4).
type AccountInfo struct {
	ID   string
	Name string
}

// connectTimeout bounds one Connect attempt (FR-A3); the retry loop owns
// the pacing between attempts.
const connectTimeout = 30 * time.Second

// hubEntry is one enrolled account: its engine plus the connection state
// Start needs.
type hubEntry struct {
	info         AccountInfo
	provider     mail.Provider
	preConnected bool
	engine       *Engine
	started      bool
}

// Hub owns one Engine per account (PLAN §4.3): enrollment, the connection
// lifecycle with failure isolation, and lookup for action routing. Each
// engine keeps its own store, rolling window, and EventSource — one
// account's connect failure or sync error never blocks another's
// (FR-A4, FR-I5). The Hub never renders; snapshots reach the UI through
// the engines' update channels.
type Hub struct {
	mu      sync.Mutex
	order   []string
	entries map[string]*hubEntry
}

// NewHub returns an empty hub; enroll accounts before StartAll.
func NewHub() *Hub {
	return &Hub{entries: map[string]*hubEntry{}}
}

// Enroll creates the account's engine and registers it in enrollment
// order (the switcher's display order). preConnected reports whether the
// provider already completed Connect — the TUI pre-flights connections so
// total failure still exits with an actionable error, while partial
// failure defers to Start's retry loop. A repeat enroll of the same id
// returns the existing engine unchanged.
func (h *Hub) Enroll(id, name string, p mail.Provider, preConnected bool, cfg Config) *Engine {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.entries[id]; ok {
		return e.engine
	}
	h.entries[id] = &hubEntry{
		info:         AccountInfo{ID: id, Name: name},
		provider:     p,
		preConnected: preConnected,
		engine:       NewEngine(p, cfg),
	}
	h.order = append(h.order, id)
	return h.entries[id].engine
}

// Engine returns the account's engine, or nil when not enrolled.
func (h *Hub) Engine(id string) *Engine {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.entries[id]; ok {
		return e.engine
	}
	return nil
}

// Provider returns the account's provider (blob upload/download route
// through it), or nil when not enrolled.
func (h *Hub) Provider(id string) mail.Provider {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.entries[id]; ok {
		return e.provider
	}
	return nil
}

// Accounts lists enrolled accounts in enrollment order.
func (h *Hub) Accounts() []AccountInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]AccountInfo, 0, len(h.order))
	for _, id := range h.order {
		out = append(out, h.entries[id].info)
	}
	return out
}

// StartAll starts live sync for every enrolled account (PLAN §4.3).
func (h *Hub) StartAll(ctx context.Context) {
	for _, a := range h.Accounts() {
		h.Start(ctx, a.ID)
	}
}

// Start brings one account up: connect when the pre-flight did not
// succeed (retrying with the standard backoff and recording each failure
// on the engine's status, so the footer and switcher show it while every
// other account keeps running — failure isolation), then launch the live
// sync loop and perform the initial identities + mailbox load (FR-B1).
// Idempotent; the connect retry ends when ctx is cancelled.
func (h *Hub) Start(ctx context.Context, id string) {
	h.mu.Lock()
	e := h.entries[id]
	if e == nil || e.started {
		h.mu.Unlock()
		return
	}
	e.started = true
	h.mu.Unlock()

	if e.provider == nil {
		// Nothing to connect with (credential/config failure): the error
		// stays on the status line until the config changes (FR-A3 —
		// visible, never fatal to the other accounts).
		e.engine.setLastError(fmt.Errorf("connect: no credentials for %q", e.info.Name))
		return
	}

	go func() {
		if !e.preConnected {
			for fails := 1; ; fails++ {
				cctx, cancel := context.WithTimeout(ctx, connectTimeout)
				err := e.provider.Connect(cctx)
				cancel()
				if err == nil {
					break
				}
				e.engine.setLastError(fmt.Errorf("connect: %w", err))
				if !sleepCtx(ctx, e.engine.backoff(fails)) {
					return
				}
			}
		}
		e.engine.Start(ctx)
		// Identity absence or failure never blocks reading (FR-A6).
		_ = e.engine.LoadIdentities(ctx)
		if err := e.engine.LoadMailboxes(ctx); err != nil {
			e.engine.setLastError(fmt.Errorf("load: %w", err))
			return
		}
		// A failed pre-flight's error is stale the moment the account is
		// actually up; the live loop stamps LastSync on the first pass.
		e.engine.streamUp()
	}()
}

// NoteError records a pre-flight failure on the account's status (FR-I5)
// so the footer and switcher show it immediately — before any retry
// attempt lands.
func (h *Hub) NoteError(id string, err error) {
	if err == nil {
		return
	}
	if e := h.Engine(id); e != nil {
		e.setLastError(err)
	}
}
