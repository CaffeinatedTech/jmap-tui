package jmapclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
)

// TestPostRetriesRateLimit: a 429 is retried, and the retry succeeds
// without surfacing the error — the bridge-throttled sweep's escape.
func TestPostRetriesRateLimit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"methodResponses":[],"sessionState":"x"}`))
	}))
	defer srv.Close()

	c := &Client{session: &jmap.Session{APIURL: srv.URL}, hc: srv.Client()}
	if _, err := c.post(context.Background(), &jmap.Request{}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one 429 then success)", calls)
	}
}

// TestPostGivesUpAfterRetries: a persistent 429 still fails, bounded, and
// reports the status so the caller can decide.
func TestPostGivesUpAfterRetries(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := &Client{session: &jmap.Session{APIURL: srv.URL}, hc: srv.Client()}
	_, err := c.post(context.Background(), &jmap.Request{})
	var se *ServerError
	if !errors.As(err, &se) || se.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want ServerError 429", err)
	}
	if calls != rateLimitRetries+1 {
		t.Fatalf("calls = %d, want %d", calls, rateLimitRetries+1)
	}
}

func TestParseRetryAfter(t *testing.T) {
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"3", 3 * time.Second, true},
		{"0", 0, true},
		{"-1", 0, false},
		{"nonsense", 0, false},
		{future, 0, true}, // an HTTP-date in the future parses
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.in)
		if ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if tc.want > 0 && (got < tc.want-time.Second || got > tc.want+time.Second) {
			t.Errorf("parseRetryAfter(%q) = %v, want ~%v", tc.in, got, tc.want)
		}
	}
}
