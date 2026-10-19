package blocks

import (
	"container/list"
	"sync"
)

type cache struct {
	mu     sync.Mutex
	budget int64
	used   int64
	order  *list.List
	files  map[int64]*list.Element
}

type cachedFile struct {
	id   int64
	data []byte
}

func newCache(budget int64) *cache {
	return &cache{budget: budget, order: list.New(), files: map[int64]*list.Element{}}
}

func (c *cache) get(id int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.files[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(cachedFile).data, true
}

func (c *cache) add(id int64, data []byte) {
	size := int64(len(data))
	c.mu.Lock()
	defer c.mu.Unlock()
	if size > c.budget {
		return
	}
	if _, ok := c.files[id]; ok {
		return
	}
	c.files[id] = c.order.PushFront(cachedFile{id: id, data: data})
	c.used += size
	for c.used > c.budget {
		oldest := c.order.Back()
		evicted := c.order.Remove(oldest).(cachedFile)
		delete(c.files, evicted.id)
		c.used -= int64(len(evicted.data))
	}
}
