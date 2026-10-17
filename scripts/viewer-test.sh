#!/usr/bin/env bash
# Drives the generated /viewer.rgis project through rustgis-cli and checks that
# its layers really load. Each check renders a copy of the project with one
# layer switched on, zoomed to a small window over that layer's centre, then
# asserts that the server saw a FlatGeobuf read for that collection. Exits
# non-zero on failure.
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
assert not p.get("tiles"), "project still carries vector tile layers"
for l in p.get("layers", []):
    src = l["source"]["path"].rsplit("/collections/", 1)[-1].split("/", 1)[0]
    print("layer", src, l["name"], sep="\t")
PY
[ -s "$TMP/inventory" ] || { say "project has no feature layers"; exit 1; }
say "project: $(grep -c '^layer' "$TMP/inventory") feature layer(s)"

if "$CLI" run --data "$TMP/project.rgis" --cmd 'info.project={}' 2>"$TMP/open.err" | grep -q '"layers"'; then
  say "ok   project opens"
else
  bad "project does not open: $(tail -1 "$TMP/open.err")"
fi

probe_window() {
  python3 - "$URL/collections/$1" <<'PY'
import json, sys, urllib.request
b = json.load(urllib.request.urlopen(sys.argv[1]))["extent"]["spatial"]["bbox"][0]
lon, lat = (b[0] + b[2]) / 2, (b[1] + b[3]) / 2
d = 0.01
print(f"{lon - d},{lat - d},{lon + d},{lat + d}", lon, lat, sep="\t")
PY
}

probe_project() {
  python3 - "$TMP/project.rgis" "$TMP/probe.rgis" "$1" <<'PY'
import json, sys
project, out, target = sys.argv[1:4]
d = json.load(open(project))
m = d["maps"][0]
for l in m.get("layers", []):
    l["visible"] = l["name"] == target
json.dump(d, open(out, "w"))
PY
}

shoot() {
  local window=$1 out=$2
  "$CLI" run --data "$TMP/probe.rgis" \
    --cmd "map.zoomToExtent={\"extent\":[$window]}" \
    --cmd "share.exportMap={\"path\":\"$out\",\"width\":512,\"height\":512}" \
    >/dev/null 2>"$TMP/cli.err"
}

while IFS=$'\t' read -r kind id name; do
  IFS=$'\t' read -r window _ _ < <(probe_window "$id")
  probe_project "$name"
  mark=$(wc -l < "$LOG")
  shoot "$window" "$TMP/$id.png"
  if tail -n +"$((mark + 1))" "$LOG" | grep -q "collections/$id/items.fgb"; then
    say "ok   features $id (FlatGeobuf read)"
  else
    bad "features $id: no FlatGeobuf read $(tail -1 "$TMP/cli.err")"
  fi
done < "$TMP/inventory"

if [ "$fail" -eq 0 ]; then say "viewer project: all layers load"; else say "viewer project: failures above"; fi
exit "$fail"
