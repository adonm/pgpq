# postfgb

OGC API Features and OGC Tiles over PostgreSQL, backed by FlatGeobuf chunks and index sidecars.

Goal: the smallest Go package that provides performant 1000x scaling of the spatial records a PostgreSQL database can hold, query, and return — by chunking records into FlatGeobuf and pushing filtering into index sidecars.

## How it works

- Features are encoded into FlatGeobuf chunks (packed Hilbert R-tree included) and stored as `bytea` rows.
- Chunk metadata (bounds, feature count) lives in plain columns; a PostGIS GiST index prunes chunks per query.
- Bounding-box filtering is two-level: chunk pruning in Postgres, feature pruning inside the chunk's R-tree.
- Fulltext and attribute filtering move to sidecar tables (`tsvector`/GIN, `jsonb`/GIN, `pg_trgm`) so Postgres stays the only moving part.
- The server is one small Go binary: huma for the OGC metadata endpoints, streaming handlers for items, tiles, and raw chunks.

Everything is built against the Amazon Aurora PostgreSQL extension allowlist, with runtime capability probing for optional accelerators.

## Quickstart

Requires [mise](https://mise.jdx.dev) and Docker (or Go 1.27+ and [just](https://github.com/casey/just)).

```sh
mise install
just demo     # postgis via docker, 1M features generated and ingested
just serve    # http://localhost:8080
```

```sh
curl localhost:8080/collections
curl "localhost:8080/collections/bench/items?bbox=-10,45,10,55&limit=100"
```

## Endpoints

| Path | Content |
| --- | --- |
| `/` | landing page |
| `/conformance` | OGC API Features conformance classes |
| `/collections` | collections with extents |
| `/collections/{id}` | collection detail |
| `/collections/{id}/items?bbox=&limit=` | GeoJSON features |
| `/collections/{id}/chunks/{chunkID}` | raw FlatGeobuf chunk |
| `/collections/{id}/tiles/WebMercatorQuad/{z}/{x}/{y}` | MVT tile |
| `/openapi.json` | OpenAPI definition |

## Benchmarks

`bench/run.sh` uses GDAL as an OGC client:

- `ogrinfo "OAPIF:http://localhost:8080"` — collection listing
- `ogrinfo -so -al "OAPIF:http://localhost:8080" bench` — full scan
- `ogrinfo -so -al -spat -10 45 10 55 "OAPIF:http://localhost:8080" bench` — server-side bbox filter
- `gdalinfo "/vsicurl/http://localhost:8080/collections/bench/tiles/WebMercatorQuad/0/0/0.mvt"` — vector tile

## Status

Pre-alpha. Working today: chunk encode/decode, ingest, chunk pruning, OGC API Features, MVT tiles, capability probing. Next: fulltext sidecar population and query path, attribute filter pushdown, Aurora benchmark numbers.

## License

Apache-2.0.
