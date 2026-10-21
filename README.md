# pgpq

GeoParquet 2.0 over PostgreSQL.

`pgpq` stores each spatial cell as a native GeoParquet 2.0 file split into 8,120-byte inline `bytea` chunks; Parquet pages are compressed with zstd, so there is no second compression layer. Geometry is Parquet `GEOMETRY` (WKB, OGC:CRS84), so Parquet's own row-group geospatial statistics do the spatial pruning.

The upload format is native GeoParquet 2.0. The server reads the Parquet file directly, splits it into level-8 S2 cells, and writes GeoParquet cells. It requires a native Parquet `GEOMETRY`/`GEOGRAPHY` column with WKB and an OGC:CRS84/EPSG:4326 CRS; GeoParquet 1.1 and DuckDB files are rejected. The core stays cgo-free and needs no DuckDB or GDAL at runtime.

DuckDB 1.5.6+ with the `spatial` extension can produce the upload files with `GEOPARQUET_VERSION 'V2'`. That is how the OSM benchmark prepares its input, outside the server process.

## Current surface

| Path | Content |
| --- | --- |
| `PUT /v2/collections/{id}` (writable) | replace a collection from a native GeoParquet 2.0 file; `id_column=` overrides the id, `cells=` scopes a load. |
| `POST /v2/collections/{id}` (writable) | merge/append into existing L8 cells; touched cells and their parent samples are rewritten and sorted |
| `GET /v2/collections/{id}/items` | `bbox`, CQL2-text property filters, `limit`, cursor paging, `overview=0\|2\|4\|6`, `numberMatched=true`, and `f=geoparquet` or `Accept: application/vnd.apache.parquet` |
| `GET /v2/collections/{id}/items/{featureID}` | one feature by stored id |
| `GET /healthz`, `/readyz` | probes |
| `GET /design` | the design contract |

JSON is the default response. `f=geoparquet` returns a filtered GeoParquet 2.0 page, one virtual GeoParquet response over the selected cells and overview level. That is the tile replacement path: clients ask for a bbox/overview and receive standard GeoParquet instead of vector tiles.

## Storage

- `pgpq.collections`: name, id, geometry column, schema.
- `pgpq.cells`: one row per collection/level/S2 cell; bounds, key range, zone, size and `cell_id`.
- `pgpq.cell_chunks`: `(cell_id, no)` primary key with 8,120-byte raw GeoParquet chunks, inline one 8 KB page (`toast_tuple_target=8160`, `STORAGE EXTERNAL`).
- `pgpq.ids`: hash and `(cell,row)` pointer for exact id lookup.
- Chunks are primary-key range reads. Parquet pages use zstd internally; chunks are never compressed again.
- One GeoParquet file per S2 L8 cell, row groups of 2,048, rows in `(__pgpq_key, id)` order. `POST` merges an insert into the affected cells; there are no small-file fragments to compact later.
- The GeoParquet rows are sorted by `(Hilbert key, id)`. A hidden `__pgpq_key` column drives cursor and row-group pruning.
- L8 is detail. L6, L4 and L2 keep up to 2,048 sampled features per cell and are built in the write commit.

## Read path

1. Prune cells by catalog bounds and zone.
2. Fetch and decompress only surviving cells.
3. Use Parquet row-group geospatial statistics for bbox and ordinary statistics for filters.
4. Read matching rows, evaluate exact bbox/filter in Go, and stop at the page limit.
5. Return JSON or a filtered GeoParquet 2.0 file.

The result is a virtual GeoParquet dataset per collection/overview without a separate tile pyramid or custom spatial index.

## Measured

Warm localhost, PostgreSQL 18.6, 6 vCPU, median of 30 requests:

| Request | Median | p95 | Response |
| --- | ---: | ---: | ---: |
| warm100k bbox → JSON | 4.3 ms | 6.1 ms | 1.1 kB |
| warm100k filter (32k matches) → JSON | 43.6 ms | 50.6 ms | 32 kB |
| warm100k id → JSON | 1.6 ms | 2.2 ms | 407 B |
| Luxembourg bbox → JSON | 10.5 ms | 15.0 ms | 33 kB |
| Monaco enriched bbox → JSON | 20.8 ms | 28.4 ms | 434 kB |
| Monaco enriched filter → JSON | 20.2 ms | 28.5 ms | 434 kB |
| L2 overview → JSON | 15.3 ms | 18.6 ms | 325 kB |
| Luxembourg bbox → GeoParquet | 53.5 ms | 63.1 ms | 101 kB |
| Monaco bbox, 1,000 rows → GeoParquet | 143.1 ms | 163.7 ms | 770 kB |

GeoParquet 2.0 ingest: Monaco layers upload in 0.15–0.21 s; Andorra layers in 0.20–0.43 s. A one-row append rewrote one Monaco cell and its parents in 0.86 s; repeating the same 4,628-row append kept the count at 4,628. The chunk table holds 13,343 rows of 8,120 bytes inline, with no TOAST indirection. Database size is about 212 MB.

## Heavy cell

One L8 cell with 1,000,000 points, generated in DuckDB:

| Operation | Result |
| --- | ---: |
| GeoParquet 2.0 upload | 19.9 s, 1.39 GB peak RSS |
| Stored detail cell | 10.0 MB, 489 row groups of 2,048 |
| bbox, 100 rows → JSON | 4.3 ms |
| bbox, 1,000 rows → JSON | 11.8 ms |
| attribute filter, 100 rows → JSON | 11.8 ms |
| id lookup | 10.5 ms |
| numberMatched over 1M rows | 1.9 s |
| overview L2 page | 8.5 ms |
| bbox, 100 rows → GeoParquet | 11.1 ms |
| one-row append (hourly batch) | **20.9 s, 1.39 GB peak; rewrites the 10 MB cell** |

The query numbers are good. The one-file-per-cell rewrite is acceptable under the intended **hourly batch** model: one `POST` per batch, each touched cell rewritten once. Do not append per row. A 1M-row cell moves 10 MB and takes ~21 s per batch; the catalog stays one row per cell with no fragment backlog. Fragment compaction remains optional if the update cadence tightens or cells grow much larger.

## OSM benchmark harness

`bench/osm/` loads one small OSM PBF as separate layers, one layer at a time. A capped DuckDB container extracts the OSM layers with the spatial extension and writes native GeoParquet 2.0; the Go worker uploads those files directly. The harness uses a disposable PostgreSQL database, a test worker on `:8094`, DuckDB-verifies every GeoParquet response, and cleans up through SQL. Start with [bench/osm/README.md](bench/osm/README.md).

```sh
bench/osm/serve.sh
bench/osm/run.sh --pbf monaco.osm.pbf --work /tmp/opencode/bench/osm-monaco --reps 10
```

Monaco and Andorra were measured end to end; the harness never targets public `:8080`/`:8081`. Luxembourg and Île-de-France are out of scope for routine runs.

## Quickstart

```sh
mise install
just pg-up
just serve
```

```sh
curl -T source.parquet -H 'Content-Type: application/vnd.apache.parquet' \
  'http://localhost:8080/v2/collections/buildings?id_column=bench_id'
curl 'http://localhost:8080/v2/collections/buildings/items?bbox=7.4,43.7,7.45,43.76&limit=100'
curl 'http://localhost:8080/v2/collections/buildings/items?bbox=7.4,43.7,7.45,43.76&limit=100&f=geoparquet' -o page.parquet
```

## Status

Pre-alpha. Working today: native GeoParquet 2.0 ingest, GeoParquet 2.0 native geometry/WKB storage, 8,120-byte inline GeoParquet chunks with internal zstd pages, S2 cell catalogs, Parquet row-group spatial and attribute pruning, L2/L4/L6 samples, id lookup, cursor paging, JSON/GeoParquet responses, readonly/writable workers and probes.

Not in this pass: collection listing/discovery, authentication, generated OpenAPI, and multi-geometry collections.
