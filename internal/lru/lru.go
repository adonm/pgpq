package lru

import (
	"container/list"
	"sync"
)

type Cache[K comparable, V any] struct {
	mu     sync.Mutex
	budget int64
	used   int64
	size   func(V) int64
	order  *list.List
	items  map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key   K
	value V
	size  int64
}

func New[K comparable, V any](budget int64, size func(V) int64) *Cache[K, V] {
	return &Cache[K, V]{budget: budget, size: size, order: list.New(), items: map[K]*list.Element{}}
}

func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*entry[K, V]).value, true
}

func (c *Cache[K, V]) Add(key K, value V) {
	n := c.size(value)
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > c.budget {
		return
	}
	if _, ok := c.items[key]; ok {
		return
	}
	c.items[key] = c.order.PushFront(&entry[K, V]{key: key, value: value, size: n})
	c.used += n
	for c.used > c.budget {
		oldest := c.order.Back()
		evicted := c.order.Remove(oldest).(*entry[K, V])
		delete(c.items, evicted.key)
		c.used -= evicted.size
	}
}
