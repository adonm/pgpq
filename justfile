dsn := env("POSTFGB_DSN", "postgres://postgres:postgres@localhost:5432/postfgb?sslmode=disable")

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

bench:
    bench/run.sh
