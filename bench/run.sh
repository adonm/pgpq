#!/usr/bin/env bash
set -euo pipefail

URL=${URL:-http://localhost:8080}
COLL=${COLL:-bench}
Z=${Z:-4}
X=${X:-8}
Y=${Y:-6}
SPAT=${SPAT:--10 45 10 55}
GDAL_IMAGE=${GDAL_IMAGE:-ghcr.io/osgeo/gdal:alpine-small-latest}

if ! command -v ogrinfo >/dev/null; then
  docker image inspect "$GDAL_IMAGE" >/dev/null 2>&1 || docker pull -q "$GDAL_IMAGE" >/dev/null
  ogrinfo() { docker run --rm --network host "$GDAL_IMAGE" ogrinfo "$@"; }
  gdalinfo() { docker run --rm --network host "$GDAL_IMAGE" gdalinfo "$@"; }
fi

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

TILE="/vsicurl_streaming/$URL/collections/$COLL/tiles/WebMercatorQuad/$Z/$X/$Y.mvt"
echo "mvt tile $Z/$X/$Y"
time_it "tile" gdalinfo -json "$TILE"
