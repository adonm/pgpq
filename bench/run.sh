#!/usr/bin/env bash
set -euo pipefail

URL=${URL:-http://localhost:8080}
COLL=${COLL:-bench}
Z=${Z:-0}
X=${X:-0}
Y=${Y:-0}
SPAT=${SPAT:--10 45 10 55}

for tool in ogrinfo gdalinfo; do
  command -v "$tool" >/dev/null || { echo "missing $tool (install gdal-bin)"; exit 1; }
done

time_it() {
  local label=$1
  shift
  TIMEFORMAT="$label %R s"
  time "$@"
}

echo "oapif collections"
time_it "list" ogrinfo "OAPIF:$URL"

echo "oapif collection scan"
time_it "scan" ogrinfo -so -al "OAPIF:$URL" "$COLL"

echo "oapif bbox filter $SPAT"
time_it "bbox" ogrinfo -so -al -spat $SPAT "OAPIF:$URL" "$COLL"

TILE="/vsicurl/$URL/collections/$COLL/tiles/WebMercatorQuad/$Z/$X/$Y.mvt"
echo "mvt tile $Z/$X/$Y"
time_it "tile" gdalinfo -json "$TILE"
