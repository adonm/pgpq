# Small OSM layer benchmark

A repeatable, low-resource harness for loading a city-sized OpenStreetMap
extract into pgpq as separate layers and timing ingest and query quality.

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
   zstd compression, and the GeoParquet 2.0 `geo` metadata.
3. `bench.py` POSTs one file per collection, then measures bbox, CQL2 filter,
   id, GeoParquet, overview 6/4/2, overview GeoParquet, and `numberMatched`.
4. Every GeoParquet response is read back with DuckDB: row count and native
   `GeometryType` are checked before the result is accepted.
5. Each test collection is deleted from the test database after its layer.
   Nothing is deleted from the public database.

Layers are defined in `layers.tsv`. `OGC_FID` is unique within a DuckDB
`ST_Read` layer, so it is used as the benchmark id instead of trying to build a
composite OSM id from nullable `osm_id`/`osm_way_id` fields.

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

# End to end: DuckDB prepare -> POST/benchmark -> cleanup.
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

The prepared upload files are the per-layer Parquet files under:

```text
/tmp/opencode/bench/osm-monaco/parquet/
```

## Stage-by-stage commands

This is useful when changing only one stage.

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
`layers.tsv` or the SQL. `run.sh` reuses the existing `meta.json` when
`--skip-prepare` is set. `run.sh --keep` leaves collections in place; use it
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

There is no `PGPQ_DUCKDB` server setting and no DuckDB child process in the
worker.

## Resource caps

| Stage | Cap | Notes |
| --- | --- | --- |
| DuckDB prepare | `--memory 768m --cpus 1` | one layer per container, removed after each run |
| DuckDB verification | `--memory 512m --cpus 1` | one read per GeoParquet page |
| Server | `GOMEMLIMIT=1GiB`, `--max-conns 2` | Go soft heap limit, not a hard RSS cap |
| Benchmark | sequential | one layer, one request at a time |
| Cleanup | SQL delete | requires psql access to the test database |

Andorra preparation took 11.3 s end to end for the three layers on this host.
The 4 MB Andorra extract is the recommended upper end for routine runs.

## Measured results

Local PostgreSQL 18 in Docker, local writable worker on `:8094`, 3 measured
requests per endpoint after one warmup, JSON limit 100, GeoParquet limit 1000,
central 25% of each layer's robust 5-95% centroid extent. Times are median /
p95 in milliseconds.

### Monaco (1.1 MB PBF)

| Layer | Features | Cells | Upload s | bbox | filter | id | GeoParquet | overview 6 | overview GeoParquet | numberMatched |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| buildings | 4,628 | 3 | 0.15 | 4.4 / 4.6 | 4.0 / 4.4 | 2.4 / 2.9 | 29.4 / 32.7 | 3.4 / 4.6 | 10.1 / 12.0 | 2.9 / 2.9 |
| places | 4,928 | 7 | 0.21 | 8.3 / 9.7 | 15.0 / 16.2 | 2.1 / 2.3 | 23.3 / 27.2 | 2.9 / 3.3 | 18.5 / 20.3 | 3.1 / 3.3 |
| roads | 4,270 | 4 | 0.13 | 6.1 / 6.4 | 3.7 / 3.7 | 2.4 / 2.5 | 22.5 / 24.0 | 4.3 / 5.8 | 10.8 / 11.7 | 2.6 / 2.8 |

### Andorra (3.8 MB PBF)

| Layer | Features | Cells | Upload s | bbox | filter | id | GeoParquet | overview 6 | overview GeoParquet | numberMatched |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| buildings | 7,638 | 6 | 0.20 | 5.2 / 5.2 | 3.7 / 3.8 | 2.4 / 3.4 | 21.4 / 21.9 | 5.2 / 5.4 | 9.6 / 13.5 | 2.9 / 3.5 |
| places | 8,961 | 8 | 0.28 | 3.5 / 3.9 | 15.6 / 17.7 | 2.1 / 2.5 | 22.3 / 28.7 | 3.1 / 3.1 | 18.8 / 20.6 | 2.3 / 2.7 |
| roads | 8,833 | 8 | 0.43 | 5.9 / 5.9 | 6.3 / 8.0 | 2.5 / 2.7 | 32.6 / 39.1 | 6.7 / 7.1 | 33.5 / 41.4 | 2.7 / 2.7 |

All checks passed: upload counts, exact `numberMatched`, bbox containment,
filter field non-nullness, id round-trip, and DuckDB reading of both
GeoParquet pages with a native `GeometryType`. Overview sample counts show the
expected pixel-size sampling: Monaco buildings return 47 rows at L6 and 0 at
L4/L2; roads return 100 at L6, 31 at L4, 0 at L2. This is the documented
`sampleTooSmall` behavior, not a missing cell.

## Caveats

- **DuckDB output is the source of truth for 2.0.** The harness uses
  `GEOPARQUET_VERSION 'V2'`, which produces native Parquet geometry and
  per-row-group geospatial statistics. A `V1` or default export is not accepted
  by the server.
- **CRS is normalized to OGC:CRS84 before writing.** OSM is already WGS 84, so
  `ST_SetCRS(geom, 'OGC:CRS84')` is enough. For other sources, transform or
  set the declared CRS in the producer before POSTing; the server only accepts
  the table CRS.
- **No DELETE HTTP route.** Cleanup is a SQL delete from
  `pgpq.collections`; foreign keys cascade to cells, chunks, and ids. Without
  psql access to the test database, use `--keep` only on a disposable server.
- **`--keep` on a populated server appends.** Use cleanup or a fresh test
  database before rerunning with the same `osm_` prefix.
- **`places` is all OSM points, not only `place=*`.** That gives a useful
  point layer to filter while the filter check uses `place IS NOT NULL`.
- **L4/L2 samples can be empty for small features.** `sampleTooSmall` drops
  features below one display pixel at that overview level.
- **One run per test database at a time.** The harness deletes `osm_*`
  collections before and after each layer; two concurrent runs with the same
  prefix would delete each other's data.
- **Luxembourg and Île-de-France are out of scope for routine runs.** The 54 MB
  and 370 MB extracts should use larger caps and a dedicated test database, or
  be clipped first.

## Files

| File | Purpose |
| --- | --- |
| `layers.tsv` | layer names, OSM layers, filters, selected columns, id SQL |
| `osmconf.ini` | minimal GDAL OSM configuration mounted into DuckDB |
| `prepare.sh` | PBF -> GeoParquet 2.0 + `meta.json` with one capped DuckDB container per layer |
| `bench.py` | POST, timing, DuckDB verification, cleanup, report |
| `run.sh` | one-command orchestration |
| `serve.sh` | isolated writable test worker on `:8094` |
| `cleanup.sh` | SQL cleanup for test collections |
| `lib.sh` | shared path, name, and psql helpers |
