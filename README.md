# postfgb

Fast OGC API Features (OAPIF) and OGC Tiles interface over PostgreSQL.

- FlatGeobuf chunks stored in Postgres
- Sidecar indexes (fulltext, statistics) do the majority of query work
- Iceberg-style predicate pushdown across billions of GeoJSON-style records
- Designed for safe persistence plus fast spatial and fulltext search

**Status:** pre-alpha / design phase.

## License

[Apache-2.0](LICENSE)
