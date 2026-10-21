dsn := env("PGPQ_DSN", "postgres://postgres:postgres@localhost:5432/pgpq?sslmode=disable")
url := env("PGPQ_URL", "http://localhost:8080")

default:
    @just --list

build:
    go build ./...

image repo:
    KO_DOCKER_REPO="{{repo}}" ko build ./cmd/pgpq

image-tar:
    ko build --push=false --tarball=pgpq-image.tar ./cmd/pgpq

fmt:
    gofmt -w .

fmt-check:
    test -z "$(gofmt -l .)"

vet:
    go vet ./...

test:
    go test -timeout 120s ./...

test-integration:
    PGPQ_TEST_DSN="{{dsn}}" go test -count=1 -timeout 300s ./...

ci: fmt-check vet test

pg-up:
    docker compose up -d --wait

pg-down:
    docker compose down -v

serve:
    go run ./cmd/pgpq serve --dsn "{{dsn}}"

serve-readonly:
    go run ./cmd/pgpq serve --dsn "{{dsn}}" --readonly

restart:
    #!/usr/bin/env bash
    set -euo pipefail
    go build -o bin/pgpq ./cmd/pgpq
    for port in 8080 8081; do
      pid=$(ss -tlnp | grep ":$port " | grep -oP 'pid=\K[0-9]+' | head -1 || true)
      if [ -n "$pid" ]; then kill "$pid"; fi
    done
    sleep 1
    setsid nohup bin/pgpq serve >/tmp/opencode/pgpq.log 2>&1 &
    setsid nohup bin/pgpq serve --addr 127.0.0.1:8081 --readonly >/tmp/opencode/pgpq-ro.log 2>&1 &
    sleep 2
    echo "writable $(curl -sfS -o /dev/null -w '%{http_code}' "{{url}}/readyz") readonly $(curl -sfS -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/readyz)"
