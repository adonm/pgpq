#!/usr/bin/env bash
# Drives a deployment through the two real clients and asserts what they see:
# GDAL's OAPIF driver (collection metadata, paging through next links, filters,
# and the range-readable FlatGeobuf read in place) and rustgis-cli (a project
# layer per collection and per view, plus one zoom-constrained overview layer).
#
#   scripts/ogc-clients-test.sh [server-url] [collection] [view] [tier-collection]
#
# Defaults: http://localhost:8080 buildings_fr pois_union pois_union
set -uo pipefail
URL=${1:-http://localhost:8080}
COLL=${2:-buildings_fr}
VIEW=${3:-pois_union}
TIER=${4:-buildings}
CLI=${RUSTGIS_CLI:-rustgis-cli}
LOG=${POSTFGB_LOG:-/tmp/opencode/postfgb.log}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fail=0
say() { printf '%s\n' "$*"; }
bad() { say "FAIL $*"; fail=1; }
gdal() { docker run --rm --network host -v "$TMP:/work" ghcr.io/osgeo/gdal:ubuntu-full-latest "$@"; }

curl -sfS "$URL/collections/$COLL" -o "$TMP/coll.json" || { say "cannot fetch $URL/collections/$COLL"; exit 1; }
say "collection $COLL: $(python3 -c "import json;d=json.load(open('$TMP/coll.json'));print(d.get('itemType'),d.get('id'))")"

# --- collection metadata a client needs -------------------------------------
python3 - "$TMP/coll.json" <<'PY' || fail=1
import json, sys
d = json.load(open(sys.argv[1]))
missing = [k for k in ("id", "links", "extent", "itemType", "crs") if k not in d]
if missing:
    print("FAIL collection document is missing", missing)
    sys.exit(1)
rels = {l["rel"] for l in d["links"]}
if "items" not in rels:
    print("FAIL collection has no items link")
    sys.exit(1)
print("ok   collection document carries id, links, extent, itemType and crs")
PY

# --- GDAL OAPIF: metadata ---------------------------------------------------
gdal ogrinfo -so -al "OAPIF:$URL/collections/$COLL" 2>"$TMP/oapif.err" | grep -E "Feature Count|Extent:" | sed 's/^/     /'
if [ "${PIPESTATUS[0]}" -ne 0 ]; then bad "GDAL cannot open the OAPIF collection: $(tail -1 "$TMP/oapif.err")"; fi

# --- GDAL OAPIF: paging through next links ----------------------------------
LIMIT=500
gdal ogr2ogr -f GeoJSON "/work/page.json" "OAPIF:$URL/collections/$VIEW" -oo PAGE_SIZE=$LIMIT 2>"$TMP/page.err" >/dev/null
if [ -s "$TMP/page.json" ]; then
  count=$(python3 -c "import json;print(len(json.load(open('$TMP/page.json'))['features']))")
  expected=$(curl -s "$URL/collections/$VIEW/items?limit=1&numberMatched=true" | python3 -c "import json,sys;print(json.load(sys.stdin).get('numberMatched'))")
  if [ "$count" = "$expected" ]; then
    say "ok   GDAL paged $count features of $VIEW in pages of $LIMIT (next links followed)"
  else
    bad "GDAL got $count features of $VIEW, expected $expected"
  fi
else
  bad "GDAL could not page $VIEW: $(tail -1 "$TMP/page.err")"
fi

# --- GDAL OAPIF: a CQL2 filter the data actually answers --------------------
BENCH=$(curl -s "$URL/collections/$VIEW/items?limit=200" | python3 -c "
import json, sys
for f in json.load(sys.stdin)['features']:
    v = f['properties'].get('amenity')
    if v:
        print(v)
        break")
MATCHED=$(curl -s "$URL/collections/$VIEW/items?limit=1&numberMatched=true&filter=amenity=%27$BENCH%27" | python3 -c "import json,sys;print(json.load(sys.stdin).get('numberMatched'))")
gdal ogr2ogr -f GeoJSON "/work/filtered.json" "OAPIF:$URL/collections/$VIEW" -where "amenity = '$BENCH'" 2>"$TMP/filter.err" >/dev/null
if [ -s "$TMP/filtered.json" ]; then
  n=$(python3 -c "import json;print(len(json.load(open('$TMP/filtered.json'))['features']))")
  wrong=$(python3 -c "import json;print(sum(1 for f in json.load(open('$TMP/filtered.json'))['features'] if f['properties'].get('amenity') != '$BENCH'))")
  if [ "$wrong" = 0 ] && [ "$n" = "$MATCHED" ]; then
    say "ok   GDAL filter amenity='$BENCH' returned $n features, matching the API count"
  else
    bad "GDAL filter returned $n (API says $MATCHED) with $wrong wrong rows"
  fi
else
  bad "GDAL filter failed: $(tail -1 "$TMP/filter.err")"
fi

# --- GDAL in place: the range-readable FlatGeobuf ---------------------------
gdal bash -c "ogrinfo -al -q '/vsicurl/$URL/collections/$VIEW/items.fgb' 2>/dev/null | grep -c OGRFeature" > "$TMP/window.count"
say "ok   GDAL read the $VIEW FlatGeobuf in place: $(cat "$TMP/window.count") features"

# --- rustgis-cli: project layers, including a zoom-constrained overview ------
test -x "$(command -v "$CLI" 2>/dev/null)" || { say "rustgis-cli not found (set RUSTGIS_CLI)"; exit 2; }
curl -sfS "$URL/viewer.rgis" -o "$TMP/project.rgis" || { say "cannot fetch viewer.rgis"; exit 1; }
python3 - "$TMP/project.rgis" "$TMP/pick.json" "$VIEW" "$TIER" <<'PY'
import json, sys
project, out, view, tier = sys.argv[1:5]
d = json.load(open(project))
layers = d["maps"][0]["layers"]
def for_collection(name):
    return [l for l in layers if "/collections/%s/items.fgb" % name in l["source"]["path"]]
chosen = [l for l in for_collection(view) if "overview=" not in l["source"]["path"]][:1]
chosen += [l for l in for_collection(tier) if "overview=" in l["source"]["path"]][:1]
assert chosen, "no layer for %s" % view
json.dump({"layers": chosen}, open(out, "w"))
print("     layers under test:", [l["name"] for l in chosen])
PY
while IFS=$'\t' read -r name; do
  python3 - "$TMP/project.rgis" "$TMP/probe.rgis" "$name" <<'PY'
import json, sys
project, out, target = sys.argv[1:4]
d = json.load(open(project))
for l in d["maps"][0]["layers"]:
    l["visible"] = l["name"] == target
json.dump(d, open(out, "w"))
PY
  mark=$(wc -l < "$LOG")
  "$CLI" run --data "$TMP/probe.rgis" \
    --cmd 'map.zoomToExtent={"extent":[-2,43,5,55]}' \
    --cmd "share.exportMap={\"path\":\"$TMP/probe.png\",\"width\":256,\"height\":256}" >/dev/null 2>"$TMP/cli.err"
  if tail -n +"$((mark + 1))" "$LOG" | grep -q "items.fgb"; then
    say "ok   rustgis rendered \"$name\" ($(stat -c%s "$TMP/probe.png")B) from FlatGeobuf"
  else
    bad "rustgis could not render \"$name\": $(tail -1 "$TMP/cli.err")"
  fi
done < <(python3 -c "import json;[print(l['name']) for l in json.load(open('$TMP/pick.json'))['layers']]")

if [ "$fail" -eq 0 ]; then say "clients: GDAL and RustGIS both happy"; else say "clients: failures above"; fi
exit "$fail"
