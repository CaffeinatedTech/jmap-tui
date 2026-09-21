// Package mail defines the provider-agnostic mail domain types and the
// Provider interface that separates the application from any specific
// protocol client. This is the swap seam: a future IMAP provider implements
// the same interface, and nothing above the sync layer knows JMAP exists.
package mail

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrCannotCalculateChanges reports a server that cannot produce a /changes
// delta for the given sinceState (RFC 8620 §5.2). Providers wrap the
// server's typed error with this sentinel so the sync engine can answer it
// with a full re-query instead of guessing (FR-B5, PLAN §4.1 case 2).
var ErrCannotCalculateChanges = errors.New("server cannot calculate changes")

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

// SearchFilter is a server-side content filter (RFC 8621 §4.4.1
// FilterCondition). Set fields combine with AND semantics; zero values are
// omitted from the wire filter. It backs the M4 search views (FR-F1).
type SearchFilter struct {
	Text       string // free text across headers and body
	From       string
	To         string
	Subject    string
	After      time.Time // receivedAt after this instant (exclusive)
	Before     time.Time // receivedAt before this instant (exclusive)
	HasKeyword string    // exact keyword presence ($seen, $flagged, …)

	// HasAttachment is tri-state: nil = any, true = has attachments,
	// false = has none. go-jmap's typed condition cannot express false on
	// the wire (bool omitempty), so providers honour true; false is
	// accepted and may degrade to "any".
	HasAttachment *bool
}

// QuerySpec describes one page of an open server-side query. OpenQuery
// issues the first page (Position/Limit); the window manager (PLAN §4.1)
// calls QueryHandle.Page for extensions at absolute positions.
type QuerySpec struct {
	// MailboxID scopes the query to a mailbox (inMailbox). In search
	// queries it is the search scope; empty means all mailboxes.
	MailboxID ID

	// ThreadID scopes the query to a thread (inThread); it overrides
	// MailboxID when non-empty.
	ThreadID ID

	// Search applies content filters (FR-F1); nil for mailbox browsing
	// and thread expansion.
	Search *SearchFilter

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

	// AnchorID and AnchorOffset address the result set by id instead of
	// absolute position (RFC 8620 queryArguments): the page starts at
	// AnchorOffset relative to the anchor id's current position. When set,
	// Position is ignored. Re-anchoring after live changes (FR-B5) uses
	// this, since absolute positions shift under the window.
	AnchorID     ID
	AnchorOffset int
}

// QueryHandle is a paged stream of ids from an open query. Page fetches
// another chunk at an absolute position for window extension; summaries
// travel with every page so each extension costs one round-trip (FR-K4).
type QueryHandle interface {
	IDs() []ID
	// Start is the absolute position of IDs()[0] in the full result set.
	Start() int
	Total() int
	State() string // queryState for mismatch detection (FR-B5)
	// EmailState is the Email/get state string accompanying the page —
	// the bootstrap value for Email/changes reconciliation (FR-B5).
	EmailState() string
	Page(ctx context.Context, position, limit int) ([]ID, []EmailSummary, error)
}

// EmailPatch is the per-email change set inside a Mutation: keyword
// presence flips and mailbox membership deltas (FR-G1, FR-G2). Nil fields
// are untouched; deltas compose, so a patch may add and remove mailboxes in
// one update (move semantics).
type EmailPatch struct {
	// SetKeywords flips keyword presence: true adds, false removes
	// ($seen, $flagged, …). Keywords not named here are untouched.
	SetKeywords map[string]bool

	// AddMailboxes adds the email to these mailboxes.
	AddMailboxes []ID

	// RemoveMailboxes removes the email from these mailboxes.
	RemoveMailboxes []ID
}

// Mutation is one batched set of server mutations (RFC 8620 §5.3 Email/set),
// finalised in M3: triage actions build one Mutation per user action so
// multi-select costs a single round-trip (FR-G3, FR-K4).
type Mutation struct {
	// Emails maps email id to its change set.
	Emails map[ID]EmailPatch

	// Destroy lists email ids to permanently delete (FR-G2: only inside
	// Trash; delete elsewhere is a move).
	Destroy []ID
}

// MutationResult reports what a Mutate actually did. Rejected ids carry
// the server's per-id error so callers can revert exactly the failed part.
type MutationResult struct {
	OldState string
	NewState string

	Updated   []ID
	Destroyed []ID

	// NotUpdated and NotDestroyed map rejected ids to their server errors.
	NotUpdated   map[ID]error
	NotDestroyed map[ID]error
}

// Draft describes a message to send. Its shape is finalised in M5 (compose).
type Draft struct{}

// SendReceipt acknowledges a sent message. Its shape is finalised in M5.
type SendReceipt struct{}

// Change describes a pushed or polled server notification that one or more
// object types changed state (RFC 8620 §7.1 StateChange). Changed maps
// account id → type name ("Email", "Mailbox") → the type's new state string.
// It says nothing about *what* changed — /changes does that (FR-B2).
type Change struct {
	Changed map[ID]map[string]string
}

// EmailChangeSet is one Email/changes delta (RFC 8620 §5.2). Updated holds
// created and modified ids alike.
type EmailChangeSet struct {
	Updated   []ID
	Destroyed []ID
	NewState  string
	HasMore   bool
}

// MailboxChangeSet is one Mailbox/changes delta (RFC 8621 §2).
type MailboxChangeSet struct {
	Updated   []ID
	Destroyed []ID
	NewState  string
	HasMore   bool
}

// MailboxList is the full mailbox tree plus the Mailbox/get state string it
// was read at — the bootstrap value for Mailbox/changes (FR-B5).
type MailboxList struct {
	Mailboxes []Mailbox
	State     string
}

// Identity is a sendable address from Identity/get (FR-B1). Reading is all
// M2 needs; compose uses it in M5.
type Identity struct {
	ID    ID
	Name  string
	Email string
}

// Provider is the protocol seam (PLAN §1). Implementations must be safe for
// concurrent use across goroutines, must never block on the UI thread, and
// must honour ctx cancellation on every network operation.
type Provider interface {
	// Connect establishes the session and discovers server capabilities.
	Connect(ctx context.Context) error

	// Mailboxes returns the full mailbox tree, flat, in server sort order,
	// with its state string (FR-B1, FR-B5).
	Mailboxes(ctx context.Context) (MailboxList, error)

	// OpenQuery opens a server-side query and returns a paged id stream
	// plus the summaries for the first page (batched by the provider).
	OpenQuery(ctx context.Context, spec QuerySpec) (QueryHandle, []EmailSummary, error)

	// FetchSummaries fetches list-window summaries for the given ids.
	FetchSummaries(ctx context.Context, ids []ID) ([]EmailSummary, error)

	// FetchBody fetches the full body of one message.
	FetchBody(ctx context.Context, id ID) (EmailBody, error)

	// EmailChanges fetches the Email delta since a state string (FR-B2).
	// A cannotCalculateChanges server error is returned unwrapped so the
	// engine can fall back to a full re-query (PLAN §4.1).
	EmailChanges(ctx context.Context, sinceState string) (EmailChangeSet, error)

	// MailboxChanges fetches the Mailbox delta since a state string.
	MailboxChanges(ctx context.Context, sinceState string) (MailboxChangeSet, error)

	// Identities returns the account's sendable identities. Servers without
	// the submission capability yield an empty list, never an error (FR-A6).
	Identities(ctx context.Context) ([]Identity, error)

	// Mutate applies one batched set of flag/move/copy/destroy operations
	// (FR-G1..G3) as a single round-trip. Per-id rejections are reported in
	// the result, not as the error: a transport-level failure is the error.
	Mutate(ctx context.Context, mutation Mutation) (MutationResult, error)

	// DownloadBlob fetches an attachment blob by id over the session
	// download URL (FR-E4). name and mediaType fill the URL template;
	// the caller closes the reader.
	DownloadBlob(ctx context.Context, blobID ID, name, mediaType string) (io.ReadCloser, error)

	// Send submits a message for delivery (M5).
	Send(ctx context.Context, draft Draft) (SendReceipt, error)

	// Subscribe opens one EventSource push stream (RFC 8620 §7.3) and
	// returns a channel of state-change notifications plus a stop function.
	// A nil channel means the server advertises no push URL and the caller
	// must poll (FR-B3). The stream closes — channel drained and closed —
	// when ctx is cancelled, stop is called, or the connection dies; the
	// implementation enforces liveness via the server ping interval.
	Subscribe(ctx context.Context) (<-chan Change, func() error)
}
