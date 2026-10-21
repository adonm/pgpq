#!/usr/bin/env bash
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

usage() {
	cat >&2 <<'EOF'
usage: prepare.sh --pbf FILE --work DIR [--layers a,b] [--force]

Converts one small OSM PBF into one native GeoParquet 2.0 file per layer with
a memory-capped DuckDB container. The spatial extension supplies the GDAL OSM
reader and the Parquet 2.0 writer; no separate GDAL container is used. Each
file is sorted by ST_Hilbert before COPY so row groups are spatially ordered.

environment:
  PGPQ_OSM_DUCKDB_IMAGE  default pgpq-duckdb:2.0.0 (writes GEOPARQUET_VERSION V2)
  PGPQ_OSM_DUCKDB_MEM    default 1g
  PGPQ_OSM_DUCKDB_CPUS   default 1
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

command -v docker >/dev/null 2>&1 || die "docker is required for the DuckDB step"
image=$(ensure_duckdb_image)
memory=${PGPQ_OSM_DUCKDB_MEM:-1g}
cpus=${PGPQ_OSM_DUCKDB_CPUS:-1}

out=$work/parquet
mkdir -p "$out"
pbf_dir=$(dirname "$pbf")
pbf_base=$(basename "$pbf")
extract=$(extract_name "$pbf")
selected=()

while IFS='|' read -r name osm_layer where select filter id_column id_sql assert_field; do
	case "$name" in ''|'#'*) continue ;; esac
	layer_wanted "$name" "$layers" || continue
	dest=$out/${extract}_${name}.parquet
	if [ -s "$dest" ] && [ "$force" -eq 0 ]; then
		note "keep existing $dest"
		selected+=("$name")
		continue
	fi
	rm -f "$dest" "$dest.tmp"
	note "duckdb $extract/$name ($osm_layer)"
	cols=${select:+, $select}
	sql="COPY (SELECT $id_sql AS \"$id_column\"$cols, ST_SetCRS(geom, 'OGC:CRS84') AS geometry FROM ST_Read('/in/$pbf_base', layer='$osm_layer', open_options=['CONFIG_FILE=/config/osmconf.ini'])"
	if [ -n "$where" ]; then
		sql+=" WHERE $where"
	fi
	sql+=" ORDER BY ST_Hilbert(geom), OGC_FID) TO '/out/${extract}_${name}.parquet' (FORMAT parquet, GEOPARQUET_VERSION 'V2', ROW_GROUP_SIZE 2048, COMPRESSION 'zstd', COMPRESSION_LEVEL 15);"
	docker run --rm --memory "$memory" --cpus "$cpus" \
		-v "$pbf_dir:/in:ro" -v "$here:/config:ro" -v "$out:/out" "$image" \
		duckdb -c "INSTALL spatial; LOAD spatial; $sql"
	[ -s "$dest" ] || die "DuckDB produced no output for $name"
	selected+=("$name")
done <"$here/layers.tsv"

[ ${#selected[@]} -gt 0 ] || die "no layers selected"
meta_sql=
for name in "${selected[@]}"; do
	if [ -n "$meta_sql" ]; then meta_sql+=" UNION ALL "; fi
	meta_sql+="SELECT '$name' AS layer, count(*) AS features, quantile_cont(ST_X(ST_Centroid(geometry)), 0.05) AS qxmin, quantile_cont(ST_Y(ST_Centroid(geometry)), 0.05) AS qymin, quantile_cont(ST_X(ST_Centroid(geometry)), 0.95) AS qxmax, quantile_cont(ST_Y(ST_Centroid(geometry)), 0.95) AS qymax, any_value(ST_GeometryType(geometry)) AS geometry_type FROM read_parquet('/out/${extract}_${name}.parquet')"
done
docker run --rm --memory "$memory" --cpus "$cpus" \
	-v "$out:/out" "$image" \
	duckdb -json -c "INSTALL spatial; LOAD spatial; $meta_sql" >"$work/meta.json"

note "parquet files in $out"
ls -lh "$out"/*.parquet
note "metadata $work/meta.json"
