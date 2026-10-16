# postfgb

OGC API Features and OGC Tiles over PostgreSQL, backed by FlatGeobuf chunks and index sidecars.

Goal: the smallest Go package that provides performant 1000x scaling of the spatial records a PostgreSQL database can hold, query, and return — by chunking records into FlatGeobuf and pushing filtering into index sidecars.

## How it works

- Features are encoded into FlatGeobuf chunks (packed Hilbert R-tree included) and stored as `bytea` rows.
- Chunk bounds are plain float columns plus an S2 cell covering stored as an `int8multirange`; a GiST index prunes candidate chunks with one range-overlap predicate.
- The app computes the S2 coverings in Go, so the database only compares ranges and floats: no geometry parsing, no PostGIS. Real spatial work happens in memory, over the FlatGeobuf Hilbert R-trees.
- Fulltext lives in a sidecar table (`tsvector`/GIN) populated at ingest; `?q=` intersects text matches with chunk and feature level spatial filtering.
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
```

## Endpoints

| Path | Content |
| --- | --- |
| `/` | landing page |
| `/conformance` | OGC API Features conformance classes |
| `/collections` | collections with extents |
| `/collections/{id}` | collection detail |
| `/collections/{id}/items?bbox=&limit=&q=` | GeoJSON features, bbox and fulltext filtered |
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

Pre-alpha. Working today: chunk encode/decode, ingest, S2 multirange chunk pruning, fulltext sidecar (`?q=`), OGC API Features, MVT tiles, capability probing, hk git hooks. Next: attribute filter pushdown via a `jsonb`/GIN sidecar, Aurora benchmark numbers.

## License

Apache-2.0.
