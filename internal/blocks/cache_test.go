package blocks

import "testing"

func TestCacheEvictsTheLeastRecentlyUsedFile(t *testing.T) {
	c := newCache(10)
	c.add(1, make([]byte, 4))
	c.add(2, make([]byte, 4))
	if _, ok := c.get(1); !ok {
		t.Fatal("file 1 should be cached")
	}
	c.add(3, make([]byte, 4))
	if _, ok := c.get(2); ok {
		t.Fatal("file 2 was least recently used and should be evicted")
	}
	for _, id := range []int64{1, 3} {
		if _, ok := c.get(id); !ok {
			t.Fatalf("file %d should still be cached", id)
		}
	}
	if c.used != 8 {
		t.Fatalf("used = %d, want 8", c.used)
	}
}

func TestCacheSkipsFilesLargerThanTheBudget(t *testing.T) {
	c := newCache(10)
	c.add(1, make([]byte, 11))
	if _, ok := c.get(1); ok || c.used != 0 {
		t.Fatal("a file larger than the budget must not be cached")
	}
}
