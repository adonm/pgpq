package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/fgb"
)

const (
	nodesPerRow  = 4096
	rowsPerPage  = 16
	nodeRowBytes = nodesPerRow * fgb.NodeBytes
)

type nodeRow struct {
	ordinal int64
	data    []byte
}

type nodeSink struct {
	tx    pgx.Tx
	table string
	tier  int
	level int
	ord   int64
	buf   []byte
	rows  []nodeRow
}

func newNodeSink(tx pgx.Tx, table string, tier, level int) *nodeSink {
	return &nodeSink{tx: tx, table: table, tier: tier, level: level, buf: make([]byte, 0, nodeRowBytes)}
}

func (s *nodeSink) write(ctx context.Context, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), nodeRowBytes-len(s.buf))
		s.buf = append(s.buf, p[:n]...)
		p = p[n:]
		if len(s.buf) == nodeRowBytes {
			if err := s.stage(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *nodeSink) stage(ctx context.Context) error {
	if len(s.buf) == 0 {
		return nil
	}
	s.rows = append(s.rows, nodeRow{ordinal: s.ord, data: s.buf})
	s.ord++
	s.buf = make([]byte, 0, nodeRowBytes)
	if len(s.rows) < rowsPerPage {
		return nil
	}
	return s.flush(ctx)
}

func (s *nodeSink) flush(ctx context.Context) error {
	if len(s.rows) == 0 {
		return nil
	}
	ordinals := make([]int64, len(s.rows))
	datas := make([][]byte, len(s.rows))
	for i, row := range s.rows {
		ordinals[i] = row.ordinal
		datas[i] = row.data
	}
	if _, err := s.tx.Exec(ctx, `insert into `+s.table+` (tier, level, ordinal, data)
		select $1, $2, input.ordinal, input.data
		from unnest($3::bigint[], $4::bytea[]) as input(ordinal, data)`, s.tier, s.level, ordinals, datas); err != nil {
		return err
	}
	s.rows = s.rows[:0]
	return nil
}

func (s *nodeSink) close(ctx context.Context) error {
	if err := s.stage(ctx); err != nil {
		return err
	}
	return s.flush(ctx)
}

func eachNodeRow(ctx context.Context, tx pgx.Tx, table string, tier, level int, fn func(ordinal int64, data []byte) error) error {
	for from := int64(0); ; from += rowsPerPage {
		rows, err := tx.Query(ctx, `select ordinal, data from `+table+`
			where tier = $1 and level = $2 and ordinal >= $3 order by ordinal limit $4`, tier, level, from, rowsPerPage)
		if err != nil {
			return err
		}
		page := make([]nodeRow, 0, rowsPerPage)
		for rows.Next() {
			var row nodeRow
			if err := rows.Scan(&row.ordinal, &row.data); err != nil {
				rows.Close()
				return err
			}
			page = append(page, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, row := range page {
			if err := fn(row.ordinal, row.data); err != nil {
				return err
			}
		}
		if len(page) < rowsPerPage {
			return nil
		}
	}
}

func firstNode(ctx context.Context, tx pgx.Tx, table string, tier, level int) ([]byte, error) {
	var data []byte
	err := tx.QueryRow(ctx, `select data from `+table+`
		where tier = $1 and level = $2 order by ordinal limit 1`, tier, level).Scan(&data)
	return data, err
}

type segmentSink struct {
	tx         pgx.Tx
	table      string
	collection string
	version    int64
	tier       int
	kind       int
	ordinal    int
	buf        []byte
}

func (s *segmentSink) write(ctx context.Context, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), int(fgb.SegmentBytes)-len(s.buf))
		s.buf = append(s.buf, p[:n]...)
		p = p[n:]
		if int64(len(s.buf)) == fgb.SegmentBytes {
			if err := s.flush(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *segmentSink) flush(ctx context.Context) error {
	if len(s.buf) == 0 {
		return nil
	}
	if _, err := s.tx.Exec(ctx, `insert into `+s.table+` (collection, version, tier, kind, ordinal, data) values ($1, $2, $3, $4, $5, $6)`,
		s.collection, s.version, s.tier, s.kind, s.ordinal, s.buf); err != nil {
		return err
	}
	s.ordinal++
	s.buf = s.buf[:0]
	return nil
}

func buildTree(ctx context.Context, tx pgx.Tx, nodeTable, fileTable, segmentTable, name string, version int64, tier int, header []byte, geometry flattypes.GeometryType, features uint64, featureBytes int64, chunks int) error {
	counts := fgb.LevelCounts(features, fgb.NodeSize)
	if len(counts) == 0 {
		return nil
	}
	starts := fgb.LevelStarts(counts)
	for level := 0; level < len(counts)-1; level++ {
		sink := newNodeSink(tx, nodeTable, tier, level+1)
		base := starts[level]
		if err := eachNodeRow(ctx, tx, nodeTable, tier, level, func(ordinal int64, data []byte) error {
			return sink.write(ctx, fgb.Parents(data, base+uint64(ordinal)*nodesPerRow, fgb.NodeSize))
		}); err != nil {
			return err
		}
		if err := sink.close(ctx); err != nil {
			return err
		}
	}
	root, err := firstNode(ctx, tx, nodeTable, tier, len(counts)-1)
	if err != nil {
		return err
	}
	blob, ok := fgb.Header(name, header, features, fgb.NodeBBox(root[:fgb.NodeBytes]), geometry)
	if !ok {
		return nil
	}
	indexBytes := int64(0)
	for _, count := range counts {
		indexBytes += int64(count) * fgb.NodeBytes
	}
	head := &segmentSink{tx: tx, table: segmentTable, collection: name, version: version, tier: tier, kind: 0}
	if err := head.write(ctx, blob); err != nil {
		return err
	}
	if err := head.flush(ctx); err != nil {
		return err
	}
	index := &segmentSink{tx: tx, table: segmentTable, collection: name, version: version, tier: tier, kind: 1}
	for level := len(counts) - 1; level >= 0; level-- {
		if err := eachNodeRow(ctx, tx, nodeTable, tier, level, func(_ int64, data []byte) error {
			return index.write(ctx, data)
		}); err != nil {
			return err
		}
	}
	if err := index.flush(ctx); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `insert into `+fileTable+` (collection, version, tier, header_bytes, index_bytes, feature_bytes, chunks, features) values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		name, version, tier, len(blob), indexBytes, featureBytes, chunks, int64(features))
	return err
}
