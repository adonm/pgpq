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

The server accepts native GeoParquet 2.0 only. The OSM benchmark uses a pinned DuckDB container to produce those files outside the server.

## Rules

- GeoParquet geometry is native `GEOMETRY`/`GEOGRAPHY` with WKB and OGC:CRS84. Do not add a sidecar spatial index.
- Parquet compression is internal zstd. Do not wrap the cell file in a second zstd frame.
- Verify `f=geoparquet` output with GDAL or DuckDB: save the response and open it with `ogrinfo` or `read_parquet`.
- Migrations are append-only; never edit an applied migration.
- Behavior changes and design-page changes ship in the same commit.

## License

Apache-2.0.
