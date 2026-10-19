package layer

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/spatial"
)

const runMagic = "IDR1"

type runEntry struct {
	hash uint64
	key  uint64
}

type idRun struct {
	hashes []uint64
	keys   []uint64
}

type candidate struct {
	cell uint64
	key  uint64
}

type foundHit struct {
	file int64
	hit  Hit
}

func idHash(id string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(id))
	return h.Sum64()
}

func encodeRun(entries []entry) []byte {
	runs := make([]runEntry, len(entries))
	for i, e := range entries {
		runs[i] = runEntry{hash: idHash(e.id), key: e.key}
	}
	slices.SortFunc(runs, func(a, b runEntry) int {
		if c := cmp.Compare(a.hash, b.hash); c != 0 {
			return c
		}
		return cmp.Compare(a.key, b.key)
	})
	out := make([]byte, 0, len(runMagic)+4+len(runs)*12)
	out = append(out, runMagic...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(runs)))
	var prev uint64
	for _, r := range runs {
		out = binary.AppendUvarint(out, r.hash-prev)
		prev = r.hash
		out = binary.LittleEndian.AppendUint64(out, r.key)
	}
	return out
}

func decodeRun(data []byte) (*idRun, error) {
	if len(data) < 8 || string(data[:4]) != runMagic {
		return nil, errors.New("layer: id run unreadable")
	}
	n := int(binary.LittleEndian.Uint32(data[4:8]))
	rest := data[8:]
	if n > len(rest)/9 {
		return nil, errors.New("layer: id run truncated")
	}
	run := &idRun{hashes: make([]uint64, 0, n), keys: make([]uint64, 0, n)}
	var prev uint64
	for range n {
		delta, used := binary.Uvarint(rest)
		if used <= 0 || len(rest) < used+8 {
			return nil, errors.New("layer: id run truncated")
		}
		prev += delta
		run.hashes = append(run.hashes, prev)
		run.keys = append(run.keys, binary.LittleEndian.Uint64(rest[used:]))
		rest = rest[used+8:]
	}
	return run, nil
}

func (l *Layer) run(ctx context.Context, fileID int64) (*idRun, error) {
	if run, ok := l.runs.Get(fileID); ok {
		return run, nil
	}
	data, err := l.blocks.ReadAll(ctx, fileID)
	if err != nil {
		return nil, err
	}
	run, err := decodeRun(data)
	if err != nil {
		return nil, err
	}
	l.runs.Add(fileID, run)
	return run, nil
}

func runSize(run *idRun) int64 {
	return int64(len(run.hashes)) * 16
}

func (l *Layer) liveRuns(ctx context.Context, name string) ([]int64, error) {
	rows, err := l.pool.Query(ctx, `select file_id from pfgb.id_runs where collection = $1 and end_snapshot is null`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (l *Layer) Lookup(ctx context.Context, name, id string) (Hit, error) {
	coll, err := l.Collection(ctx, name)
	if err != nil {
		return Hit{}, err
	}
	fileIDs, err := l.liveRuns(ctx, name)
	if err != nil {
		return Hit{}, err
	}
	want := idHash(id)
	var probes []candidate
	for _, fileID := range fileIDs {
		run, err := l.run(ctx, fileID)
		if err != nil {
			return Hit{}, err
		}
		for i := sort.Search(len(run.hashes), func(i int) bool { return run.hashes[i] >= want }); i < len(run.hashes) && run.hashes[i] == want; i++ {
			probes = append(probes, candidate{cell: spatial.Partition(run.keys[i]), key: run.keys[i]})
		}
	}
	slices.SortFunc(probes, compareCandidates)
	probes = slices.Compact(probes)
	seen := map[int64]bool{}
	var hits []Hit
	for _, probe := range probes {
		found, err := l.findInChunks(ctx, coll, probe, id)
		if err != nil {
			return Hit{}, err
		}
		for _, f := range found {
			if !seen[f.file] {
				seen[f.file] = true
				hits = append(hits, f.hit)
			}
		}
	}
	switch len(hits) {
	case 0:
		return Hit{}, ErrFeatureNotFound
	case 1:
		return hits[0], nil
	}
	return Hit{}, fmt.Errorf("id %q: %w", id, ErrDuplicate)
}

func compareCandidates(a, b candidate) int {
	if c := cmp.Compare(a.cell, b.cell); c != 0 {
		return c
	}
	return cmp.Compare(a.key, b.key)
}

func (l *Layer) findInChunks(ctx context.Context, coll Collection, probe candidate, id string) ([]foundHit, error) {
	rows, err := l.pool.Query(ctx, `select file_id from pfgb.chunks where collection = $1 and cell = $2 and first_key <= $3 and last_key >= $3 and begin_snapshot <= $4 and (end_snapshot is null or end_snapshot > $4)`,
		coll.Name, spatial.ToOrdered(probe.cell), spatial.ToOrdered(probe.key), coll.Snapshot)
	if err != nil {
		return nil, err
	}
	var fileIDs []int64
	for rows.Next() {
		var fileID int64
		if err := rows.Scan(&fileID); err != nil {
			rows.Close()
			return nil, err
		}
		fileIDs = append(fileIDs, fileID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []foundHit
	for _, fileID := range fileIDs {
		data, err := l.blocks.ReadAll(ctx, fileID)
		if err != nil {
			return nil, err
		}
		c, err := chunk.Open(data)
		if err != nil {
			return nil, err
		}
		features, err := c.SearchAll()
		if err != nil {
			return nil, err
		}
		cols := c.Columns()
		for _, f := range features {
			stored, err := storedID(f, cols, coll.IDColumn)
			if err != nil {
				return nil, err
			}
			if stored != id {
				continue
			}
			bounds, err := featureBounds(c, f)
			if err != nil {
				return nil, err
			}
			out = append(out, foundHit{file: fileID, hit: Hit{Key: spatial.Key(bounds), ID: id, Chunk: c, Feature: f}})
		}
	}
	return out, nil
}
