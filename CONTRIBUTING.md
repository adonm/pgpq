# Contributing

## Shape

`pgpq` is a small Go server for GeoParquet 2.0 over PostgreSQL. Keep the core cgo-free, prefer the standard library, and keep storage to one `bytea` GeoParquet file per cell.

## Checks

| Command | Purpose |
| --- | --- |
| `just ci` | gofmt, vet and unit tests |
| `just pg-up` / `just pg-down` | local PostgreSQL 18 |
| `just serve` | writable server on :8080 |
| `just test-integration` | integration tests against `PGPQ_TEST_DSN` |

DuckDB ingest needs the external `duckdb` binary; set `PGPQ_DUCKDB` when it is not on `PATH`.

## Rules

- GeoParquet geometry is native `GEOMETRY`/`GEOGRAPHY` with WKB and OGC:CRS84. Do not reintroduce FlatGeobuf or a hand-written spatial index.
- Parquet compression is internal zstd. Do not wrap the cell file in a second zstd frame.
- Verify `f=geoparquet` output with GDAL: save the response and run `ogrinfo` on it.
- Migrations are append-only; never edit an applied migration.
- Behavior changes and design-page changes ship in the same commit.

## License

Apache-2.0.
