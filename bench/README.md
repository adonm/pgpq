# OSM benchmark

This harness loads OpenStreetMap buildings into the v2 server and measures the current chunk + Parquet path.

## Profiles

- `buildings`: OSM building footprints, 26 OSM fields, 1e-7 degree coordinates.
- `enriched`: 129 fields (5x the OSM schema), 1e-7 degree coordinates, polygon edges segmented at about 1 m. This is the design assumption for detailed structures at scale; 1 cm vertex spacing is not used because it explodes geometry size by roughly two orders of magnitude.

Data comes from OpenStreetMap France extracts, attributed to OpenStreetMap contributors. Monaco is dense; Luxembourg is the larger throughput sample.

## Run

```sh
just bench-osm luxembourg
```

The script downloads the PBF, converts with GDAL in Docker, starts a writable server on `127.0.0.1:8094`, loads both profiles, and prints load/storage/read numbers. It expects the local compose PostgreSQL and the `POSTFGB_DSN` in `.env`.

## Measured 2026-10-19

| profile | features | upload | stored | columns | load |
| --- | ---: | ---: | ---: | ---: | ---: |
| Luxembourg buildings | 228,273 | 86.7 MB | 26.1 MB | 3.9 MB | 13.8 s |
| Monaco enriched | 4,628 | 12.4 MB | 4.8 MB | 0.66 MB | 1.9 s |

The enriched Monaco FGB is 3.3 MB after zstd -19. Per feature: 2,687 B raw, 711 B zstd upload, 1,182 B stored, 142 B columns. The building-only L4/L2 samples are empty because the subpixel filter removes building footprints; mixed OSM layers keep larger features. That is an open decision for structure-only collections.

Luxembourg is rural: its 95,863 max is ~74 buildings/km². The 2.5M dense-cell assumption is ~1,930/km² (L8 cells average ~1,297 km²), which is plausible for a dense metro but was not measured here; a dense city extract is the next benchmark.
