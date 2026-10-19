# postfgb design — next (draft)

Status: proposal from the October 2026 design discussion; not implemented. When built, it replaces the storage, ingest and read-path contract in [DESIGN.md](DESIGN.md), and the two documents are reconciled in the same commit as the code.

## Implementation status

Build step 1 is in progress under `/v2`, alongside the current contract. DESIGN.md is unchanged until cutover.

- **Built:** S2 keys and level-8 partitions (`internal/spatial`); pgvfs-layout blocks in schema `pfgb` (`internal/blocks`, migration 0031); key-ordered chunks with catalog and snapshots; whole-collection and partition-scoped replace loads with 422 validation and per-collection commit serialization (`internal/layer`); snapshot-pinned bbox paging with 72 h cursor expiry; GeoJSON and FlatGeobuf responses on `PUT /v2/collections/{id}` and `GET /v2/collections/{id}/items`.
- **Not yet:** column files and zone maps (step 2); id runs and cross-partition uniqueness (step 3, so a duplicate id in another partition is not detected yet); postings and overviews (steps 4 and 5); conditional `base=` loads, reaping and expired-snapshot cleanup (step 6); spill-to-disk for loads larger than memory (requests are held in memory); a worker block cache (every read fetches its blocks); an R-tree (bbox pruning is a range query on chunk envelopes).
- **Measured on `warm_100k`** (100k features spread thinly across ~7,600 level-8 cells):

  | | chunks | logical bytes | load | bbox (-5,40,10,50) |
  | --- | --- | --- | --- | --- |
  | v1 | 19 | 8.9 MB | 3.4 s | 15,047 features, 0.46 s |
  | v2 | 7,614 (13 features each) | 15 MB | 5.9 s | 15,047 features, 1.0 s |

  GDAL `ogrinfo` reads the v2 FlatGeobuf output (100,000 features, same extent as v1).
- **Finding:** a fixed level-8 partition costs one chunk per populated cell, and each chunk carries about 0.8 KB of FlatGeobuf header and index. Dense partitions are unaffected. Sparse data pays heavily: here 1.7× the storage and 400× the chunk rows. The fix to decide is to pack adjacent sparse partitions into shared chunks up to the chunk target. A replace then reads back at most one chunk's worth of neighbouring rows, and dense partitions still load alone.

## Goal

OGC API Features and FlatGeobuf over PostgreSQL, for billions of features at national and global scale, loaded by area of interest (AOI).

- PostgreSQL stores immutable files plus a small catalog.
- Go workers hold the catalog in memory and do every per-feature step.
- No cgo, no PostgreSQL extensions, and no dependencies outside Go.

## Four artifacts

|                  | Row (FlatGeobuf) | Column |
| ---------------- | ---------------- | ------ |
| Detail           | FGB chunks within each partition, each with its own packed R-tree | one column file per chunk, in the same row order |
| Overview / index | overview files: per partition for levels 10 and 8, per cell for levels 6, 4 and 2; an in-memory R-tree over chunk envelopes | zone maps per chunk and column; one postings segment per partition; id runs |

## Partitions

- **Feature key:** the level-30 S2 CellID of the feature's bbox center (`golang/geo/s2`).
- **Partition:** the feature's level-8 S2 cell, `key.Parent(8)`, fixed for every collection. Level 8 is chosen so that a loader reingesting the densest partition reads and sends about 1 GB.
- **The partition is the one unit** of load, replace, lock, conditional check, chunking, postings and fine overviews. Nothing crosses a partition boundary.
- **Geometry (exact):**
  - 393,216 partitions globally, about 115k of them touching land.
  - Area 786–1,632 km² (average 1,297 km², a ratio of 2.08); edges 23.6–42.4 km.
  - The extreme-area cells are empty: the smallest is centred near the Xinjiang–Mongolia border, the largest in the South Atlantic. Dense cities fall in cells of average size or larger: Dhaka 1,298 km², Cairo 1,309 km², Lagos 1,349 km², Jakarta 1,519 km² and Paris 940 km².

### Partition sizes

These are buildings, at about 470 B of upload FGB, 100 B of stored detail and 20 B of column files each (measured on `buildings_fr`). Counts are order-of-magnitude estimates from Google Open Buildings densities.

| Partition | Features | Upload | Stored detail | Column files | Chunks | Postings |
| --- | --- | --- | --- | --- | --- | --- |
| Densest (Dhaka, Jakarta or Cairo cells) | ~2.5M (1.5–4M) | ~1.2 GB | ~250 MB | ~50 MB | ~500 | 0.05–2.5 MB |
| Mean, 1e9 features over ~80k populated partitions (the median is far lower) | ~12k | ~6 MB | ~1.2 MB | ~0.25 MB | ~3 | none |
| Smallest populated | 1 | ~0.5 KB | ~0.1 KB | negligible | 1 | none |

Empty partitions have no rows at all.

## Chunks

- Within a partition, features are sorted by `(key, id)` and cut into chunks of about 2 MiB, at most 50,000 features each. Each partition's last chunk is partial: at most one per populated partition, so up to about 90k extra chunks over the ~200k full ones at 1e9. They are small, so they cost catalog rows and files, not bytes.
- **A chunk** is an immutable `(chunk_id, partition, seq, key range, envelope, rows)`. Reloading a partition produces new chunks.
- **Row order:** row `i` of a chunk's column file is feature `i` of its FGB. The encoder writes features in `(key, id)` order and must not reorder them.
- **Envelopes:** they stay the spatial correctness key. A feature is keyed by its center, but its chunk's envelope covers its full extent.

## Identity

- Every feature carries an id that is present and unique within its collection. The loader enforces this and the server verifies it. Ids are the only stable external reference; `(chunk, ord)` addresses are internal to one snapshot.
- **Id index:** append-only sorted runs mapping `hash(id)` to the feature's key. Each load writes one run, with a bloom filter, and runs are merged in the background.
- **Lookups:** a lookup resolves the key to its partition and chunk, and verifies the id against that chunk's id column. Stale entries (features since removed or moved) fail verification and are dropped by merges.
- **Size:** about 8–12 B per feature, held in worker memory.

## Write path

### The load request

- **Request:** the loader sends a FlatGeobuf body plus the partitions it replaces, as level-8 cells or coarser cells expanded to level 8. Route name to be decided.
- **Replace semantics:** the body is the complete new contents of the listed partitions. Their old chunks are retired without being read.
- **Validation:** a feature outside the listed partitions answers 422, and so do missing or duplicate ids within the body. An empty body empties the listed partitions.
- **Conditional loads:** `base=<snapshot>` answers 409 if any listed partition changed after that snapshot, so a loader's read-modify-write cannot overwrite a concurrent change.
- **Whole-collection replace:** `PUT /collections/{id}` with a FlatGeobuf body replaces every partition.
- **No per-feature writes.** Deletes and corrections are reloads of the partitions involved.
- **Request size:** the densest partition is the largest single unit, about 2.5M features and 1.2 GB of FGB upload. At the measured ingest rate (27–32 s per 1M features on 6 cores) it builds in about 1–1.5 minutes, and it needs the spilling sort in step 1.

### Steps

1. Stream the body once (`ParseFlat`, `DecodePropertyText`), key every feature and bucket it by partition, spilling to temporary files beyond a memory budget.
2. For each partition, in parallel:
   - sort by `(key, id)` and cut chunks;
   - encode each chunk's FGB, column file and zone maps;
   - build the partition's postings segment and its level-10 and level-8 overview files.
3. Write every file as blocks, each in its own transaction. Files stay unpublished until the commit.
4. Check the new ids against the id index. A hit in an unlisted partition that verifies is a conflict: 409, naming the id.
5. Commit in one short transaction:
   - lock the collection's catalog row, so commits serialize per collection and snapshot numbers follow commit order; check `base` against the listed partitions' `changed` snapshots;
   - create the new snapshot, end-snapshot the partitions' old chunk, postings and overview rows, and insert the new ones;
   - append the id run, and enqueue the partitions' level-6 ancestors.

Cost is proportional to the loaded partitions, never to the collection. Blocks for concurrent loads are written in parallel. Commits serialize per collection, which is what keeps a snapshot's contents fixed once it is visible: a reader pinned to snapshot `s` must never see a snapshot `s' < s` commit after it. The commit transaction is milliseconds, because all bulk work happens before it.

## Postings

- **One segment per partition** with at least 8 chunks, built during the load from the column values in hand, and so always current. Smaller partitions are scanned.
- **Indexed columns:** low- and medium-cardinality columns and list columns. A column whose distinct count in the partition exceeds a budget is not indexed; zone maps serve it, and the id index serves ids.
- **Content:** each value maps to the ordinals of the partition's chunks that hold it, as a sorted list or a bitset, whichever is smaller. At most about 63 B per value at 500 chunks.
- **Busy values:** a value held by more than 20 % of the partition's chunks is marked busy and gets no list; a query scans those chunks instead.
- **Sizes:** the densest segment is about 0.05–2.5 MB, mostly medium-cardinality columns. Most partitions have fewer than 8 chunks and no segment.

## Overview levels

Each overview level is a sampled, simplified copy of the collection in FlatGeobuf, without column files. Proposed levels are 10, 8, 6, 4 and 2, with `cap` = 256; detail serves zooms finer than level 10.

- **Sampling:** level `L` keeps, in every level-`L` S2 cell, the `cap` features with the smallest `hash(id)`, simplified with that level's Douglas–Peucker tolerance.
  - Sparse cells keep all their features and dense ones keep `cap`, so every level covers the extent evenly.
  - A feature kept at a coarse level is kept at every finer one, so each level is built from the next finer level, never from a rescan.
- **Levels 10 and 8:** level 10 is finer than a partition and level 8 is the partition itself. Each has one file per partition, built at load and atomic with detail.
- **Levels 6, 4 and 2** are coarser: one file per level-`L` cell, at most `cap` features each, rebuilt in the background from the dirty-cell queue. They may lag from minutes to an hour while loading, and a stale file only shows older data.

## Read path

- **Worker catalog:** for each collection, each worker holds the current snapshot's catalog — partitions, chunks (with key ranges, envelopes, counts, files and zone maps; about 300k at 1e9), postings, overview files and id runs. It loads the catalog once, then applies deltas by snapshot.
- **bbox:** an in-memory R-tree over chunk envelopes (`tidwall/rtree`) selects chunk FGBs, and each chunk's own R-tree tests the features.
- **Attributes:**
  1. Zone maps prune chunks.
  2. Postings give exact chunk sets for rare values.
  3. Only the predicate's column sections are read, and dictionary-coded equality and `IN` compare integer codes.

  The result is the exact `(chunk, ord)` set, and only those features' FGB bytes are fetched. `numberMatched` is exact and never decodes geometry. Filters always read detail.
- **By id:** id runs give the key; the key gives the partition and chunk; verification finds the feature.
- **Overview:** the requested zoom maps to an S2 level, and that level's files cover the view (detail when finer than level 10). The response is one streamed FGB (or GeoJSON). There is no single-file `items.fgb`.
- **Paging:** a cursor carries its snapshot and the `(key, id)` of the last feature it returned. A pagination must complete within the 72 h snapshot retention; an older cursor answers 410.
- **CQL2:** evaluation is unchanged (`internal/cql`), applied to column values.

## Storage in PostgreSQL

### Blocks

- Blocks live in our own schema, using the pgvfs layout but written by our own Go code. Every artifact is an immutable file stored as 8,120-byte rows keyed `(file_id, n)`, one per 8 KB heap page, inline and without TOAST.
- A file is written with one binary `COPY` and read by primary-key range, at most 8 MB per read.
- Workers cache file bytes by `file_id` indefinitely.
- Autovacuum on the block table runs at 1 % dead rows. WAL is about 1× the bytes written.

### Catalog

The catalog is small and mutable, and lives in the same database:

| Table | Columns |
| --- | --- |
| `collections` | name, uid, current snapshot, schema |
| `snapshots` | |
| `partitions` | collection, cell, changed snapshot |
| `chunks` | collection, partition, seq, `chunk_id`, key range, envelope, rows, FGB file, column file, zone maps, begin/end snapshot |
| `postings` | collection, partition, file, begin/end |
| `overviews` | collection, level, cell (a partition for levels 10 and 8), file, begin/end |
| `id_runs` | collection, file, bloom, begin/end |
| `dirty_cells` | |
| `files` | `file_id`, collection, kind, bytes, published, reap after |

### Snapshots and retention

- A snapshot is the set of catalog rows valid at it: `begin ≤ s < end`.
- Snapshots expire 72 h after creation, and files referenced only by expired snapshots are then reaped.
- Peak storage is the live data plus everything replaced in the last 72 h.
- Catalog and blocks share one database, so a read replica never sees a snapshot that names files it lacks, and readonly workers can read replicas.
- There are no per-collection partitions in the PostgreSQL sense, no staging schema, no `ATTACH`, and no DDL on the write path.

## Collections, not views

Collections are independent. A client that wants several together (RustGIS layers, or an OGC client querying each collection) composes them itself.

## Formats and libraries (pure Go)

| Library | Role |
| --- | --- |
| `golang/geo/s2` | keys, partitions, coverings |
| `tidwall/rtree` | chunk-envelope index |
| `flatgeobuf/src/go` | detail and overview files |
| own column codec | evolved from `internal/chunk/attributes.go`: fixed-width typed vectors, per-file dictionaries, per-column zstd |
| `klauspost/compress` | zstd |
| `jackc/pgx/v5` `CopyFrom` | block writes |
| `hashicorp/golang-lru/v2` + `singleflight` | block cache |

## Scale targets (1e9 features)

| Item | Target |
| --- | --- |
| Detail chunks | about 200k full, plus up to about 90k partial |
| FGB detail | about 100 GB |
| Column files | 15–50 GB |
| Overview files | +10–15 % (bounded by `cap` × populated cells per level) |
| Postings | one segment per partition with at least 8 chunks, under 1 GB in total |
| Id runs | about 8–12 GB |
| Catalog | under 1 GB |

Worker memory is the catalog, the id runs and a column/block cache budget. All of this is to be measured at 10⁷–10⁸ features before anything is claimed.

## Kept from the current implementation

- FGB encode/decode (`ParseFlat`, `DecodePropertyText`, `EncodeCols`)
- the ingest worker pool
- the column codec
- CQL2 parsing and evaluation
- the OGC/STAC API surface
- readonly and writable workers

## Removed

- per-collection LIST partitions, the staging schema and the `ATTACH` swap
- `virtual_file` / `virtual_segment` and the single-file `items.fgb`
- `chunk_stats` SQL pruning and its Bloom filters, and `attr_chunks`
- `feature_base` and offset paging by global ordinal
- stride overview tiers
- views
- create-or-replace as the only write

## Build order

1. S2 keys, level-8 partitions, chunks, catalog and blocks, whole-partition replace loads, bbox reads, streamed FGB, snapshot cursors.
2. Column files and zone maps: attribute filters and exact `numberMatched`.
3. Id runs: `/items/{id}` and cross-partition uniqueness.
4. Postings and the busy rule.
5. Overview levels: 10 and 8 at load, 6, 4 and 2 in the background.
6. Snapshot expiry, reaping, and conditional loads (`base=`).

## Open decisions

- Overview levels, `cap` and simplification tolerances. Size the overhead on a real layer first.
- The distinct-count budget above which a column is not indexed.
- The chunk target (2 MiB / 50,000 features), to retune with measurements.
