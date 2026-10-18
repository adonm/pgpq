#!/usr/bin/env bash
set -euo pipefail

OUT=${1:?usage: parity.sh OUTPUT_DIR [BASE_URL]}
API=${2:-${POSTFGB_URL:-http://localhost:8080}}
COLL=corpus_mixed
DIR=${BASH_SOURCE[0]%/*}
[ "$DIR" = "${BASH_SOURCE[0]}" ] && DIR=.
FGB=$DIR/mixed.fgb
FILTERS=$DIR/filters.txt

cleanup() {
  curl -sS -o /dev/null -X DELETE "$API/collections/$COLL" || true
}
trap cleanup EXIT

curl -sS -o /dev/null -X DELETE "$API/collections/$COLL" || true
curl -fsS -T "$FGB" -H "Content-Type: application/flatgeobuf" "$API/collections/$COLL?flatten=on"
echo
mkdir -p "$OUT"

python3 - "$API" "$COLL" "$FILTERS" "$OUT" <<'PY'
import json, sys, urllib.parse, urllib.request

api, coll, filters, out = sys.argv[1:5]

def encode(params):
    pairs = []
    for part in params.split('&'):
        key, _, value = part.partition('=')
        pairs.append(urllib.parse.quote(key, safe='') + '=' + urllib.parse.quote(value, safe=''))
    return '&'.join(pairs)

def ids(params):
    url = '%s/collections/%s/items?%s&limit=1000000' % (api, coll, encode(params))
    found = []
    while url:
        with urllib.request.urlopen(url, timeout=300) as resp:
            doc = json.load(resp)
        found.extend(f['id'] for f in doc['features'])
        url = None
        for link in doc.get('links', []):
            if link.get('rel') == 'next':
                href = link['href']
                url = href if href.startswith('http') else api + href
    return found

order = lambda v: (isinstance(v, str), v)
for line in open(filters):
    line = line.rstrip('\n')
    if not line or line.startswith('#'):
        continue
    name, params, expected = line.split('\t')
    got = sorted(ids(params), key=order)
    with open(out + '/' + name + '.ids', 'w') as fh:
        fh.write(''.join(str(v) + '\n' for v in got))
    print('%-28s %6d  expected %s' % (name, len(got), expected))
PY
