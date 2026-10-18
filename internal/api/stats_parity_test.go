package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
)

func parityFeatureFGB(t *testing.T, count int) []byte {
	t.Helper()
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeInt},
		{Name: "n", Type: flattypes.ColumnTypeLong},
		{Name: "s", Type: flattypes.ColumnTypeString},
		{Name: "b", Type: flattypes.ColumnTypeBool},
		{Name: "d", Type: flattypes.ColumnTypeDateTime},
	}
	features := make([]chunk.InputFeature, 0, count)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		coords, err := json.Marshal([]float64{-10 + float64(i%200)*0.1, 40 + float64(i/200)})
		if err != nil {
			t.Fatal(err)
		}
		props := map[string]json.RawMessage{"id": json.RawMessage(strconv.Itoa(i))}
		if i%10 != 0 {
			props["n"] = json.RawMessage(strconv.Itoa((i * 7) % 100))
		}
		switch i % 10 {
		case 3:
		case 7:
			props["s"] = json.RawMessage("null")
		default:
			props["s"] = json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("cat_%02d", i%5)))
		}
		props["b"] = json.RawMessage(strconv.FormatBool(i%2 == 0))
		props["d"] = json.RawMessage(fmt.Sprintf("%q", base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339)))
		features = append(features, chunk.InputFeature{
			Geometry:   chunk.RawGeometry{Type: "Point", Coordinates: coords},
			Properties: props,
		})
	}
	body, err := chunk.EncodeCols("itest", cols, features)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPostgresMissingStatisticsFail(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	name := "itest_stats_missing"
	st.DropCollection(ctx, name)
	t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	h := New(st)
	if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", parityFeatureFGB(t, 200)); w.Code != 201 {
		t.Fatalf("ingest = %d: %s", w.Code, w.Body.String())
	}
	if _, err := st.Pool.Exec(ctx, `update collections set stats_version = 0 where name = $1`, name); err != nil {
		t.Fatal(err)
	}
	query := url.Values{"filter": {"n = 7"}, "limit": {"10"}, "numberMatched": {"true"}}
	w := call(h, "GET", "/collections/"+name+"/items?"+query.Encode(), "", nil)
	if w.Code != 500 {
		t.Fatalf("filter without statistics = %d, want 500: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), name) {
		t.Fatalf("error should name the collection: %s", w.Body.String())
	}
	if w := call(h, "GET", "/collections/"+name+"/items/0", "", nil); w.Code != 500 || !strings.Contains(w.Body.String(), name) {
		t.Fatalf("feature lookup without statistics = %d: %s", w.Code, w.Body.String())
	}
}

func itemsBody(t *testing.T, h http.Handler, name string, query url.Values) string {
	t.Helper()
	w := call(h, "GET", "/collections/"+name+"/items?"+query.Encode(), "", nil)
	if w.Code != 200 {
		t.Fatalf("%s: %d: %s", query.Encode(), w.Code, w.Body.String())
	}
	var doc struct {
		NumberMatched *int64            `json:"numberMatched"`
		Features      []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, raw := range doc.Features {
		var feature struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(raw, &feature); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, string(feature.ID))
	}
	sort.Strings(ids)
	matched := "nil"
	if doc.NumberMatched != nil {
		matched = strconv.FormatInt(*doc.NumberMatched, 10)
	}
	return matched + "|" + strings.Join(ids, ",")
}

func corpusFilters(t *testing.T) []struct {
	name, params, expected string
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "corpus", "filters.txt"))
	if err != nil {
		t.Skipf("corpus filters unavailable: %v", err)
	}
	var out []struct{ name, params, expected string }
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatalf("malformed filter line %q", line)
		}
		out = append(out, struct{ name, params, expected string }{parts[0], parts[1], parts[2]})
	}
	if len(out) == 0 {
		t.Fatal("no filters parsed")
	}
	return out
}

func corpusQuery(t *testing.T, params string) (url.Values, bool) {
	t.Helper()
	query := url.Values{}
	withBBox := false
	for _, pair := range strings.Split(params, "&") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			t.Fatalf("malformed param %q", pair)
		}
		if key == "bbox" {
			withBBox = true
		}
		query.Set(key, value)
	}
	query.Set("limit", "1000000")
	query.Set("numberMatched", "true")
	return query, withBBox
}

func TestCorpusParity(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	name := "itest_corpus"
	st.DropCollection(ctx, name)
	t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	h := New(st)
	fgb, err := os.ReadFile(filepath.Join("..", "..", "testdata", "corpus", "mixed.fgb"))
	if err != nil {
		t.Skipf("corpus unavailable: %v", err)
	}
	if w := call(h, "PUT", "/collections/"+name+"?flatten=on", "application/flatgeobuf", fgb); w.Code != 201 {
		t.Fatalf("ingest = %d: %s", w.Code, w.Body.String())
	}
	info, err := st.Collection(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !store.StatsReady(info) {
		t.Fatalf("corpus was not marked stats-ready: version %d stats %d", info.Version, info.StatsVersion)
	}

	for _, filter := range corpusFilters(t) {
		query, withBBox := corpusQuery(t, filter.params)
		got := itemsBody(t, h, name, query)
		want, err := strconv.ParseInt(filter.expected, 10, 64)
		if err != nil {
			t.Fatalf("%s: expected count %q: %v", filter.name, filter.expected, err)
		}
		matched, _, _ := strings.Cut(got, "|")
		if withBBox {
			if matched != "nil" {
				t.Fatalf("%s: numberMatched should be omitted with a bbox: %s", filter.name, got)
			}
		} else if matched != strconv.FormatInt(want, 10) {
			t.Fatalf("%s: numberMatched = %s, want %d", filter.name, matched, want)
		}
	}

	corpusOracle(t, st, info)
}

type oracleFeature struct {
	chunk int64
	ord   int
	id    string
	props map[string]any
}

func loadCorpusFeatures(t *testing.T, st *store.Store, name string) ([]oracleFeature, map[string]flattypes.ColumnType) {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `select id, data from chunks where collection = $1 and tier = 0 order by id`, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []oracleFeature
	var columns map[string]flattypes.ColumnType
	for rows.Next() {
		var chunkID int64
		var data []byte
		if err := rows.Scan(&chunkID, &data); err != nil {
			t.Fatal(err)
		}
		c, err := chunk.Open(data)
		if err != nil {
			t.Fatal(err)
		}
		features, err := c.SearchAll()
		if err != nil {
			t.Fatal(err)
		}
		cols := c.Columns()
		if columns == nil {
			columns = chunk.ColumnTypes(cols)
		}
		for i, f := range features {
			props := chunk.DecodeProperties(f.PropertiesBytes(), cols)
			out = append(out, oracleFeature{chunk: chunkID, ord: i, id: fmt.Sprint(props["id"]), props: props})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out, columns
}

type oracleNode struct {
	kind  string
	name  string
	op    string
	value any
	left  *oracleNode
	right *oracleNode
}

func oracleLeaves() []struct {
	name string
	kind string
	gen  func(*rand.Rand) any
} {
	return []struct {
		name string
		kind string
		gen  func(*rand.Rand) any
	}{
		{"id", "num", func(r *rand.Rand) any { return int64(r.IntN(30000)) }},
		{"c_byte", "num", func(r *rand.Rand) any { return int64(r.IntN(9) - 4) }},
		{"c_short", "num", func(r *rand.Rand) any { return int64(r.IntN(2000) - 1000) }},
		{"c_ulong", "num", func(r *rand.Rand) any { return int64(10000000000 + r.IntN(10000)) }},
		{"c_float", "num", func(r *rand.Rand) any { return float64(r.IntN(100)) / 2 }},
		{"c_double", "num", func(r *rand.Rand) any { return float64(r.IntN(1000))/8 + 0.125 }},
		{"c_string", "str", func(r *rand.Rand) any { return fmt.Sprintf("cat_%02d", r.IntN(25)) }},
		{"c_json_kind", "str", func(r *rand.Rand) any { return []string{"alpha", "beta", "gamma", "delta"}[r.IntN(4)] }},
		{"c_json_label", "str", func(r *rand.Rand) any { return fmt.Sprintf("L%03d", r.IntN(50)) }},
		{"c_json_score", "num", func(r *rand.Rand) any { return int64(r.IntN(1000)) }},
		{"c_json_flag", "bool", func(r *rand.Rand) any { return r.IntN(2) == 0 }},
		{"c_date", "date", func(r *rand.Rand) any {
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			return base.Add(time.Duration(r.IntN(30*24)) * time.Hour).Format(time.RFC3339)
		}},
	}
}

func randomOracleNode(r *rand.Rand, depth int) *oracleNode {
	if depth <= 0 || r.IntN(10) < 3 {
		if r.IntN(10) == 0 {
			return &oracleNode{kind: "isnull", name: oracleLeaves()[r.IntN(len(oracleLeaves()))].name}
		}
		leaf := oracleLeaves()[r.IntN(len(oracleLeaves()))]
		ops := []string{"=", "<>", "<", "<=", ">", ">=", "IN"}
		if leaf.kind == "bool" {
			ops = []string{"=", "<>"}
		}
		return &oracleNode{kind: "leaf", name: leaf.name, op: ops[r.IntN(len(ops))], value: leaf.gen(r)}
	}
	switch r.IntN(3) {
	case 0:
		return &oracleNode{kind: "not", left: randomOracleNode(r, depth-1)}
	case 1:
		return &oracleNode{kind: "and", left: randomOracleNode(r, depth-1), right: randomOracleNode(r, depth-1)}
	default:
		return &oracleNode{kind: "or", left: randomOracleNode(r, depth-1), right: randomOracleNode(r, depth-1)}
	}
}

func (n *oracleNode) text() string {
	switch n.kind {
	case "not":
		return "NOT (" + n.left.text() + ")"
	case "and":
		return "(" + n.left.text() + " AND " + n.right.text() + ")"
	case "or":
		return "(" + n.left.text() + " OR " + n.right.text() + ")"
	case "isnull":
		return n.name + " IS NULL"
	default:
		if n.op == "IN" {
			return fmt.Sprintf("%s IN (%s)", n.name, oracleLiteral(n.value))
		}
		return fmt.Sprintf("%s %s %s", n.name, n.op, oracleLiteral(n.value))
	}
}

func oracleLiteral(value any) string {
	switch v := value.(type) {
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return "'" + strings.ReplaceAll(fmt.Sprint(v), "'", "''") + "'"
	}
}

func corpusOracle(t *testing.T, st *store.Store, info store.CollectionInfo) {
	features, columns := loadCorpusFeatures(t, st, info.Name)
	trees := 300
	if raw := os.Getenv("POSTFGB_ORACLE_TREES"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			trees = n
		}
	}
	rng := rand.New(rand.NewPCG(20261018, 1))
	mismatches := 0
	for i := 0; i < trees; i++ {
		node := randomOracleNode(rng, 3)
		expr, err := cql.Parse(node.text())
		if err != nil {
			t.Fatalf("tree %d %q: %v", i, node.text(), err)
		}
		var reference, pruned []string
		for _, f := range features {
			if cql.Match(expr, f.props, columns) {
				reference = append(reference, f.id)
			}
		}
		ids, err := st.CandidateChunks(context.Background(), info, nil, expr)
		if err != nil {
			t.Fatalf("tree %d %q: %v", i, node.text(), err)
		}
		keep := make(map[int64]bool, len(ids))
		for _, id := range ids {
			keep[id] = true
		}
		for _, f := range features {
			if keep[f.chunk] && cql.Match(expr, f.props, columns) {
				pruned = append(pruned, f.id)
			}
		}
		if strings.Join(reference, ",") != strings.Join(pruned, ",") {
			mismatches++
			if mismatches <= 3 {
				t.Errorf("tree %d %q: %d reference matches, %d pruned matches", i, node.text(), len(reference), len(pruned))
			}
		}
	}
	if mismatches != 0 {
		t.Fatalf("%d of %d random trees mismatched", mismatches, trees)
	}
}
