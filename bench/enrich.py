#!/usr/bin/env python3
import os
attrs = ["printf('m-%s-%s', coalesce(osm_id,''), coalesce(osm_way_id,'')) AS bench_id"]
for i in range(1, int(os.environ.get("BENCH_ATTRS", "34")) + 1):
    attrs.append(f"printf('structure-{i:03d}-%s', substr(coalesce(name,'x'),1,6)) AS attr_s_{i:03d}")
    attrs.append(f"CAST(substr(coalesce(osm_id,'0'), -4) AS INTEGER) + {i} AS attr_n_{i:03d}")
    attrs.append(f"(length(coalesce(name,'')) + {i}) / 7.0 AS attr_f_{i:03d}")
print("SELECT *, " + ", ".join(attrs) + " FROM multipolygons WHERE building IS NOT NULL")
