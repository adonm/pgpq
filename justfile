dsn := env("POSTFGB_DSN", "postgres://postgres:postgres@localhost:5432/postfgb?sslmode=disable")
url := env("POSTFGB_URL", "http://localhost:8080")
gdal := "ghcr.io/osgeo/gdal:ubuntu-full-latest"

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

tiles coll maxzoom="3":
    docker run --rm --network host -v "{{justfile_directory()}}:/work" -w /work {{gdal}} bash -c "ogr2ogr -f MVT /work/{{coll}}.mbtiles -dsco MAXZOOM={{maxzoom}} OAPIF:{{url}} {{coll}}"

bench:
    bench/run.sh

layercake layer bbox="" nlt="GEOMETRY":
    LAYER={{layer}} BBOX="{{bbox}}" NLT={{nlt}} docker run --rm -i -e LAYER -e BBOX -e NLT {{gdal}} bash -c 'set -euo pipefail; SPAT=""; if [ -n "$BBOX" ]; then SPAT="-spat $BBOX"; fi; NLTARG=""; if [ -n "$NLT" ]; then NLTARG="-nlt $NLT"; fi; ogr2ogr -f FlatGeobuf /tmp/out.fgb "/vsicurl/https://data.openstreetmap.us/layercake/${LAYER}.parquet" "$LAYER" $SPAT $NLTARG -mapFieldType StringList=String,IntegerList=Integer,RealList=Real,Integer64List=Integer64 -lco SPATIAL_INDEX=NO && exec cat /tmp/out.fgb' | curl -sfS -X PUT -H "Content-Type: application/flatgeobuf" --data-binary @- "{{url}}/collections/{{layer}}"
