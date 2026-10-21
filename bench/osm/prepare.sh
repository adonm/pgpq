#!/usr/bin/env bash
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

usage() {
	cat >&2 <<'EOF'
usage: prepare.sh --pbf FILE --work DIR [--layers a,b] [--force]

Converts one small OSM PBF into one GeoParquet 2.0 file per layer with the
GDAL Docker image. Layers are converted sequentially in a memory-capped
container. The output is an intermediate; build.sh turns it into the DuckDB
upload file.

environment:
  PGPQ_OSM_GDAL_IMAGE  default ghcr.io/osgeo/gdal:ubuntu-full-latest
  PGPQ_OSM_GDAL_MEM    default 768m
  PGPQ_OSM_GDAL_CPUS   default 1
EOF
	exit 2
}

pbf=
work=
layers=
force=0
while [ $# -gt 0 ]; do
	case "$1" in
	--pbf) pbf=${2:-}; shift 2 ;;
	--work) work=${2:-}; shift 2 ;;
	--layers) layers=${2:-}; shift 2 ;;
	--force) force=1; shift ;;
	-h|--help) usage ;;
	*) usage ;;
	esac
done
[ -n "$pbf" ] && [ -n "$work" ] || usage
[ -f "$pbf" ] || die "PBF not found: $pbf"
pbf=$(realpath "$pbf")
work=$(realpath -m "$work")

image=${PGPQ_OSM_GDAL_IMAGE:-ghcr.io/osgeo/gdal:ubuntu-full-latest}
memory=${PGPQ_OSM_GDAL_MEM:-768m}
cpus=${PGPQ_OSM_GDAL_CPUS:-1}
command -v docker >/dev/null 2>&1 || die "docker is required for the GDAL step"

out=$work/parquet
mkdir -p "$out"
pbf_dir=$(dirname "$pbf")
pbf_base=$(basename "$pbf")
extract=$(extract_name "$pbf")

while IFS='|' read -r name osm_layer where select filter id_sql assert_field; do
	case "$name" in ''|'#'*) continue ;; esac
	layer_wanted "$name" "$layers" || continue
	dest=$out/${extract}_${name}.parquet
	if [ -s "$dest" ] && [ "$force" -eq 0 ]; then
		note "keep existing $dest"
		continue
	fi
	rm -f "$dest" "$dest.tmp"
	note "gdaling $extract/$name ($osm_layer)"
	args=(-q -f Parquet "/out/${extract}_${name}.parquet" "/in/$pbf_base" "$osm_layer")
	if [ -n "$where" ]; then
		args+=(-where "$where")
	fi
	args+=(-select "$select"
		-lco GEOPARQUET_VERSION=2.0
		-lco COMPRESSION=ZSTD
		-lco GEOMETRY_ENCODING=WKB
		-lco WRITE_COVERING_BBOX=NO
		-lco ROW_GROUP_SIZE=16384)
	docker run --rm --memory "$memory" --cpus "$cpus" \
		-v "$pbf_dir:/in:ro" -v "$out:/out" "$image" \
		ogr2ogr "${args[@]}"
	[ -s "$dest" ] || die "GDAL produced no output for $name"
done <"$here/layers.tsv"

note "parquet files in $out"
ls -lh "$out"/*.parquet
