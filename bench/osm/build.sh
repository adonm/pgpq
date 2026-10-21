#!/usr/bin/env bash
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

usage() {
	cat >&2 <<'EOF'
usage: build.sh --work DIR --extract NAME [--layers a,b] [--prefix osm_]

Builds a self-contained DuckDB upload file from the intermediate GeoParquet
produced by prepare.sh. Every layer becomes a table plus one layer_catalog row.
All work runs with a bounded DuckDB memory limit and one thread.
EOF
	exit 2
}

work=
extract=
layers=
prefix=${PGPQ_OSM_PREFIX:-osm_}
while [ $# -gt 0 ]; do
	case "$1" in
	--work) work=${2:-}; shift 2 ;;
	--extract) extract=${2:-}; shift 2 ;;
	--layers) layers=${2:-}; shift 2 ;;
	--prefix) prefix=${2:-}; shift 2 ;;
	-h|--help) usage ;;
	*) usage ;;
	esac
done
[ -n "$work" ] && [ -n "$extract" ] || usage
work=$(realpath -m "$work")
duckdb=$(find_duckdb) || die "set PGPQ_DUCKDB or put duckdb on PATH"

mkdir -p "$work/duckdb-tmp"
db=$work/$extract.duckdb
tmp=$work/$extract.duckdb.tmp
meta=$work/$extract.meta.json
sql=$work/build.sql
rm -f "$db" "$tmp" "$meta" "$sql"

memory=${PGPQ_OSM_DUCKDB_MEM:-512MB}
threads=${PGPQ_OSM_DUCKDB_THREADS:-1}
qwork=$(sql_quote "$work/duckdb-tmp")

catalog_values=()
meta_selects=()
while IFS='|' read -r name osm_layer where select filter id_sql assert_field; do
	case "$name" in ''|'#'*) continue ;; esac
	layer_wanted "$name" "$layers" || continue
	collection=${prefix}${name}
	valid_name "$collection" || die "invalid collection name: $collection"
	parquet=$work/parquet/${extract}_${name}.parquet
	[ -s "$parquet" ] || die "missing $parquet (run prepare.sh)"
	qparquet=$(sql_quote "$parquet")
	cols=$select
	if [ -n "$cols" ]; then
		cols=", $cols"
	fi
	{
		printf 'CREATE TABLE "layer_%s" AS SELECT %s AS bench_id%s, ST_SetCRS(geometry, '\''OGC:CRS84'\'') AS geometry FROM read_parquet('\''%s'\'');\n' \
			"$name" "$id_sql" "$cols" "$qparquet"
		printf 'ALTER TABLE "layer_%s" ALTER COLUMN bench_id SET NOT NULL;\n' "$name"
		printf 'ALTER TABLE "layer_%s" ALTER COLUMN geometry SET NOT NULL;\n' "$name"
		printf 'CREATE UNIQUE INDEX "layer_%s_id" ON "layer_%s" (bench_id);\n' "$name" "$name"
	} >>"$sql"
	catalog_values+=("('$(sql_quote "$collection")','layer_$name','bench_id','geometry')")
	meta_selects+=("SELECT '$(sql_quote "$name")' AS layer, '$(sql_quote "$collection")' AS collection, '$(sql_quote "$filter")' AS filter, '$(sql_quote "$assert_field")' AS assert_field, count(*) AS features, count(DISTINCT ST_GeometryType(geometry)) AS types, any_value(ST_GeometryType(geometry)) AS geom_type, ST_XMin(ST_Extent_Agg(geometry)) AS xmin, ST_YMin(ST_Extent_Agg(geometry)) AS ymin, ST_XMax(ST_Extent_Agg(geometry)) AS xmax, ST_YMax(ST_Extent_Agg(geometry)) AS ymax, quantile_cont(ST_X(ST_Centroid(geometry)), 0.05) AS qxmin, quantile_cont(ST_Y(ST_Centroid(geometry)), 0.05) AS qymin, quantile_cont(ST_X(ST_Centroid(geometry)), 0.95) AS qxmax, quantile_cont(ST_Y(ST_Centroid(geometry)), 0.95) AS qymax FROM \"layer_$name\"")
done <"$here/layers.tsv"

[ ${#catalog_values[@]} -gt 0 ] || die "no layers selected"

qmeta=$(sql_quote "$meta")
{
	printf "SET memory_limit='%s';\n" "$memory"
	printf "SET threads=%s;\n" "$threads"
	printf "SET preserve_insertion_order=false;\n"
	printf "SET temp_directory='%s';\n" "$qwork"
	printf 'INSTALL spatial; LOAD spatial;\n'
	cat "$sql"
	printf 'CREATE TABLE layer_catalog(layer_name VARCHAR, view_name VARCHAR, id_column VARCHAR, geom_column VARCHAR);\n'
	printf 'INSERT INTO layer_catalog VALUES %s;\n' "$(printf '%s,' "${catalog_values[@]}" | sed 's/,$//')"
	printf 'COPY (%s) TO '\''%s'\'' (FORMAT JSON, ARRAY true);\n' "$(printf '%s UNION ALL ' "${meta_selects[@]}" | sed 's/ UNION ALL $//')" "$qmeta"
} >"$sql.full"

note "duckdb $db"
"$duckdb" "$tmp" <"$sql.full"
mv "$tmp" "$db"
rm -f "$sql" "$sql.full"
note "built $db"
ls -lh "$db" "$meta"
