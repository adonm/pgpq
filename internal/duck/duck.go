package duck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/adonm/pgpq/internal/geo"
)

type catalogRow struct {
	View     string `json:"view_name"`
	IDColumn string `json:"id_column"`
	Geometry string `json:"geom_column"`
}

func Read(ctx context.Context, data []byte, name, idColumn, view string) (geo.Dataset, error) {
	dir, err := os.MkdirTemp("", "pgpq-duck-*")
	if err != nil {
		return geo.Dataset{}, err
	}
	defer os.RemoveAll(dir)
	dbPath := dir + "/source.duckdb"
	if err := os.WriteFile(dbPath, data, 0600); err != nil {
		return geo.Dataset{}, err
	}
	row, err := catalog(ctx, dbPath, name, view)
	if err != nil {
		return geo.Dataset{}, err
	}
	if row.IDColumn == "" {
		row.IDColumn = idColumn
	}
	if row.IDColumn == "" {
		return geo.Dataset{}, errors.New("duckdb: collection needs an id_column")
	}
	if row.View == "" {
		row.View = name
	}
	outPath := dir + "/cell.parquet"
	query := fmt.Sprintf("copy (select * from %s) to '%s' (format parquet);", quoteIdent(row.View), strings.ReplaceAll(outPath, "'", "''"))
	if err := run(ctx, dbPath, query); err != nil {
		return geo.Dataset{}, err
	}
	data, err = os.ReadFile(outPath)
	if err != nil {
		return geo.Dataset{}, err
	}
	return geo.ReadDataset(bytes.NewReader(data), geo.Options{IDColumn: row.IDColumn})
}

func catalog(ctx context.Context, dbPath, name, view string) (catalogRow, error) {
	var row catalogRow
	query := fmt.Sprintf(`select view_name, id_column, geom_column from layer_catalog where layer_name = %s or view_name = %s limit 1`, quote(name), quote(name))
	if view != "" {
		query = fmt.Sprintf(`select view_name, id_column, geom_column from layer_catalog where view_name = %s limit 1`, quote(view))
	}
	raw, err := output(ctx, dbPath, query)
	if err == nil {
		var rows []catalogRow
		if json.Unmarshal(raw, &rows) == nil && len(rows) > 0 {
			return rows[0], nil
		}
	}
	lookup := name
	if view != "" {
		lookup = view
	}
	query = fmt.Sprintf(`select table_name as view_name, '' as id_column, 'geometry' as geom_column from information_schema.tables where table_name = %s limit 1`, quote(lookup))
	raw, err = output(ctx, dbPath, query)
	if err != nil {
		return row, err
	}
	var rows []catalogRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return row, err
	}
	if len(rows) == 0 {
		return row, fmt.Errorf("duckdb: collection %q not found", name)
	}
	return rows[0], nil
}

func output(ctx context.Context, dbPath, query string) ([]byte, error) {
	binary := os.Getenv("PGPQ_DUCKDB")
	if binary == "" {
		binary = "duckdb"
	}
	cmd := exec.CommandContext(ctx, binary, dbPath, "-json", "-c", query)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("duckdb: %w: %s", err, commandError(out))
	}
	return out, nil
}

func run(ctx context.Context, dbPath, query string) error {
	binary := os.Getenv("PGPQ_DUCKDB")
	if binary == "" {
		binary = "duckdb"
	}
	cmd := exec.CommandContext(ctx, binary, dbPath, "-c", query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("duckdb: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func commandError(out []byte) string {
	return strings.TrimSpace(string(out))
}

func quote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}

func quoteIdent(text string) string {
	return `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
}
