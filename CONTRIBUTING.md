# Contributing

## Goal

The smallest Go package that provides performant 1000x scaling of the spatial records a PostgreSQL database can hold, query, and return — by chunking records into FlatGeobuf and pruning reads with per-chunk statistics.

Design decisions live in the design page (`internal/api/design.html`, served at `/design`); the agent guide is [AGENTS.md](AGENTS.md).

## Development setup

Requirements: [mise](https://mise.jdx.dev) (installs Go, just, hk and ko per `mise.toml`) and Docker.

```sh
mise install
hk install --mise   # wire the pre-commit hook
just pg-up
just ci
```

No mise? Go 1.27+ and [just](https://github.com/casey/just) work the same.

## Recipes

Run `just` for the full list. The common ones:

| Recipe | Purpose |
| --- | --- |
| `just ci` | gofmt check, vet, unit tests |
| `just pg-up` / `just pg-down` | PostgreSQL 18 via docker compose |
| `just serve` | run the server on `:8080` |
| `just serve-readonly` | run the read-only worker on `:8080` |
| `just test-integration` | integration tests against `POSTFGB_TEST_DSN` |
| `just image-tar` | build the container image with ko |

## Code style

- No comments. Names and small functions carry the meaning.
- Smallest reasonable implementation; stdlib first, one dependency must justify itself.
- The core path uses no PostgreSQL extensions. Do not add optional extension accelerators (`pg_roaringbitmap`, `hll`, `pg_trgm`, ...): nothing probes for them, and the core path must never need one.
- Chunk sizing, property encoding, and the FlatGeobuf wire format must stay interoperable with GDAL, QGIS, and the reference implementations. When in doubt, verify with `ogrinfo`.

## Testing

- Unit: `go test ./...` covers FlatGeobuf chunk round-trips, attribute encoding, CQL evaluation, spatial indexing and ingest flattening.
- Integration: `just pg-up && just test-integration` runs the database-backed layers and API tests against the migrations in `internal/store/migrations/`.
- The v2 routes are `PUT /v2/collections/{id}` and `GET /v2/collections/{id}/items...`; there is no OAPIF collection listing yet, so GDAL verification is limited to FlatGeobuf pages.

## Pull requests

- `just ci` must pass.
- One logical change per PR.
- Performance-affecting changes include benchmark output.
- Schema changes only in `internal/store/migrations/` as a new numbered file; migrations are append-only.

## License

Apache-2.0. Contributions are accepted under the same license. `testdata/poly_landmarks.fgb` is copied from the [FlatGeobuf repository](https://github.com/flatgeobuf/flatgeobuf) and licensed BSD-2-Clause.
