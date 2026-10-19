# Mixed-type FlatGeobuf corpus

`mixed.fgb` is a self-contained FlatGeobuf collection used by the chunk and attribute
round-trip tests. Every FlatGeobuf scalar column type appears, together with the encoding
edge cases (nulls, missing keys, arrays and JSON objects).

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
| `c_date` | DateTime | `2026-01-01T00:00:00Z + id hours`, unique |
| `c_json` | String | JSON object text |
| `c_tags` | String | JSON array of strings |

Geometry is a Point on a `-10..9.8` by `40..48.97` grid, one point per feature.
`c_json` is `{"kind":…,"score":…,"flag":…,"tags":[…],"label":…}` with `kind` cycling over
`alpha/beta/gamma/delta`, `score = id % 1000`, `flag` following `id % 2`, `tags` a two- or
three-element array of strings, and `label` one of `L000..L049`; a `sparse` key
(`"s3"`, `"s0"`, …) is present only when `id % 3 == 0`. `c_tags` is a top-level JSON
array of strings (`r0..r6`, `u0`/`u1`), carrying `needle_tag` for the same 10 features as
`c_string`.

Twenty features (`id % 1500 == 0`) omit `c_byte`, `c_float`, `c_string`, `c_ulong`,
`c_json` and `c_tags`, so those columns have real nulls.
