#!/usr/bin/env python3
"""Upload one DuckDB layer file per layer, measure pgpq, and clean up.

The harness is deliberately sequential: one POST, one collection, one set of
requests, one DELETE. It never talks to a public worker and never runs more
than one layer at a time.
"""

import argparse
import json
import math
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


class BenchError(RuntimeError):
    pass


def fail(message):
    raise BenchError(message)


def note(message):
    print(f"== {message}", file=sys.stderr, flush=True)


def http_call(url, method="GET", data=None, headers=None, timeout=180):
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
    if status != 200:
        fail(f"{method} {url} -> HTTP {status}: {body[:300]!r}")
    return elapsed_ms, body, response_headers


def stats(values):
    if not values:
        return {}
    ordered = sorted(values)
    rank = max(0, math.ceil(0.95 * len(ordered)) - 1)
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
        elapsed_ms, body = fn()
        samples.append(elapsed_ms)
        last = body
    out = stats(samples)
    out["bytes"] = len(last) if last is not None else 0
    return out


def parse_float_list(text, count, name):
    parts = [part.strip() for part in text.split(",")]
    if len(parts) != count:
        fail(f"{name} needs {count} comma-separated numbers")
    try:
        return [float(part) for part in parts]
    except ValueError:
        fail(f"{name} must contain numbers")


def overlap(a, b, epsilon=1e-9):
    return a[0] <= b[2] + epsilon and a[2] >= b[0] - epsilon and a[1] <= b[3] + epsilon and a[3] >= b[1] - epsilon


def geometry_bounds(geometry):
    if not isinstance(geometry, dict):
        return None
    acc = [math.inf, math.inf, -math.inf, -math.inf]

    def visit(coords):
        if not isinstance(coords, list) or not coords:
            return
        if isinstance(coords[0], (int, float)):
            if len(coords) < 2:
                return
            acc[0] = min(acc[0], float(coords[0]))
            acc[1] = min(acc[1], float(coords[1]))
            acc[2] = max(acc[2], float(coords[0]))
            acc[3] = max(acc[3], float(coords[1]))
            return
        for child in coords:
            visit(child)

    if geometry.get("type") == "GeometryCollection":
        for child in geometry.get("geometries") or []:
            child_bounds = geometry_bounds(child)
            if child_bounds:
                acc[0] = min(acc[0], child_bounds[0])
                acc[1] = min(acc[1], child_bounds[1])
                acc[2] = max(acc[2], child_bounds[2])
                acc[3] = max(acc[3], child_bounds[3])
    else:
        visit(geometry.get("coordinates"))
    if math.isinf(acc[0]):
        return None
    return acc


def psql_run(psql_cmd, sql):
    parts = shlex.split(psql_cmd)
    if not parts:
        fail("empty psql command")
    result = subprocess.run(
        parts + ["-v", "ON_ERROR_STOP=1", "-Atc", sql],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
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
    url = f"{base_url}/v2/collections/{urllib.parse.quote(collection, safe='')}/items?limit=1"
    try:
        http_call(url, timeout=timeout)
        return True
    except BenchError as exc:
        if "HTTP 404" in str(exc):
            return False
        raise


def verify_parquet(path, expected, gdal_image, timeout=180):
    if not gdal_image:
        return {"status": "skipped", "reason": "no gdal image"}
    if not path.exists():
        return {"status": "failed", "reason": "missing file"}
    directory = path.parent.resolve()
    command = [
        "docker", "run", "--rm", "--memory", "512m", "--cpus", "1",
        "-v", f"{directory}:/data:ro", gdal_image,
        "ogrinfo", "-ro", "-so", f"/data/{path.name}", path.stem,
    ]
    try:
        result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    except FileNotFoundError:
        return {"status": "skipped", "reason": "docker not found"}
    except subprocess.TimeoutExpired:
        return {"status": "failed", "reason": "ogrinfo timed out"}
    output = result.stdout
    count_match = re.search(r"Feature Count:\s*(-?\d+)", output)
    geometry_match = re.search(r"^Geometry:\s*(.+)$", output, re.MULTILINE)
    count = int(count_match.group(1)) if count_match else None
    geometry = geometry_match.group(1).strip() if geometry_match else None
    ok = result.returncode == 0 and count == expected
    if expected > 0:
        ok = ok and geometry not in (None, "None", "Unknown (any)")
    if not ok:
        tail = "\n".join(output.strip().splitlines()[-12:])
        return {
            "status": "failed",
            "reason": tail or f"ogrinfo exit {result.returncode}",
            "feature_count": count,
            "geometry": geometry,
        }
    return {"status": "ok", "feature_count": count, "geometry": geometry}


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default=os.environ.get("PGPQ_OSM_URL", "http://127.0.0.1:8094"))
    parser.add_argument("--duckdb-file", required=True)
    parser.add_argument("--meta", required=True)
    parser.add_argument("--layers", default="")
    parser.add_argument("--prefix", default=os.environ.get("PGPQ_OSM_PREFIX", "osm_"))
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--reps", type=int, default=10)
    parser.add_argument("--limit", type=int, default=100)
    parser.add_argument("--parquet-limit", type=int, default=1000)
    parser.add_argument("--bbox", default="")
    parser.add_argument("--bbox-fraction", type=float, default=0.25)
    parser.add_argument("--timeout", type=float, default=180.0)
    parser.add_argument("--psql-cmd", default="")
    parser.add_argument("--keep", action="store_true")
    parser.add_argument("--gdal-image", default=os.environ.get("PGPQ_OSM_GDAL_IMAGE", "ghcr.io/osgeo/gdal:ubuntu-full-latest"))
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
    meta_rows = json.loads(Path(args.meta).read_text())
    meta = {row["layer"]: row for row in meta_rows}
    wanted = [part for part in args.layers.split(",") if part]
    if not wanted:
        wanted = list(meta)
    missing = [layer for layer in wanted if layer not in meta]
    if missing:
        fail(f"layers missing from meta: {', '.join(missing)}")

    duckdb_file = Path(args.duckdb_file).resolve()
    if not duckdb_file.is_file():
        fail(f"missing DuckDB file: {duckdb_file}")
    body = duckdb_file.read_bytes()
    stage_info = {}
    if args.stages:
        try:
            stage_info = json.loads(Path(args.stages).read_text())
        except (OSError, ValueError):
            stage_info = {}

    bbox_override = parse_float_list(args.bbox, 4, "--bbox") if args.bbox else None
    limits = {
        "gdal_memory": os.environ.get("PGPQ_OSM_GDAL_MEM", "768m"),
        "gdal_cpus": os.environ.get("PGPQ_OSM_GDAL_CPUS", "1"),
        "duckdb_memory": os.environ.get("PGPQ_OSM_DUCKDB_MEM", "512MB"),
        "duckdb_threads": os.environ.get("PGPQ_OSM_DUCKDB_THREADS", "1"),
        "go_memory": os.environ.get("GOMEMLIMIT", "1GiB"),
        "request_mode": "sequential",
    }

    report = {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "base_url": base_url,
        "duckdb_file": str(duckdb_file),
        "duckdb_bytes": duckdb_file.stat().st_size,
        "meta": str(Path(args.meta).resolve()),
        "reps": args.reps,
        "limit": args.limit,
        "parquet_limit": args.parquet_limit,
        "bbox_fraction": args.bbox_fraction,
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
        if bbox_override:
            bbox = bbox_override
        else:
            if row.get("qxmin") is not None:
                xmin, ymin, xmax, ymax = (float(row["qxmin"]), float(row["qymin"]), float(row["qxmax"]), float(row["qymax"]))
            else:
                xmin, ymin, xmax, ymax = (float(row["xmin"]), float(row["ymin"]), float(row["xmax"]), float(row["ymax"]))
            width = xmax - xmin
            height = ymax - ymin
            cx = (xmax + xmin) / 2.0
            cy = (ymax + ymin) / 2.0
            half_w = width * args.bbox_fraction / 2.0
            half_h = height * args.bbox_fraction / 2.0
            bbox = [cx - half_w, cy - half_h, cx + half_w, cy + half_h]
        bbox_text = ",".join(f"{value:.7f}" for value in bbox)

        def items(params):
            return f"{base_url}/v2/collections/{urllib.parse.quote(collection, safe='')}/items?{urllib.parse.urlencode(params)}"

        def call_json(params):
            def run():
                elapsed, payload, _headers = http_call(items(params), timeout=args.timeout)
                return elapsed, payload
            return run

        def call_parquet(params):
            def run():
                elapsed, payload, _headers = http_call(items(params), timeout=args.timeout)
                return elapsed, payload
            return run

        entry = {
            "layer": layer,
            "collection": collection,
            "source": row,
            "bbox": bbox,
            "checks": [],
            "warnings": [],
        }

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
            note(f"POST {duckdb_file.name} -> {collection}")
            elapsed, payload, _headers = http_call(
                f"{base_url}/v2/collections/{urllib.parse.quote(collection, safe='')}",
                method="POST",
                data=body,
                headers={"Content-Type": "application/vnd.duckdb"},
                timeout=args.timeout,
            )
            loaded = json.loads(payload)
            entry["upload"] = {"seconds": elapsed / 1000.0, **loaded}
            check("upload features", loaded.get("features") == row["features"], f"{loaded.get('features')} != {row['features']}")
            check("upload cells", loaded.get("cells", 0) > 0)
            check("single geometry type", row.get("types") == 1, f"{row.get('types')} geometry types")

            sanity = json.loads(http_call(items({"limit": 1}), timeout=args.timeout)[1])
            check("collection readable", sanity.get("numberReturned") == 1)

            entry["matched"] = measure(call_json({"limit": 1, "numberMatched": "true"}), max(3, args.reps // 2))
            matched_payload = json.loads(http_call(items({"limit": 1, "numberMatched": "true"}), timeout=args.timeout)[1])
            check("numberMatched", matched_payload.get("numberMatched") == row["features"], f"{matched_payload.get('numberMatched')} != {row['features']}")

            entry["bbox"] = measure(call_json({"bbox": bbox_text, "limit": args.limit}), args.reps)
            bbox_payload = json.loads(http_call(items({"bbox": bbox_text, "limit": args.limit}), timeout=args.timeout)[1])
            bbox_features = bbox_payload.get("features") or []
            check("bbox returns features", len(bbox_features) > 0, f"{len(bbox_features)} rows")
            bad_bbox = [feature.get("id") for feature in bbox_features if not (geometry_bounds(feature.get("geometry")) and overlap(geometry_bounds(feature["geometry"]), bbox))]
            check("bbox geometry", not bad_bbox, f"outside bbox: {bad_bbox[:3]}")

            entry["filter"] = measure(call_json({"filter": row["filter"], "limit": args.limit}), args.reps)
            filter_payload = json.loads(http_call(items({"filter": row["filter"], "limit": args.limit}), timeout=args.timeout)[1])
            filter_features = filter_payload.get("features") or []
            check("filter returns features", len(filter_features) > 0, f"{len(filter_features)} rows")
            field = row.get("assert_field") or ""
            if field:
                bad_filter = [feature.get("id") for feature in filter_features if (feature.get("properties") or {}).get(field) is None]
                check("filter property", not bad_filter, f"null {field}: {bad_filter[:3]}")

            feature_id = bbox_features[0].get("id") if bbox_features else (sanity.get("features") or [{}])[0].get("id")
            if feature_id is None:
                fail(f"{layer}: no feature id to look up")
            feature_id_text = str(feature_id)
            id_url = f"{base_url}/v2/collections/{urllib.parse.quote(collection, safe='')}/items/{urllib.parse.quote(feature_id_text, safe='')}"
            entry["id"] = measure(lambda: http_call(id_url, timeout=args.timeout)[:2], args.reps)
            id_payload = json.loads(http_call(id_url, timeout=args.timeout)[1])
            check("id lookup", str(id_payload.get("id")) == feature_id_text, f"{id_payload.get('id')} != {feature_id_text}")

            entry["parquet"] = measure(call_parquet({"bbox": bbox_text, "limit": args.parquet_limit, "f": "geoparquet"}), args.reps)
            parquet_path = pages / f"{layer}_bbox.parquet"
            _, parquet_payload, _ = http_call(items({"bbox": bbox_text, "limit": args.parquet_limit, "f": "geoparquet"}), timeout=args.timeout)
            parquet_path.write_bytes(parquet_payload)
            parquet_rows = len(json.loads(http_call(items({"bbox": bbox_text, "limit": args.parquet_limit}), timeout=args.timeout)[1]).get("features") or [])
            entry["parquet_verify"] = verify_parquet(parquet_path, parquet_rows, None if args.no_verify else args.gdal_image)
            if args.no_verify:
                entry["checks"].append({"name": "geoparquet", "ok": True, "detail": "verification skipped"})
            else:
                check("geoparquet", entry["parquet_verify"].get("status") == "ok", str(entry["parquet_verify"]))

            entry["overview"] = {}
            overview_payloads = {}
            for level in (6, 4, 2):
                entry["overview"][level] = measure(call_json({"overview": level, "limit": args.limit}), args.reps)
                overview_payloads[level] = json.loads(http_call(items({"overview": level, "limit": args.limit}), timeout=args.timeout)[1])
            check("overview 6 returns features", len(overview_payloads[6].get("features") or []) > 0)
            entry["overview_counts"] = {str(level): len(payload.get("features") or []) for level, payload in overview_payloads.items()}

            entry["overview_parquet"] = measure(
                call_parquet({"bbox": bbox_text, "overview": 6, "limit": args.parquet_limit, "f": "geoparquet"}),
                args.reps,
            )
            overview_parquet_path = pages / f"{layer}_overview6.parquet"
            _, overview_parquet_payload, _ = http_call(
                items({"bbox": bbox_text, "overview": 6, "limit": args.parquet_limit, "f": "geoparquet"}),
                timeout=args.timeout,
            )
            overview_parquet_path.write_bytes(overview_parquet_payload)
            overview_parquet_rows = len(
                json.loads(http_call(items({"bbox": bbox_text, "overview": 6, "limit": args.parquet_limit}), timeout=args.timeout)[1]).get("features") or []
            )
            entry["overview_parquet_verify"] = verify_parquet(
                overview_parquet_path, overview_parquet_rows, None if args.no_verify else args.gdal_image
            )
            if args.no_verify:
                entry["checks"].append({"name": "overview geoparquet", "ok": True, "detail": "verification skipped"})
            else:
                check("overview geoparquet", entry["overview_parquet_verify"].get("status") == "ok", str(entry["overview_parquet_verify"]))
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


def timing_overview(row):
    item = (row.get("overview") or {}).get(6) or (row.get("overview") or {}).get("6") or {}
    return f"{fmt(item.get('median_ms', 0))} / {fmt(item.get('p95_ms', 0))}"


def render_markdown(report):
    lines = []
    lines.append("# OSM layer benchmark")
    lines.append("")
    lines.append(f"- Generated: {report['generated_at']}")
    lines.append(f"- Test worker: `{report['base_url']}`")
    lines.append(f"- DuckDB source: `{report['duckdb_file']}` ({report['duckdb_bytes'] / 1024 / 1024:.1f} MiB)")
    lines.append(f"- Reps: {report['reps']}; JSON limit: {report['limit']}; GeoParquet limit: {report['parquet_limit']}")
    lines.append(f"- BBox: central {report['bbox_fraction']:.0%} of each layer's robust 5-95% centroid extent")
    lines.append("")
    limits = report["resource_limits"]
    lines.append("Resource caps: GDAL `{}` / {} CPU, DuckDB `{}` / {} thread, server `GOMEMLIMIT={}`; requests are sequential.".format(
        limits["gdal_memory"], limits["gdal_cpus"], limits["duckdb_memory"], limits["duckdb_threads"], limits["go_memory"]
    ))
    if report.get("stages"):
        stages = report["stages"]
        parts = []
        if "prepare_seconds" in stages:
            parts.append(f"prepare {stages['prepare_seconds']:.2f} s")
        if "build_seconds" in stages:
            parts.append(f"build {stages['build_seconds']:.2f} s")
        if "build_rss_kb" in stages:
            parts.append(f"build peak {stages['build_rss_kb'] / 1024:.0f} MiB")
        if parts:
            lines.append("")
            lines.append("Stages: " + ", ".join(parts) + ".")
    lines.append("")
    lines.append("## Ingest")
    lines.append("")
    lines.append("| Layer | Features | Cells | Partitions | Upload s |")
    lines.append("| --- | ---: | ---: | ---: | ---: |")
    for row in report["layers"]:
        upload = row.get("upload") or {}
        lines.append(f"| {row['layer']} | {upload.get('features', 0)} | {upload.get('cells', 0)} | {upload.get('partitions', 0)} | {upload.get('seconds', 0):.2f} |")
    lines.append("")
    lines.append("## Query timing (median / p95, ms)")
    lines.append("")
    lines.append("| Layer | bbox | filter | id | GeoParquet | overview 6 | overview GeoParquet | numberMatched |")
    lines.append("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
    for row in report["layers"]:
        lines.append(
            f"| {row['layer']} | {timing(row, 'bbox')} | {timing(row, 'filter')} | {timing(row, 'id')} | "
            f"{timing(row, 'parquet')} | {timing_overview(row)} | {timing(row, 'overview_parquet')} | {timing(row, 'matched')} |"
        )
    lines.append("")
    lines.append("## Overview sample counts")
    lines.append("")
    lines.append("| Layer | L6 rows returned | L4 rows returned | L2 rows returned |")
    lines.append("| --- | ---: | ---: | ---: |")
    for row in report["layers"]:
        counts = row.get("overview_counts") or {}
        lines.append(f"| {row['layer']} | {counts.get('6', 0)} | {counts.get('4', 0)} | {counts.get('2', 0)} |")
    lines.append("")
    lines.append("L4/L2 may be zero: pgpq drops features smaller than one display pixel at that level.")
    lines.append("")
    lines.append("## Checks")
    lines.append("")
    for row in report["layers"]:
        lines.append(f"### {row['layer']}")
        lines.append("")
        for item in row["checks"]:
            mark = "ok" if item["ok"] else "FAIL"
            suffix = f" - {item['detail']}" if item.get("detail") else ""
            lines.append(f"- **{mark}** {item['name']}{suffix}")
        for name in ("parquet_verify", "overview_parquet_verify"):
            verify = row.get(name) or {}
            lines.append(f"- {name}: {verify.get('status', 'missing')} ({verify.get('geometry', '')}, {verify.get('feature_count', '')} rows)")
        lines.append("")
    return "\n".join(lines)


if __name__ == "__main__":
    try:
        main()
    except BenchError as exc:
        print(f"bench: {exc}", file=sys.stderr)
        sys.exit(1)
