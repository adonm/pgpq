package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/fgb"
)

var ErrInvalidView = errors.New("store: invalid view")

func memberNames(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("view members: %w", err)
	}
	return names, nil
}

func (s *Store) ViewMemberNames(_ context.Context, info CollectionInfo) ([]string, error) {
	return memberNames(info.Members)
}

func viewMembers(ctx context.Context, conn db, raw []byte) ([]CollectionInfo, error) {
	names, err := memberNames(raw)
	if err != nil {
		return nil, err
	}
	var members []CollectionInfo
	for _, name := range names {
		member, err := collectionInfo(ctx, conn, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, nil
}

func (s *Store) PutView(ctx context.Context, name string, members []string, doc []byte, meta CollectionMeta) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	if len(members) == 0 || len(members) > 64 {
		return fmt.Errorf("%w: needs 1 to 64 members", ErrInvalidView)
	}
	if err := s.checkMembers(ctx, name, members); err != nil {
		return err
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
		_, err = tx.Exec(ctx, `insert into collections (name, kind, members) values ($1, 'view', $2::jsonb)`, name, body)
	case err != nil:
		return err
	case kind != "view":
		return ErrConflict
	default:
		_, err = tx.Exec(ctx, `update collections set members = $2::jsonb where name = $1`, name, body)
	}
	if err != nil {
		return err
	}
	if len(doc) > 0 {
		if err := putMeta(ctx, tx, name, doc, meta); err != nil {
			return err
		}
	}
	info, err := collectionInfo(ctx, tx, name)
	if err != nil {
		return err
	}
	if err := s.buildView(ctx, tx, info); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) checkMembers(ctx context.Context, name string, members []string) error {
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
		if schema != nil && !bytes.Equal(metas[0].Columns, schema) {
			return fmt.Errorf("%w: member %q has a different schema", ErrInvalidView, member)
		}
		schema = metas[0].Columns
	}
	return nil
}

func (s *Store) rebuildMemberViews(ctx context.Context, tx pgx.Tx, member string) error {
	views, err := queryAll(ctx, tx, pgx.RowTo[string],
		`select name from collections where kind = 'view' and members @> jsonb_build_array($1::text)`, member)
	if err != nil {
		return err
	}
	for _, name := range views {
		info, err := collectionInfo(ctx, tx, name)
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
	members, err := viewMembers(ctx, tx, info.Members)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return deleteCollectionRows(ctx, tx, info.Name)
	}
	var (
		leafGroups   [][]fgb.Leaf
		lengths      []int64
		names        []string
		memberHeader []byte
		geometry     flattypes.GeometryType
		schema       []byte
		features     int64
	)
	for _, member := range members {
		names = append(names, member.Name)
		layout, err := virtualLayout(ctx, tx, member.Name, member.Version, 0)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("view %s: member %s has no index yet", info.Name, member.Name)
		}
		if err != nil {
			return err
		}
		var columns []byte
		if err := tx.QueryRow(ctx, `select columns from chunks where collection = $1 and tier = 0 order by id limit 1`, member.Name).Scan(&columns); err != nil {
			return err
		}
		if schema != nil && !bytes.Equal(columns, schema) {
			return fmt.Errorf("%w: members %q and %q of view %q have different schemas", ErrInvalidView, names[0], member.Name, info.Name)
		}
		schema = columns
		header, err := readVirtual(ctx, tx, member.Name, member.Version, 0, 0, 0, int64(layout.HeaderBytes))
		if err != nil {
			return err
		}
		if memberHeader == nil {
			memberHeader, geometry = header, fgb.GeometryOf(header)
		} else if fgb.GeometryOf(header) != geometry {
			geometry = flattypes.GeometryTypeUnknown
		}
		count, err := storedFeatures(ctx, tx, member.Name, layout)
		if err != nil {
			return err
		}
		leaves, err := readLeaves(ctx, tx, member.Name, member.Version, layout, count)
		if err != nil {
			return err
		}
		leafGroups = append(leafGroups, leaves)
		lengths = append(lengths, layout.FeatureBytes)
		features += count
	}
	index, extent, ok := fgb.Index(leafGroups, lengths, fgb.NodeSize)
	if !ok {
		return errors.New("view: empty index")
	}
	header, ok := fgb.Header(info.Name, memberHeader, uint64(features), extent, geometry)
	if !ok {
		return errors.New("view: unreadable member header")
	}
	if _, err := tx.Exec(ctx, `
		update collections set version = version + 1, updated_at = now(), chunk_count = $2, feature_count = $3,
			minx = s.minx, miny = s.miny, maxx = s.maxx, maxy = s.maxy, members = to_jsonb($4::text[])
		from (select min(minx) minx, min(miny) miny, max(maxx) maxx, max(maxy) maxy from collections where name = any($4::text[])) s
		where collections.name = $1`, info.Name, len(lengths), features, names); err != nil {
		return err
	}
	var version int64
	if err := tx.QueryRow(ctx, `select version from collections where name = $1`, info.Name).Scan(&version); err != nil {
		return err
	}
	return writeLayouts(ctx, tx, info.Name, version, map[int]tierLayout{0: {
		header: header, index: index, features: features,
		chunks: int64(len(lengths)), featureBytes: total(lengths),
	}})
}
