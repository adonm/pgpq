# postfgb

OGC API Features over PostgreSQL, backed by FlatGeobuf cell files stored as database blocks.

Goal: the smallest Go package that provides performant 1000x scaling of the spatial records a PostgreSQL database can hold, query, and return — by partitioning records into FlatGeobuf cell files and pruning reads with R-trees, key fences and per-cell statistics.

The target deployment is [RustGIS](https://rustgis.app/): reads are served over HTTP at maximum speed, while writes are batch-oriented and free to sort and rewrite cell files.

Design decisions are recorded in the design page served at `/design` (`internal/api/design.html`), which is the source of truth. The repository is mid-migration to that v2 design: only the v2 read and write routes below are served today.

## Current surface

| Path | Content |
| --- | --- |
| `PUT /v2/collections/{id}` (writable) | create or replace a collection from a FlatGeobuf body, reprojected to WGS84; an id column is required (`id`, `fid`, `gid`, `ogc_fid`, or `id_column=`); JSON columns are flattened into typed columns by default (`flatten=off` disables); `cells=` scopes a load to the listed level-8 cells |
| `PUT /v2/collections/{id}/cells/{cell}` (writable, trusted) | load one prepared bundle for a level-8 cell: a tar archive with a canonical FGB, key fence, Parquet columns, id rows, block hashes and manifest; the server verifies counts, the index root, hashes and samples before copying |
| `GET /v2/collections/{id}/items` | features with `bbox`, property filters, CQL2-text `filter=`, `limit`, opaque `cursor` paging with `links rel=next`, `overview=0\|2\|4\|6`, `numberMatched=true` (without a bbox), and `f=flatgeobuf` or `Accept: application/flatgeobuf` |
| `GET /v2/collections/{id}/items/{featureID}` | one feature by its stored id |
| `/healthz`, `/readyz`, `/metrics` | probes and Prometheus metrics |
| `/design` | the design page |

`postfgb serve` is writable; `postfgb serve --readonly` registers only the read routes. There is no authentication in the server: run it behind your own auth layer, expose readonly workers publicly, and keep writable workers internal. Requests log one structured line with route, status, duration and bytes.

## How it works

- Uploads stream one FlatGeobuf file. Each feature is assigned to a level-8 S2 cell, sorted within the cell by Hilbert key and id, and encoded into one FlatGeobuf file per cell with a packed Hilbert R-tree. A key fence every 256 features locates a cursor or id in the file. The file is sliced into 8120-byte pieces and each piece is zstd-compressed in `pfgb.blocks`, catalogued by `pfgb.chunks`; ids live in `pfgb.ids`. L8 is detail; the L6, L4 and L2 samples keep 2,048 features per cell and also store one file per cell. L6 and L4 are built in the commit, L2 by a background worker, and the coarse samples drop subpixel lines and areas while keeping points.
- Reads load the collection's cell catalog from PostgreSQL, prune cells by bounding box and per-cell zones, then read the FGB R-tree to map a bbox to feature ranges and the key fence to resume a cursor or find an id. Filters evaluate exactly in Go over the stored Parquet column files; PFA1 sidecars from earlier collections remain readable. Cursors resume by `(cell, key, id)`.
- A trusted loader can send the already-sorted per-cell FGB with its fence, Parquet columns and id rows instead of a source FlatGeobuf. The server checks the manifest, file and block hashes, FGB index root, counts, fence anchors and samples, then copies the parts. The normal PUT path still validates and derives everything for untrusted uploads.
- PostgreSQL only stores blocks and scalar catalog columns; there is no PostGIS dependency, and the core path needs no PostgreSQL extensions.
- JSON columns are detected and flattened one level into typed columns at ingest. Scalar keys become ordinary columns; array-valued keys stay JSON. Key selection is deterministic for the same input.

Not in this pass: collection metadata/listing, STAC, the RustGIS viewer, views, range-readable `items.fgb`, and the generated OpenAPI document. The append-only migrations that created the legacy v1 tables are still applied, but no v1 route serves them.

## Quickstart

Requires [mise](https://mise.jdx.dev) (installs Go, just and hk) and Docker.

```sh
mise install
hk install --mise
just pg-up
just serve
```

Load a FlatGeobuf file and read it back:

```sh
curl -T features.fgb -H "Content-Type: application/flatgeobuf" \
  "http://localhost:8080/v2/collections/demo?id_column=id"
curl "http://localhost:8080/v2/collections/demo/items?limit=10"
curl "http://localhost:8080/v2/collections/demo/items?bbox=-10,45,10,55&filter=name LIKE 'feature-6%'"
```

## Status

Pre-alpha. Working today: FlatGeobuf ingest and per-cell file storage as PostgreSQL blocks, cell-ordered catalogs with id indexes and key fences, FGB R-tree range reads, Parquet column range reads, overview levels, bbox and attribute-filter pruning with exact evaluation in Go, cursor paging, FlatGeobuf pages, trusted prepared-bundle loads, readonly/writable worker modes, probes, metrics and the design page. The design page records the remaining build order.

## License

Apache-2.0.
