# AGENTS.md

The design page (`internal/api/design.html`, served at `/design`) is the contract: read it before changing behavior. README describes what exists today.

## Commands

- `just ci` — gofmt check, vet, tidy check, unit tests (the hk hook runs this on every commit)
- `just pg-up` / `just pg-down` — PostgreSQL 18 via docker compose
- `just serve` — the OGC API server on :8080
- `just image-tar` — build the container image with ko

## Environment

- mise provides Go, just, hk and ko. Docker provides PostgreSQL and the pinned DuckDB benchmark container.
- The server accepts native GeoParquet 2.0 only; it must not depend on DuckDB, GDAL or another converter at runtime.
- Verify GeoParquet output with GDAL or DuckDB before calling read-path work done: save `/v2/collections/{id}/items?f=geoparquet` and read it with `ogrinfo` or `read_parquet`.

## Rules

- No comments in Go; names and small functions carry meaning.
- Smallest reasonable implementation; stdlib first; a dependency must justify itself.
- No cgo; external tools are never linked into the server.
- Geometry is native Parquet GEOMETRY/GEOGRAPHY with WKB. Parquet pages use zstd; the cell `bytea` is not compressed again.
- Filters are enforced server-side, always; writes are served in writable mode only. No auth in the app: it runs behind an adjacent auth layer, with readonly workers for public traffic.
- Migrations are append-only; never edit an applied migration.
- Behavior changes and design page changes ship in the same commit.
