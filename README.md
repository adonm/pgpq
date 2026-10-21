# pgpq

GeoParquet 2.0 over PostgreSQL.

`pgpq` stores each spatial cell as a native GeoParquet 2.0 file in one `bytea`; Parquet pages are compressed with zstd, so there is no second compression layer. Geometry is Parquet `GEOMETRY` (WKB, OGC:CRS84), so Parquet's own row-group geospatial statistics do the spatial pruning. There is no FlatGeobuf sidecar and no custom spatial index.

The upload format is a DuckDB file. A view in the file becomes one collection; `layer_catalog` can supply the view, id column and geometry column. The server exports the view with the DuckDB CLI, splits it into level-8 S2 cells, and writes GeoParquet cells. The core stays cgo-free; DuckDB is an external command, configured by `PGPQ_DUCKDB`.

GeoParquet 2.0 input is also accepted directly for loader compatibility.

## Current surface

| Path | Content |
| --- | --- |
| `PUT /v2/collections/{id}` (writable) | load a DuckDB file; `view=` selects a view, `id_column=` overrides the id, `cells=` scopes a load. GeoParquet 2.0 bodies are accepted directly. |
| `GET /v2/collections/{id}/items` | `bbox`, CQL2-text property filters, `limit`, cursor paging, `overview=0\|2\|4\|6`, `numberMatched=true`, and `f=geoparquet` or `Accept: application/vnd.apache.parquet` |
| `GET /v2/collections/{id}/items/{featureID}` | one feature by stored id |
| `GET /healthz`, `/readyz` | probes |
| `GET /design` | the design contract |

JSON is the default response. `f=geoparquet` returns a filtered GeoParquet 2.0 page, one virtual GeoParquet response over the selected cells and overview level. That is the tile replacement path: clients ask for a bbox/overview and receive standard GeoParquet instead of vector tiles.

## Storage

- `pgpq.collections`: name, id, geometry column, schema.
- `pgpq.cells`: one row per collection/level/S2 cell; bounds, key range, zone, and `data bytea`.
- `pgpq.ids`: hash and `(cell,row)` pointer for exact id lookup.
- `data` is the GeoParquet file itself with internal zstd page compression, forced to `STORAGE EXTERNAL` so Postgres does not compress it again. Small cells are fetched whole; large cells are read as 64 KiB `substring` ranges from external TOAST.
- The GeoParquet rows are sorted by `(Hilbert key, id)`. A hidden `__pgpq_key` column drives cursor and row-group pruning.
- L8 is detail. L6, L4 and L2 keep up to 2,048 sampled features per cell and are built in the write commit.

## Read path

1. Prune cells by catalog bounds and zone.
2. Fetch and decompress only surviving cells.
3. Use Parquet row-group geospatial statistics for bbox and ordinary statistics for filters.
4. Read matching rows, evaluate exact bbox/filter in Go, and stop at the page limit.
5. Return JSON or a filtered GeoParquet 2.0 file.

The result is a virtual GeoParquet dataset per collection/overview without a separate tile pyramid or FGB index.

## Measured

Warm localhost, PostgreSQL 18.6, 6 vCPU, median of 30 requests:

| Request | Median | p95 | Response |
| --- | ---: | ---: | ---: |
| warm100k bbox → JSON | 4.1 ms | 5.1 ms | 1.1 kB |
| warm100k filter (32k matches) → JSON | 40.1 ms | 45.0 ms | 32 kB |
| warm100k id → JSON | 1.6 ms | 1.8 ms | 407 B |
| Luxembourg bbox → JSON | 12.2 ms | 16.3 ms | 33 kB |
| Monaco enriched bbox → JSON | 21.1 ms | 26.7 ms | 434 kB |
| Monaco enriched filter → JSON | 23.5 ms | 38.2 ms | 434 kB |
| L2 overview → JSON | 17.5 ms | 21.5 ms | 325 kB |
| Luxembourg bbox → GeoParquet | 53.8 ms | 60.4 ms | 101 kB |
| Monaco bbox, 1,000 rows → GeoParquet | 143.5 ms | 162.0 ms | 770 kB |

DuckDB ingest: Monaco 12.6 MB / 4,628 features in 1.24–1.27 s; Luxembourg 48.2 MB / 228,273 features in 8.18 s. A 64-byte external-TOAST slice touched 8 buffers versus 142 for the whole cell.

## Quickstart

```sh
mise install
just pg-up
PGPQ_DUCKDB=/path/to/duckdb just serve
```

```sh
curl -T source.duckdb -H 'Content-Type: application/vnd.duckdb' \
  'http://localhost:8080/v2/collections/buildings?view=layer_buildings&id_column=bench_id'
curl 'http://localhost:8080/v2/collections/buildings/items?bbox=7.4,43.7,7.45,43.76&limit=100'
curl 'http://localhost:8080/v2/collections/buildings/items?bbox=7.4,43.7,7.45,43.76&limit=100&f=geoparquet' -o page.parquet
```

## Status

Pre-alpha. Working today: DuckDB and GeoParquet ingest, GeoParquet 2.0 native geometry/WKB storage, zstd-compressed Parquet bytea cells, S2 cell catalogs, Parquet row-group spatial and attribute pruning, L2/L4/L6 samples, id lookup, cursor paging, JSON/GeoParquet responses, readonly/writable workers and probes.

Not in this pass: collection listing/discovery, authentication, generated OpenAPI, and multi-geometry collections.
