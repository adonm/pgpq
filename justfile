dsn := env("POSTFGB_DSN", "postgres://postgres:postgres@localhost:5432/postfgb?sslmode=disable")
url := env("POSTFGB_URL", "http://localhost:8080")

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

bench-osm region="luxembourg":
    bench/run.sh "{{region}}"

ci: fmt-check vet test

pg-up:
    docker compose up -d --wait

pg-down:
    docker compose down -v

serve:
    go run ./cmd/postfgb serve --dsn "{{dsn}}"

serve-readonly:
    go run ./cmd/postfgb serve --dsn "{{dsn}}" --readonly

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
