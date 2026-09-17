package handlers

import (
	"container/list"
	"strings"
	"sync"
)

// segCache is a byte-bounded LRU of produced HLS segments, keyed by "hash_file_audio_N". It is the
// single source for serving: the ffmpeg producer writes segments to a temp dir, a watcher moves each
// completed segment in here (and deletes the file), and requests are answered from here. This
// decouples serving from the producer's lifecycle — repositioning/killing ffmpeg never removes an
// already-produced segment, so backward/repeat seeks are cache hits and never trigger a restart.
type segCache struct {
	mu       sync.Mutex
	maxBytes int64
	curBytes int64
	ll       *list.List // front = most-recently-used, back = least
	items    map[string]*list.Element
}

type segEntry struct {
	key  string
	data []byte
}

const defaultSegCacheBytes = 512 << 20 // 512 MiB rolling window of segments

// stats reports the segment cache's current bytes, entry count and ceiling (for diagnostics).
func (c *segCache) stats() (bytes int64, count int, max int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes, len(c.items), c.maxBytes
}

func (c *segCache) ensure() {
	if c.ll == nil {
		c.ll = list.New()
		c.items = make(map[string]*list.Element)
		if c.maxBytes == 0 {
			c.maxBytes = defaultSegCacheBytes
		}
	}
}

func (c *segCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensure()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*segEntry).data, true
	}
	return nil, false
}

func (c *segCache) put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensure()
	if e, ok := c.items[key]; ok {
		ent := e.Value.(*segEntry)
		c.curBytes += int64(len(data)) - int64(len(ent.data))
		ent.data = data
		c.ll.MoveToFront(e)
	} else {
		e := c.ll.PushFront(&segEntry{key: key, data: data})
		c.items[key] = e
		c.curBytes += int64(len(data))
	}
	// Evict least-recently-used until under budget (never evict below one entry).
	for c.curBytes > c.maxBytes && c.ll.Len() > 1 {
		back := c.ll.Back()
		if back == nil {
			break
		}
		ent := back.Value.(*segEntry)
		c.curBytes -= int64(len(ent.data))
		c.ll.Remove(back)
		delete(c.items, ent.key)
	}
}

// purgePrefix drops every cached segment whose key starts with prefix (used when a torrent is
// removed). Keys are "hash_file_audio_N", so a "hash_" prefix clears the whole torrent.
func (c *segCache) purgePrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensure()
	for k, e := range c.items {
		if strings.HasPrefix(k, prefix) {
			ent := e.Value.(*segEntry)
			c.curBytes -= int64(len(ent.data))
			c.ll.Remove(e)
			delete(c.items, k)
		}
	}
}
