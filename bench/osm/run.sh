#!/usr/bin/env bash
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

usage() {
	cat >&2 <<'EOF'
usage: run.sh --pbf FILE [options]

Prepare and benchmark one small OSM PBF against a test worker.
Defaults to http://127.0.0.1:8094 and refuses public 8080/8081.

options:
  --work DIR            default /tmp/opencode/bench/osm-<extract>
  --url URL             test worker, default http://127.0.0.1:8094
  --layers a,b          subset of buildings,places,roads
  --prefix NAME         collection prefix, default osm_
  --reps N              measured requests per endpoint, default 10
  --force               rebuild intermediate GeoParquet
  --skip-prepare        reuse existing GeoParquet
  --keep                do not delete test collections
  --no-verify           skip the DuckDB GeoParquet check
  --psql CMD            psql command prefix for cleanup
  --allow-remote        allow a non-loopback test worker

environment:
  PGPQ_OSM_PSQL          psql command prefix, e.g. "psql postgres://.../pgpq_osm_bench"
                         or "docker exec -i postfgb-postgres-1 psql -U postgres -d pgpq_osm_bench"
  PGPQ_OSM_DUCKDB_IMAGE  default duckdb/duckdb:1.5.6
  PGPQ_OSM_DUCKDB_MEM    default 768m
  PGPQ_OSM_DUCKDB_CPUS   default 1
  GOMEMLIMIT             server cap, default set by serve.sh to 1GiB

Start a disposable worker first:
  bench/osm/serve.sh
EOF
	exit 2
}

original=("$@")
pbf=
work=
url=${PGPQ_OSM_URL:-http://127.0.0.1:8094}
layers=
prefix=${PGPQ_OSM_PREFIX:-osm_}
reps=10
force=0
skip_prepare=0
keep=0
verify=1
psql_cmd=${PGPQ_OSM_PSQL:-}
allow_remote=0
while [ $# -gt 0 ]; do
	case "$1" in
	--pbf) pbf=${2:-}; shift 2 ;;
	--work) work=${2:-}; shift 2 ;;
	--url) url=${2:-}; shift 2 ;;
	--layers) layers=${2:-}; shift 2 ;;
	--prefix) prefix=${2:-}; shift 2 ;;
	--reps) reps=${2:-}; shift 2 ;;
	--force) force=1; shift ;;
	--skip-prepare) skip_prepare=1; shift ;;
	--keep) keep=1; shift ;;
	--no-verify) verify=0; shift ;;
	--psql) psql_cmd=${2:-}; shift 2 ;;
	--allow-remote) allow_remote=1; shift ;;
	-h|--help) usage ;;
	*) usage ;;
	esac
done
[ -n "$pbf" ] || usage
[ -f "$pbf" ] || die "PBF not found: $pbf"
pbf=$(realpath "$pbf")
extract=$(extract_name "$pbf")
[ -n "$work" ] || work=/tmp/opencode/bench/osm-$extract
work=$(realpath -m "$work")
url=${url%/}
case "$url" in
*:8080|*:8080/*|*:8081|*:8081/*) die "refusing public 8080/8081; use a test worker such as http://127.0.0.1:8094" ;;
esac
case "$url" in
http://127.0.0.1*|http://localhost*|http://\[::1\]*) ;;
*) [ "$allow_remote" -eq 1 ] || die "refusing non-loopback URL without --allow-remote: $url" ;;
esac
curl -fsS -o /dev/null "$url/readyz" || die "test worker not ready at $url (start bench/osm/serve.sh)"
if [ "$keep" -eq 0 ] && [ -z "$psql_cmd" ]; then
	psql_cmd=$(find_psql_command) || die "cleanup needs --psql or PGPQ_OSM_PSQL; pass --keep for a disposable server"
fi

mkdir -p "$work"
printf '%q ' "$here/run.sh" "${original[@]}" >"$work/command.txt"
printf '\n' >>"$work/command.txt"
note "extract $extract"
note "work $work"
note "worker $url"

prepare_seconds=
if [ "$skip_prepare" -eq 1 ]; then
	note "prepare skipped"
else
	start=$(date +%s%N)
	args=(--pbf "$pbf" --work "$work")
	if [ -n "$layers" ]; then args+=(--layers "$layers"); fi
	if [ "$force" -eq 1 ]; then args+=(--force); fi
	"$here/prepare.sh" "${args[@]}"
	end=$(date +%s%N)
	prepare_seconds=$(awk -v a="$start" -v b="$end" 'BEGIN { printf "%.3f", (b-a)/1000000000 }')
fi

if [ -n "$prepare_seconds" ]; then
	printf '{"prepare_seconds":%s}\n' "$prepare_seconds" >"$work/stages.json"
else
	rm -f "$work/stages.json"
fi

args=(
	--base-url "$url"
	--parquet-dir "$work/parquet"
	--extract "$extract"
	--meta "$work/meta.json"
	--layers-file "$here/layers.tsv"
	--duckdb-image "${PGPQ_OSM_DUCKDB_IMAGE:-duckdb/duckdb:1.5.6}"
	--layers "$layers"
	--prefix "$prefix"
	--out-dir "$work/report"
	--reps "$reps"
	--stages "$work/stages.json"
)
if [ "$verify" -eq 0 ]; then args+=(--no-verify); fi
if [ "$keep" -eq 1 ]; then args+=(--keep); fi
if [ -n "$psql_cmd" ]; then args+=(--psql-cmd "$psql_cmd"); fi
if [ "$allow_remote" -eq 1 ]; then args+=(--allow-remote); fi
"${PYTHON:-python3}" "$here/bench.py" "${args[@]}"
