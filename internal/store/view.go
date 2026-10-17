package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/fgb"
)

type ViewMember struct {
	Name    string
	Version int64
	MinX    *float64
	MinY    *float64
	MaxX    *float64
	MaxY    *float64
}

func parseViewMembers(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("view members: %w", err)
	}
	return names, nil
}

func (s *Store) viewMembers(ctx context.Context, conn db, info CollectionInfo) ([]ViewMember, error) {
	names, err := parseViewMembers(info.Members)
	if err != nil {
		return nil, err
	}
	members := make([]ViewMember, 0, len(names))
	for _, name := range names {
		var member ViewMember
		member.Name = name
		err := conn.QueryRow(ctx,
			`select version, minx, miny, maxx, maxy from collections where name = $1`, name).
			Scan(&member.Version, &member.MinX, &member.MinY, &member.MaxX, &member.MaxY)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, nil
}

var ErrInvalidView = errors.New("store: invalid view")

func (s *Store) PutView(ctx context.Context, name string, members []string, doc []byte, meta CollectionMeta) error {
	if !collectionName.MatchString(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	if len(members) == 0 || len(members) > 64 {
		return fmt.Errorf("%w: needs 1 to 64 members", ErrInvalidView)
	}
	seen := map[string]bool{}
	var schema []byte
	for _, member := range members {
		if member == "" || member == name || seen[member] {
			return fmt.Errorf("%w: duplicate or empty member %q", ErrInvalidView, member)
		}
		seen[member] = true
		info, err := s.Collection(ctx, member)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: unknown member %q", ErrInvalidView, member)
		}
		if err != nil {
			return err
		}
		if info.Kind != "native" {
			return fmt.Errorf("%w: member %q is not a native collection", ErrInvalidView, member)
		}
		if _, err := s.VirtualLayout(ctx, member, info.Version, 0); err != nil {
			return fmt.Errorf("%w: member %q has no index", ErrInvalidView, member)
		}
		metas, err := s.ChunkMetas(ctx, member, nil, 1, 0, 0)
		if err != nil {
			return err
		}
		if len(metas) == 0 {
			return fmt.Errorf("%w: member %q is empty", ErrInvalidView, member)
		}
		if schema == nil {
			schema = metas[0].Columns
			continue
		}
		if string(metas[0].Columns) != string(schema) {
			return fmt.Errorf("%w: member %q has a different schema", ErrInvalidView, member)
		}
	}
	body, err := json.Marshal(members)
	if err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var kind string
	err = tx.QueryRow(ctx, `select kind from collections where name = $1 for update`, name).Scan(&kind)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx,
			`insert into collections (name, kind, members) values ($1, 'view', $2::jsonb)`, name, body); err != nil {
			return err
		}
	case err != nil:
		return err
	case kind != "view":
		return ErrConflict
	default:
		if _, err := tx.Exec(ctx,
			`update collections set members = $2::jsonb where name = $1`, name, body); err != nil {
			return err
		}
	}
	if len(doc) > 0 {
		if err := putMeta(ctx, tx, name, doc, meta); err != nil {
			return err
		}
	}
	info, err := s.collection(ctx, tx, name)
	if err != nil {
		return err
	}
	if err := s.buildView(ctx, tx, info); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) collection(ctx context.Context, conn db, name string) (CollectionInfo, error) {
	var info CollectionInfo
	err := conn.QueryRow(ctx, collectionColumns+` where name = $1`, name).
		Scan(&info.Name, &info.Kind, &info.Version, &info.ChunkCount, &info.FeatureCount,
			&info.MinX, &info.MinY, &info.MaxX, &info.MaxY, &info.TimeMin, &info.TimeMax, &info.IDColumn,
			&info.TimeColumn, &info.Sortable, &info.Title, &info.Description, &info.License, &info.Keywords, &info.Stac,
			&info.Members)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, ErrNotFound
	}
	return info, err
}

func (s *Store) ViewMemberNames(ctx context.Context, info CollectionInfo) ([]string, error) {
	return parseViewMembers(info.Members)
}

func (s *Store) rebuildMemberViews(ctx context.Context, tx pgx.Tx, member string) error {
	rows, err := tx.Query(ctx, `select name from collections where kind = 'view' and members @> $1::jsonb`,
		`["`+member+`"]`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range names {
		info, err := s.collection(ctx, tx, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if err := s.buildView(ctx, tx, info); err != nil {
			return err
		}
		slog.Info("view rebuilt", "view", name, "member", member)
	}
	return nil
}

func (s *Store) buildView(ctx context.Context, tx pgx.Tx, info CollectionInfo) error {
	members, err := s.viewMembers(ctx, tx, info)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		_, err := tx.Exec(ctx, `delete from collections where name = $1`, info.Name)
		return err
	}
	var (
		leafGroups             [][]fgb.Leaf
		lengths                []int64
		headerBlob             []byte
		features               int64
		minX, minY, maxX, maxY float64
		spatial                bool
	)
	for _, member := range members {
		layout, err := layoutWithin(ctx, tx, member.Name, member.Version, 0)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("view %s: member %s has no index yet", info.Name, member.Name)
		}
		if err != nil {
			return err
		}
		if headerBlob == nil {
			headerBlob, err = readVirtual(ctx, tx, member.Name, member.Version, 0, 0, 0, int64(layout.HeaderBytes))
			if err != nil {
				return err
			}
		}
		memberFeatures, err := storedFeatures(ctx, tx, member.Name, layout)
		if err != nil {
			return err
		}
		leaves, err := readLeaves(ctx, tx, member.Name, member.Version, layout, memberFeatures)
		if err != nil {
			return err
		}
		leafGroups = append(leafGroups, leaves)
		lengths = append(lengths, layout.FeatureBytes)
		features += memberFeatures
		if member.MinX != nil && member.MinY != nil && member.MaxX != nil && member.MaxY != nil {
			if !spatial {
				minX, minY, maxX, maxY = *member.MinX, *member.MinY, *member.MaxX, *member.MaxY
				spatial = true
			} else {
				minX, maxX = min(minX, *member.MinX), max(maxX, *member.MaxX)
				minY, maxY = min(minY, *member.MinY), max(maxY, *member.MaxY)
			}
		}
	}
	indexBytes, extent, ok := fgb.Index(leafGroups, lengths, fgb.NodeSize)
	if !ok {
		return errors.New("view: empty index")
	}
	if headerBlob == nil {
		return errors.New("view: no member header")
	}
	header, ok := fgb.Header(info.Name, headerBlob, uint64(features), extent)
	if !ok {
		return errors.New("view: unreadable member header")
	}
	var featureBytes int64
	for _, length := range lengths {
		featureBytes += length
	}
	if _, err := tx.Exec(ctx, `update collections set version = version + 1, updated_at = now(),
			chunk_count = $2, feature_count = $3, minx = $4, miny = $5, maxx = $6, maxy = $7
			where name = $1`,
		info.Name, len(lengths), features, nullableFloat(spatial, minX), nullableFloat(spatial, minY),
		nullableFloat(spatial, maxX), nullableFloat(spatial, maxY)); err != nil {
		return err
	}
	var version int64
	if err := tx.QueryRow(ctx, `select version from collections where name = $1`, info.Name).Scan(&version); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from virtual_file where collection = $1 and version <> $2`, info.Name, version); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from virtual_segment where collection = $1 and version <> $2`, info.Name, version); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into virtual_file (collection, version, tier, header_bytes, index_bytes, feature_bytes, chunks, features)
		values ($1, $2, 0, $3, $4, $5, $6, $7)
		on conflict (collection, version, tier) do update set
			header_bytes = excluded.header_bytes, index_bytes = excluded.index_bytes,
			feature_bytes = excluded.feature_bytes, chunks = excluded.chunks, features = excluded.features`,
		info.Name, version, len(header), len(indexBytes), featureBytes, len(lengths), features); err != nil {
		return err
	}
	for kind, data := range map[int][]byte{0: header, 1: indexBytes} {
		for ordinal, at := 0, 0; at < len(data); ordinal++ {
			end := at + int(fgb.SegmentBytes)
			if end > len(data) {
				end = len(data)
			}
			if _, err := tx.Exec(ctx, `
				insert into virtual_segment (collection, version, tier, kind, ordinal, data)
				values ($1, $2, 0, $3, $4, $5)
				on conflict (collection, version, tier, kind, ordinal) do update set data = excluded.data`,
				info.Name, version, kind, ordinal, data[at:end]); err != nil {
				return err
			}
			at = end
		}
	}
	return nil
}

func layoutWithin(ctx context.Context, conn db, collection string, version int64, tier int) (VirtualLayout, error) {
	var layout VirtualLayout
	err := conn.QueryRow(ctx, `
		select header_bytes, index_bytes, feature_bytes, chunks, features from virtual_file
		where collection = $1 and version = $2 and tier = $3`, collection, version, tier).
		Scan(&layout.HeaderBytes, &layout.IndexBytes, &layout.FeatureBytes, &layout.Chunks, &layout.Features)
	if errors.Is(err, pgx.ErrNoRows) {
		return layout, ErrNotFound
	}
	return layout, err
}

func readVirtual(ctx context.Context, conn db, collection string, version int64, tier, kind int, from, to int64) ([]byte, error) {
	first := int(from / fgb.SegmentBytes)
	last := int((to - 1) / fgb.SegmentBytes)
	rows, err := conn.Query(ctx, `
		select ordinal, data from virtual_segment
		where collection = $1 and version = $2 and tier = $3 and kind = $4 and ordinal between $5 and $6
		order by ordinal`, collection, version, tier, kind, first, last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]byte, 0, to-from)
	for rows.Next() {
		var ordinal int
		var data []byte
		if err := rows.Scan(&ordinal, &data); err != nil {
			return nil, err
		}
		start := int64(ordinal) * fgb.SegmentBytes
		lo := max(from-start, 0)
		hi := min(to-start, int64(len(data)))
		if lo >= hi {
			continue
		}
		out = append(out, data[lo:hi]...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if int64(len(out)) != to-from {
		return nil, errors.New("virtual: segment gap")
	}
	return out, nil
}

func storedFeatures(ctx context.Context, conn db, collection string, layout VirtualLayout) (int64, error) {
	if layout.Features > 0 {
		return layout.Features, nil
	}
	var count int64
	err := conn.QueryRow(ctx,
		`select coalesce(sum(feature_count), 0) from chunks where collection = $1 and tier = 0`, collection).Scan(&count)
	return count, err
}

func readLeaves(ctx context.Context, conn db, collection string, version int64, layout VirtualLayout, count int64) ([]fgb.Leaf, error) {
	index, err := readVirtual(ctx, conn, collection, version, 0, 1, 0, layout.IndexBytes)
	if err != nil {
		return nil, err
	}
	leaves, ok := fgb.Leaves(index, uint64(count), fgb.NodeSize)
	if !ok {
		return nil, errors.New("virtual: unreadable leaves")
	}
	if int64(len(leaves)) != count {
		return nil, fmt.Errorf("virtual: read %d leaves of %d", len(leaves), count)
	}
	return leaves, nil
}

func nullableFloat(set bool, value float64) any {
	if !set {
		return nil
	}
	return value
}

func (s *Store) ViewRuns(ctx context.Context, info CollectionInfo) ([]VirtualRun, error) {
	members, err := s.viewMembers(ctx, s.Pool, info)
	if err != nil {
		return nil, err
	}
	var runs []VirtualRun
	var base int64
	for _, member := range members {
		layout, err := s.VirtualLayout(ctx, member.Name, member.Version, 0)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rows, err := s.Pool.Query(ctx, `
			select id, feature_bytes, coalesce(sum(feature_bytes) over (order by id rows between unbounded preceding and 1 preceding), 0)
			from chunks where collection = $1 and tier = 0 order by id`, member.Name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			run := VirtualRun{Collection: member.Name, Base: base}
			if err := rows.Scan(&run.ChunkID, &run.Bytes, &run.Base); err != nil {
				rows.Close()
				return nil, err
			}
			run.Base += base
			runs = append(runs, run)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		base += layout.FeatureBytes
	}
	return runs, nil
}
