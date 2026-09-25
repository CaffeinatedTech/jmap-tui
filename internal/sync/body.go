package sync

import (
	"container/list"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// bodyCache is the bounded LRU of fetched message bodies (NFR-2: ≤100
// bodies by default). Bodies are the only large objects held past their
// use; everything else is small summaries bounded by the window cap.
type bodyCache struct {
	cap  int
	ll   *list.List                // front = most recently used
	ents map[mail.ID]*list.Element // element value = bodyEntry
}

func newBodyCache(capacity int) *bodyCache {
	return &bodyCache{cap: capacity, ll: list.New(), ents: map[mail.ID]*list.Element{}}
}

// bodyEntry is one cached fetch: the display text the preview renders
// (HTML already converted, FR-E2) plus the whole EmailBody, whose
// addressing and threading headers the composer reuses for replies
// (FR-H2) so opening a reply on a message you just read costs nothing.
type bodyEntry struct {
	id   mail.ID
	text string
	body mail.EmailBody
}

// put inserts/refreshes an entry, evicting the least recently used beyond
// capacity. text is the display text; body carries everything else.
func (c *bodyCache) put(id mail.ID, text string, body mail.EmailBody) {
	if el, ok := c.ents[id]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*bodyEntry).text = text
		el.Value.(*bodyEntry).body = body
		return
	}
	el := c.ll.PushFront(&bodyEntry{id: id, text: text, body: body})
	c.ents[id] = el
	for c.ll.Len() > c.cap {
		back := c.ll.Back()
		if back == nil {
			return
		}
		c.ll.Remove(back)
		delete(c.ents, back.Value.(*bodyEntry).id)
	}
}

// get returns the cached display text and full body, marking it recently
// used.
func (c *bodyCache) get(id mail.ID) (text string, body mail.EmailBody, ok bool) {
	el, ok := c.ents[id]
	if !ok {
		return "", mail.EmailBody{}, false
	}
	c.ll.MoveToFront(el)
	e := el.Value.(*bodyEntry)
	return e.text, e.body, true
}
