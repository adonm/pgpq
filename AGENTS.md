# AGENTS.md

Read DESIGN.md before changing behavior — it is the contract. README describes what exists today.

## Commands

- `just ci` — gofmt check, vet, tidy check, unit tests (the hk hook runs this on every commit)
- `just pg-up` / `just pg-down` — PostgreSQL 18 via docker compose
- `just serve` — the OGC API server on :8080
- `just image-tar` — build the container image with ko

## Environment

- mise provides Go, just, hk and ko. Docker provides PostgreSQL and GDAL for verification.
- Verify with GDAL as an OGC client before calling any read-path work done: `ogrinfo`/`ogr2ogr` against `OAPIF:http://localhost:8080`, tiles through `/vsicurl_streaming/...mvt`. `ghcr.io/osgeo/gdal:ubuntu-full-latest` has the MVT driver.

## Rules

- No comments in Go; names and small functions carry meaning.
- Smallest reasonable implementation; stdlib first; a dependency must justify itself.
- No cgo; the core path requires no PostgreSQL extensions.
- Filters are enforced server-side, always; raw chunk and manifest endpoints stay behind the admin token.
- Migrations are append-only; never edit an applied migration.
- Behavior changes and DESIGN.md changes ship in the same commit.
