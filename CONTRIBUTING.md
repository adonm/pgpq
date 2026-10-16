# Contributing

## Goal

The smallest Go package that provides performant 1000x scaling of the spatial records a PostgreSQL database can hold, query, and return — by chunking records into FlatGeobuf and pushing filtering into index sidecars.

## Development setup

Requirements: [mise](https://mise.jdx.dev) (installs Go and just per `mise.toml`), Docker, and optionally GDAL (`apt install gdal-bin`) for benchmarks.

```sh
mise install
just pg-up
just ci
```

No mise? Go 1.27+ and [just](https://github.com/casey/just) work the same.

## Recipes

Run `just` for the full list. The common ones:

| Recipe | Purpose |
| --- | --- |
| `just ci` | gofmt check, vet, unit tests |
| `just pg-up` / `just pg-down` | local PostGIS via docker compose |
| `just gen` | generate 1M synthetic features into `bench/features.geojson` |
| `just ingest` | chunk and load the generated file |
| `just serve` | run the OGC API server on `:8080` |
| `just demo` | pg-up + gen + ingest |
| `just bench` | GDAL client benchmark against a running server |

## Code style

- No comments. Names and small functions carry the meaning.
- Smallest reasonable implementation; stdlib first, one dependency must justify itself.
- Keep the core path within the Amazon Aurora PostgreSQL extension allowlist: `postgis`, `pg_trgm`, `pg_bigm`, `unaccent`, `btree_gin`, `btree_gist`, `bloom`, `hll`, `pg_roaringbitmap` (PG18 only), `pgvector`, `h3-pg`. Anything outside that list must be an optional accelerator discovered at runtime via `pg_available_extensions` (see `store.Capabilities`), never a hard requirement.
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
