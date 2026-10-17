# postfgb

OGC API Features and OGC Tiles over PostgreSQL, backed by FlatGeobuf chunks and index sidecars.

Goal: the smallest Go package that provides performant 1000x scaling of the spatial records a PostgreSQL database can hold, query, and return — by chunking records into FlatGeobuf and pushing filtering into index sidecars.

The target deployment is [RustGIS](https://rustgis.app/): reads are served over HTTP at maximum speed, while writes are batch-oriented and free to sort and rewrite chunks.

Design decisions are recorded in [DESIGN.md](DESIGN.md).

## How it works

- Features are encoded into FlatGeobuf chunks (packed Hilbert R-tree included) sized to ~2 MiB by a self-calibrating estimator, Hilbert-sorted into coherent chunks at ingest, zstd-compressed in Go, and stored as `bytea` rows in per-collection partitions with per-collection indexes; each collection is also exposed as a `layers.<name>` SQL view.
- Chunk bounds are plain float columns plus an S2 cell covering stored as an `int8multirange`; a GiST index prunes candidate chunks with one range-overlap predicate.
- The app computes the S2 coverings in Go, so the database only compares ranges and floats: no geometry parsing, no PostGIS. Real spatial work happens in memory, over the FlatGeobuf Hilbert R-trees.
- Fulltext lives in a sidecar table (`tsvector`/GIN) populated at ingest; `?q=` intersects text matches with chunk and feature level spatial filtering.
- Ingest also builds two overview tiers (10% and 1%): ordinal-stride samples, Douglas-Peucker simplified, with small non-point features dropped. Unfiltered tile and items requests read the smallest tier their zoom or bbox area allows; filtered requests, exports and raw chunks always use full detail (tiers add ~11% storage on the demo set).
- Attribute equality filters live in a `jsonb`/GIN sidecar (`chunk_props`); any non-reserved query parameter becomes a containment filter intersected with the spatial and text paths.
- Responses page with `links` (`rel=next`), so GDAL and other OGC clients walk the entire collection.
- Tiles are served through a shared Postgres cache keyed by collection version, tile coordinates and filter; ETags derive from the same version, so CDNs and clients revalidate with cheap 304s.
- A view is catalog metadata: an ordered member list with per-member CQL2 filters and column mappings (union-by-name, projection and rename); items, tiles and manifests union members live, and views are read-only and one level deep.
- Read clients can fetch `/collections/{id}/manifest` and prune chunks locally, downloading only the FlatGeobuf chunks they need; chunk responses are immutable, with `Cache-Control` and ETags.
- The raw chunk and manifest endpoints are part of the writable surface only; readonly workers serve just the OGC and STAC read paths, so public deployments never expose raw chunks that bypass filtering. The server does no authentication — run it behind your own auth layer (Kubernetes Gateway API, reverse proxy, service mesh). Filtered FlatGeobuf is served as an alternate encoding of items, never as raw chunks, so clients cannot bypass filters.
- CQL2-text `filter=` supports comparisons, `IN`, `BETWEEN`, `LIKE`, `IS NULL` and `AND`/`OR`/`NOT`; clauses compile to type-guarded SQL over the jsonb sidecar, with equality served by GIN containment. Spatial `S_INTERSECTS(geometry, ...)` accepts polygonal WKT or GeoJSON: chunks are pruned by the shape's bounds and features are tested exactly in Go (top-level AND only).
- The server is one small Go binary: huma for the OGC metadata endpoints, streaming handlers for items, tiles, and raw chunks.

The core path needs zero PostgreSQL extensions and runs on plain Amazon Aurora PostgreSQL (16–18); optional accelerators are discovered at startup via `pg_available_extensions`.

## Quickstart

Requires [mise](https://mise.jdx.dev) (installs Go, just and hk) and Docker.

```sh
mise install
hk install --mise   # pre-commit hooks: gofmt, go vet, go mod tidy, go test
just demo           # PostgreSQL 18 via Docker plus OSM fixture layers (Layercake)
just serve          # http://localhost:8080
```

Workers run in two modes. `postfgb serve` is writable: ingest, delete and raw chunk/manifest access. `postfgb serve --readonly` (or `POSTFGB_READONLY=true`) is the serving mode: only the OGC and STAC read surface is registered — writes and raw chunks answer 404 — and migrations are skipped unless `--migrate` is passed explicitly. Workers are stateless (`--addr` sets the listen address and defaults to `127.0.0.1:8080`, so nothing is exposed unless you ask; `--max-conns`/`--min-conns` tune the PostgreSQL pool, `--max-chunk-bytes` bounds the per-request chunk fetch budget) and all speak to the same PostgreSQL, so scale out with as many readonly replicas as you like. SIGTERM drains in-flight requests and shuts down cleanly; `/healthz`, `/readyz` and `/metrics` cover probes and monitoring, and one structured log line per request carries route, status, duration and bytes.

The server never authenticates requests. Put it behind your own auth layer — Kubernetes Gateway API, an ingress with path rules, a reverse proxy or a service mesh — and expose only the readonly workers publicly; keep the writable ones (which can ingest and stream raw chunks) on an internal route. The read surface sends permissive CORS headers (`Access-Control-Allow-Origin: *`, ETag/Content-Crs exposed) so browser clients like RustGIS can query a deployment directly. Readonly workers still render tiles through the shared PostgreSQL tile cache; with a strictly read-only database role the renders simply go uncached, which is logged, not fatal.

```sh
curl localhost:8080/collections
curl "localhost:8080/collections/bench/items?bbox=-10,45,10,55&limit=100"
curl 'localhost:8080/collections/bench/items?q="feature-42"'
curl "localhost:8080/collections/bench/items?name=feature-42&limit=10"
curl -G "localhost:8080/collections/bench/items" --data-urlencode "filter=i between 5 and 7 and name like 'feature-6%'"
curl -T features.fgb -H "Content-Type: application/flatgeobuf" "localhost:8080/collections/demo"
curl -X PUT -H "Content-Type: application/json" -d '{"title":"Demo features"}' "localhost:8080/collections/demo"
```

## Endpoints

| Path | Content |
| --- | --- |
| `/` | landing page |
| `/conformance` | conformance classes |
| `/collections` | collections with extents |
| `/collections/{id}` | collection detail |
| `/collections/{id}/items?bbox=&limit=&offset=&q=&filter=&{property}=&f=` | features: bbox, fulltext, CQL2 (text and JSON) filters including spatial `S_INTERSECTS`, paged (50,000 default); `overview=auto|0|1|2` picks or bypasses the overview tier (views serve full detail unless set); `numberMatched=true` adds the match count; `f=flatgeobuf` or `Accept: application/flatgeobuf` streams the filtered result as one FlatGeobuf |
| `/collections/{id}/items/{featureId}` | single feature; ids come from an id-like property (`id`, `fid`, ...) or the synthetic `chunkID-ord` |
| `/collections/{id}/queryables` | JSON Schema of the filterable properties |
| `PUT /collections/{id}?sortable=&id_column=&time_column=` (writable) | create-or-replace the collection from a whole FlatGeobuf body (any non-JSON content type); transactional, reprojected to WGS84, readers keep the old version until the swap; `sortable` declares up to 4 indexed columns |
| `PUT /collections/{id}` (JSON body, writable) | write the STAC collection document: `title`, `description`, `license`, `keywords` and free-form STAC fields are stored and echoed by the STAC facade; `members` defines or replaces a union view |
| `DELETE /collections/{id}` (writable) | drop a collection and its partitions |
| `/stac`, `/stac/collections`, `/stac/collections/{id}` | STAC catalog and collections |
| `/stac/collections/{id}/items?bbox=&datetime=&limit=` | STAC items (full detail, GeoJSON) |
| `/stac/search` | STAC item search, `GET` and `POST` |
| `/collections/{id}/chunks/{chunkID}` (writable) | raw FlatGeobuf chunk |
| `/collections/{id}/manifest` (writable) | chunk manifest: bboxes, counts, sizes, checksums, columns, chunk URLs; version ETag with 304 revalidation |
| `/collections/{id}/tiles` | tileset list with bounding box and per-zoom tile matrix limits |
| `/collections/{id}/tiles/WebMercatorQuad/{z}/{x}/{y}` | MVT tile; accepts `q`, `filter` (CQL2 text and JSON) and `{property}=` filters, always evaluated against full detail; unfiltered tiles read the overview tiers at low zooms and honor `overview=` |
| `/tileMatrixSets`, `/tileMatrixSets/WebMercatorQuad` | tile matrix set list and WebMercatorQuad definition |
| `/api` | OpenAPI 3.0 definition (OGC content type) |
| `/viewer` | embedded MapLibre browser: collection picker, tiles, `q=`/`filter=` boxes, click-to-inspect (CDN assets, one static HTML page) |
| `/healthz` | liveness probe, always 200 |
| `/readyz` | readiness probe: PostgreSQL ping plus migration check |
| `/metrics` | Prometheus text metrics: requests, durations, bytes, tiles, chunks, ingest counters and pool gauges |

## Real data (OSM Layer Cake)

[Layercake](https://layercake.openstreetmap.us) publishes OpenStreetMap as planet-wide GeoParquet layers with HTTP range access. GDAL (sidecar container) reads them with row-group pruning, converts to FlatGeobuf and streams it straight into a PUT — no temporary files on the host:

```sh
just layercake settlements "-74.05 40.65 -73.92 40.82"   # NYC, ~3 s
just layercake buildings "7.40 43.72 7.44 43.76"         # Monaco, ~10 s
```

The arguments map to `https://data.openstreetmap.us/layercake/<layer>.parquet` and an optional `-spat` bbox; omit the bbox to stream an entire layer (settlements is 188 MiB, buildings is 42.9 GiB planet-wide). Uploads stream with `curl -T` (or `-T -` for a pipe from `ogr2ogr`): the body never sits in curl's memory — `--data-binary` buffers the whole upload in RAM, which kills multi-gigabyte loads. Layer types are normalized with `-nlt GEOMETRY` by default so mixed-geometry layers (highways, POIs) load as-is, and list-valued tag columns are flattened to scalars because FlatGeobuf has no list types. Layers land with their real tags, OSM timestamps (time filtering and `extent.temporal` come along) and polygon geometry. `just fixtures` loads the six main layers over one shared bbox, ready for view and union tests.

## Benchmarks

`bench/run.sh` uses GDAL as an OGC client:

- `ogrinfo "OAPIF:http://localhost:8080"` — collection listing
- `ogrinfo -so -al "OAPIF:http://localhost:8080" bench` — full scan
- `ogrinfo -so -al -spat -10 45 10 55 "OAPIF:http://localhost:8080" bench` — server-side bbox filter
- `gdalinfo "/vsicurl_streaming/http://localhost:8080/collections/bench/tiles/WebMercatorQuad/4/8/6.mvt"` — vector tile

## Exports

Bulk data leaves the server as FlatGeobuf and nothing else — clients convert locally with GDAL, like any other file:

```sh
curl -s "localhost:8080/collections/bench/items?f=flatgeobuf&filter=i%3E999990" -o filtered.fgb
ogr2ogr filtered.gpkg filtered.fgb
```

The `just export` recipe runs that OAPIF-to-OGR conversion client-side in one step, and single-tile MVT is generated in-process (pure Go, clipped and simplified). GDAL stays a sidecar container; the server remains cgo-free:

```sh
just export demo FlatGeobuf demo.fgb    # OAPIF -> any OGR format: GPKG, KML, Shapefile, GeoParquet, ...
just export bench GPKG bench.gpkg
just tiles bench 4                      # whole MVT tileset as one MBTiles container
```

The tiles recipe chains GDAL's MVT writer into MBTiles: one self-contained tileset file any GDAL stack can read offline.

Keep filtering on the server side: GDAL evaluates `-where` clauses against OAPIF sources client-side (it downloads the whole collection), while server-side `filter=` goes through chunk pruning and the jsonb sidecar. Ask for `f=flatgeobuf` and the server assembles one filtered FlatGeobuf (spatial index included) that GDAL reads directly:

```sh
ogr2ogr filtered.gpkg "/vsicurl_streaming/http://localhost:8080/collections/bench/items?f=flatgeobuf&filter=i%3E999990"
```

## Status

Pre-alpha. Working today: chunk encode/decode, HTTP FlatGeobuf ingest (content-negotiated create-or-replace PUT, staged loads swapped in at commit, reprojected to WGS84, declared sortable indexes) with 10%/1% overview tiers, zstd-compressed ~2 MiB chunks in per-collection partitions, union views with per-member filters and column mapping, OGC API Features with windowed next-link paging (50,000 default) and `overview=` overrides, single-feature, queryables and `numberMatched` endpoints, S2 multirange chunk pruning, fulltext (`?q=`), property filters, CQL2-text and CQL2-JSON filters including spatial `S_INTERSECTS`, MVT tiles with server-side filters, overview tiers below full detail and feature ids matching items, tilesets with bounding box and per-zoom limits, chunk manifest with checksums and version ETags, immutable chunk caching, per-request chunk byte budget, gzip on JSON, FlatGeobuf downloads for local conversion, STAC catalog/collections/items/search facade, embedded MapLibre viewer at `/viewer`, permissive CORS for browser clients, writable/readonly worker modes, `/healthz`/`/readyz`/`/metrics`, graceful shutdown, structured request logs, PostgreSQL-backed integration tests in CI, capability probing, hk git hooks. Benchmarked with GDAL as an OGC client on 1M features: full paged scan ~69s, single FlatGeobuf export 7s, bbox page ~2s, tiles ~0.6s; a 50M-feature staged load runs in ~45 min with reads on other collections staying under 25 ms throughout. Next: the build order in DESIGN.md (deeper performance work).

## License

Apache-2.0.
