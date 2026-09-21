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

// Send implements mail.Provider; compose (M5) drives it.
func (c *Client) Send(ctx context.Context, draft mail.Draft) (mail.SendReceipt, error) {
	return mail.SendReceipt{}, fmt.Errorf("jmapclient: Send: %w (lands in M5)", ErrUnimplemented)
}
