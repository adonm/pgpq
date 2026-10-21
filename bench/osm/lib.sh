#!/usr/bin/env bash

repo_root() {
	cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd
}

die() {
	echo "error: $*" >&2
	exit 1
}

note() {
	echo "== $*" >&2
}

find_psql_command() {
	if [ -n "${PGPQ_OSM_PSQL:-}" ]; then
		echo "$PGPQ_OSM_PSQL"
		return
	fi
	if command -v psql >/dev/null 2>&1 && [ -n "${PGPQ_OSM_DSN:-}" ]; then
		printf 'psql %s\n' "$PGPQ_OSM_DSN"
		return
	fi
	local container="${PGPQ_OSM_CONTAINER:-postfgb-postgres-1}"
	local user="${PGPQ_OSM_PGUSER:-postgres}"
	local db="${PGPQ_OSM_DB:-pgpq_osm_bench}"
	if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$container"; then
		printf 'docker exec -i %s psql -U %s -d %s\n' "$container" "$user" "$db"
		return
	fi
	return 1
}

sql_quote() {
	printf '%s' "${1//\'/\'\'}"
}

extract_name() {
	local base
	base=$(basename "$1")
	base=${base%.osm.pbf}
	base=${base%.pbf}
	base=${base%.osm}
	echo "$base"
}

layer_wanted() {
	local name=$1 list=$2
	[ -z "$list" ] || [[ ",$list," == *",$name,"* ]]
}

valid_name() {
	[[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,47}$ ]]
}
