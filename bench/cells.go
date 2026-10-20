package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/spatial"
)

type result struct {
	Features int            `json:"features"`
	Cells    int            `json:"cells"`
	Mean     float64        `json:"mean"`
	Max      int            `json:"max"`
	MaxToken string         `json:"max_token"`
	Top      map[string]int `json:"top"`
}

func main() {
	path := flag.String("file", "", "flatgeobuf file")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "usage: cells -file input.fgb")
		os.Exit(2)
	}
	f, err := os.Open(*path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	r, err := fgb.NewReader(f)
	if err != nil {
		panic(err)
	}
	counts := map[uint64]int{}
	total := 0
	for {
		feat, err := r.Next()
		if err != nil {
			break
		}
		shape, err := chunk.ParseFlat(r.Header(), feat)
		if err != nil || shape == nil {
			continue
		}
		bounds, ok := shape.Bound()
		if !ok {
			continue
		}
		cell := spatial.Partition(spatial.Key(bounds))
		counts[cell]++
		total++
	}
	type pair struct {
		cell  uint64
		count int
	}
	pairs := make([]pair, 0, len(counts))
	for c, n := range counts {
		pairs = append(pairs, pair{c, n})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].count > pairs[j].count })
	top := map[string]int{}
	for i, p := range pairs {
		if i >= 10 {
			break
		}
		top[spatial.Token(p.cell)] = p.count
	}
	res := result{Features: total, Cells: len(counts), Max: pairs[0].count, MaxToken: spatial.Token(pairs[0].cell), Top: top}
	if len(pairs) > 0 {
		res.Mean = float64(total) / float64(len(pairs))
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
}
