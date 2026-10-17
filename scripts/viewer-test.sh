#!/usr/bin/env bash
# Drives the generated /viewer.rgis project through rustgis-cli and checks that
# its layers really load: the project opens, every tile layer draws (the server
# sees the request for the tile the view needs, in the right x/y order), and
# every streamed feature layer reads its FlatGeobuf. Exits non-zero on failure.
#
#   scripts/viewer-test.sh [server-url]
#
# rustgis-cli comes from the RustGIS release tarball (RUSTGIS_CLI overrides the
# binary). POSTFGB_LOG points at the worker log used for the request assertions.
set -uo pipefail
URL=${1:-http://localhost:8080}
CLI=${RUSTGIS_CLI:-rustgis-cli}
LOG=${POSTFGB_LOG:-/tmp/opencode/postfgb.log}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fail=0

say() { printf '%s\n' "$*"; }
bad() { say "FAIL $*"; fail=1; }

command -v "$CLI" >/dev/null || { say "rustgis-cli not found (set RUSTGIS_CLI)"; exit 2; }
test -r "$LOG" || { say "cannot read $LOG (set POSTFGB_LOG)"; exit 2; }
curl -sfS "$URL/viewer.rgis" -o "$TMP/project.rgis" || { say "cannot fetch $URL/viewer.rgis"; exit 1; }

python3 - "$TMP/project.rgis" > "$TMP/inventory" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))["maps"][0]
for t in p.get("tiles", []):
    print("tile", t["layers"][0], t["name"], sep="\t")
for l in p.get("layers", []):
    src = l["dataset"]["source"].rsplit("/collections/", 1)[-1].split("/", 1)[0]
    print("layer", src, l["name"], sep="\t")
PY
say "project: $(grep -c '^tile' "$TMP/inventory") tile layer(s), $(grep -c '^layer' "$TMP/inventory") feature layer(s)"

if "$CLI" run --data "$TMP/project.rgis" --cmd 'info.project={}' >"$TMP/open.json" 2>"$TMP/open.err"; then
  say "ok   project opens"
else
  bad "project does not open: $(tail -1 "$TMP/open.err")"
fi

expected_tile() {
  python3 - "$URL/collections/$1" "$2" <<'PY'
import json, math, sys, urllib.request
c = json.load(urllib.request.urlopen(sys.argv[1]))
b = c["extent"]["spatial"]["bbox"][0]
z = int(sys.argv[2]); n = 1 << z
lon, lat = (b[0] + b[2]) / 2, (b[1] + b[3]) / 2
x = int((lon + 180) / 360 * n)
y = int((1 - math.log(math.tan(math.radians(lat)) + 1 / math.cos(math.radians(lat))) / math.pi) / 2 * n)
print(f"{b[0]},{b[1]},{b[2]},{b[3]}", z, x, y, sep="\t")
PY
}

while IFS=$'\t' read -r kind id name; do
  [ "$kind" = tile ] || continue
  IFS=$'\t' read -r bbox z x y < <(expected_tile "$id" 12)
  mark=$(wc -l < "$LOG")
  if "$CLI" run \
      --cmd "service.add={\"url\":\"$URL/collections\",\"type\":\"ogcapi\",\"layer\":\"$name\"}" \
      --cmd "layer.visibility={\"layer\":\"$name\",\"visible\":true}" \
      --cmd "map.zoomToExtent={\"extent\":[$bbox]}" \
      --cmd "share.exportMap={\"path\":\"$TMP/$id-tiles.png\",\"width\":512,\"height\":512}" \
      >"$TMP/cli.json" 2>"$TMP/cli.err" \
      && tail -n +"$((mark + 1))" "$LOG" | grep -q "tiles/WebMercatorQuad/$z/$x/$y"; then
    say "ok   tiles $id (asked $z/$x/$y)"
  else
    got=$(tail -n +"$((mark + 1))" "$LOG" | grep -oE "tiles/WebMercatorQuad/[0-9]+/[0-9]+/[0-9]+" | tail -1)
    bad "tiles $id: wanted $z/$x/$y, client asked ${got:-nothing} $(tail -1 "$TMP/cli.err")"
  fi
done < "$TMP/inventory"

while IFS=$'\t' read -r kind id name; do
  [ "$kind" = layer ] || continue
  mark=$(wc -l < "$LOG")
  if "$CLI" run \
      --cmd "layer.visibility={\"layer\":\"$name\",\"visible\":true}" \
      --cmd "layer.zoomTo={\"layer\":\"$name\"}" \
      --cmd "share.exportMap={\"path\":\"$TMP/$id.png\",\"width\":512,\"height\":512}" \
      >"$TMP/cli.json" 2>"$TMP/cli.err" \
      && tail -n +"$((mark + 1))" "$LOG" | grep -q "collections/$id/items?f=flatgeobuf"; then
    say "ok   features $id (source read)"
  else
    bad "features $id: $(tail -1 "$TMP/cli.err")"
  fi
done < "$TMP/inventory"

if [ "$fail" -eq 0 ]; then say "viewer project: all layers load"; else say "viewer project: failures above"; fi
exit "$fail"
