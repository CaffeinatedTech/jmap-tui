package jmapclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// Event-source tuning (PLAN §4.2). The ping interval is requested from the
// server via the eventSourceUrl template; the watchdog ends a stream that
// has gone silent past it so the engine's reconnect loop can replace it.
// The generous floor tolerates servers that ignore the ping request: worst
// case such a server sees a reconnect per silence window instead of never
// noticing a dead connection.
const (
	defaultPingSeconds = 30
	watchdogSilence    = 75 * time.Second
	changeBuffer       = 64
)

// Subscribe implements mail.Provider: one EventSource stream attempt per
// call (RFC 8620 §7.3). The returned channel receives StateChange
// notifications converted to mail.Change; it closes when the connection
// dies, stop is called, or ctx is cancelled. A nil channel means the
// session advertises no eventSourceUrl and the caller must poll (FR-B3).
//
// go-jmap's push.EventSource is not used: it appends types/ping/closeafter
// as query parameters instead of expanding the {types}/{closeafter}/{ping}
// path placeholders the RFC puts in the template, takes no context, and has
// no liveness detection (PLAN §9: extend the wrapper, not the app).
func (c *Client) Subscribe(ctx context.Context) (<-chan mail.Change, func() error) {
	if c.session == nil || c.session.EventSourceURL == "" {
		return nil, func() error { return nil }
	}

	streamCtx, stop := context.WithCancel(ctx)
	ch := make(chan mail.Change, changeBuffer)
	es := &eventStream{
		url:    expandEventSourceURL(c.session.EventSourceURL, defaultPingSeconds),
		client: c.streamHC,
		logger: c.opts.Logger,
	}
	go es.listen(streamCtx, ch)

	return ch, func() error {
		stop()
		return nil
	}
}

// eventStream owns one SSE connection: it parses the text/event-stream body
// and delivers state changes until the stream ends for any reason.
type eventStream struct {
	url    string
	client *http.Client
	logger *slog.Logger
}

// listen connects, parses, and closes; it is the whole lifecycle of one
// stream attempt. ch is closed on return in every path.
func (es *eventStream) listen(ctx context.Context, ch chan<- mail.Change) {
	defer close(ch)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, es.url, nil)
	if err != nil {
		es.log("eventsource: build request", "error", err)
		return
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := es.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			es.log("eventsource: connect failed", "error", err)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		es.log("eventsource: connect refused", "status", resp.StatusCode)
		return
	}

	// Watchdog: any line (state event or ping comment) proves liveness;
	// silence past the deadline closes the body, which unblocks the scanner
	// and ends the stream like any other failure.
	watchdog := time.AfterFunc(watchdogSilence, func() { _ = resp.Body.Close() })
	defer watchdog.Stop()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event string
	for scanner.Scan() {
		watchdog.Reset(watchdogSilence)
		line := scanner.Text()
		switch {
		case line == "" || strings.HasPrefix(line, ":"):
			// keepalive comment or event separator
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if event == "state" {
				es.deliverState(ctx, data, ch)
			}
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		es.log("eventsource: stream ended", "error", err)
	}
}

// deliverState decodes one `data:` payload of a `state` event and forwards
// it. Malformed payloads are dropped (logged): the stream stays healthy and
// the next event or poll tick reconciles eventually.
func (es *eventStream) deliverState(ctx context.Context, data string, ch chan<- mail.Change) {
	var sc jmap.StateChange
	if err := json.Unmarshal([]byte(data), &sc); err != nil {
		es.log("eventsource: bad state payload", "error", err)
		return
	}
	changed := make(map[mail.ID]map[string]string, len(sc.Changed))
	for acc, states := range sc.Changed {
		byType := make(map[string]string, len(states))
		for typ, state := range states {
			byType[typ] = state
		}
		changed[mail.ID(acc)] = byType
	}
	select {
	case ch <- mail.Change{Changed: changed}:
	case <-ctx.Done():
	}
}

// expandEventSourceURL fills the RFC 8620 §7.3 template placeholders. Types
// is "*": the client reconciles per type from the StateChange payloads, and
// a single stream per account is the rate-courtesy rule (FR-K4). Templates
// without placeholders fall back to query parameters, matching servers that
// advertise that shape.
func expandEventSourceURL(tmpl string, pingSeconds int) string {
	ping := fmt.Sprintf("%d", pingSeconds)
	expanded := strings.NewReplacer(
		"{types}", "*", "%7Btypes%7D", "*",
		"{closeafter}", "no", "%7Bcloseafter%7D", "no",
		"{ping}", ping, "%7Bping%7D", ping,
	).Replace(tmpl)
	if expanded != tmpl {
		return expanded
	}
	sep := "?"
	if strings.Contains(tmpl, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%stypes=*&closeafter=no&ping=%s", tmpl, sep, ping)
}

func (es *eventStream) log(msg string, args ...any) {
	if es.logger != nil {
		es.logger.Info(msg, append(args, "url", redactURL(es.url))...)
	}
}

// redactURL trims the query string; the EventSource URL carries no
// credentials by design, but the log line is built defensive anyway.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i] + "?…"
	}
	return u
}
