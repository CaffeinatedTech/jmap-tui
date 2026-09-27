package jmapclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxJSONResponse bounds every JSON response body (SECURITY_AUDIT_PLAN.md
// D-4: hard-coded 32 MiB, not configurable). Without it a hostile or
// broken server streams an endless body into json decoding and forces
// unbounded allocation (finding F-7).
const maxJSONResponse = 32 << 20

// maxAttachment bounds one attachment download (SECURITY_AUDIT_PLAN.md
// D-4: hard-coded 100 MiB). The read fails once the cap is passed — it
// never truncates silently (finding F-8).
const maxAttachment = 100 << 20

// errTooLarge names the D-4 cap in every size-limit error so callers and
// tests can recognise it (both findings ask for a clean error that says
// which limit tripped).
var errTooLarge = errors.New("response exceeds size limit")

// errAttachmentTooLarge is the cappedReader failure for downloads.
var errAttachmentTooLarge = fmt.Errorf("attachment exceeds %d MiB limit", maxAttachment>>20)

// decodeJSON reads at most maxJSONResponse bytes of r and unmarshals them
// into v. A body past the cap fails with an error naming the limit —
// never a truncated decode (finding F-7). The buffer is bounded by the
// cap itself: the decoder never sees more than 32 MiB + 1 byte.
func decodeJSON(r io.Reader, v any) error {
	data, err := io.ReadAll(io.LimitReader(r, maxJSONResponse+1))
	if err != nil {
		return err
	}
	if len(data) > maxJSONResponse {
		return fmt.Errorf("%w (limit %d MiB)", errTooLarge, maxJSONResponse>>20)
	}
	return json.Unmarshal(data, v)
}

// cappedReader reads at most n bytes from rc and then fails with err —
// the streaming counterpart of decodeJSON for bodies that must not be
// buffered (attachment downloads, finding F-8). A body of exactly n
// bytes still ends in a clean EOF: the limit is checked by probing for
// one byte past the cap, so only bodies over the limit fail. Close
// delegates to the underlying body so the response is released either
// way.
type cappedReader struct {
	rc  io.ReadCloser
	n   int64
	err error
}

func newCappedReader(rc io.ReadCloser, n int64, err error) *cappedReader {
	return &cappedReader{rc: rc, n: n, err: err}
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		var probe [1]byte
		n, err := c.rc.Read(probe[:])
		if n > 0 {
			return 0, c.err
		}
		return 0, err
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.rc.Read(p)
	c.n -= int64(n)
	return n, err
}

func (c *cappedReader) Close() error { return c.rc.Close() }
