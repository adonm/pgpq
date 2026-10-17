# Contributing

## Goal

The smallest Go package that provides performant 1000x scaling of the spatial records a PostgreSQL database can hold, query, and return — by chunking records into FlatGeobuf and pushing filtering into index sidecars.

Design decisions live in [DESIGN.md](DESIGN.md); the agent guide is [AGENTS.md](AGENTS.md).

## Development setup

Requirements: [mise](https://mise.jdx.dev) (installs Go, just, hk and ko per `mise.toml`) and Docker. Benchmarks use GDAL: a local install, or the bench script pulls `ghcr.io/osgeo/gdal:alpine-small-latest`.

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
| `just gen` | generate 1M synthetic features into `bench/features.geojson` |
| `just ingest` | upload `bench/features.fgb` to the running server over HTTP |
| `just serve` | run the OGC API server on `:8080` |
| `just image-tar` | build the container image with ko |
| `just demo` | pg-up + gen + ingest |
| `just bench` | GDAL client benchmark against a running server |
| `just layercake <layer> <bbox>` | stream an OSM Layercake GeoParquet layer into a collection over HTTP |

## Code style

- No comments. Names and small functions carry the meaning.
- Smallest reasonable implementation; stdlib first, one dependency must justify itself.
- The core path uses no PostgreSQL extensions: chunk pruning is an S2 cell `int8multirange` overlap plus scalar float comparisons. Never push geometry parsing or spatial library functions into the query path — real spatial work happens in Go over FlatGeobuf chunks. Optional accelerators (`pg_roaringbitmap`, `hll`, `pg_trgm`, ...) must be discovered at runtime via `pg_available_extensions` (see `store.Capabilities`) and never required.
- Chunk sizing, property encoding, and the FlatGeobuf wire format must stay interoperable with GDAL, QGIS, and the reference implementations. When in doubt, verify with `ogrinfo`.

## Testing

- Unit: `go test ./...` covers chunk round-trip (encode, search, property decode) and reads the upstream `poly_landmarks.fgb` fixture.
- Integration: `just pg-up && just demo && just serve`, then query the OGC endpoints.
- Benchmarks: `just bench` drives GDAL as an OGC client:
  - OAPIF via `ogrinfo "OAPIF:$URL"` — collection listing, full scan (`-so -al`), and server-side bbox filtering (`-spat`).
  - Tiles via `gdalinfo "/vsicurl/$URL/collections/.../tiles/WebMercatorQuad/{z}/{x}/{y}.mvt"`.
- Record benchmark numbers (feature count, chunk count, wall times) for any change that touches chunking, storage, query, or encoding.

## Benchmark methodology

`just gen` produces 1M point features; `just ingest` loads them as ~50k-feature chunks. The scaling claim to defend: as feature count grows, Postgres row count, chunk pruning cost, and query latency stay near constant. Report:
- features and chunks ingested,
- OAPIF full-scan wall time from GDAL,
- OAPIF `-spat` wall time,
- MVT tile wall time.

## Pull requests

- `just ci` must pass.
- One logical change per PR.
- Performance-affecting changes include benchmark output.
- Schema changes only in `internal/store/migrations/` as a new numbered file.

## License

Apache-2.0. Contributions are accepted under the same license. `testdata/poly_landmarks.fgb` is copied from the [FlatGeobuf repository](https://github.com/flatgeobuf/flatgeobuf) and licensed BSD-2-Clause.
