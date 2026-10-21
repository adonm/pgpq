# Small OSM virtual GeoParquet benchmark

A repeatable, low-resource harness for loading city-sized OpenStreetMap
extracts into pgpq as separate collections and measuring upload plus virtual
Parquet Range reads.

The harness uses one PBF at a time, one DuckDB container at a time, and one
HTTP request at a time. It never points at the public workers on
`:8080` / `:8081`; the default test worker is `http://127.0.0.1:8094`.

DuckDB is only a **producer** here. It reads the OSM PBF, sorts by
`ST_Hilbert`, and writes native GeoParquet 2.0 with
`GEOPARQUET_VERSION 'V2'`. The Go server receives those files directly and
has no DuckDB, GDAL or converter dependency.

## What it does

1. `prepare.sh` runs one capped DuckDB container per layer. The spatial
   extension supplies the GDAL OSM reader, `ST_Hilbert`, and the Parquet 2.0
   writer. It produces:

   - `buildings` from `multipolygons`
   - `places` from all `points`
   - `roads` from `lines`

2. Each output has a `bench_id` column (`OGC_FID`), the selected OSM fields,
   and a native `geometry` column in OGC:CRS84. Files use 2,048-row groups,
   zstd compression, and GeoParquet 2.0 metadata.
3. `bench.py` POSTs each file, then measures upload, `HEAD`, the full virtual
   download, a first-byte Range, and a footer-request Range against
   `/v2/collections/{id}.parquet`.
4. The downloaded virtual file is verified with DuckDB: row count and native
   `GeometryType` are checked before the result is accepted.
5. Each test collection is deleted from the test database after its layer.
   Nothing is deleted from the public database.

Layers are defined in `layers.tsv`. `OGC_FID` is unique within a DuckDB
`ST_Read` layer, so it is used as the benchmark id.

## Prerequisites

- Docker, with `duckdb/duckdb:1.5.6` available (the harness pulls it).
- Python 3 with only the standard library, and `curl` for the readiness probe.
- A PostgreSQL test database and a writable pgpq worker. The examples use the
  local Docker PostgreSQL created by `just pg-up`.
- `bin/pgpq` built from this checkout:

  ```sh
  go build -o bin/pgpq ./cmd/pgpq
  ```

`osmium` is not required. The DuckDB spatial extension includes GDAL's OSM
reader; `osmconf.ini` is mounted into the container. If a bbox clip is needed,
`osmium extract` before `prepare.sh` is the cheapest option; keep one small
filtered PBF.

DuckDB 1.5.6 is the pinned stable container. There is no official
`duckdb/duckdb:nightly` tag on Docker Hub; 1.5.6 already supports the
`GEOPARQUET_VERSION 'V2'` writer. Set `PGPQ_OSM_DUCKDB_IMAGE` to test another
image.

## Quick start

```sh
# Local PostgreSQL 18 on :5432, if it is not already up.
just pg-up

# One writable test worker on :8094. Run this in a second terminal.
bench/osm/serve.sh

# End to end: DuckDB prepare -> POST -> virtual-file checks -> cleanup.
bench/osm/run.sh \
  --pbf /tmp/opencode/bench/monaco.osm.pbf \
  --work /tmp/opencode/bench/osm-monaco \
  --reps 10
```

Results are written to:

```text
/tmp/opencode/bench/osm-monaco/report/report.md
/tmp/opencode/bench/osm-monaco/report/report.json
```

The prepared upload files are under `parquet/`; the downloaded virtual file
for each layer is written to `report/pages/{layer}.parquet`.

## Stage-by-stage commands

```sh
PBF=/tmp/opencode/bench/andorra.osm.pbf
WORK=/tmp/opencode/bench/osm-andorra

# 1. PBF -> one GeoParquet 2.0 file per layer, plus meta.json.
bench/osm/prepare.sh --pbf "$PBF" --work "$WORK"

# 2. Start the test worker (separate terminal).
bench/osm/serve.sh

# 3. Upload, measure, verify, delete each collection.
bench/osm/run.sh --pbf "$PBF" --work "$WORK" --skip-prepare --reps 10

# 4. Explicit cleanup if a run was interrupted.
bench/osm/cleanup.sh --prefix osm_
```

`prepare.sh` keeps existing Parquet files; add `--force` after changing
`layers.tsv` or the SQL. `run.sh --keep` leaves collections in place; use it
only with a disposable database.

## Exact test worker

`serve.sh` creates `pgpq_osm_bench` in the local Docker PostgreSQL container if
it does not exist, then runs:

```sh
GOMEMLIMIT=1GiB bin/pgpq serve \
  --addr 127.0.0.1:8094 \
  --dsn 'postgres://postgres:<password>@localhost:5432/pgpq_osm_bench?sslmode=disable' \
  --max-conns 2
```

It refuses a non-loopback address and ports `8080` / `8081`. The password is
read from `POSTGRES_PASSWORD`, or from the repository `.env` if that variable
is not already set. Override everything with `PGPQ_OSM_DSN`, `PGPQ_OSM_DB`,
`PGPQ_OSM_CONTAINER`, `PGPQ_OSM_PGUSER`, and `PGPQ_OSM_ADDR`.

## Resource caps

| Stage | Cap | Notes |
| --- | --- | --- |
| DuckDB prepare | `--memory 768m --cpus 1` | one layer per container |
| DuckDB verification | `--memory 512m --cpus 1` | one virtual file read |
| Server | `GOMEMLIMIT=1GiB`, `--max-conns 2` | Go soft heap limit |
| Benchmark | sequential | one layer, one request at a time |
| Cleanup | SQL delete | cascades collections, shards and chunks |

## Measured results

Local PostgreSQL 18 in Docker, local writable worker on `:8094`, 2–3 measured
requests per endpoint after one warmup. Times are median / p95 in milliseconds.

### Monaco (1.1 MB PBF)

| Layer | Features | Shards | Upload s | Virtual bytes | HEAD | Full GET | First range | Footer range |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| buildings | 4,628 | 1 | 0.19 | 522,785 | 2.2 | 6.1 / 6.1 | 2.1 / 2.1 | 2.5 / 2.7 |
| places | 4,928 | 1 | 0.17 | 223,140 | 1.9 | 6.1 / 6.1 | 2.1 / 2.1 | 2.0 / 2.0 |
| roads | 4,270 | 1 | 0.15 | 382,937 | 1.8 | 4.7 / 4.7 | 2.1 / 2.2 | 1.7 / 1.9 |

### Andorra (3.8 MB PBF)

| Layer | Features | Shards | Upload s | Virtual bytes | HEAD | Full GET | First range | Footer range |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| buildings | 7,638 | 1 | 0.25 | 838,041 | 1.4 | 8.9 / 8.9 | 2.0 / 2.3 | 2.1 / 2.1 |
| places | 8,961 | 1 | 0.30 | 382,001 | 2.3 | 5.6 / 5.6 | 1.8 / 2.1 | 2.0 / 2.0 |
| roads | 8,833 | 1 | 0.34 | 2,569,617 | 1.7 | 27.4 / 27.4 | 2.1 / 2.3 | 1.6 / 1.8 |

All checks passed: upload counts, version advancement, `HEAD` length,
`Accept-Ranges`, ETag consistency, full-download length, DuckDB reading of the
virtual file with a native `GeometryType`, `206` first-byte ranges, footer
ranges, and trailing `PAR1`.

The small city extracts collapse to one default shard. With a test override
of `PGPQ_SHARD_BYTES=150000`, the 4,628 Monaco buildings split into 5 shards;
the resulting 533,113-byte virtual file was read correctly by DuckDB and GDAL,
and cross-shard Range responses matched the full file byte-for-byte. The
512 MiB production target itself is not exercised by these city extracts. The
footer-size risk is the next benchmark to add with larger synthetic layers.

## Caveats

- **DuckDB output is the source of truth for 2.0.** The harness uses
  `GEOPARQUET_VERSION 'V2'`, which produces native Parquet geometry and
  per-row-group geospatial statistics. A `V1` or default export is not accepted.
- **CRS is normalized to OGC:CRS84 before writing.** OSM is already WGS 84, so
  `ST_SetCRS(geom, 'OGC:CRS84')` is enough. For other sources, transform or
  set the declared CRS in the producer before POSTing; the server only accepts
  the table CRS.
- **No query endpoints.** bbox/filter checks moved to DuckDB against the
  downloaded virtual file; the server returns bytes only.
- **No DELETE HTTP route.** Cleanup is a SQL delete from
  `pgpq.collections`; shards and chunks cascade. Use `--keep` only on a
  disposable database.
- **One run per test database at a time.** The harness deletes `osm_*`
  collections before and after each layer.
- **Luxembourg and Île-de-France are out of scope for routine runs.**

## Files

| File | Purpose |
| --- | --- |
| `layers.tsv` | layer names, OSM layers, filters, selected columns, id SQL |
| `osmconf.ini` | minimal GDAL OSM configuration mounted into DuckDB |
| `prepare.sh` | PBF -> GeoParquet 2.0 + `meta.json` |
| `bench.py` | POST, virtual-file Range timing, DuckDB verification, cleanup, report |
| `run.sh` | one-command orchestration |
| `serve.sh` | isolated writable test worker on `:8094` |
| `cleanup.sh` | SQL cleanup for test collections |
| `lib.sh` | shared path, name, and psql helpers |
