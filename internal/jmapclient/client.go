// Package jmapclient is the thin wrapper over go-jmap and the ONLY package
// in the repo allowed to import it (PLAN §1 dependency rule). It implements
// mail.Provider over JMAP: everything above this layer works with
// internal/mail types only. Transport lives here because go-jmap's own
// client cannot take a context for session discovery and reports HTTP
// errors as opaque strings.
package jmapclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	jmapmail "git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// Options configures a Client.
type Options struct {
	// ServerURL is the server base URL; the session resource is discovered
	// at /.well-known/jmap unless SessionURL is set.
	ServerURL string

	// SessionURL is an explicit session endpoint override.
	SessionURL string

	// Username and Password are used for HTTP Basic auth on every endpoint
	// (FR-A2): API, session discovery, upload, download, and EventSource.
	Username string
	Password string

	// Timeout is the per-request HTTP timeout; 30s when zero.
	Timeout time.Duration
}

// Client is the JMAP implementation of mail.Provider.
type Client struct {
	opts      Options
	hc        *http.Client
	session   *jmap.Session
	sessionAt string
	accountID string
}

// Compile-time proof that the wrapper satisfies the provider seam.
var _ mail.Provider = (*Client)(nil)

// New returns a client for the given options. Call Connect before use.
func New(opts Options) *Client {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		opts: opts,
		hc: &http.Client{
			Timeout:   timeout,
			Transport: basicAuthTransport{username: opts.Username, password: opts.Password},
		},
	}
}

// Connect fetches the JMAP session, verifies mail capability, and resolves
// the primary mail account. It is ctx-aware end to end (FR-A3 landings for
// retry/backoff come in M1; this is the plain bounded request).
func (c *Client) Connect(ctx context.Context) error {
	sessionURL := c.opts.SessionURL
	if sessionURL == "" {
		if c.opts.ServerURL == "" {
			return fmt.Errorf("jmapclient: no server URL configured: %w", ErrNoServerURL)
		}
		sessionURL = strings.TrimRight(c.opts.ServerURL, "/") + "/.well-known/jmap"
	}

	s, err := c.fetchSession(ctx, sessionURL)
	if err != nil {
		return err
	}
	c.session = s
	c.sessionAt = sessionURL

	if _, ok := s.RawCapabilities[jmapmail.URI]; !ok {
		return fmt.Errorf("jmapclient: server at %s does not advertise JMAP mail capability (%s)", c.opts.ServerURL, jmapmail.URI)
	}

	id, err := primaryMailAccount(s)
	if err != nil {
		return err
	}
	c.accountID = string(id)
	return nil
}

// SessionInfo is the provider-agnostic view of the session resource. All
// fields are non-secret.
type SessionInfo struct {
	Username           string
	Accounts           []AccountInfo
	PrimaryMailAccount string
	Capabilities       []string
	APIURL             string
	UploadURL          string
	DownloadURL        string
	EventSourceURL     string
	State              string
}

// AccountInfo describes one account on the server.
type AccountInfo struct {
	ID         string
	Name       string
	IsPersonal bool
}

// SessionInfo returns a snapshot of the connected session. Connect must have
// succeeded first.
func (c *Client) SessionInfo() (SessionInfo, error) {
	if c.session == nil {
		return SessionInfo{}, errors.New("jmapclient: not connected")
	}
	s := c.session
	info := SessionInfo{
		Username:       s.Username,
		APIURL:         s.APIURL,
		UploadURL:      s.UploadURL,
		DownloadURL:    s.DownloadURL,
		EventSourceURL: s.EventSourceURL,
		State:          s.State,
	}
	for uri := range s.RawCapabilities {
		info.Capabilities = append(info.Capabilities, string(uri))
	}
	sort.Strings(info.Capabilities)
	for id, a := range s.Accounts {
		info.Accounts = append(info.Accounts, AccountInfo{ID: string(id), Name: a.Name, IsPersonal: a.IsPersonal})
	}
	sort.Slice(info.Accounts, func(i, j int) bool { return info.Accounts[i].ID < info.Accounts[j].ID })
	info.PrimaryMailAccount = c.accountID
	return info, nil
}

// Mailboxes returns the full mailbox list in server sort order (FR-B1).
func (c *Client) Mailboxes(ctx context.Context) ([]mail.Mailbox, error) {
	if c.session == nil {
		return nil, errors.New("jmapclient: not connected")
	}

	query := &mailbox.Query{
		Account:        jmap.ID(c.accountID),
		Sort:           []*mailbox.SortComparator{{Property: "sortOrder", IsAscending: true}},
		CalculateTotal: true,
	}
	inv, err := c.do(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("jmapclient: Mailbox/query: %w", err)
	}
	qr, ok := inv.Args.(*mailbox.QueryResponse)
	if !ok {
		return nil, fmt.Errorf("jmapclient: Mailbox/query: unexpected response type %T", inv.Args)
	}

	get := &mailbox.Get{Account: jmap.ID(c.accountID), IDs: qr.IDs}
	inv, err = c.do(ctx, get)
	if err != nil {
		return nil, fmt.Errorf("jmapclient: Mailbox/get: %w", err)
	}
	gr, ok := inv.Args.(*mailbox.GetResponse)
	if !ok {
		return nil, fmt.Errorf("jmapclient: Mailbox/get: unexpected response type %T", inv.Args)
	}

	byID := make(map[jmap.ID]*mailbox.Mailbox, len(gr.List))
	for _, mb := range gr.List {
		byID[mb.ID] = mb
	}
	out := make([]mail.Mailbox, 0, len(qr.IDs))
	for _, id := range qr.IDs {
		mb, ok := byID[id]
		if !ok {
			continue
		}
		out = append(out, convertMailbox(mb))
	}
	return out, nil
}

// do performs one batched request carrying the single method m and returns
// its response invocation.
func (c *Client) do(ctx context.Context, m jmap.Method) (*jmap.Invocation, error) {
	req := &jmap.Request{Context: ctx}
	callID := req.Invoke(m)

	invs, err := c.runBatch(ctx, req)
	if err != nil {
		return nil, err
	}
	inv, ok := invs[callID]
	if !ok {
		return nil, fmt.Errorf("jmapclient: no response for %s call %q", m.Name(), callID)
	}
	return inv, nil
}

// runBatch posts one JMAP request and indexes its response invocations by
// call id. An "error" invocation fails the whole batch with a
// MethodCallError naming the offending call (FR-K4: batched round-trips).
func (c *Client) runBatch(ctx context.Context, req *jmap.Request) (map[string]*jmap.Invocation, error) {
	resp, err := c.post(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*jmap.Invocation, len(resp.Responses))
	for _, inv := range resp.Responses {
		if inv.Name == "error" {
			me, ok := inv.Args.(*jmap.MethodError)
			if !ok {
				return nil, fmt.Errorf("jmapclient: server returned error invocation with args %T", inv.Args)
			}
			return nil, fmt.Errorf("jmapclient: call %s: %w", inv.CallID, &MethodCallError{Type: me.Type, Description: deref(me.Description)})
		}
		out[inv.CallID] = inv
	}
	return out, nil
}

// post is the JMAP API transport: one HTTP round-trip carrying the request,
// with ctx propagation and typed HTTP error mapping. This is the seam that
// keeps go-jmap's non-ctx/non-typed client out of the call path.
func (c *Client) post(ctx context.Context, req *jmap.Request) (*jmap.Response, error) {
	if c.session == nil {
		return nil, errors.New("jmapclient: not connected")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("jmapclient: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.session.APIURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("jmapclient: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("jmapclient: POST %s: %w", c.session.APIURL, err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
		if httpResp.StatusCode == http.StatusUnauthorized || httpResp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("jmapclient: POST %s: %w", c.session.APIURL, ErrAuth)
		}
		return nil, &ServerError{Status: httpResp.StatusCode, Detail: strings.TrimSpace(string(detail))}
	}

	resp := &jmap.Response{}
	if err := json.NewDecoder(httpResp.Body).Decode(resp); err != nil {
		return nil, fmt.Errorf("jmapclient: decode response: %w", err)
	}
	return resp, nil
}

func (c *Client) fetchSession(ctx context.Context, sessionURL string) (*jmap.Session, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sessionURL, nil)
	if err != nil {
		return nil, fmt.Errorf("jmapclient: build session request: %w", err)
	}
	httpResp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("jmapclient: GET %s: %w", sessionURL, err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode == http.StatusUnauthorized || httpResp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("jmapclient: GET %s: %w", sessionURL, ErrAuth)
	}
	if httpResp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
		return nil, &ServerError{Status: httpResp.StatusCode, Detail: strings.TrimSpace(string(detail))}
	}

	s := &jmap.Session{}
	if err := json.NewDecoder(httpResp.Body).Decode(s); err != nil {
		return nil, fmt.Errorf("jmapclient: decode session from %s: %w", sessionURL, err)
	}
	return s, nil
}

// primaryMailAccount picks the account ids operate on. Multi-account
// selection arrives in M6; until then the primary mail account wins, with a
// single-account fallback for servers that omit primaryAccounts.
func primaryMailAccount(s *jmap.Session) (jmap.ID, error) {
	if id, ok := s.PrimaryAccounts[jmapmail.URI]; ok {
		return id, nil
	}
	if len(s.Accounts) == 1 {
		for id := range s.Accounts {
			return id, nil
		}
	}
	return "", errors.New("jmapclient: server advertises no primary mail account and has multiple accounts; account selection lands in M6")
}

func convertMailbox(mb *mailbox.Mailbox) mail.Mailbox {
	return mail.Mailbox{
		ID:           mail.ID(mb.ID),
		ParentID:     mail.ID(mb.ParentID),
		Name:         mb.Name,
		Role:         mail.Role(mb.Role),
		SortOrder:    int(mb.SortOrder),
		TotalEmails:  int(mb.TotalEmails),
		UnreadEmails: int(mb.UnreadEmails),
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
