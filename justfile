dsn := env("POSTFGB_DSN", "postgres://postgres:postgres@localhost:5432/postfgb?sslmode=disable")
url := env("POSTFGB_URL", "http://localhost:8080")
gdal := "ghcr.io/osgeo/gdal:ubuntu-full-latest"

default:
    @just --list

build:
    go build ./...

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

gen:
    go run ./cmd/postfgb gen --n 1000000 --out bench/features.geojson

ingest:
    go run ./cmd/postfgb ingest --dsn "{{dsn}}" --collection bench --file bench/features.geojson

serve:
    go run ./cmd/postfgb serve --dsn "{{dsn}}"

demo: pg-up gen ingest

export coll format out:
    docker run --rm --network host -v "{{justfile_directory()}}:/work" -w /work {{gdal}} ogr2ogr -f "{{format}}" "/work/{{out}}" "OAPIF:{{url}}" "{{coll}}"

tiles coll maxzoom="3":
    docker run --rm --network host -v "{{justfile_directory()}}:/work" -w /work {{gdal}} bash -c "ogr2ogr -f MVT /work/{{coll}}.mbtiles -dsco MAXZOOM={{maxzoom}} OAPIF:{{url}} {{coll}}"

bench:
    bench/run.sh
