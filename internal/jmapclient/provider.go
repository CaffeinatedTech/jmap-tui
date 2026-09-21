package jmapclient

import (
	"context"
	"fmt"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// The provider surface below is fixed by mail.Provider (PLAN §1) but each
// method's implementation lands with its milestone. Returning a typed,
// documented error keeps the interface assertion honest without inventing
// half-protocols early.

// OpenQuery implements mail.Provider; the rolling window (M1) drives it.
func (c *Client) OpenQuery(ctx context.Context, spec mail.QuerySpec) (mail.QueryHandle, error) {
	return nil, fmt.Errorf("jmapclient: OpenQuery: %w (lands in M1)", ErrUnimplemented)
}

// FetchSummaries implements mail.Provider; the list window (M1) drives it.
func (c *Client) FetchSummaries(ctx context.Context, ids []mail.ID) ([]mail.EmailSummary, error) {
	return nil, fmt.Errorf("jmapclient: FetchSummaries: %w (lands in M1)", ErrUnimplemented)
}

// FetchBody implements mail.Provider; the preview pane (M1) drives it.
func (c *Client) FetchBody(ctx context.Context, id mail.ID) (mail.EmailBody, error) {
	return mail.EmailBody{}, fmt.Errorf("jmapclient: FetchBody: %w (lands in M1)", ErrUnimplemented)
}

// Mutate implements mail.Provider; triage actions (M3) drive it.
func (c *Client) Mutate(ctx context.Context, mutation mail.Mutation) error {
	return fmt.Errorf("jmapclient: Mutate: %w (lands in M3)", ErrUnimplemented)
}

// Send implements mail.Provider; compose (M5) drives it.
func (c *Client) Send(ctx context.Context, draft mail.Draft) (mail.SendReceipt, error) {
	return mail.SendReceipt{}, fmt.Errorf("jmapclient: Send: %w (lands in M5)", ErrUnimplemented)
}

// Subscribe implements mail.Provider; the live sync engine (M2) drives it.
func (c *Client) Subscribe(ctx context.Context) (<-chan mail.Change, func() error) {
	return nil, func() error { return fmt.Errorf("jmapclient: Subscribe: %w (lands in M2)", ErrUnimplemented) }
}
