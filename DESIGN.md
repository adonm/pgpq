# postfgb design

Settled decisions for the architecture. The README describes what exists today; this file records where the project is going and why.

## Goal

The smallest Go package that serves OGC API Features and OGC Tiles over spatial records held in PostgreSQL, at maximum read speed for rustgis.app. Writes are batch-oriented and HTTP-only; the server may rewrite chunks freely. External software (GDAL, rustgis-cli) appears only as a client or a sidecar — the server never links cgo.

## Architecture

Features are encoded into FlatGeobuf chunks (packed Hilbert R-tree included) and stored as bytea. Chunk bounds are scalar floats plus an S2 covering stored as int8multirange with a GiST index; the database compares ranges and floats and never parses geometry. Filtering lives in index sidecars: tsvector/GIN for fulltext, jsonb/GIN for attributes. huma serves the metadata endpoints; items, tiles and chunks stream from handlers.

## Storage — one database, one virtual GeoPackage

- `chunks`, `chunk_text` and `chunk_props` are LIST-partitioned by collection: every collection owns three physical partitions with their own heaps and indexes, which removes today's cross-collection GIN noise and scopes vacuum and drop.
- Chunk ids stay globally unique (identity on the parent). Primary keys: `(collection, id)` and `(collection, chunk_id, feature_ord)`. Sidecar integrity comes from the partition lifecycle, not foreign keys — partitioned FKs would block partition swaps.
- Chunk payloads are zstd-compressed in Go before insert and stored out-of-line uncompressed (`STORAGE EXTERNAL`): Postgres does no compression work and ships stored bytes as-is; checksums and ETags cover the raw FlatGeobuf.
- All queries already carry `collection = $1`, so partition pruning turns access into per-collection index scans with unchanged SQL.
- Each collection also gets a `layers.<name>` SQL view over its tier-0 chunks for database-side introspection, GeoPackage-style.
- The `collections` catalog row carries per-collection stats (feature count, spatial and temporal extent, kind, members, declared sortable columns), maintained in the ingest transaction; the collection endpoints never scan chunk tables.
- The core path requires zero PostgreSQL extensions; the legacy `pg_trgm` create is removed.

## Views — managed union views

- A view is catalog metadata: an ordered list of members, each a collection plus an optional CQL2 filter and column mapping. No extra storage.
- Schema rule: union by name — member columns merge, missing columns are absent. The view can project and rename columns.
- Members walk in order for paging; the manifest unions member chunk metadata (chunk URLs remain member-scoped); tiles render across members. Items on views read full detail unless `overview=` asks otherwise; unfiltered tiles resolve tiers per member by zoom.
- Spatial and temporal extents are live unions of member extents. Views are read-only and exactly one level deep.

## Write path — HTTP only

- `PUT /collections/{id}/data` (admin bearer token) takes a whole-file FlatGeobuf body — not pre-chunked. Create-or-replace.
- Synchronous within one transaction: readers always see the old collection or the new one. Failure or disconnect rolls back; loads are retried from scratch (accepted cost).
- Input CRS: any CRS supported by the pure-Go projection layer ([wroge/wgs84](https://github.com/wroge/wgs84)) is reprojected to WGS84 during ingest; anything else is rejected at upload time. Stored geometries are 2D (Z dropped), and chunks declare EPSG:4326.
- The FGB header supplies the uniform schema, and features stream in file order: uploads are expected in packed-Hilbert order (GDAL and `gen` write it), so chunking yields spatially coherent chunks and small queries touch few chunks. Server-side chunking targets roughly 2 MiB per chunk with a self-calibrating byte estimator; overview tiers accumulate to the same target.
- Time column: first DateTime-typed column (overridable). Feature id preference: an id-like property (`id`, `fid`, `gid`, `ogc_fid`), then the synthetic `chunkID-ord`.
- Sortable columns are declared on the PUT request and indexed during the load (btree expression indexes per partition).
- Overview tiers (10%, 1%) are rebuilt inside the load transaction. `DELETE /collections/{id}` removes the collection.
- The ingest CLI is removed; the development loop is `gen` (emits FlatGeobuf) plus curl against a local server.

## Read path

- Unfiltered scans stream chunk windows in id order with no fixed chunk cap; offset paging stays exact at any collection size.
- Filtered requests fan out over matched chunks under a per-request byte budget; short pages always carry honest next links.
- Items: GeoJSON default; `f=flatgeobuf` streams full detail as a single FGB. Default page 50,000 (1,000,000 for FlatGeobuf), max 1,000,000. `overview=auto|0|1|2` overrides tier selection for unfiltered requests; filters and exports always read full detail. `numberMatched` is opt-in. `/collections/{id}/items/{featureId}` serves one feature.
- Tiles: MVT rendered per request in Go — clip, simplify, remove-empty — gzipped. Unfiltered `z <= 2` reads the 1% tier, `z <= 4` the 10% tier, deeper zoom reads full detail; filtered tiles always read full detail. Empty tiles are valid empty layers (200). Tileset metadata advertises per-collection zoom limits and bbox; MVT features carry the same ids as items.
- Overview tiers: ordinal stride 10 / 100 with Douglas-Peucker simplification and small non-point geometries dropped; ~10-15% extra storage; a collection without tiers falls back to full detail.
- Queryables come from catalog columns; `datetime` filters the recorded time column; `sortby` works only on declared, indexed columns; `crs` and `bbox-crs` are limited to a curated EPSG list; CQL2 text and JSON cover comparisons, `IN`/`BETWEEN`/`LIKE`/`IS NULL`, and spatial predicates implemented with polyclip-go over an in-house WKT parser.
- Conformance classes are only claimed when implemented — no over-claiming. The Part 1 Core single-feature path is missing today and lands with the feature endpoint.

## HTTP caching

Chunks are immutable with checksum ETags. Tiles carry an ETag derived from collection state plus z/x/y, long-lived. The manifest carries a collection-state ETag. Items stay uncached.

## Exports

`fgb` streams directly. `geojson`, `gpkg`, `parquet`, `shp`, `kml` and `gpx` convert through `ogr2ogr` only (the rustgis-cli engine is removed). Synchronous, 1,000,000 feature cap, 404 when nothing matches.

## Ops and build

- Images build with [ko](https://ko.build): no Dockerfile, static base (`cgr.dev/chainguard/static`), CGO_ENABLED=0, git-SHA tags, SBOM support. Exports need an operator-supplied ogr2ogr (mounted binary or sidecar) in that image; a GDAL-based base is the alternative when in-image exports matter.
- mise provides Go, just, hk and ko. hk runs gofmt/vet/tidy/tests on every commit; GitHub Actions runs unit tests, PostgreSQL-backed integration tests (ingest -> query -> tiles) and a ko image build.
- `/healthz` and `/readyz`; a minimal hand-rolled Prometheus `/metrics`; graceful shutdown; pool-size flags; structured request logging. Deployment stays raw binary / generated image; no release pipeline yet.
- The admin bearer token gates manifest, chunks, export, ingest, delete and view management. OGC endpoints stay open and always filter server-side.

## Performance

Design target: ~100M features in one collection on one PostgreSQL node. Chunk fetches run in parallel within a request; tiles get an in-process LRU; every request carries a chunk byte budget. Measured baseline (1M synthetic features, GDAL as OGC client): full paged scan ~69s, single FlatGeobuf export 7s, bbox page ~2s, tile ~0.6s.

## Rejected or deferred

- Virtual PMTiles archives and the go-pmtiles dependency (removed; OGC Tiles is the tile interface).
- Materialized views and DB-native view discovery; views of views.
- Dynamic per-collection tables and schema-per-collection storage.
- Async ingest jobs and upload sessions (synchronous chosen); versioned blue/green replace (transactional chosen).
- cgo PROJ; the rustgis-cli export engine; runtime extension accelerators (probing stays as a future hook).

## Build order

1. ko image builds (mise, .ko.yaml, just recipes, CI job).
2. Partitioned storage, catalog stats, layers views, `pg_trgm` removal.
3. HTTP FlatGeobuf ingest: PUT/delete, transactional replace, reprojection, declared sortables; remove the ingest CLI.
4. Read correctness: windowed streaming, `overview=`, single-feature endpoint and ids, `numberMatched`, queryables.
5. Union views serving.
6. Client features: datetime, sortby, CRS/bbox-crs, CQL2 spatial.
7. Exports through ogr2ogr only.
8. Ops: healthz/readyz, metrics, lifecycle and pool flags, integration tests in CI.
9. Performance: parallel chunk fetch, tile LRU, byte budget; tile and manifest ETags, MVT ids, tileset limits.
