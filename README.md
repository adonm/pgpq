# postfgb

OGC API Features and OGC Tiles over PostgreSQL, backed by FlatGeobuf chunks and index sidecars.

Goal: the smallest Go package that provides performant 1000x scaling of the spatial records a PostgreSQL database can hold, query, and return — by chunking records into FlatGeobuf and pushing filtering into index sidecars.

The target deployment is [RustGIS](https://rustgis.app/): reads are served over HTTP at maximum speed, while writes are batch-oriented and free to sort and rewrite chunks.

## How it works

- Features are encoded into FlatGeobuf chunks (packed Hilbert R-tree included) and stored as `bytea` rows.
- Chunk bounds are plain float columns plus an S2 cell covering stored as an `int8multirange`; a GiST index prunes candidate chunks with one range-overlap predicate.
- The app computes the S2 coverings in Go, so the database only compares ranges and floats: no geometry parsing, no PostGIS. Real spatial work happens in memory, over the FlatGeobuf Hilbert R-trees.
- Fulltext lives in a sidecar table (`tsvector`/GIN) populated at ingest; `?q=` intersects text matches with chunk and feature level spatial filtering.
- Ingest also builds two overview tiers (10% and 1%): ordinal-stride samples, Douglas-Peucker simplified, with small non-point features dropped. Unfiltered tile and items requests read the smallest tier their zoom or bbox area allows; filtered requests, exports and raw chunks always use full detail (tiers add ~11% storage on the demo set).
- Attribute equality filters live in a `jsonb`/GIN sidecar (`chunk_props`); any non-reserved query parameter becomes a containment filter intersected with the spatial and text paths.
- Responses page with `links` (`rel=next`), so GDAL and other OGC clients walk the entire collection.
- Read clients can fetch `/collections/{id}/manifest` and prune chunks locally, downloading only the FlatGeobuf chunks they need; chunk responses are immutable, with `Cache-Control` and ETags.
- The raw chunk and manifest endpoints can be locked with `--admin-token` (bearer auth); OGC endpoints stay open and always apply server-side filtering. Filtered FlatGeobuf is served as an alternate encoding of items, never as raw chunks, so clients cannot bypass filters.
- CQL2-text `filter=` supports comparisons, `IN`, `BETWEEN`, `LIKE`, `IS NULL` and `AND`/`OR`/`NOT`; clauses compile to type-guarded SQL over the jsonb sidecar, with equality served by GIN containment.
- The server is one small Go binary: huma for the OGC metadata endpoints, streaming handlers for items, tiles, and raw chunks.

The core path needs zero PostgreSQL extensions and runs on plain Amazon Aurora PostgreSQL (16–18); optional accelerators are discovered at startup via `pg_available_extensions`.

## Quickstart

Requires [mise](https://mise.jdx.dev) (installs Go, just and hk) and Docker.

```sh
mise install
hk install --mise   # pre-commit hooks: gofmt, go vet, go mod tidy, go test
just demo           # PostgreSQL 18 via Docker, 1M features generated and ingested
just serve          # http://localhost:8080
```

```sh
curl localhost:8080/collections
curl "localhost:8080/collections/bench/items?bbox=-10,45,10,55&limit=100"
curl 'localhost:8080/collections/bench/items?q="feature-42"'
curl "localhost:8080/collections/bench/items?name=feature-42&limit=10"
curl -G "localhost:8080/collections/bench/items" --data-urlencode "filter=i between 5 and 7 and name like 'feature-6%'"
```

## Endpoints

| Path | Content |
| --- | --- |
| `/` | landing page |
| `/conformance` | conformance classes |
| `/collections` | collections with extents |
| `/collections/{id}` | collection detail |
| `/collections/{id}/items?bbox=&limit=&offset=&q=&filter=&{property}=&f=` | features: bbox, fulltext, CQL2 (text and JSON) filters, paged (50,000 default); `f=flatgeobuf` or `Accept: application/flatgeobuf` streams the filtered result as one FlatGeobuf |
| `/collections/{id}/chunks/{chunkID}` | raw FlatGeobuf chunk |
| `/collections/{id}/manifest` | chunk manifest: bboxes, counts, sizes, checksums, columns, chunk URLs |
| `/collections/{id}/export?format=` | filtered features converted on the fly: fgb, geojson, gpkg, parquet, shp, kml, gpx (needs `--rustgis-cli` or `--ogr2ogr`; admin token when configured) |
| `/collections/{id}/tiles` | tileset list |
| `/collections/{id}/tiles/WebMercatorQuad/{z}/{x}/{y}` | MVT tile; accepts `q`, `filter` (CQL2 text and JSON) and `{property}=` filters, always evaluated against full detail; unfiltered tiles read the overview tiers at low zooms |
| `/tileMatrixSets`, `/tileMatrixSets/WebMercatorQuad` | tile matrix set list and WebMercatorQuad definition |
| `/api` | OpenAPI 3.0 definition (OGC content type) |

## Benchmarks

`bench/run.sh` uses GDAL as an OGC client:

- `ogrinfo "OAPIF:http://localhost:8080"` — collection listing
- `ogrinfo -so -al "OAPIF:http://localhost:8080" bench` — full scan
- `ogrinfo -so -al -spat -10 45 10 55 "OAPIF:http://localhost:8080" bench` — server-side bbox filter
- `gdalinfo "/vsicurl_streaming/http://localhost:8080/collections/bench/tiles/WebMercatorQuad/4/8/6.mvt"` — vector tile

## Exports

The server converts on demand too: `GET /collections/{id}/export?format=gpkg&filter=...` runs the normal pushdown, assembles one FlatGeobuf and converts it with the configured engine — `--rustgis-cli` for its native Rust set, `--ogr2ogr` for the long tail — then streams the file with the right media type. The admin token is required when configured. Tiles are served per-tile by the OGC endpoint at every zoom, filtered or not, so there are no batch archives to build or cache (rustgis-cli cannot write vector tiles; its tileset writer makes raster PNG pyramids).

Single-tile MVT is generated in-process (pure Go, clipped and simplified). For bulk conversion the server stays cgo-free and GDAL runs as a sidecar container:

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

Pre-alpha. Working today: chunk encode/decode, ingest with 10%/1% overview tiers, S2 multirange chunk pruning, fulltext (`?q=`), property filters, CQL2-text and CQL2-JSON filters, OGC API Features with next-link paging (50,000 default), MVT tiles with server-side filters and overview tiers below full detail, tileset and tile matrix set metadata, chunk manifest with checksums, immutable chunk caching, gzip on JSON, on-demand filtered exports through rustgis-cli or ogr2ogr, capability probing, hk git hooks. Benchmarked with GDAL as an OGC client on 1M features: full paged scan ~69s, single FlatGeobuf export 7s, bbox page ~2s, tiles ~0.6s. Next: CQL2 spatial operators, collection versioning for atomic rewrites, Aurora client numbers.

## License

Apache-2.0.
