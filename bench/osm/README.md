# Small OSM layer benchmark

A repeatable, low-resource harness for loading a city-sized OpenStreetMap
extract into pgpq as separate layers and timing ingest and query quality.

The harness uses one PBF at a time, one GDAL layer at a time, one DuckDB build
at a time, and one HTTP request at a time. It never points at the public
workers on `:8080` / `:8081`; the default test worker is
`http://127.0.0.1:8094`.

## What it does

1. `prepare.sh` uses the GDAL OSM driver to convert one PBF into one
   GeoParquet 2.0 file per layer:
   `buildings`, `places` (all OSM points), and `roads`.
2. `build.sh` loads those files into one self-contained DuckDB database and
   creates `layer_catalog`:

   ```sql
   layer_name | view_name       | id_column | geom_column
   osm_buildings | layer_buildings | bench_id | geometry
   osm_places    | layer_places    | bench_id | geometry
   osm_roads     | layer_roads     | bench_id | geometry
   ```

3. `bench.py` POSTs that DuckDB file once per layer, then measures bbox,
   CQL2 filter, id, GeoParquet, overview 6/4/2, overview GeoParquet, and
   `numberMatched`.
4. Every GeoParquet response is checked with GDAL (`ogrinfo`) before it is
   accepted.
5. Each test collection is deleted from the test database after its layer.
   Nothing is deleted from the public database.

Layers are defined in `layers.tsv`. The composite building id is
`b:<osm_id>:<osm_way_id>` because the OSM driver leaves `osm_id` null for many
closed-way multipolygons. Point and line ids are `p:<osm_id>` and `r:<osm_id>`.

## Prerequisites

- Docker, for `ghcr.io/osgeo/gdal:ubuntu-full-latest`.
- DuckDB 1.5+ CLI with the `spatial` extension (the binary used here is
  `/tmp/opencode/bench/duckdb-bin/duckdb`).
- Python 3 with only the standard library, and `curl` for the readiness probe.
- A PostgreSQL test database and a writable pgpq worker. The examples use the
  local Docker PostgreSQL created by `just pg-up`.
- `bin/pgpq` built from this checkout:

  ```sh
  go build -o bin/pgpq ./cmd/pgpq
  ```

`osmium` is not required. GDAL reads `.osm.pbf` directly. If a bbox clip is
needed, `osmium extract` before `prepare.sh` is the cheapest option; keep one
small filtered PBF.

## Quick start

```sh
export PGPQ_DUCKDB=/tmp/opencode/bench/duckdb-bin/duckdb

# Local PostgreSQL 18 on :5432, if it is not already up.
just pg-up

# One writable test worker on :8094. Run this in a second terminal.
bench/osm/serve.sh

# End to end: prepare -> DuckDB -> POST/benchmark -> cleanup.
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

The DuckDB upload file is:

```text
/tmp/opencode/bench/osm-monaco/monaco.duckdb
```

## Stage-by-stage commands

This is useful when changing only one stage.

```sh
export PGPQ_DUCKDB=/tmp/opencode/bench/duckdb-bin/duckdb
PBF=/tmp/opencode/bench/andorra.osm.pbf
WORK=/tmp/opencode/bench/osm-andorra

# 1. PBF -> GeoParquet. Sequential Docker runs, 768 MB / 1 CPU each.
bench/osm/prepare.sh --pbf "$PBF" --work "$WORK"

# 2. GeoParquet -> self-contained DuckDB + layer_catalog.
bench/osm/build.sh --work "$WORK" --extract andorra --prefix osm_

# 3. Start the test worker (separate terminal).
bench/osm/serve.sh

# 4. Upload, measure, verify, delete each collection.
bench/osm/run.sh --pbf "$PBF" --work "$WORK" --skip-prepare --reps 10

# 5. Explicit cleanup if a run was interrupted.
bench/osm/cleanup.sh --prefix osm_
```

`prepare.sh` keeps existing parquet files; add `--force` after changing
`layers.tsv`. `run.sh` always rebuilds the DuckDB file from the parquet files.
`run.sh --keep` leaves collections in place; use it only with a disposable
database.

## Exact test worker

`serve.sh` creates `pgpq_osm_bench` in the local Docker PostgreSQL container if
it does not exist, then runs:

```sh
GOMEMLIMIT=1GiB PGPQ_DUCKDB=... bin/pgpq serve \
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
| GDAL Docker | `--memory 768m --cpus 1` | one layer per container, removed after each run |
| DuckDB build | `memory_limit=512MB`, `threads=1` | `<work>/duckdb-tmp` spill directory |
| Server | `GOMEMLIMIT=1GiB`, `--max-conns 2` | Go soft heap limit, not a hard RSS cap |
| Benchmark | sequential | one layer, one request at a time |
| Cleanup | SQL delete | requires psql access to the test database |

Measured peak for the DuckDB build: **79 MiB** (Monaco) and **86 MiB**
(Andorra). One Andorra upload while sampling the worker's `/proc` RSS peaked at
**228 MiB**. These are small enough that the 4 MB Andorra extract is the
recommended upper end for routine runs.

The pgpq server invokes the DuckDB CLI itself for a `COPY`; that child process
does not inherit `GOMEMLIMIT`. The small-layer design keeps that export
bounded. Do not feed a country extract to this harness without raising the
caps intentionally.

## Measured results

Local PostgreSQL 18 in Docker, local writable worker on `:8094`, 10 measured
requests per endpoint after one warmup, JSON limit 100, GeoParquet limit 1000,
central 25% of each layer's robust 5-95% centroid extent. Times are
median / p95 in milliseconds.

### Monaco (1.1 MB PBF)

DuckDB file 4.0 MiB; build 0.38 s, peak 79 MiB.

| Layer | Features | Cells | Upload s | bbox | filter | id | GeoParquet | overview 6 | overview GeoParquet | numberMatched |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| buildings | 4,628 | 3 | 0.25 | 4.3 / 7.0 | 3.8 / 5.7 | 2.3 / 4.2 | 33.6 / 43.8 | 3.3 / 3.9 | 12.7 / 22.1 | 2.7 / 2.9 |
| places | 4,928 | 7 | 0.28 | 8.8 / 16.4 | 25.9 / 36.3 | 2.6 / 4.0 | 26.8 / 42.5 | 3.4 / 7.2 | 19.2 / 28.1 | 2.5 / 2.7 |
| roads | 4,270 | 4 | 0.23 | 7.7 / 9.6 | 4.3 / 5.0 | 2.5 / 3.1 | 30.8 / 34.0 | 4.3 / 6.1 | 12.8 / 22.2 | 3.0 / 3.4 |

### Andorra (4.0 MB PBF)

DuckDB file 10.3 MiB; build 0.43 s, peak 86 MiB.

| Layer | Features | Cells | Upload s | bbox | filter | id | GeoParquet | overview 6 | overview GeoParquet | numberMatched |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| buildings | 7,638 | 6 | 0.31 | 5.3 / 7.6 | 3.6 / 4.0 | 2.6 / 3.2 | 24.9 / 30.9 | 5.0 / 8.9 | 9.3 / 13.6 | 2.7 / 2.8 |
| places | 8,961 | 8 | 0.40 | 4.7 / 6.0 | 21.8 / 23.8 | 2.3 / 2.9 | 35.8 / 46.3 | 3.1 / 4.8 | 22.8 / 27.8 | 2.5 / 2.9 |
| roads | 8,833 | 8 | 0.61 | 7.5 / 8.1 | 6.1 / 8.6 | 2.6 / 9.5 | 45.7 / 62.7 | 8.8 / 20.4 | 40.5 / 58.3 | 3.1 / 3.4 |

All checks passed: upload counts, exact `numberMatched`, bbox containment,
filter field non-nullness, id round-trip, and GDAL reading of both GeoParquet
pages. Overview sample counts show the expected pixel-size sampling: Monaco
buildings return 47 rows at L6 and 0 at L4/L2; roads return 100 at L6, 31 at
L4, 0 at L2. This is the documented `sampleTooSmall` behavior, not a missing
cell.

## Caveats

- **CRS warning from DuckDB 1.5.6.** DuckDB prints
  `GEOMETRY columns ... CRS will not be persisted` for storage v1.0. The
  geometry still round-trips as WKB, and the server's GeoParquet output carries
  OGC:CRS84; both pages were verified with GDAL. The harness sets
  `ST_SetCRS(..., 'OGC:CRS84')` so the in-session type is explicit.
- **No DELETE HTTP route.** Cleanup is a SQL delete from
  `pgpq.collections`; foreign keys cascade to cells, chunks, and ids. Without
  psql access to the test database, use `--keep` only on a disposable server.
  If the worker uses a database other than `pgpq_osm_bench`, set
  `PGPQ_OSM_DB` or `PGPQ_OSM_PSQL` so cleanup targets the same database.
- **`--keep` on a populated server appends.** Use cleanup or a fresh test
  database before rerunning with the same `osm_` prefix.
- **Server DuckDB export is not capped by `GOMEMLIMIT`.** `pgpq` shells out to
  DuckDB for `COPY (select * from view)`. Keep layers city-sized and run one
  ingest at a time.
- **`places` is all OSM points, not only `place=*`.** That gives a useful
  point layer to filter (4,928 points in Monaco) while the filter check uses
  `place IS NOT NULL` (13 rows in Monaco). Change `layers.tsv` if a denser
  filter target is wanted.
- **L4/L2 samples can be empty for small features.** `sampleTooSmall` drops
  features below one display pixel at that overview level.
- **One run per test database at a time.** The harness deletes `osm_*`
  collections before and after each layer; two concurrent runs with the same
  prefix would delete each other's data.
- **Luxembourg and Île-de-France are out of scope for routine runs.** The 56 MB
  and 387 MB extracts should use larger caps and a dedicated test database, or
  be clipped first.
- **GDAL Parquet 2.0 loses CRS in DuckDB 1.5.6.** This is why the intermediate
  is read with `ST_SetCRS` and why the final GeoParquet is verified with GDAL
  rather than trusted from metadata alone.

## Files

| File | Purpose |
| --- | --- |
| `layers.tsv` | layer names, OSM layers, filters, selected columns, id SQL |
| `prepare.sh` | PBF -> GeoParquet 2.0 with capped GDAL Docker runs |
| `build.sh` | GeoParquet -> DuckDB tables + `layer_catalog` + metadata |
| `bench.py` | POST, timing, GeoParquet verification, cleanup, report |
| `run.sh` | one-command orchestration |
| `serve.sh` | isolated writable test worker on `:8094` |
| `cleanup.sh` | SQL cleanup for test collections |
| `lib.sh` | shared path, name, and psql helpers |
