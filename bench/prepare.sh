#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
region="${1:-luxembourg}"
data="${BENCH_DATA:-bench/data}"
gdal="${GDAL_IMAGE:-ghcr.io/osgeo/gdal:alpine-small-latest}"
mkdir -p "$data"
pbf="$data/$region.osm.pbf"
[ -s "$pbf" ] || curl -fsSL -o "$pbf" "https://download.openstreetmap.fr/extracts/europe/$region.osm.pbf"
cat > "$data/id.sql" <<'SQL'
SELECT *, printf('m-%s-%s', coalesce(osm_id,''), coalesce(osm_way_id,'')) AS bench_id FROM multipolygons WHERE building IS NOT NULL
SQL
docker run --rm -v "$PWD/$data:/data" "$gdal" ogr2ogr -f FlatGeobuf "/data/${region}_buildings.fgb" "/data/$region.osm.pbf" -dialect SQLITE -sql "$(cat "$data/id.sql")" -nlt PROMOTE_TO_MULTI -lco SPATIAL_INDEX=YES
python3 bench/enrich.py > "$data/enrich.sql"
docker run --rm -v "$PWD/$data:/data" "$gdal" ogr2ogr -f FlatGeobuf "/data/${region}_enriched.fgb" "/data/$region.osm.pbf" -dialect SQLITE -sql "$(cat "$data/enrich.sql")" -segmentize 0.000009 -nlt PROMOTE_TO_MULTI -lco SPATIAL_INDEX=YES
printf 'prepared %s: %s buildings, %s enriched\n' "$region" "$(du -h "$data/${region}_buildings.fgb" | cut -f1)" "$(du -h "$data/${region}_enriched.fgb" | cut -f1)"
