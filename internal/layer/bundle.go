package layer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"

	"github.com/adonm/postfgb/internal/blocks"
	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/spatial"
)

const bundleVersion = 1
const maxBundlePartBytes = 4 << 30

const (
	bundleManifestName = "manifest.json"
	bundleFGB          = "cell.fgb"
	bundleFence        = "fence.bin"
	bundleColumns      = "columns.parquet"
	bundleIDs          = "ids.bin"
)

var bundleParts = []string{bundleFGB, bundleFence, bundleColumns, bundleIDs}

var idRowsMagic = [8]byte{'p', 'g', 'i', 'd', 'r', 'o', 'w', '1'}

type bundleFile struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type bundleSample struct {
	Row int    `json:"row"`
	Key uint64 `json:"key"`
	ID  string `json:"id"`
}

type bundleManifest struct {
	Version      int                   `json:"version"`
	Collection   string                `json:"collection"`
	Cell         string                `json:"cell"`
	Level        int                   `json:"level"`
	IDColumn     string                `json:"id_column"`
	Columns      []columnJSON          `json:"columns"`
	Features     int                   `json:"features"`
	FirstKey     uint64                `json:"first_key"`
	FirstID      string                `json:"first_id"`
	LastKey      uint64                `json:"last_key"`
	LastID       string                `json:"last_id"`
	Bounds       [4]float64            `json:"bounds"`
	IndexRoot    [4]float64            `json:"index_root"`
	Zone         string                `json:"zone"`
	FenceEntries int                   `json:"fence_entries"`
	Files        map[string]bundleFile `json:"files"`
	Blocks       []string              `json:"blocks"`
	Samples      []bundleSample        `json:"samples"`
}

type idRow struct {
	Key  uint64
	ID   string
	Hash uint64
}

type cellBundle struct {
	manifest bundleManifest
	columns  []chunk.Column
	fgb      []byte
	fence    []byte
	parquet  []byte
	ids      []byte
}

func BuildBundle(name string, ds ingest.Dataset, cell uint64) ([]byte, error) {
	if ds.IDColumn == "" {
		return nil, invalidf("the collection needs an id column")
	}
	entries, err := indexFeatures(ds, []uint64{cell})
	if err != nil {
		return nil, err
	}
	rows := groupByCell(entries)[cell]
	if len(rows) == 0 {
		return nil, invalidf("cell %s has no features", spatial.Token(cell))
	}
	batch := make([]chunk.InputFeature, len(rows))
	bounds := make([][4]float64, len(rows))
	keys := make([]uint64, len(rows))
	ids := make([]string, len(rows))
	rowsOut := make([]idRow, len(rows))
	for i, e := range rows {
		batch[i] = ds.Features[e.index]
		bounds[i] = e.bounds
		keys[i] = e.key
		ids[i] = e.id
		rowsOut[i] = idRow{Key: e.key, ID: e.id, Hash: e.hash}
	}
	encoded, err := encodeCell(name, ds.Columns, batch, bounds, keys, ids)
	if err != nil {
		return nil, err
	}
	layout, err := fgb.ReadLayout(bytes.NewReader(encoded.fgb), int64(len(encoded.fgb)))
	if err != nil {
		return nil, err
	}
	root, ok := layout.RootBBox()
	if !ok {
		return nil, errors.New("layer: bundle FGB has no index root")
	}
	manifest := bundleManifest{
		Version:      bundleVersion,
		Collection:   name,
		Cell:         spatial.Token(cell),
		Level:        detailLevel,
		IDColumn:     ds.IDColumn,
		Columns:      columnList(ds.Columns),
		Features:     len(rows),
		FirstKey:     rows[0].key,
		FirstID:      rows[0].id,
		LastKey:      rows[len(rows)-1].key,
		LastID:       rows[len(rows)-1].id,
		Bounds:       unionBounds(rows),
		IndexRoot:    root,
		Zone:         encoded.zone,
		FenceEntries: len(encoded.fence),
		Files:        map[string]bundleFile{},
		Blocks:       blockHashes(encoded.fgb),
		Samples:      bundleSamples(rowsOut),
	}
	idData := encodeIDRows(rowsOut)
	parts := map[string][]byte{
		bundleFGB:     encoded.fgb,
		bundleFence:   fgb.EncodeFence(encoded.fence),
		bundleColumns: encoded.columns,
		bundleIDs:     idData,
	}
	for _, part := range bundleParts {
		manifest.Files[part] = bundleFile{Bytes: int64(len(parts[part])), SHA256: hashHex(parts[part])}
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeTarPart(tw, bundleManifestName, manifestData); err != nil {
		return nil, err
	}
	for _, part := range bundleParts {
		if err := writeTarPart(tw, part, parts[part]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (l *Layer) LoadBundle(ctx context.Context, name string, cell uint64, data []byte) (LoadResult, error) {
	bundle, err := parseBundle(name, data)
	if err != nil {
		return LoadResult{}, err
	}
	bundleCellID, err := bundleCell(bundle.manifest)
	if err != nil {
		return LoadResult{}, err
	}
	if bundleCellID != cell {
		return LoadResult{}, invalidf("bundle cell %q does not match the request", bundle.manifest.Cell)
	}
	rows, err := decodeIDRows(bundle.ids)
	if err != nil {
		return LoadResult{}, err
	}
	if err := verifyBundle(bundle, cell, rows); err != nil {
		return LoadResult{}, err
	}
	entries := make([]entry, len(rows))
	for i, row := range rows {
		entries[i] = entry{key: row.Key, id: row.ID, hash: row.Hash, cell: cell, index: i}
	}
	chunks := []chunkRow{{
		cell:   cell,
		level:  detailLevel,
		seq:    0,
		first:  entries[0],
		last:   entries[len(entries)-1],
		bounds: bundle.manifest.Bounds,
		rows:   len(entries),
		zone:   bundle.manifest.Zone,
	}}
	touched := []uint64{cell}
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return LoadResult{}, err
	}
	defer tx.Rollback(ctx)
	put := func(ctx context.Context, collection, kind, encoding string, data []byte) (int64, error) {
		return l.blocks.PutTxEncoded(ctx, tx, collection, kind, encoding, data)
	}
	fileID, err := put(ctx, name, "fgb", blocks.EncodingZstdBlock, bundle.fgb)
	if err != nil {
		return LoadResult{}, err
	}
	colFile, err := put(ctx, name, "cols", "", bundle.parquet)
	if err != nil {
		return LoadResult{}, err
	}
	fenceFile, err := put(ctx, name, "fence", "", bundle.fence)
	if err != nil {
		return LoadResult{}, err
	}
	chunks[0].fileID = fileID
	chunks[0].colFile = colFile
	chunks[0].fenceFile = fenceFile
	if err := tx.Commit(ctx); err != nil {
		return LoadResult{}, err
	}
	return l.commitRows(ctx, name, bundle.columns, bundle.manifest.IDColumn, touched, chunks, entries)
}

func parseBundle(name string, data []byte) (cellBundle, error) {
	var bundle cellBundle
	parts := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return bundle, invalidf("bundle is not a tar archive: %v", err)
		}
		if header.Size < 0 || header.Size > maxBundlePartBytes {
			return bundle, invalidf("bundle part %q has an invalid size", header.Name)
		}
		if _, duplicate := parts[header.Name]; duplicate {
			return bundle, invalidf("bundle part %q appears twice", header.Name)
		}
		part, err := io.ReadAll(io.LimitReader(tr, header.Size+1))
		if err != nil || int64(len(part)) != header.Size {
			return bundle, invalidf("bundle part %q is truncated", header.Name)
		}
		parts[header.Name] = part
	}
	manifestData, ok := parts[bundleManifestName]
	if !ok {
		return bundle, invalidf("bundle has no manifest")
	}
	if err := json.Unmarshal(manifestData, &bundle.manifest); err != nil {
		return bundle, invalidf("bundle manifest: %v", err)
	}
	if bundle.manifest.Collection != name {
		return bundle, invalidf("bundle names collection %q", bundle.manifest.Collection)
	}
	var err error
	if bundle.columns, err = decodeManifestColumns(bundle.manifest.Columns); err != nil {
		return bundle, err
	}
	for _, part := range bundleParts {
		if _, ok := parts[part]; !ok {
			return bundle, invalidf("bundle has no %s", part)
		}
	}
	if len(parts) != len(bundleParts)+1 {
		return bundle, invalidf("bundle has unexpected parts")
	}
	bundle.fgb = parts[bundleFGB]
	bundle.fence = parts[bundleFence]
	bundle.parquet = parts[bundleColumns]
	bundle.ids = parts[bundleIDs]
	return bundle, nil
}

func bundleCell(manifest bundleManifest) (uint64, error) {
	if manifest.Version != bundleVersion {
		return 0, invalidf("bundle version %d is not supported", manifest.Version)
	}
	if manifest.Level != detailLevel {
		return 0, invalidf("a bundle carries one detail cell")
	}
	cells, err := spatial.Cells([]string{manifest.Cell})
	if err != nil || len(cells) != 1 {
		return 0, invalidf("bundle cell %q is not one level-8 cell", manifest.Cell)
	}
	return cells[0], nil
}

func verifyBundle(bundle cellBundle, cell uint64, rows []idRow) error {
	m := bundle.manifest
	if m.Features <= 0 || m.Features != len(rows) {
		return invalidf("bundle manifest counts %d features, id rows hold %d", m.Features, len(rows))
	}
	if m.IDColumn == "" {
		return invalidf("bundle has no id column")
	}
	if !idColumnPresent(bundle.columns, m.IDColumn) {
		return invalidf("bundle id column %q is missing from its columns", m.IDColumn)
	}
	if len(m.Blocks) != (len(bundle.fgb)+blocks.Size-1)/blocks.Size {
		return invalidf("bundle has %d block hashes for %d bytes", len(m.Blocks), len(bundle.fgb))
	}
	for _, part := range bundleParts {
		file, ok := m.Files[part]
		if !ok {
			return invalidf("bundle manifest has no hash for %s", part)
		}
		var data []byte
		switch part {
		case bundleFGB:
			data = bundle.fgb
		case bundleFence:
			data = bundle.fence
		case bundleColumns:
			data = bundle.parquet
		default:
			data = bundle.ids
		}
		if file.Bytes != int64(len(data)) || file.SHA256 != hashHex(data) {
			return invalidf("bundle part %s does not match its manifest", part)
		}
	}
	for i := 0; i*blocks.Size < len(bundle.fgb); i++ {
		end := min((i+1)*blocks.Size, len(bundle.fgb))
		if m.Blocks[i] != hashHex(bundle.fgb[i*blocks.Size:end]) {
			return invalidf("bundle FGB block %d does not match its hash", i)
		}
	}
	layout, err := fgb.ReadLayout(bytes.NewReader(bundle.fgb), int64(len(bundle.fgb)))
	if err != nil {
		return invalidf("bundle FGB: %v", err)
	}
	if layout.Count() != m.Features {
		return invalidf("bundle FGB holds %d features, manifest says %d", layout.Count(), m.Features)
	}
	root, ok := layout.RootBBox()
	if !ok || root != m.IndexRoot || root != m.Bounds {
		return invalidf("bundle FGB index root does not match the manifest")
	}
	cols := chunk.HeaderColumns(layout.Header())
	if !sameColumns(cols, bundle.columns) {
		return invalidf("bundle FGB columns do not match the manifest")
	}
	if !idColumnPresent(bundle.columns, m.IDColumn) {
		return invalidf("bundle FGB has no id column %q", m.IDColumn)
	}
	headerChunk := chunk.HeaderOnly(layout.Header())
	parquetFile, err := parquet.OpenFile(bytes.NewReader(bundle.parquet), int64(len(bundle.parquet)))
	if err != nil {
		return invalidf("bundle columns: %v", err)
	}
	if parquetFile.NumRows() != int64(m.Features) {
		return invalidf("bundle columns hold %d rows, manifest says %d", parquetFile.NumRows(), m.Features)
	}
	if !sameParquetColumns(parquetFile, bundle.columns) {
		return invalidf("bundle column file does not match the manifest")
	}
	if _, err := decodeZone(m.Zone); err != nil {
		return invalidf("bundle zone: %v", err)
	}
	fence, err := fgb.DecodeFence(bundle.fence)
	if err != nil {
		return invalidf("bundle fence: %v", err)
	}
	if fence.Len() != m.FenceEntries || fence.Len() != (m.Features+fgb.FenceEvery-1)/fgb.FenceEvery {
		return invalidf("bundle fence has %d entries", fence.Len())
	}
	if rows[0].Key != m.FirstKey || rows[0].ID != m.FirstID || rows[len(rows)-1].Key != m.LastKey || rows[len(rows)-1].ID != m.LastID {
		return invalidf("bundle id rows do not match the manifest ends")
	}
	seenID := make(map[string]bool, len(rows))
	seenKey := make(map[[2]uint64]bool, len(rows))
	for i, row := range rows {
		if i > 0 && compareKeyID(rows[i-1].Key, rows[i-1].ID, row.Key, row.ID) >= 0 {
			return invalidf("bundle id rows are not in (key, id) order")
		}
		if spatial.Partition(row.Key) != cell {
			return invalidf("bundle feature %q lies outside the cell", row.ID)
		}
		if seenID[row.ID] {
			return invalidf("bundle repeats id %q", row.ID)
		}
		seenID[row.ID] = true
		key := [2]uint64{row.Hash, row.Key}
		if seenKey[key] {
			return invalidf("bundle repeats an id index row")
		}
		seenKey[key] = true
		if row.Hash != idHash(row.ID) {
			return invalidf("bundle id row %d has an unusable hash", i)
		}
	}
	for i, sample := range m.Samples {
		if sample.Row < 0 || sample.Row >= len(rows) || rows[sample.Row].Key != sample.Key || rows[sample.Row].ID != sample.ID {
			return invalidf("bundle sample %d does not match the id rows", i)
		}
		feature, err := layout.Feature(bytes.NewReader(bundle.fgb), fgb.Row{Index: sample.Row, Offset: 0})
		if err != nil {
			return invalidf("bundle sample %d: %v", i, err)
		}
		bounds, err := featureBounds(headerChunk, feature)
		if err != nil {
			return invalidf("bundle sample %d: %v", i, err)
		}
		id, err := storedID(feature, cols, m.IDColumn)
		if err != nil {
			return invalidf("bundle sample %d: %v", i, err)
		}
		if spatial.Key(bounds) != sample.Key || id != sample.ID {
			return invalidf("bundle sample %d does not match the FGB", i)
		}
	}
	for i, entry := range fence.Entries() {
		row := i * fgb.FenceEvery
		if entry.Key != rows[row].Key || entry.ID != rows[row].ID {
			return invalidf("bundle fence entry %d does not match the id rows", i)
		}
		offset, ok := layout.FeatureOffset(row)
		if !ok || entry.Offset != uint64(offset) {
			return invalidf("bundle fence entry %d does not match the FGB", i)
		}
		feature, err := layout.Feature(bytes.NewReader(bundle.fgb), fgb.Row{Index: row})
		if err != nil {
			return invalidf("bundle fence entry %d: %v", i, err)
		}
		bounds, err := featureBounds(headerChunk, feature)
		if err != nil {
			return invalidf("bundle fence entry %d: %v", i, err)
		}
		id, err := storedID(feature, cols, m.IDColumn)
		if err != nil {
			return invalidf("bundle fence entry %d: %v", i, err)
		}
		if spatial.Key(bounds) != entry.Key || id != entry.ID {
			return invalidf("bundle fence entry %d does not match its feature", i)
		}
	}
	return nil
}

func bundleSamples(rows []idRow) []bundleSample {
	if len(rows) == 0 {
		return nil
	}
	var out []bundleSample
	for _, index := range []int{0, len(rows) / 2, len(rows) - 1} {
		if len(out) > 0 && out[len(out)-1].Row == index {
			continue
		}
		out = append(out, bundleSample{Row: index, Key: rows[index].Key, ID: rows[index].ID})
	}
	return out
}

func encodeIDRows(rows []idRow) []byte {
	size := 16
	for _, row := range rows {
		size += 12 + len(row.ID)
	}
	out := make([]byte, 16, size)
	copy(out, idRowsMagic[:])
	binary.LittleEndian.PutUint64(out[8:], uint64(len(rows)))
	for _, row := range rows {
		out = binary.LittleEndian.AppendUint64(out, row.Key)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(row.ID)))
		out = append(out, row.ID...)
	}
	return out
}

func decodeIDRows(data []byte) ([]idRow, error) {
	if len(data) < 16 {
		return nil, invalidf("bundle id rows are truncated")
	}
	for i := range idRowsMagic {
		if data[i] != idRowsMagic[i] {
			return nil, invalidf("bundle id rows have the wrong magic")
		}
	}
	count := binary.LittleEndian.Uint64(data[8:])
	if count > uint64((len(data)-16)/12) {
		return nil, invalidf("bundle id rows are truncated")
	}
	rows := make([]idRow, 0, count)
	at := 16
	for i := uint64(0); i < count; i++ {
		if at+12 > len(data) {
			return nil, invalidf("bundle id rows are truncated")
		}
		key := binary.LittleEndian.Uint64(data[at:])
		length := int(binary.LittleEndian.Uint32(data[at+8:]))
		at += 12
		if length < 0 || at+length > len(data) {
			return nil, invalidf("bundle id row is truncated")
		}
		id := string(data[at : at+length])
		at += length
		rows = append(rows, idRow{Key: key, ID: id, Hash: idHash(id)})
	}
	return rows, nil
}

func columnList(cols []chunk.Column) []columnJSON {
	out := make([]columnJSON, len(cols))
	for i, col := range cols {
		out[i] = columnJSON{Name: col.Name, Type: int(col.Type)}
	}
	return out
}

func decodeManifestColumns(cols []columnJSON) ([]chunk.Column, error) {
	out := make([]chunk.Column, len(cols))
	for i, col := range cols {
		if col.Name == "" {
			return nil, invalidf("bundle column %d has no name", i)
		}
		out[i] = chunk.Column{Name: col.Name, Type: flattypes.ColumnType(col.Type)}
	}
	return out, nil
}

func sameColumns(a, b []chunk.Column) bool {
	return slices.Equal(a, b)
}

func sameParquetColumns(file *parquet.File, cols []chunk.Column) bool {
	fields := file.Schema().Fields()
	if len(fields) != len(cols) {
		return false
	}
	kinds := make(map[string]columnKind, len(cols))
	for _, col := range cols {
		kinds[col.Name] = parquetColumnKind(col.Type)
	}
	for _, field := range fields {
		if kinds[field.Name()] != parquetFieldKind(field.Type()) {
			return false
		}
	}
	return true
}

func parquetFieldKind(t parquet.Type) columnKind {
	if logical := t.LogicalType(); logical != nil {
		if _, ok := logical.Value.(*format.JsonType); ok {
			return columnKindJSON
		}
	}
	switch t.Kind() {
	case parquet.Boolean:
		return columnKindBool
	case parquet.Int64:
		return columnKindInt
	case parquet.Double:
		return columnKindDouble
	}
	return columnKindString
}

func idColumnPresent(cols []chunk.Column, name string) bool {
	for _, col := range cols {
		if col.Name == name {
			return true
		}
	}
	return false
}

func blockHashes(data []byte) []string {
	out := make([]string, 0, (len(data)+blocks.Size-1)/blocks.Size)
	for at := 0; at < len(data); at += blocks.Size {
		out = append(out, hashHex(data[at:min(at+blocks.Size, len(data))]))
	}
	return out
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeTarPart(w *tar.Writer, name string, data []byte) error {
	header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}
	if err := w.WriteHeader(header); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}
