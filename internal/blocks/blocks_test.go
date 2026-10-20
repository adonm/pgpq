package blocks

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"os"
	"testing"

	"github.com/adonm/postfgb/internal/store"
)

func openTest(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("POSTFGB_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTFGB_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st, ctx
}

func TestBlocksRoundTrip(t *testing.T) {
	st, ctx := openTest(t)
	s := New(st.Pool, 0)
	for _, size := range []int{0, 1, Size - 1, Size, Size + 1, 250000} {
		data := make([]byte, size)
		rand.New(rand.NewSource(int64(size))).Read(data)
		id, err := s.Put(ctx, "blocks_test", "test", data)
		if err != nil {
			t.Fatalf("put %d bytes: %v", size, err)
		}
		got, err := s.ReadAll(ctx, id)
		if err != nil {
			t.Fatalf("read %d bytes: %v", size, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("%d bytes did not round trip", size)
		}
	}
	if _, err := st.Pool.Exec(ctx, `delete from pfgb.blocks where file_id in (select file_id from pfgb.files where collection = 'blocks_test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `delete from pfgb.files where collection = 'blocks_test'`); err != nil {
		t.Fatal(err)
	}
}

func TestReadAllServesRepeatReadsFromCache(t *testing.T) {
	st, ctx := openTest(t)
	const collection = "blocks_cache_test"
	t.Cleanup(func() {
		if _, err := st.Pool.Exec(context.Background(), `delete from pfgb.blocks where file_id in (select file_id from pfgb.files where collection = $1)`, collection); err != nil {
			t.Error(err)
		}
		if _, err := st.Pool.Exec(context.Background(), `delete from pfgb.files where collection = $1`, collection); err != nil {
			t.Error(err)
		}
	})
	data := make([]byte, 3*Size+7)
	rand.New(rand.NewSource(7)).Read(data)
	cached := New(st.Pool, 1<<20)
	uncached := New(st.Pool, 0)
	id, err := cached.Put(ctx, collection, "test", data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cached.ReadAll(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `delete from pfgb.blocks where file_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := uncached.ReadAll(ctx, id); err == nil {
		t.Fatal("an uncached read of removed blocks should fail")
	}
	got, err := cached.ReadAll(ctx, id)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("repeat read after the blocks were removed: %v", err)
	}
}

func TestReaderAtRanges(t *testing.T) {
	st, ctx := openTest(t)
	const collection = "blocks_reader_at_test"
	t.Cleanup(func() {
		if _, err := st.Pool.Exec(context.Background(), `delete from pfgb.blocks where file_id in (select file_id from pfgb.files where collection = $1)`, collection); err != nil {
			t.Error(err)
		}
		if _, err := st.Pool.Exec(context.Background(), `delete from pfgb.files where collection = $1`, collection); err != nil {
			t.Error(err)
		}
	})
	data := make([]byte, 3*Size+17)
	rand.New(rand.NewSource(19)).Read(data)
	for _, cacheBytes := range []int64{0, 1 << 20} {
		s := New(st.Pool, cacheBytes)
		id, err := s.Put(ctx, collection, "test", data)
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Size() != int64(len(data)) {
			t.Fatalf("size = %d, want %d", r.Size(), len(data))
		}
		for _, tc := range []struct{ off, n int }{{0, 7}, {Size - 3, 11}, {2 * Size, 21}, {len(data) - 5, 10}} {
			got := make([]byte, tc.n)
			n, err := r.ReadAt(got, int64(tc.off))
			wantN := min(tc.n, len(data)-tc.off)
			if err != nil && !(err == io.EOF && wantN < tc.n) {
				t.Fatalf("cache %d read %d+%d: %v", cacheBytes, tc.off, tc.n, err)
			}
			if n != wantN || !bytes.Equal(got[:n], data[tc.off:tc.off+wantN]) {
				t.Fatalf("cache %d read %d+%d: n=%d want %d", cacheBytes, tc.off, tc.n, n, wantN)
			}
		}
	}
}
