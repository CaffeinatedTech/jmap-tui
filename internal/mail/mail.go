// Package mail defines the provider-agnostic mail domain types and the
// Provider interface that separates the application from any specific
// protocol client. This is the swap seam: a future IMAP provider implements
// the same interface, and nothing above the sync layer knows JMAP exists.
package mail

import (
	"context"
	"time"
)

// ID is an opaque server-assigned object identifier. Callers must treat it as
// an opaque string: the format differs between providers.
type ID string

// Role is a named common purpose of a mailbox, per RFC 8621 §2. Empty means
// the server assigned no role.
type Role string

// Standard mailbox roles from RFC 8621 §2.
const (
	RoleAll     Role = "all"
	RoleArchive Role = "archive"
	RoleDrafts  Role = "drafts"
	RoleFlagged Role = "flagged"
	RoleInbox   Role = "inbox"
	RoleJunk    Role = "junk"
	RoleSent    Role = "sent"
	RoleTrash   Role = "trash"
)

// Mailbox is a named folder of messages.
type Mailbox struct {
	ID           ID
	ParentID     ID // empty for top-level mailboxes
	Name         string
	Role         Role
	SortOrder    int
	TotalEmails  int
	UnreadEmails int
}

// Address is a sender or recipient.
type Address struct {
	Name  string
	Email string
}

// Keywords is a presence set of IMAP-style flags ($seen, $flagged, …).
type Keywords map[string]struct{}

// Has reports whether the named keyword is present.
func (k Keywords) Has(name string) bool {
	_, ok := k[name]
	return ok
}

// EmailSummary is the small property set held for every message in a rolling
// window. Bodies are fetched lazily and never stored here.
type EmailSummary struct {
	ID            ID
	ThreadID      ID
	MailboxIDs    []ID
	Keywords      Keywords
	From          []Address
	To            []Address
	Subject       string
	ReceivedAt    time.Time
	Size          int64
	HasAttachment bool
	Preview       string
}

// Attachment is metadata for an attachment blob; the bytes are fetched on
// demand at save time only (NFR-4).
type Attachment struct {
	BlobID ID
	Name   string
	Type   string
	Size   int64
}

// EmailBody is the fetched content of a single message. Providers should
// populate Text when a text/plain part exists; HTML carries the raw
// text/html part for in-repo conversion otherwise (FR-E2).
type EmailBody struct {
	ID          ID
	Text        string
	HTML        string
	Attachments []Attachment
}

// SortCriterion is one level of a server-side query sort.
type SortCriterion struct {
	Property     string // e.g. "receivedAt", "subject"
	IsDescending bool
}

// QuerySpec describes one page of an open server-side query. OpenQuery
// issues the first page (Position/Limit); the window manager (PLAN §4.1)
// calls QueryHandle.Page for extensions at absolute positions.
type QuerySpec struct {
	// MailboxID scopes the query to a mailbox (inMailbox).
	MailboxID ID

	// ThreadID scopes the query to a thread (inThread); it overrides
	// MailboxID when non-empty.
	ThreadID ID

	// CollapseThreads asks the server to collapse thread members into
	// their representative (collapseThreads=true, FR-D2).
	CollapseThreads bool

	// Sort orders the result server-side. Empty means receivedAt
	// descending.
	Sort []SortCriterion

	// Position and Limit define the first page: absolute offset into the
	// (collapsed) result set and page size.
	Position int
	Limit    int
}

// QueryHandle is a paged stream of ids from an open query. Page fetches
// another chunk at an absolute position for window extension; summaries
// travel with every page so each extension costs one round-trip (FR-K4).
type QueryHandle interface {
	IDs() []ID
	Total() int
	State() string // queryState for mismatch detection (FR-B5)
	Page(ctx context.Context, position, limit int) ([]ID, []EmailSummary, error)
}

// Mutation describes a pending change to server objects. Its shape is
// finalised in M3 (triage actions); the zero value performs no mutation.
type Mutation struct{}

// Draft describes a message to send. Its shape is finalised in M5 (compose).
type Draft struct{}

// SendReceipt acknowledges a sent message. Its shape is finalised in M5.
type SendReceipt struct{}

// Change describes a server-side change pushed or polled to the client. Its
// shape is finalised in M2 (live sync).
type Change struct{}

// Provider is the protocol seam (PLAN §1). Implementations must be safe for
// concurrent use across goroutines, must never block on the UI thread, and
// must honour ctx cancellation on every network operation.
type Provider interface {
	// Connect establishes the session and discovers server capabilities.
	Connect(ctx context.Context) error

	// Mailboxes returns the full mailbox tree, flat, in server sort order.
	Mailboxes(ctx context.Context) ([]Mailbox, error)

	// OpenQuery opens a server-side query and returns a paged id stream
	// plus the summaries for the first page (batched by the provider).
	OpenQuery(ctx context.Context, spec QuerySpec) (QueryHandle, []EmailSummary, error)

	// FetchSummaries fetches list-window summaries for the given ids.
	FetchSummaries(ctx context.Context, ids []ID) ([]EmailSummary, error)

	// FetchBody fetches the full body of one message.
	FetchBody(ctx context.Context, id ID) (EmailBody, error)

	// Mutate applies flags/move/copy/destroy operations (M3).
	Mutate(ctx context.Context, mutation Mutation) error

	// Send submits a message for delivery (M5).
	Send(ctx context.Context, draft Draft) (SendReceipt, error)

	// Subscribe returns a channel of live changes and a stop function; a nil
	// channel means no push support and the caller must poll (FR-B3).
	Subscribe(ctx context.Context) (<-chan Change, func() error)
}
