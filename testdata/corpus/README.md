# Filter parity corpus

`mixed.fgb` is a self-contained FlatGeobuf collection used to compare attribute-filter
results feature by feature while the filter path moves from the PostgreSQL `chunk_props`
sidecar to per-chunk statistics plus exact evaluation in Go. Every FlatGeobuf scalar
column type appears, together with the filter edge cases (nulls, missing keys, arrays,
LIKE, ranges, datetime, bbox).

## Regenerating

```sh
mise exec -- go run ./testdata/corpus/gen   # writes testdata/corpus/mixed.fgb
```

The generator is deterministic: the same bytes on every run.

## Columns

`ogrinfo -so -al testdata/corpus/mixed.fgb` reports:

| column | FlatGeobuf type | values |
| --- | --- | --- |
| `id` | Int | `0..29999`, unique, the feature id |
| `c_bool` | Bool | `true` when `id` is even |
| `c_byte` | Byte | `id % 9 - 4`, so `-4..4` |
| `c_ubyte` | UByte | `(id % 10) * 25`, so `0..225` |
| `c_short` | Short | `id % 2000 - 1000` |
| `c_ushort` | UShort | `30000 + id % 1000` |
| `c_uint` | UInt | `id * 3` |
| `c_long` | Long | `id * 1000003` (past int32, inside int64) |
| `c_ulong` | ULong | `10000000000 + id % 10000` (past uint32) |
| `c_float` | Float | `id % 100 / 2` (float32) |
| `c_double` | Double | `id % 1000 / 8 + 0.125` |
| `c_string` | String | `cat_00..cat_24` by `id % 25`, `"needle"` for exactly 10 features (`id % 3000 == 7`) |
| `c_date` | DateTime | `2026-01-01T00:00:00Z + id hours`, unique; the detected time column |
| `c_json` | String | JSON object text, see below |
| `c_tags` | String | JSON array of strings, see below |

Geometry is a Point on a `-10..9.8` by `40..48.97` grid, one point per feature.
`c_json` is `{"kind":…,"score":…,"flag":…,"tags":[…],"label":…}` with `kind` cycling over
`alpha/beta/gamma/delta`, `score = id % 1000`, `flag` following `id % 2`, `tags` a two- or
three-element array of strings, and `label` one of `L000..L049`; a `sparse` key
(`"s3"`, `"s0"`, …) is present only when `id % 3 == 0`. `c_tags` is a top-level JSON
array of strings (`r0..r6`, `u0`/`u1`), carrying `needle_tag` for the same 10 features as
`c_string`.

Twenty features (`id % 1500 == 0`) omit `c_byte`, `c_float`, `c_string`, `c_ulong`,
`c_json` and `c_tags`, so those columns have real nulls to filter on.

`flatten=on` promotes `c_json` to a Json column and flattens its scalar keys into
`c_json_kind`, `c_json_score`, `c_json_flag`, `c_json_label` and `c_json_sparse`. The
array-valued `c_json_tags` is *not* created: the current ingest skips array values when
choosing flattened keys, so `c_json.tags` stays inside the parent `c_json`. Array
membership is therefore exercised through the top-level `c_tags` Json column. `c_tags`
is likewise left as a Json column holding the array.

## Loading

```sh
curl -T testdata/corpus/mixed.fgb -H 'Content-Type: application/flatgeobuf' \
  'http://localhost:8080/collections/corpus_mixed?flatten=on'
```

Expected response (writable worker on `:8080`):

```json
{"chunks":6,"collection":"corpus_mixed","features":30000}
```

Expected stored chunk count for the full-detail tier:

```sh
docker exec postfgb-postgres-1 psql "$(cat /tmp/opencode/dsn.txt)" \
  -tAc "select count(*) from chunks where collection='corpus_mixed' and tier=0"
# 6
```

So the corpus is 30,000 features in 6 chunks (the ~2 MiB chunk target).

## filters.txt

One request per line, tab-separated: `name`, `params`, `expected`.

- `name` is an identifier and the output file name (`<name>.ids`).
- `params` is a **raw, unencoded** query string: `k=v` pairs joined by `&`, e.g.
  `filter=id >= 10 AND id < 20` or `bbox=-10,40,10,49&c_bool=true`. `parity.sh`
  percent-encodes each pair's key and value, so `=` inside a CQL2 filter value is fine
  but a literal `&` inside a value is not (none are used).
- `expected` is the independently computed match count, or `?` when it is left as a
  placeholder. **All 100 filters here have a computed count** (no `?`).

The 80 hand-written filters cover `=`, `<>`, `<`, `<=`, `>`, `>=`, `BETWEEN`, `IN`,
`LIKE`, `IS NULL`, `IS NOT NULL`, `NOT`, `AND`, `OR` and nested `NOT`; every typed
column; quoted and unquoted numeric literals; boolean literals; array membership;
missing/unknown keys; null values; bbox-only; bbox with a filter; and datetime windows.
`rand_01..rand_20` are 20 randomly generated predicate trees (fixed seed
`20261018`), rendered as CQL2 text.

Counts were computed from the file itself: `mixed.fgb` was decoded with the flatbuffers
reference reader and every predicate evaluated in Python, modelling the current filter
semantics (equality is `jsonb` containment, so a missing key yields no match; ordered
comparisons, `LIKE` and `IS NULL` use the guarded expression, so a missing key yields
NULL). All 100 counts agree with `numberMatched` on the running server today. GDAL
(`ogrinfo -so`) reports the same column list, 30,000 features and extent. The expected
counts are an anchor, not part of a parity check: `parity.sh` compares two runs against
each other.

## parity.sh

```sh
testdata/corpus/parity.sh /tmp/run-old
# change the filter path, then
testdata/corpus/parity.sh /tmp/run-new
diff -r /tmp/run-old /tmp/run-new
```

`parity.sh OUTPUT_DIR [BASE_URL]` (BASE_URL defaults to `$POSTFGB_URL` or
`http://localhost:8080`):

1. deletes any previous `corpus_mixed`,
2. PUTs `mixed.fgb` as `corpus_mixed?flatten=on`,
3. fetches every filter in `filters.txt` (following `rel=next` links) and writes the
   matched `id`s, sorted numerically, to `OUTPUT_DIR/<name>.ids`,
4. deletes `corpus_mixed` on exit (success or failure).

It touches only `corpus_mixed` and never other collections. With 30,000 features and the
100,000,000 page cap each filter fits in one page, so the sorted lists are stable across
runs.
