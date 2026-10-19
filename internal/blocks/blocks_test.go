package blocks

import (
	"bytes"
	"context"
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
	s := New(st.Pool)
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
