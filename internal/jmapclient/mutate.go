package jmapclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// registerEmailSetResponse installs the wrapper's flexible Email/set
// response decoder, replacing go-jmap's typed one (whose Updated field only
// accepts the map form of RFC 8620 §5.3 IdSet). Idempotent.
func registerEmailSetResponse() {
	registerEmailSetOnce.Do(func() {
		jmap.RegisterMethod("Email/set", func() jmap.MethodResponse { return &flexibleSetResponse{} })
	})
}

var registerEmailSetOnce sync.Once

// flexibleSetResponse is the wrapper's Email/set response.
type flexibleSetResponse struct {
	Account      string                     `json:"accountId"`
	OldState     string                     `json:"oldState"`
	NewState     string                     `json:"newState"`
	Updated      setIDList                  `json:"updated"`
	Destroyed    []jmap.ID                  `json:"destroyed"`
	Created      map[string]json.RawMessage `json:"created"`
	NotCreated   map[string]*jmap.SetError  `json:"notCreated"`
	NotUpdated   map[string]*jmap.SetError  `json:"notUpdated"`
	NotDestroyed map[string]*jmap.SetError  `json:"notDestroyed"`
}

// setIDList accepts both RFC 8620 §5.3 IdSet wire forms for the updated
// member: an id array (Stalwart) or an object of id → value (go-jmap's
// typed assumption). PLAN §9: extend the wrapper, not the app.
type setIDList []mail.ID

func (l *setIDList) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*l = nil
		return nil
	}
	var arr []jmap.ID
	if err := json.Unmarshal(data, &arr); err == nil {
		*l = convertIDs(arr)
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("jmapclient: Email/set updated: %w", err)
	}
	out := make([]mail.ID, 0, len(obj))
	for id := range obj {
		out = append(out, mail.ID(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	*l = out
	return nil
}

// Mutate implements mail.Provider: one batched Email/set carries every
// update and destroy in the mutation, so a multi-select triage action costs
// a single round-trip (FR-G3, FR-K4). Keyword flips travel as per-keyword
// patches (keywords/$seen: true|null) so concurrent server-side keyword
// changes survive; mailbox membership travels as mailboxIds/<id> patches.
func (c *Client) Mutate(ctx context.Context, m mail.Mutation) (mail.MutationResult, error) {
	registerEmailSetResponse()
	set := &email.Set{}
	if len(m.Emails) > 0 {
		set.Update = make(map[jmap.ID]jmap.Patch, len(m.Emails))
	}
	for id, p := range m.Emails {
		if p.SetKeywords == nil && len(p.AddMailboxes) == 0 && len(p.RemoveMailboxes) == 0 {
			continue
		}
		patch := jmap.Patch{}
		for kw, on := range p.SetKeywords {
			if on {
				patch["keywords/"+kw] = true
			} else {
				patch["keywords/"+kw] = nil // RFC 8620 §5.3: null removes
			}
		}
		for _, mb := range p.AddMailboxes {
			patch["mailboxIds/"+string(mb)] = true
		}
		for _, mb := range p.RemoveMailboxes {
			patch["mailboxIds/"+string(mb)] = false
		}
		set.Update[jmap.ID(id)] = patch
	}
	if len(m.Destroy) > 0 {
		set.Destroy = jmapIDs(m.Destroy)
	}
	if len(set.Update) == 0 && len(set.Destroy) == 0 {
		// A no-op mutation needs neither a connection nor the network.
		return mail.MutationResult{}, nil
	}
	if c.session == nil {
		return mail.MutationResult{}, errors.New("jmapclient: not connected")
	}
	set.Account = jmap.ID(c.accountID)

	inv, err := c.do(ctx, set)
	if err != nil {
		return mail.MutationResult{}, fmt.Errorf("jmapclient: Email/set: %w", err)
	}
	sr, ok := inv.Args.(*flexibleSetResponse)
	if !ok {
		return mail.MutationResult{}, fmt.Errorf("jmapclient: Email/set: unexpected response type %T", inv.Args)
	}
	return convertSetResponse(sr), nil
}

// convertSetResponse maps the wrapper's set response into provider terms.
// Updated ids are sorted: map iteration order must not leak into UI or
// undo bookkeeping.
func convertSetResponse(sr *flexibleSetResponse) mail.MutationResult {
	out := mail.MutationResult{
		OldState:     sr.OldState,
		NewState:     sr.NewState,
		NotUpdated:   make(map[mail.ID]error),
		NotDestroyed: make(map[mail.ID]error),
	}
	out.Updated = append(out.Updated, sr.Updated...)
	sort.Slice(out.Updated, func(i, j int) bool { return out.Updated[i] < out.Updated[j] })
	out.Destroyed = convertIDs(sr.Destroyed)
	sort.Slice(out.Destroyed, func(i, j int) bool { return out.Destroyed[i] < out.Destroyed[j] })
	for id, se := range sr.NotUpdated {
		out.NotUpdated[mail.ID(id)] = setErr(se)
	}
	for id, se := range sr.NotDestroyed {
		out.NotDestroyed[mail.ID(id)] = setErr(se)
	}
	return out
}

// setErr renders a server SetError as an error value.
func setErr(se *jmap.SetError) error {
	if se == nil {
		return errors.New("rejected by server")
	}
	if se.Description != nil && *se.Description != "" {
		return fmt.Errorf("%s: %s", se.Type, *se.Description)
	}
	return errors.New(se.Type)
}
