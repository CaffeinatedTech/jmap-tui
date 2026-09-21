package jmapclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// emailFixtures exercises query/get surfaces: a 2-member thread, an
// HTML-only message, and one with an attachment.
func emailFixtures() []mockjmap.Email {
	base := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	return []mockjmap.Email{
		{
			ID: "e3", ThreadID: "t2", MailboxIDs: []string{"mb-inbox"},
			Keywords: map[string]bool{"$seen": true},
			From:     []mockjmap.Address{{Name: "Carol", Email: "carol@example.test"}},
			To:       []mockjmap.Address{{Email: "me@example.test"}},
			Subject:  "HTML only", ReceivedAt: base.Add(2 * time.Hour),
			Size: 2048, Preview: "Rich text preview",
			HTMLBody: "<html><body><p>Rich <b>text</b></p></body></html>",
		},
		{
			ID: "e2", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Bob", Email: "bob@example.test"}},
			To:      []mockjmap.Address{{Email: "me@example.test"}},
			Subject: "Re: thread starter", ReceivedAt: base.Add(1 * time.Hour),
			Size: 1024, Preview: "The reply body",
			TextBody: "The reply body.\n",
		},
		{
			ID: "e1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
			To:      []mockjmap.Address{{Email: "me@example.test"}},
			Subject: "thread starter", ReceivedAt: base,
			Size: 4096, HasAttachment: true, Preview: "The original body",
			TextBody:    "The original body.\n",
			Attachments: []mockjmap.Attachment{{BlobID: "blob-att1", Name: "notes.txt", Type: "text/plain", Size: 99}},
		},
	}
}

func newEmailTestClient(t *testing.T) (*Client, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", testPassword, fixtures())
	srv.SetEmails(emailFixtures())
	t.Cleanup(srv.Close)
	c := New(Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: testPassword})
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c, srv
}

func TestOpenQueryReturnsSortedPageWithSummaries(t *testing.T) {
	c, _ := newEmailTestClient(t)
	ctx := context.Background()

	h, sums, err := c.OpenQuery(ctx, mail.QuerySpec{
		MailboxID: "mb-inbox", CollapseThreads: true, Position: 0, Limit: 50,
	})
	if err != nil {
		t.Fatalf("OpenQuery: %v", err)
	}
	// receivedAt descending: e3, then thread t1's representative — its
	// newest member e2 (servers collapse to the first row in sort order).
	ids := h.IDs()
	if len(ids) != 2 || ids[0] != "e3" || ids[1] != "e2" {
		t.Fatalf("ids = %v, want [e3 e2] (thread t1 collapsed)", ids)
	}
	if h.Total() != 2 || h.State() == "" {
		t.Fatalf("total/state = %d/%q", h.Total(), h.State())
	}
	if len(sums) != 2 {
		t.Fatalf("summaries = %d, want 2", len(sums))
	}
	first := sums[0]
	if first.Subject != "HTML only" || first.From[0].Name != "Carol" || first.Size != 2048 {
		t.Fatalf("summary[0] = %+v", first)
	}
	if first.ReceivedAt.IsZero() || first.ThreadID != "t2" {
		t.Fatalf("summary[0] metadata incomplete: %+v", first)
	}
	if !first.Keywords.Has("$seen") {
		t.Fatalf("summary[0] keywords = %v, want $seen", first.Keywords)
	}
	if sums[1].ThreadID != "t1" {
		t.Fatalf("summary[1] thread = %q, want t1", sums[1].ThreadID)
	}
}

func TestOpenQueryWithoutCollapseKeepsThreadMembers(t *testing.T) {
	c, _ := newEmailTestClient(t)
	ctx := context.Background()

	h, _, err := c.OpenQuery(ctx, mail.QuerySpec{MailboxID: "mb-inbox", Limit: 50})
	if err != nil {
		t.Fatalf("OpenQuery: %v", err)
	}
	if ids := h.IDs(); len(ids) != 3 || ids[0] != "e3" || ids[1] != "e2" || ids[2] != "e1" {
		t.Fatalf("ids = %v, want [e3 e2 e1]", ids)
	}
}

func TestQueryPageExtends(t *testing.T) {
	c, srv := newEmailTestClient(t)
	srv.SetSyntheticMailbox(mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: 120})
	ctx := context.Background()

	h, sums, err := c.OpenQuery(ctx, mail.QuerySpec{MailboxID: "mb-big", Limit: 50})
	if err != nil {
		t.Fatalf("OpenQuery: %v", err)
	}
	if len(h.IDs()) != 50 || h.Total() != 120 {
		t.Fatalf("first page ids %d total %d", len(h.IDs()), h.Total())
	}
	if len(sums) != 50 || sums[0].Subject != "Synthetic message 000000" {
		t.Fatalf("first summary = %+v", sums[0])
	}

	ids, sums2, err := h.Page(ctx, 50, 50)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(ids) != 50 || ids[0] != "syn-000050" {
		t.Fatalf("page ids[0] = %s, want syn-000050", ids[0])
	}
	if len(sums2) != 50 {
		t.Fatalf("page summaries = %d", len(sums2))
	}
}

func TestQueryThreadScope(t *testing.T) {
	c, _ := newEmailTestClient(t)
	ctx := context.Background()

	h, sums, err := c.OpenQuery(ctx, mail.QuerySpec{ThreadID: "t1", Limit: 50})
	if err != nil {
		t.Fatalf("OpenQuery: %v", err)
	}
	// inThread ascends by receivedAt (default sort desc — but with only
	// two members either order must contain both, newest first).
	if ids := h.IDs(); len(ids) != 2 || ids[0] != "e2" || ids[1] != "e1" {
		t.Fatalf("ids = %v, want [e2 e1]", ids)
	}
	if len(sums) != 2 {
		t.Fatalf("summaries = %d", len(sums))
	}
}

func TestFetchBodyPrefersTextAndParsesAttachments(t *testing.T) {
	c, _ := newEmailTestClient(t)
	ctx := context.Background()

	body, err := c.FetchBody(ctx, "e1")
	if err != nil {
		t.Fatalf("FetchBody(e1): %v", err)
	}
	if !strings.HasPrefix(body.Text, "The original body.") || body.HTML != "" {
		t.Fatalf("text preference failed: text %q html %q", body.Text, body.HTML)
	}
	if len(body.Attachments) != 1 || body.Attachments[0].BlobID != "blob-att1" || body.Attachments[0].Name != "notes.txt" {
		t.Fatalf("attachments = %+v", body.Attachments)
	}

	html, err := c.FetchBody(ctx, "e3")
	if err != nil {
		t.Fatalf("FetchBody(e3): %v", err)
	}
	if html.Text != "" || !strings.Contains(html.HTML, "Rich <b>text</b>") {
		t.Fatalf("html-only body: text %q html %q", html.Text, html.HTML)
	}
}

func TestFetchBodyNotFound(t *testing.T) {
	c, _ := newEmailTestClient(t)
	_, err := c.FetchBody(context.Background(), "e-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchSummariesTargeted(t *testing.T) {
	c, _ := newEmailTestClient(t)
	sums, err := c.FetchSummaries(context.Background(), []mail.ID{"e1", "e2"})
	if err != nil {
		t.Fatalf("FetchSummaries: %v", err)
	}
	if len(sums) != 2 || sums[0].ID != "e1" || sums[1].ID != "e2" {
		t.Fatalf("summaries = %+v", sums)
	}
}
