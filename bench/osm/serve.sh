#!/usr/bin/env bash
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

usage() {
	cat >&2 <<'EOF'
usage: serve.sh

Starts one writable test worker on 127.0.0.1:8094 against a dedicated database.
It never starts a public listener. Run it in the foreground (or redirect it).

environment:
  PGPQ_BIN             default <repo>/bin/pgpq
  PGPQ_OSM_ADDR        default 127.0.0.1:8094
  PGPQ_OSM_DB          default pgpq_osm_bench
  PGPQ_OSM_CONTAINER   default postfgb-postgres-1
  PGPQ_OSM_PGUSER      default postgres
  PGPQ_OSM_DSN         overrides the constructed DSN
  POSTGRES_PASSWORD    read from the environment or <repo>/.env
EOF
	exit 2
}

addr=${PGPQ_OSM_ADDR:-127.0.0.1:8094}
db=${PGPQ_OSM_DB:-pgpq_osm_bench}
container=${PGPQ_OSM_CONTAINER:-postfgb-postgres-1}
user=${PGPQ_OSM_PGUSER:-postgres}

case "$addr" in
127.0.0.1:*|localhost:*|\[::1\]:*) ;;
*) die "refusing non-loopback addr: $addr" ;;
esac
case "$addr" in
*:8080|*:8081) die "refusing public 8080/8081" ;;
esac
bin=${PGPQ_BIN:-$(repo_root)/bin/pgpq}
[ -x "$bin" ] || die "missing $bin; build it with: go build -o bin/pgpq ./cmd/pgpq"
[[ "$db" =~ ^[A-Za-z0-9_]+$ ]] || die "invalid database name: $db"

if [ -z "${POSTGRES_PASSWORD:-}" ] && [ -f "$(repo_root)/.env" ]; then
	POSTGRES_PASSWORD=$(sed -n 's/^POSTGRES_PASSWORD=//p' "$(repo_root)/.env" | head -n 1)
fi
dsn=${PGPQ_OSM_DSN:-postgres://postgres:${POSTGRES_PASSWORD:-postgres}@localhost:5432/$db?sslmode=disable}

if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$container"; then
	exists=$(docker exec "$container" psql -U "$user" -Atc "select 1 from pg_database where datname = '$db'" 2>/dev/null || true)
	if [ "$exists" != "1" ]; then
		note "creating database $db"
		docker exec "$container" createdb -U "$user" "$db"
	fi
else
	note "docker container $container not found; assuming database $db already exists"
fi

note "worker $addr database $db"
exec env GOMEMLIMIT="${GOMEMLIMIT:-1GiB}" \
	"$bin" serve --addr "$addr" --dsn "$dsn" --max-conns 2 --migrate
