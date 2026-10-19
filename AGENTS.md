# AGENTS.md

The design page (`internal/api/design.html`, served at `/design`) is the contract: read it before changing behavior. README describes what exists today.

## Commands

- `just ci` — gofmt check, vet, tidy check, unit tests (the hk hook runs this on every commit)
- `just pg-up` / `just pg-down` — PostgreSQL 18 via docker compose
- `just serve` — the OGC API server on :8080
- `just image-tar` — build the container image with ko

## Environment

- mise provides Go, just, hk and ko. Docker provides PostgreSQL and GDAL for verification.
- Verify FlatGeobuf output with GDAL before calling any read-path work done: save `/v2/collections/{id}/items?f=flatgeobuf` and read it with `ogrinfo`. No OAPIF collection listing is served in the v2-only surface.

## Rules

- No comments in Go; names and small functions carry meaning.
- Smallest reasonable implementation; stdlib first; a dependency must justify itself.
- No cgo; the core path requires no PostgreSQL extensions.
- Filters are enforced server-side, always; writes are served in writable mode only. No auth in the app: it runs behind an adjacent auth layer, with readonly workers for public traffic.
- Migrations are append-only; never edit an applied migration.
- Behavior changes and design page changes ship in the same commit.
