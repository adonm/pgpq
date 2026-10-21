# pgpq

Virtual GeoParquet 2.0 over PostgreSQL.

`pgpq` stores each collection as immutable native GeoParquet 2.0 shards and serves one virtual GeoParquet file per collection. A generated footer makes the shards addressable as a single standard Parquet file over HTTP `Range`; clients fetch only the row groups they need.

The upload format is native GeoParquet 2.0 only. Geometry must be a Parquet `GEOMETRY`/`GEOGRAPHY` with WKB and an OGC:CRS84/EPSG:4326 CRS. The server has no DuckDB, GDAL or other converter at runtime. DuckDB 1.5.6+ with the `spatial` extension can produce the upload files with `GEOPARQUET_VERSION 'V2'` outside the server.

## Current surface

| Path | Purpose |
| --- | --- |
| `PUT /v2/collections/{id}` (writable) | replace a collection from one native GeoParquet 2.0 file |
| `POST /v2/collections/{id}` (writable) | upsert a batch by id; rewrites only overlapping shards and splits oversized shards |
| `GET /v2/collections/{id}.parquet` | current version as one virtual Parquet file |
| `GET`/`HEAD` with `Range` | standard byte ranges, `206`, `Content-Range`, `ETag` |
| `GET /v2/collections/{id}` | collection metadata and virtual file URL |
| `GET /v2/collections` | collection list |
| `GET /healthz`, `/readyz` | probes |
| `GET /design` | the design contract |

There are no JSON feature responses, CQL filters, bboxes, overviews, id lookup routes, cursor paging, or client manifest. Clients use Parquet row-group statistics and do their own filtering.

## Storage

- `pgpq.collections`: name, current version, schema, rows and bytes.
- `pgpq.shards`: immutable complete GeoParquet files, target max 512 MiB.
- `pgpq.collection_shards`: version-to-shard references and virtual byte bases.
- `pgpq.shard_chunks`: `(shard_id,no)` bytea chunks for range access.
- `pgpq.virtual_files`: synthesized footer, length and ETag per version.
- `pgpq.ids`: a small hash/id-to-shard index used only to find old rows for an upsert.

Rows are sorted by `(spatial key, id)`. A `PUT` writes a new version. A `POST` reads only shards holding matching ids or overlapping key ranges, merges survivors with the batch, rewrites those shards, and leaves untouched shards referenced by the new version. Old versions remain readable; shard payloads are never rewritten in place.

## Read path

1. Resolve the current version and cached virtual footer.
2. Return `Content-Length`, `Accept-Ranges`, `ETag` and the requested byte ranges.
3. Map virtual offsets through `virtual_base` to shard data.
4. The client reads the footer, selects row groups using Parquet statistics, and fetches only those ranges.
5. The client decodes geometry and applies its own bbox/filter logic.

## Measured

Monaco city layers against a local writable worker on `:8094`, 2–3 measured requests after warmup:

| Layer | Rows | Shards | Upload | Virtual bytes | Footer | HEAD | Full GET | First range | Footer range |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| buildings | 4,628 | 1 | 0.19 s | 522,785 | 8,384 B | 2.2 ms | 6.1 ms | 2.1 ms | 2.5 ms |
| places | 4,928 | 1 | 0.17 s | 223,140 | ~8 KB | 1.9 ms | 6.1 ms | 2.1 ms | 2.0 ms |
| roads | 4,270 | 1 | 0.15 s | 382,937 | 8,459 B | 1.8 ms | 4.7 ms | 2.1 ms | 1.7 ms |

A test override of `PGPQ_SHARD_BYTES=150000` split the 4,628 buildings into 5 shards; the resulting 533,113-byte virtual file was read correctly by DuckDB and GDAL, and cross-shard Range responses matched the full file.

## OSM benchmark harness

`bench/osm/` loads one small OSM PBF at a time. A capped DuckDB container with the spatial extension reads the PBF, sorts by `ST_Hilbert`, and writes native GeoParquet 2.0 with 2,048-row groups. The Go worker uploads those files directly and the harness measures upload, full virtual download, `HEAD`, and Range requests, verifying the virtual file with DuckDB.

```sh
bench/osm/serve.sh
bench/osm/run.sh --pbf monaco.osm.pbf --work /tmp/opencode/bench/osm-monaco --reps 10
```

See [bench/osm/README.md](bench/osm/README.md).

## Quickstart

```sh
mise install
just pg-up
just serve
```

```sh
curl -X PUT -T source.parquet \
  -H 'Content-Type: application/vnd.apache.parquet' \
  'http://localhost:8080/v2/collections/buildings?id_column=bench_id'
curl -I 'http://localhost:8080/v2/collections/buildings.parquet'
curl -H 'Range: bytes=0-65535' \
  'http://localhost:8080/v2/collections/buildings.parquet' -o first.parquet
```

## Status

Pre-alpha. Working today: native GeoParquet 2.0 ingest, immutable shard versions, batch upsert, synthesized virtual Parquet files, HTTP Range/HEAD/ETag serving, DuckDB-produced OSM layers, readonly/writable workers and probes.

Not in this pass: server-side query, server-side filtering, authentication, compaction/GC of old versions, and multi-geometry collections.
