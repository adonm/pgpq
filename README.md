# postfgb

OGC API Features over PostgreSQL, backed by FlatGeobuf cell files stored as database blocks.

Goal: the smallest Go package that provides performant 1000x scaling of the spatial records a PostgreSQL database can hold, query, and return — by partitioning records into FlatGeobuf cell files and pruning reads with R-trees, key fences and per-cell statistics.

The target deployment is [RustGIS](https://rustgis.app/): reads are served over HTTP at maximum speed, while writes are batch-oriented and free to sort and rewrite cell files.

Design decisions are recorded in the design page served at `/design` (`internal/api/design.html`), which is the source of truth. The repository is mid-migration to that v2 design: only the v2 read and write routes below are served today.

## Current surface

| Path | Content |
| --- | --- |
| `PUT /v2/collections/{id}` (writable) | create or replace a collection from a canonical EPSG:4326 FlatGeobuf; an id column is required (`id`, `fid`, `gid`, `ogc_fid`, or `id_column=`); `cells=` scopes a load to the listed level-8 cells |
| `GET /v2/collections/{id}/items` | features with `bbox`, property filters, CQL2-text `filter=`, `limit`, opaque `cursor` paging with `links rel=next`, `overview=0\|2\|4\|6`, `numberMatched=true` (without a bbox), and `f=flatgeobuf` or `Accept: application/flatgeobuf` |
| `GET /v2/collections/{id}/items/{featureID}` | one feature by its stored id |
| `/healthz`, `/readyz` | liveness and readiness probes |
| `/design` | the design page |

`postfgb serve` is writable; `postfgb serve --readonly` registers only the read routes. There is no authentication in the server: run it behind your own auth layer, expose readonly workers publicly, and keep writable workers internal. Requests log one structured line with route, status, duration and bytes.

## How it works

- Uploads stream one canonical FlatGeobuf file. Each feature is assigned to a level-8 S2 cell, sorted within the cell by Hilbert key and id, and packed into one FlatGeobuf file per cell with a packed R-tree. The original size-prefixed feature bytes are copied unchanged; only the cell header, index and fence are rebuilt. A key fence every 256 features locates a cursor or id in the file. Files are stored in `pfgb.blocks`, catalogued by one `pfgb.chunks` row per cell and level; ids live in `pfgb.ids`. L8 is detail; the L6, L4 and L2 samples keep 2,048 features per cell. All samples are rebuilt in the commit, and the coarse samples drop subpixel lines and areas while keeping points.
- Reads load the collection's cell catalog from PostgreSQL, prune cells by bounding box and per-cell zones, then read the FGB R-tree to map a bbox to feature ranges and the key fence to resume a cursor or find an id. Filters evaluate exactly in Go over the stored Parquet column files. Bbox+filter requests skip Parquet row groups with no R-tree candidate rows. Cursors resume by `(cell, key, id)`.
- PostgreSQL only stores blocks and scalar catalog columns; there is no PostGIS dependency, and the core path needs no PostgreSQL extensions.
- JSON columns stay JSON; flattening is the loader's job, not the server's. The server preserves the uploaded schema and feature bytes exactly.

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

Pre-alpha. Working today: raw FlatGeobuf ingest and per-cell file storage as PostgreSQL blocks, one catalog row per cell with id indexes and key fences, FGB R-tree range reads, Parquet column range reads, synchronous L2/L4/L6 samples, bbox and attribute-filter pruning with exact evaluation in Go, cursor paging, FlatGeobuf pages, readonly/writable worker modes, probes and the design page. The design page records the remaining build order.

## License

Apache-2.0.
