#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
region="${1:-luxembourg}"
data="${BENCH_DATA:-bench/data}"
addr="${BENCH_ADDR:-127.0.0.1:8094}"
[ -f .env ] && set -a && . ./.env && set +a
[ -n "${POSTFGB_DSN:-}" ] || { echo "POSTFGB_DSN is required" >&2; exit 2; }
[ -s "$data/${region}_buildings.fgb" ] || bench/prepare.sh "$region"
go build -o "$data/postfgb-bench" ./cmd/postfgb
"$data/postfgb-bench" serve --addr "$addr" >/tmp/postfgb-bench.log 2>&1 &
pid=$!
trap 'kill "$pid" 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -fsS "http://$addr/readyz" >/dev/null 2>&1 && break; sleep 0.5; done
stamp=$(date +%s)
for profile in buildings enriched; do
  name="bench_${region}_${profile}_${stamp}"
  file="$data/${region}_${profile}.fgb"
  out="$data/${name}.load"
  curl -fsS -w 'HTTP %{http_code} upload=%{size_upload} time=%{time_total}\n' -T "$file" -H 'Content-Type: application/flatgeobuf' "http://$addr/v2/collections/${name}?id_column=bench_id" | tee "$out"
  case "$region" in
    monaco) bbox="${BENCH_BBOX:-7.40,43.72,7.45,43.76}" ;;
    andorra) bbox="${BENCH_BBOX:-1.4,42.4,1.8,42.7}" ;;
    *) bbox="${BENCH_BBOX:-5.7,49.4,6.6,50.2}" ;;
  esac
  curl -fsS -o /dev/null -w "bbox HTTP %{http_code} time=%{time_total} bytes=%{size_download}\n" "http://$addr/v2/collections/${name}/items?bbox=${bbox}&limit=100"
  docker compose exec -T postgres psql -U postgres -d postfgb -Atc "select '${profile}', kind, sum(bytes), count(*) from pfgb.files where collection='${name}' group by kind order by kind; select '${profile}', level, count(*), sum(features), avg(features), max(features) from pfgb.chunks where collection='${name}' group by level order by level;" | tee -a "$out"
done
printf 'results in %s/*.load; server log /tmp/postfgb-bench.log\n' "$data"
