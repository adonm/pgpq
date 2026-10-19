package lru

import "testing"

func sized(b []byte) int64 { return int64(len(b)) }

func TestCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	c := New[int64, []byte](10, sized)
	c.Add(1, make([]byte, 4))
	c.Add(2, make([]byte, 4))
	if _, ok := c.Get(1); !ok {
		t.Fatal("1 should be cached")
	}
	c.Add(3, make([]byte, 4))
	if _, ok := c.Get(2); ok {
		t.Fatal("2 was least recently used and should be evicted")
	}
	for _, key := range []int64{1, 3} {
		if _, ok := c.Get(key); !ok {
			t.Fatalf("%d should still be cached", key)
		}
	}
	if c.used != 8 {
		t.Fatalf("used = %d, want 8", c.used)
	}
}

func TestCacheSkipsValuesLargerThanTheBudget(t *testing.T) {
	c := New[int64, []byte](10, sized)
	c.Add(1, make([]byte, 11))
	if _, ok := c.Get(1); ok || c.used != 0 {
		t.Fatal("a value larger than the budget must not be cached")
	}
}
