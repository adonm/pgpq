# postfgb design — next (draft)

Status: proposal from the October 2026 design discussion, partly implemented under `/v2`. When built, it replaces the storage, ingest and read-path contract in [DESIGN.md](DESIGN.md), and the two documents are reconciled in the same commit as the code.

## Implementation status

Steps 1 to 3 are built under `/v2`, alongside the current contract. DESIGN.md is unchanged until cutover.

- **Built in step 1:** S2 keys and level-8 partitions (`internal/spatial`); pgvfs-layout blocks in schema `pfgb` (`internal/blocks`, migration 0031); key-ordered chunks with catalog and snapshots; whole-collection and partition-scoped replace loads with 422 validation and per-collection commit serialization (`internal/layer`); snapshot-pinned bbox paging with 72 h cursor expiry; GeoJSON and FlatGeobuf responses on `PUT /v2/collections/{id}` and `GET /v2/collections/{id}/items`.
- **Built in step 2:** a column file per chunk, in the same row order as its FlatGeobuf; zone maps (`min`, `max` and `nulls` per column) in the chunk row (migration 0033); CQL2 and property-equality filters evaluated exactly on the column values; `numberMatched` for filter-only requests.
- **Built in step 3:** id runs (migration 0032) and `GET /v2/collections/{id}/items/{featureID}`. Uniqueness across partitions is the loader's job, so the server checks ids only within a body (422), and a lookup that finds the id in more than one partition answers 409.
- **Built for caching:** byte-budgeted LRUs per worker (`internal/lru`, 256 MiB each): file bytes by `file_id` (filled once per file), each snapshot's chunk list (the catalog cache, which loads the whole list rather than applying deltas; snapshots are immutable, so entries never go stale), and decoded id runs. On the 30k corpus a repeated count over 1,751 chunks falls from 0.69 s to 0.13 s, and a repeated bbox plus filter from 0.25 s to 0.11 s, both with the block cache alone; the catalog cache is not yet measured at scale.
- **Built in step 5:** overview levels 8 and 10 per partition, level 12 for partitions above 100,000 features, and levels 6, 4 and 2 rebuilt in the same commit as each load, from the level below. Samples keep full geometry. Overview requests take a level, no filter, and are paged like detail; lookups and `numberMatched` read detail only. Conditional loads (`base=`) are built too: they answer 409 when a listed partition changed after the base snapshot.
- **Decided, not built:** one `.fgb.zst` per level-8 cell is the only upload, so the streaming, bucketing and spill steps go; each upload is its own snapshot; id runs are merged in the background. The `/v2` load still takes multi-cell FlatGeobuf bodies and buckets them in memory. See Write path and Decisions.
- **Verified:**
  - `testdata/corpus/mixed.fgb` (30,000 features: every scalar type, nulls, arrays, LIKE, ranges, datetime and bbox) loaded as a v2 collection in 2.8 s. The 93 filters in `filters.txt` without a bbox return the expected count through both the item list and `numberMatched`, and the five bbox filters return their expected counts through the item list. The two `datetime` lines are v1-only and skipped.
  - GDAL `ogrinfo` reads filtered FlatGeobuf over `/vsicurl/`: 30,000 features unfiltered, 12,000 for a LIKE filter and 3,340 for a bbox plus LIKE filter.
  - Lookups: a feature moved within its partition resolves to its new key, a feature moved to another partition resolves to its live copy, and a duplicate created by two partition loads answers 409.
- **Not yet:** reaping and expired-snapshot cleanup (step 6); merging id runs (decided, see Decisions); spill-to-disk for loads larger than memory (requests are held in memory); an R-tree (bbox pruning is a range query on chunk envelopes).
- **Measured on `warm_100k`** (100k features spread thinly across ~7,600 level-8 cells):

  | | chunks | logical bytes | load | bbox (-5,40,10,50) |
  | --- | --- | --- | --- | --- |
  | v1 | 19 | 8.9 MB | 3.4 s | 15,047 features, 0.46 s |
  | v2 | 7,614 (13 features each) | 15 MB | 5.9 s | 15,047 features, 1.0 s |

  GDAL `ogrinfo` reads the v2 FlatGeobuf output (100,000 features, same extent as v1).
- **Finding:** a fixed level-8 partition costs one chunk per populated cell, and each chunk carries about 0.8 KB of FlatGeobuf header and index. Dense partitions are unaffected. Sparse data pays heavily: here 1.7× the storage and 400× the chunk rows. **Decided:** one chunk per partition, with no packing across partitions. The overhead is accepted, and the worker caches absorb the read cost (see Read path).

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

- Within a partition, features are sorted by `(key, id)` and cut into chunks of about 2 MiB, at most 50,000 features each. Each partition's last chunk is partial: at most one per populated partition, so up to about 90k extra chunks over the ~200k full ones at 1e9. They are small, so each costs a catalog row and about 0.8 KB of FlatGeobuf header and index. Chunks never span partitions: a sparse partition keeps its own chunk, so every populated partition has at least one.
- **A chunk** is an immutable `(chunk_id, partition, seq, key range, envelope, rows)`. Reloading a partition produces new chunks.
- **Row order:** row `i` of a chunk's column file is feature `i` of its FGB. The encoder writes features in `(key, id)` order and must not reorder them.
- **Envelopes:** they stay the spatial correctness key. A feature is keyed by its center, but its chunk's envelope covers its full extent.

## Identity

- Every feature carries an id that is present and unique within its collection. Ids are the only stable external reference; `(chunk, ord)` addresses are internal to one snapshot.
- **Uniqueness is the loader's job.** The server rejects an id repeated within one body (422) and does not check other partitions on write. A duplicate that two loads create is reported at lookup, as 409 naming the id, rather than one copy being silently chosen.
- **Id index:** sorted runs mapping `hash(id)` to the feature's key. Each upload writes one run: FNV-1a 64-bit hashes of the canonical id text in ascending order, delta-coded as varints, with an 8-byte key each. An upload does not end older runs; a background merge drops their stale entries (see Decisions). Runs are not merged or Bloom-filtered yet.
- **Lookups:** every live run is binary-searched for the id's hash. Each candidate key names a partition, and that partition's chunks whose key range covers the key are read at the current snapshot and searched for the id. Stale entries (features since removed or moved) find nothing and are ignored; merges will drop them later.
- **Size:** about 13–16 B per feature per run, before merging, held in worker memory.

## Write path

### The upload

- **One upload is one level-8 cell.** The loader sends a zstd-compressed FlatGeobuf (`.fgb.zst`) holding exactly that cell's features. Route name to be decided.
- **Replace semantics:** the file is the complete new contents of its cell. An empty file empties the cell.
- **Validation:** a feature outside the cell, a duplicate id in the file, or unusable geometry answers 422. `base=<snapshot>` answers 409 if the cell changed after that snapshot, so a loader's read-modify-write cannot overwrite a concurrent change.
- **A reload is many uploads:** one per cell, each its own load, snapshot and commit.
- **No streaming, bucketing or spill.** The worker decompresses the file into memory and parses it once. The densest cell (about 2.5M features, about 1.2 GB of FGB) therefore sets the worker's memory.
- **No per-feature writes.** Deletes and corrections are uploads of the cells involved.

### Steps for one upload

1. Decompress the file and parse its features once: key each by its bbox centre (S2 level 30), check the cell and the ids, and keep (key, id, offset).
2. Sort by `(key, id)` and cut chunks at 2 MiB or 50,000 features.
3. Encode each chunk: FGB (zstd), the column file in the same row order, and the zone maps.
4. Build the cell's overviews: samples at levels 8 and 10, and at level 12 above 100,000 features. Build the cell's id run.
5. Write every file as blocks, each in its own transaction, unpublished until the commit.
6. Commit in one short transaction: lock the collection row, so commits serialize; check `base` against the cell's `changed` snapshot; end the cell's live chunks and overview rows; insert the new rows; rebuild the level-6, 4 and 2 ancestors from the samples below them, in the same commit; insert the id run and the snapshot row; advance the collection; publish the files.

Cost is proportional to one cell plus its ancestors, never to the collection. Commits serialize per collection, which is what keeps a snapshot's contents fixed once it is visible: a reader pinned to snapshot `s` must never see a snapshot `s' < s` commit after it. The commit transaction is milliseconds, because all bulk work happens before it.

## Postings (cut for now; see Decisions)

- **One segment per partition** with at least 8 chunks, built during the load from the column values in hand, and so always current. Smaller partitions are scanned.
- **Indexed columns:** low- and medium-cardinality columns and list columns. A column whose distinct count in the partition exceeds a budget is not indexed; zone maps serve it, and the id index serves ids.
- **Content:** each value maps to the ordinals of the partition's chunks that hold it, as a sorted list or a bitset, whichever is smaller. At most about 63 B per value at 500 chunks.
- **Busy values:** a value held by more than 20 % of the partition's chunks is marked busy and gets no list; a query scans those chunks instead.
- **Sizes:** the densest segment is about 0.05–2.5 MB, mostly medium-cardinality columns. Most partitions have fewer than 8 chunks and no segment.

## Overview levels

Each overview level is a sampled copy of the collection in FlatGeobuf, and all of them are built: levels 8 and 10 per partition at load, level 12 for partitions above 100,000 features, and levels 6, 4 and 2 per cell in the commit. The server does not map zoom to levels; the client asks with `overview=`.

- **Sampling:** level `L` keeps, in every level-`L` S2 cell, the `cap` features with the smallest `hash(id)`, simplified with that level's Douglas–Peucker tolerance.
  - Sparse cells keep all their features and dense ones keep `cap`, so every level covers the extent evenly.
  - A feature kept at a coarse level is kept at every finer one, so each level is built from the next finer level, never from a rescan.
- **Levels 10 and 8:** level 10 is finer than a partition and level 8 is the partition itself. Each has one file per partition, built at load and atomic with detail.
- **Levels 6, 4 and 2** are coarser: one overview per level-`L` cell, at most `cap` features each, rebuilt in the commit of every upload under the cell, from the samples below it. The rebuild shares the upload's snapshot, so `base=` checks stay exact.

## Read path

- **Worker catalog:** for each collection, each worker holds the current snapshot's catalog — partitions, chunks (with key ranges, envelopes, counts, files and zone maps; about 300k at 1e9), postings, overview files and id runs. As built, each snapshot's chunk list is loaded whole, since snapshots never change.
- **bbox:** as built, a linear scan of the snapshot's chunk list tests each chunk envelope, and each candidate chunk's packed R-tree tests its features. An in-memory R-tree over the envelopes (`tidwall/rtree`) is planned, not built.
- **Attributes:**
  1. Zone maps prune chunks.
  2. Postings give exact chunk sets for rare values.
  3. Only the predicate's column sections are read, and dictionary-coded equality and `IN` compare integer codes.

  The result is the exact `(chunk, ord)` set, and only those features' FGB bytes are fetched. `numberMatched` is exact and never decodes geometry. Filters always read detail.

  As built: steps 1 and 3, without the integer-code comparison. Zone maps hold each column's `min`, `max` and null count from the column file header, and a chunk is skipped only when no row can match. Numeric comparisons and `BETWEEN` use non-strict tests on float64 bounds, because float64 rounding is monotone and a strict test could drop a match near 2^53. `IS NULL` needs a null, and `IN` is an OR of equalities. `NOT`, `LIKE`, `<>`, text and boolean columns, and unknown columns prune nothing. There are no Bloom filters. A candidate chunk's FGB is read whole, not only the matching features. `numberMatched` is returned for filter-only requests and omitted with a bbox, as in v1, because counting it would decode geometry.
- **By id:** each live id run is binary-searched for the id's hash. Each candidate key names a partition, and the chunks of that partition whose key range covers the key are read at the current snapshot and searched for the id. No hit answers 404, one hit answers with the feature, and more answer 409.
- **Overview:** `overview=N` names the S2 level directly (2, 4, 6, 8, 10 or 12), and the client maps zoom to N. The response is one streamed FGB (or GeoJSON). There is no single-file `items.fgb`.
- **Paging:** a cursor carries its snapshot and the `(key, id)` of the last feature it returned. A pagination must complete within the 72 h snapshot retention; an older cursor answers 410.
- **CQL2:** evaluation is unchanged (`internal/cql`), applied to column values.

## Storage in PostgreSQL

### Blocks

- Blocks live in our own schema, using the pgvfs layout but written by our own Go code. Every artifact is an immutable file stored as 8,120-byte rows keyed `(file_id, n)`, one per 8 KB heap page, inline and without TOAST.
- A file is written with one binary `COPY` and read whole: its rows are selected in `(file_id, n)` order and assembled into the file-bytes LRU.
- Workers cache file bytes by `file_id` in a byte-budgeted LRU, filled once per file and shared by concurrent readers (256 MiB per worker for now). Files are immutable, so an entry is never stale; eviction is the only way out.
- Autovacuum on the block table runs at 1 % dead rows. WAL is about 1× the bytes written.

### Catalog

The catalog is small and mutable, and lives in the same database:

| Table | Columns | Indexes |
| --- | --- | --- |
| `collections` | `name` pk, `uid` identity, `current_snapshot`, `id_column`, `columns_json`, `created` | pk (`name`) |
| `snapshots` | `collection`, `id`, `created` | pk (`collection`, `id`) |
| `partitions` | `collection`, `cell`, `changed` | pk (`collection`, `cell`) |
| `files` | `file_id` identity pk, `collection`, `kind` (`fgb`, `cols` or `idrun`), `bytes`, `published`, `created` | pk (`file_id`) |
| `blocks` | `file_id`, `n`, `data` (`bytea`, stored plain: one 8,120-byte row) | pk (`file_id`, `n`) |
| `chunks` | `chunk_id` identity pk, `collection`, `cell`, `seq`, first and last `key` and `id`, `minx`, `miny`, `maxx`, `maxy`, `features`, `file_id`, `col_file`, `zone` (JSON text), `level`, `begin_snapshot`, `end_snapshot` | `(collection, begin_snapshot)`; `(collection, cell)` where `end_snapshot` is null |
| `id_runs` | `file_id` pk, `collection`, `entries`, `begin_snapshot`, `end_snapshot` | `(collection)` where `end_snapshot` is null |

Overview levels are chunks with `level` set, so there is no overview table. Postings and the dirty-cell queue are not built, and the `reap after` column waits for reaping.

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
| `golang.org/x/sync/singleflight` and a stdlib LRU in `internal/lru` (`container/list`) | block cache, chunk lists and id runs; `golang-lru` counts entries, not bytes |

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

1. (built) S2 keys, level-8 partitions, chunks, catalog and blocks, whole-partition replace loads, bbox reads, streamed FGB, snapshot cursors.
2. (built) Column files and zone maps: attribute filters and exact `numberMatched`.
3. (built) Id runs: `/items/{id}` and duplicate detection at lookup. Uniqueness across partitions stays the loader's job.
4. Postings and the busy rule: cut from v2 for now (see Decisions).
5. Overview levels: built (see Implementation status).
6. Snapshot expiry and reaping (next). Conditional loads (`base=`) are built.
7. One upload per level-8 cell, with no streaming, bucketing or spill (decided, not built): the multi-cell body path goes, and each upload commits its own snapshot.
8. Background id-run merge (decided, not built), then a fence index per run before lookups scale (see Risks).

## Decisions

These close the open questions. Where the gain is marginal, each one takes the option with less code.

- **Overview levels:** 10 and 8 per partition at load, 12 for partitions above 100,000 features, and 6, 4 and 2 per cell, rebuilt in the same commit from the level below. `cap` = 256 at every level. Samples keep full geometry and column files, so there is no simplification step yet. Douglas–Peucker from `paulmach/orb` is the tool if response sizes need it.
- **Zoom mapping:** none on the server. The client asks for a level with `overview=`.
- **Chunk target:** 2 MiB and 50,000 features, kept.
- **Postings:** cut from v2. Zone maps and column sidecars answer filters, and the cost is a column scan per chunk in the partition. Reintroduce postings only when a measured filter on a dense partition exceeds about 1 s cold. This saves about 250 lines.
- **Upload:** one `.fgb.zst` per level-8 cell. No streaming, bucketing or spill: the worker holds the decompressed cell in memory, so the densest cell sets the memory budget. Decided, not built yet.
- **Snapshot per upload:** each upload is one commit with its own snapshot. A reload of many cells is therefore not atomic: readers can see old and new cells together until the last upload commits. A commit that spans uploads would make it atomic (about 60 lines, not built). Decided.
- **Id runs:** one run per upload. Above 64 live runs, a background merge combines the 16 smallest into one run and drops stale entries. No Bloom filters, because a binary search over at most 64 runs is cheap. Decided, not built yet.
- **Reaping:** keep the 72 h retention. Chunk, run and file rows that no retained snapshot can see are deleted with their blocks. Decided, not built yet.
- **Code budget:** 4,000 lines of production Go for v2. Today it is about 1,920 (1,628 in the v2 packages and about 290 in API routes and shared code), plus about 1,070 test lines and 66 SQL lines. The earlier figure of about 2,750 counted test files.

## Risks

- **Memory per upload:** the densest cell (about 2.5M features, about 1.2 GB of FGB) is held in memory whole, and nothing spills.
- **Mixed snapshots:** a multi-cell reload is not one snapshot; see Decisions.
- **Lookups decode whole runs:** delta coding means a lookup decodes each live run in full, held in the decoded-run LRU. At 1e9 features the runs total about 15 GB, so a fence index per run (one hash every few thousand entries) is needed before lookups scale.
- **Coarse rebuild:** every upload reads its ancestors' sibling samples. Measure at 10 million features.
- **Dense cities at zoom 13:** about 240k features on screen. Use overviews until detail is small, around zoom 16 or 17.
- **Overview size:** overview responses carry full geometry, so they can be large.
- **Filters on dense cells:** column files are scanned until postings exist.
- **Old collections:** collections loaded before migrations 0032 to 0034 need a reload before lookups and filters work.
