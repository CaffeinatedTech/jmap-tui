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

type bodyEntry struct {
	id   mail.ID
	text string
	html string
}

// put inserts/refreshes an entry, evicting the least recently used beyond
// capacity.
func (c *bodyCache) put(id mail.ID, text, html string) {
	if el, ok := c.ents[id]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*bodyEntry).text = text
		el.Value.(*bodyEntry).html = html
		return
	}
	el := c.ll.PushFront(&bodyEntry{id: id, text: text, html: html})
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

// get returns the cached body and marks it recently used.
func (c *bodyCache) get(id mail.ID) (text, html string, ok bool) {
	el, ok := c.ents[id]
	if !ok {
		return "", "", false
	}
	c.ll.MoveToFront(el)
	e := el.Value.(*bodyEntry)
	return e.text, e.html, true
}
