#!/usr/bin/env python3
"""Upload prepared GeoParquet 2.0 layers and measure virtual-file Range reads."""

import argparse
import json
import os
import re
import shlex
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,47}$")
LAYERS_FILE = Path(__file__).with_name("layers.tsv")


class BenchError(RuntimeError):
    pass


def fail(message):
    raise BenchError(message)


def note(message):
    print(f"== {message}", file=sys.stderr, flush=True)


def http_call(url, method="GET", data=None, headers=None, timeout=180, allowed=(200,)):
    request = urllib.request.Request(url, data=data, headers=headers or {}, method=method)
    start = time.perf_counter()
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = response.read()
            status = response.status
            response_headers = dict(response.headers)
    except urllib.error.HTTPError as exc:
        body = exc.read()
        raise BenchError(f"{method} {url} -> HTTP {exc.code}: {body[:300]!r}") from exc
    except urllib.error.URLError as exc:
        raise BenchError(f"{method} {url} failed: {exc}") from exc
    elapsed_ms = (time.perf_counter() - start) * 1000.0
    if status not in allowed:
        fail(f"{method} {url} -> HTTP {status}: {body[:300]!r}")
    return elapsed_ms, body, response_headers


def stats(values):
    if not values:
        return {}
    ordered = sorted(values)
    rank = max(0, min(len(ordered) - 1, int(0.95 * len(ordered))))
    return {
        "count": len(ordered),
        "median_ms": statistics.median(ordered),
        "p95_ms": ordered[rank],
        "min_ms": ordered[0],
        "max_ms": ordered[-1],
    }


def measure(fn, reps):
    fn()
    samples = []
    last = None
    for _ in range(reps):
        elapsed_ms, body, headers = fn()
        samples.append(elapsed_ms)
        last = (body, headers)
    out = stats(samples)
    out["bytes"] = len(last[0]) if last else 0
    return out


def psql_run(psql_cmd, sql):
    parts = shlex.split(psql_cmd)
    if not parts:
        fail("empty psql command")
    result = subprocess.run(parts + ["-v", "ON_ERROR_STOP=1", "-Atc", sql], text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode != 0:
        fail(f"psql failed: {result.stderr.strip() or result.stdout.strip()}")
    return result.stdout.strip()


def sql_literal(text):
    return "'" + text.replace("'", "''") + "'"


def cleanup_collection(psql_cmd, collection):
    if not psql_cmd:
        return
    psql_run(psql_cmd, "SELECT 1")
    psql_run(psql_cmd, f"DELETE FROM pgpq.collections WHERE name = {sql_literal(collection)}")
    note(f"deleted {collection}")


def collection_present(base_url, collection, timeout=30):
    url = f"{base_url}/v2/collections/{urllib.parse.quote(collection, safe='')}"
    try:
        http_call(url, timeout=timeout)
        return True
    except BenchError as exc:
        if "HTTP 404" in str(exc):
            return False
        raise


def load_json_rows(path):
    text = Path(path).read_text().strip()
    if not text:
        return []
    try:
        rows = json.loads(text)
    except ValueError:
        try:
            rows = [json.loads(line) for line in text.splitlines() if line.strip()]
        except ValueError as exc:
            fail(f"cannot parse {path}: {exc}")
    if not isinstance(rows, list):
        fail(f"{path} must contain JSON rows")
    return rows


def read_layer_defs(path):
    rows = {}
    try:
        lines = Path(path).read_text().splitlines()
    except OSError as exc:
        fail(f"cannot read layers file {path}: {exc}")
    for number, line in enumerate(lines, 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        fields = line.split("|")
        if len(fields) != 8:
            fail(f"{path}:{number}: expected 8 pipe-separated fields, got {len(fields)}")
        name, osm_layer, where, select, cql_filter, id_column, id_sql, assert_field = fields
        if not NAME_RE.match(name):
            fail(f"{path}:{number}: invalid layer name {name!r}")
        rows[name] = {
            "name": name,
            "osm_layer": osm_layer,
            "where": where,
            "select": select,
            "filter": cql_filter,
            "id_column": id_column,
            "id_sql": id_sql,
            "assert_field": assert_field,
        }
    if not rows:
        fail(f"no layers in {path}")
    return rows


def verify_parquet(path, expected, duckdb_image, timeout=180):
    if not duckdb_image:
        return {"status": "skipped", "reason": "no DuckDB image"}
    if not path.exists():
        return {"status": "failed", "reason": "missing file"}
    directory = path.parent.resolve()
    name = path.name.replace("'", "''")
    sql = (
        "INSTALL spatial; LOAD spatial; "
        f"SELECT count(*) AS features, any_value(ST_GeometryType(geometry)) AS geometry_type, "
        f"(SELECT logical_type FROM parquet_schema('/data/{name}') WHERE name = 'geometry') AS logical_type "
        f"FROM read_parquet('/data/{name}')"
    )
    command = ["docker", "run", "--rm", "--memory", "1g", "--cpus", "1", "-v", f"{directory}:/data:ro", duckdb_image, "duckdb", "-json", "-c", sql]
    try:
        result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    except FileNotFoundError:
        return {"status": "skipped", "reason": "docker not found"}
    except subprocess.TimeoutExpired:
        return {"status": "failed", "reason": "DuckDB verification timed out"}
    try:
        row = json.loads(result.stdout)[0]
    except (ValueError, IndexError):
        return {"status": "failed", "reason": result.stdout.strip()[-600:] or f"DuckDB exit {result.returncode}"}
    count = row.get("features")
    geometry = row.get("geometry_type")
    logical = row.get("logical_type") or ""
    ok = result.returncode == 0 and count == expected and "GeometryType" in logical
    if expected > 0:
        ok = ok and geometry not in (None, "None", "UNKNOWN")
    if not ok:
        return {"status": "failed", "reason": f"features={count} expected={expected} geometry={geometry} logical={logical[:120]}", "feature_count": count, "geometry": geometry}
    return {"status": "ok", "feature_count": count, "geometry": geometry, "logical_type": logical}


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default=os.environ.get("PGPQ_OSM_URL", "http://127.0.0.1:8094"))
    parser.add_argument("--parquet-dir", required=True)
    parser.add_argument("--extract", required=True)
    parser.add_argument("--meta", required=True)
    parser.add_argument("--layers-file", default=str(LAYERS_FILE))
    parser.add_argument("--duckdb-image", default=os.environ.get("PGPQ_OSM_DUCKDB_IMAGE", "pgpq-duckdb:2.0.0"))
    parser.add_argument("--layers", default="")
    parser.add_argument("--prefix", default=os.environ.get("PGPQ_OSM_PREFIX", "osm_"))
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--reps", type=int, default=10)
    parser.add_argument("--timeout", type=float, default=180.0)
    parser.add_argument("--psql-cmd", default="")
    parser.add_argument("--keep", action="store_true")
    parser.add_argument("--no-verify", action="store_true")
    parser.add_argument("--allow-remote", action="store_true")
    parser.add_argument("--stages", default="")
    return parser


def main():
    args = build_parser().parse_args()
    base_url = args.base_url.rstrip("/")
    parsed = urllib.parse.urlparse(base_url)
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        fail("base URL must be http(s)")
    if parsed.port in (8080, 8081):
        fail("refusing public 8080/8081; use a test worker such as http://127.0.0.1:8094")
    if parsed.hostname not in ("127.0.0.1", "localhost", "::1") and not args.allow_remote:
        fail("refusing a non-loopback test worker without --allow-remote")
    if not NAME_RE.match(args.prefix):
        fail(f"invalid prefix {args.prefix!r}")
    if args.reps < 1:
        fail("--reps must be positive")
    if not args.keep and not args.psql_cmd:
        fail("cleanup needs --psql-cmd, or pass --keep against a disposable server")

    work = Path(args.out_dir)
    pages = work / "pages"
    pages.mkdir(parents=True, exist_ok=True)
    parquet_dir = Path(args.parquet_dir).resolve()
    if not parquet_dir.is_dir():
        fail(f"missing parquet directory: {parquet_dir}")
    defs = read_layer_defs(args.layers_file)
    wanted = [part for part in args.layers.split(",") if part]
    if not wanted:
        wanted = list(defs)
    missing = [layer for layer in wanted if layer not in defs]
    if missing:
        fail(f"layers missing from {args.layers_file}: {', '.join(missing)}")
    meta_rows = load_json_rows(args.meta)
    meta_by_layer = {row["layer"]: row for row in meta_rows}
    meta = {}
    for layer in wanted:
        source = parquet_dir / f"{args.extract}_{layer}.parquet"
        if not source.is_file():
            fail(f"missing prepared layer: {source}")
        if layer not in meta_by_layer:
            fail(f"layer missing from {args.meta}: {layer}")
        meta[layer] = {**defs[layer], **meta_by_layer[layer]}

    stage_info = {}
    if args.stages:
        try:
            stage_info = json.loads(Path(args.stages).read_text())
        except (OSError, ValueError):
            stage_info = {}

    limits = {
        "duckdb_image": args.duckdb_image,
        "duckdb_memory": os.environ.get("PGPQ_OSM_DUCKDB_MEM", "1g"),
        "duckdb_cpus": os.environ.get("PGPQ_OSM_DUCKDB_CPUS", "1"),
        "go_memory": os.environ.get("GOMEMLIMIT", "1GiB"),
        "request_mode": "sequential",
    }
    report = {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "base_url": base_url,
        "parquet_dir": str(parquet_dir),
        "extract": args.extract,
        "meta": str(Path(args.meta).resolve()),
        "reps": args.reps,
        "prefix": args.prefix,
        "resource_limits": limits,
        "stages": stage_info,
        "layers": [],
    }

    note(f"benchmarking {len(wanted)} layer(s) against {base_url}")
    for layer in wanted:
        row = meta[layer]
        collection = f"{args.prefix}{layer}"
        if not NAME_RE.match(collection):
            fail(f"invalid collection name {collection!r}")
        source = parquet_dir / f"{args.extract}_{layer}.parquet"
        file_url = f"{base_url}/v2/collections/{urllib.parse.quote(collection, safe='')}.parquet"
        meta_url = f"{base_url}/v2/collections/{urllib.parse.quote(collection, safe='')}"
        entry = {"layer": layer, "collection": collection, "source": row, "checks": []}

        def check(name, condition, detail=""):
            passed = bool(condition)
            entry["checks"].append({"name": name, "ok": passed, "detail": detail if not passed else ""})
            if not passed:
                fail(f"{layer}: check failed: {name} {detail}".strip())

        if not args.keep:
            cleanup_collection(args.psql_cmd, collection)
            if collection_present(base_url, collection):
                fail(f"{collection} still exists after cleanup; does --psql-cmd target the worker's database?")
        try:
            body = source.read_bytes()
            note(f"POST {source.name} -> {collection}")
            upload_url = f"{base_url}/v2/collections/{urllib.parse.quote(collection, safe='')}?{urllib.parse.urlencode({'id_column': row['id_column']})}"
            elapsed, payload, _ = http_call(upload_url, method="POST", data=body, headers={"Content-Type": "application/vnd.apache.parquet"}, timeout=args.timeout)
            loaded = json.loads(payload)
            entry["upload"] = {"seconds": elapsed / 1000.0, **loaded}
            check("upload features", loaded.get("features") == row["features"], f"{loaded.get('features')} != {row['features']}")
            check("upload shards", loaded.get("shards", 0) > 0)
            check("upload version", loaded.get("version", 0) > 0)

            # HEAD, full download, and two Range probes.
            head_ms, _, head_headers = http_call(file_url, method="HEAD", timeout=args.timeout)
            entry["head_ms"] = head_ms
            length = int(head_headers.get("Content-Length", "0"))
            check("head length", length > 0, f"Content-Length={length}")
            check("accept ranges", head_headers.get("Accept-Ranges") == "bytes", head_headers.get("Accept-Ranges", ""))

            def get_full():
                return http_call(file_url, timeout=args.timeout)

            entry["download"] = measure(get_full, max(1, args.reps // 2))
            _, full_body, full_headers = get_full()
            virtual_path = pages / f"{layer}.parquet"
            virtual_path.write_bytes(full_body)
            check("download length", len(full_body) == length, f"{len(full_body)} != {length}")
            check("etag", full_headers.get("ETag") == head_headers.get("ETag"), f"{full_headers.get('ETag')} != {head_headers.get('ETag')}")
            if args.no_verify:
                entry["verify"] = {"status": "skipped"}
                check("virtual geoparquet", True, "verification skipped")
            else:
                entry["verify"] = verify_parquet(virtual_path, row["features"], args.duckdb_image)
                check("virtual geoparquet", entry["verify"].get("status") == "ok", str(entry["verify"]))

            first_end = min(length - 1, 65535)
            first_range = f"bytes=0-{first_end}"
            entry["range_first"] = measure(lambda: http_call(file_url, headers={"Range": first_range}, timeout=args.timeout, allowed=(206,)), args.reps)
            _, _, first_headers = http_call(file_url, headers={"Range": first_range}, timeout=args.timeout, allowed=(206,))
            check("first range", first_headers.get("Content-Range", "").startswith("bytes 0-"), first_headers.get("Content-Range", ""))

            suffix = min(length, 65536)
            suffix_range = f"bytes=-{suffix}"
            entry["range_footer"] = measure(lambda: http_call(file_url, headers={"Range": suffix_range}, timeout=args.timeout, allowed=(206,)), args.reps)
            _, tail, tail_headers = http_call(file_url, headers={"Range": suffix_range}, timeout=args.timeout, allowed=(206,))
            check("footer range", tail_headers.get("Content-Range", "").startswith(f"bytes {length - suffix}-"), tail_headers.get("Content-Range", ""))
            check("parquet footer magic", tail.endswith(b"PAR1"), tail[-16:].hex())
        finally:
            if not args.keep:
                cleanup_collection(args.psql_cmd, collection)
                try:
                    if collection_present(base_url, collection):
                        note(f"warning: {collection} still exists after cleanup")
                except BenchError as exc:
                    note(f"warning: could not confirm cleanup of {collection}: {exc}")
        report["layers"].append(entry)

    report_path = work / "report.json"
    report_path.write_text(json.dumps(report, indent=2) + "\n")
    markdown_path = work / "report.md"
    markdown_path.write_text(render_markdown(report) + "\n")
    note(f"wrote {report_path} and {markdown_path}")
    print(markdown_path)


def fmt(value, digits=1):
    return f"{value:.{digits}f}"


def timing(row, key):
    item = row.get(key) or {}
    return f"{fmt(item.get('median_ms', 0))} / {fmt(item.get('p95_ms', 0))}"


def render_markdown(report):
    lines = [
        "# OSM virtual GeoParquet benchmark",
        "",
        f"- Generated: {report['generated_at']}",
        f"- Test worker: `{report['base_url']}`",
        f"- GeoParquet source: `{report['parquet_dir']}` ({report['extract']})",
        f"- Reps: {report['reps']}",
        "",
    ]
    limits = report["resource_limits"]
    lines.append("Resource caps: DuckDB `{}` / {} CPU, server `GOMEMLIMIT={}`; requests are sequential.".format(
        f"{limits.get('duckdb_image', 'duckdb')} ({limits.get('duckdb_memory', '')})", limits.get("duckdb_cpus", "1"), limits["go_memory"]))
    if report.get("stages") and "prepare_seconds" in report["stages"]:
        lines.append(f"Prepare: {report['stages']['prepare_seconds']:.2f} s")
    lines += ["", "## Ingest and virtual file", "", "| Layer | Features | Shards | Upload s | Bytes | HEAD ms | Full GET | First range | Footer range |", "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
    for row in report["layers"]:
        upload = row.get("upload") or {}
        lines.append(f"| {row['layer']} | {upload.get('features', 0)} | {upload.get('shards', 0)} | {upload.get('seconds', 0):.2f} | {upload.get('bytes', 0)} | {fmt(row.get('head_ms', 0))} | {timing(row, 'download')} | {timing(row, 'range_first')} | {timing(row, 'range_footer')} |")
    lines += ["", "## Checks", ""]
    for row in report["layers"]:
        lines.append(f"### {row['layer']}")
        lines.append("")
        for item in row["checks"]:
            mark = "ok" if item["ok"] else "FAIL"
            suffix = f" - {item['detail']}" if item.get("detail") else ""
            lines.append(f"- **{mark}** {item['name']}{suffix}")
        verify = row.get("verify") or {}
        lines.append(f"- verify: {verify.get('status', 'missing')} ({verify.get('geometry', '')}, {verify.get('feature_count', '')} rows)")
        lines.append("")
    return "\n".join(lines)


if __name__ == "__main__":
    try:
        main()
    except BenchError as exc:
        print(f"bench: {exc}", file=sys.stderr)
        sys.exit(1)
