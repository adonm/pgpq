#!/usr/bin/env bash
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

usage() {
	cat >&2 <<'EOF'
usage: cleanup.sh [--names a,b] [--prefix osm_] [--psql CMD]

Deletes the named test collections from a pgpq test database. The delete
cascades to cells, chunks and id rows. Without --names it removes every layer
defined in layers.tsv with the given prefix.

environment: PGPQ_OSM_PSQL, or PGPQ_OSM_DSN plus psql on PATH, or a local
Docker PostgreSQL container (PGPQ_OSM_CONTAINER, PGPQ_OSM_DB, PGPQ_OSM_PGUSER).
EOF
	exit 2
}

names=
prefix=${PGPQ_OSM_PREFIX:-osm_}
psql_cmd=${PGPQ_OSM_PSQL:-}
while [ $# -gt 0 ]; do
	case "$1" in
	--names) names=${2:-}; shift 2 ;;
	--prefix) prefix=${2:-}; shift 2 ;;
	--psql) psql_cmd=${2:-}; shift 2 ;;
	-h|--help) usage ;;
	*) usage ;;
	esac
done
[ -n "$psql_cmd" ] || psql_cmd=$(find_psql_command) || die "no psql command; pass --psql or set PGPQ_OSM_PSQL"

if [ -z "$names" ]; then
	while IFS='|' read -r name osm_layer where select filter id_column id_sql assert_field; do
		case "$name" in ''|'#'*) continue ;; esac
		names=${names:+$names,}${prefix}${name}
	done <"$here/layers.tsv"
fi

quoted=
IFS=',' read -r -a list <<<"$names"
for name in "${list[@]}"; do
	valid_name "$name" || die "invalid collection name: $name"
	quoted=${quoted:+$quoted,}$(printf "'%s'" "$(sql_quote "$name")")
done
[ -n "$quoted" ] || die "no collections to delete"

sql="DELETE FROM pgpq.collections WHERE name IN ($quoted) RETURNING name;"
read -r -a parts <<<"$psql_cmd"
note "deleting from test database"
"${parts[@]}" -v ON_ERROR_STOP=1 -Atc "$sql"
