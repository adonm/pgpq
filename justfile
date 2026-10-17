dsn := env("POSTFGB_DSN", "postgres://postgres:postgres@localhost:5432/postfgb?sslmode=disable")
url := env("POSTFGB_URL", "http://localhost:8080")
gdal := "ghcr.io/osgeo/gdal:ubuntu-full-latest"
rustgis_cli := env("RUSTGIS_CLI", "/tmp/opencode/RustGIS-0.3.0-linux-x86_64/rustgis-cli")

default:
    @just --list

build:
    go build ./...

image repo:
    KO_DOCKER_REPO="{{repo}}" ko build ./cmd/postfgb

image-tar:
    ko build --push=false --tarball=postfgb-image.tar ./cmd/postfgb

fmt:
    gofmt -w .

fmt-check:
    test -z "$(gofmt -l .)"

vet:
    go vet ./...

test:
    go test -timeout 120s ./...

test-integration:
    POSTFGB_TEST_DSN="{{dsn}}" go test -count=1 -timeout 300s ./...

viewer-test:
    RUSTGIS_CLI={{rustgis_cli}} scripts/viewer-test.sh {{url}}

pg-stats:
    @docker exec postfgb-postgres-1 sh -c 'psql -U postgres -d postfgb -X \
      -c "select pg_size_pretty(pg_database_size(current_database())) db, temp_files, pg_size_pretty(temp_bytes) temp_bytes from pg_stat_database where datname = current_database()" \
      -c "select state, count(*), max(now() - xact_start) oldest from pg_stat_activity where datname = current_database() group by 1 order by 2 desc" \
      -c "select collection, count(*) chunks, sum(feature_count) features, pg_size_pretty(sum(length(data))) stored from chunks where tier = 0 group by 1 order by sum(length(data)) desc limit 8"; du -sh /var/lib/postgresql/18/docker/pg_wal'

ci: fmt-check vet test

pg-up:
    docker compose up -d --wait

pg-down:
    docker compose down -v

fixtures:
    just layercake settlements "7.40 43.72 7.44 43.76"
    just layercake buildings "7.40 43.72 7.44 43.76"
    just layercake highways "7.40 43.72 7.44 43.76"
    just layercake parks "7.40 43.72 7.44 43.76"
    just layercake waterways "7.40 43.72 7.44 43.76"
    just layercake pois "7.40 43.72 7.44 43.76"

demo: pg-up fixtures

serve:
    go run ./cmd/postfgb serve --dsn "{{dsn}}"

serve-readonly:
    go run ./cmd/postfgb serve --dsn "{{dsn}}" --readonly


export coll format out:
    docker run --rm --network host -v "{{justfile_directory()}}:/work" -w /work {{gdal}} ogr2ogr -f "{{format}}" "/work/{{out}}" "OAPIF:{{url}}" "{{coll}}"


bench:
    bench/run.sh

layercake layer bbox="" nlt="GEOMETRY":
    LAYER={{layer}} BBOX="{{bbox}}" NLT={{nlt}} LISTS='StringList=String(JSON),IntegerList=String(JSON),RealList=String(JSON),Integer64List=String(JSON)' docker run --rm -i -e LAYER -e BBOX -e NLT -e LISTS -e CPL_VSIL_CURL_CHUNK_SIZE=4194304 --network host {{gdal}} bash -c 'set -euo pipefail; SPAT=""; if [ -n "$BBOX" ]; then SPAT="-spat $BBOX"; fi; NLTARG=""; if [ -n "$NLT" ]; then NLTARG="-nlt $NLT"; fi; ogr2ogr -f FlatGeobuf /tmp/out.fgb "/vsicurl/https://data.openstreetmap.us/layercake/${LAYER}.parquet" "$LAYER" $SPAT $NLTARG -mapFieldType "$LISTS" -lco SPATIAL_INDEX=NO && exec cat /tmp/out.fgb' | curl -sfS -T - -H "Content-Type: application/flatgeobuf" "{{url}}/collections/{{layer}}"

reset:
    #!/usr/bin/env bash
    set -euo pipefail
    for id in $(curl -sfS "{{url}}/collections" | python3 -c "import json,sys; print('\n'.join(c['id'] for c in json.load(sys.stdin)['collections']))"); do
      curl -sS -o /dev/null -X DELETE "{{url}}/collections/$id" || true
      echo "dropped $id"
    done
    just fixtures
    curl -sfS -X PUT -H "Content-Type: application/json" \
      -d '{"id":"monaco","title":"Monaco (all fixtures)","members":[{"collection":"settlements"},{"collection":"buildings"},{"collection":"highways"},{"collection":"parks"},{"collection":"waterways"},{"collection":"pois"}]}' \
      "{{url}}/collections/monaco" >/dev/null
    echo "fixtures loaded, monaco view created"

restart:
    #!/usr/bin/env bash
    set -euo pipefail
    go build -o bin/postfgb ./cmd/postfgb
    for port in 8080 8081; do
      pid=$(ss -tlnp | grep ":$port " | grep -oP 'pid=\K[0-9]+' | head -1 || true)
      if [ -n "$pid" ]; then kill "$pid"; fi
    done
    sleep 1
    setsid nohup bin/postfgb serve >/tmp/opencode/postfgb.log 2>&1 &
    setsid nohup bin/postfgb serve --addr 127.0.0.1:8081 --readonly >/tmp/opencode/postfgb-ro.log 2>&1 &
    sleep 2
    echo "writable $(curl -sfS -o /dev/null -w '%{http_code}' "{{url}}/readyz") readonly $(curl -sfS -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/readyz)"
