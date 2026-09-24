package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// PageSize is one page of search results — the UI pages through results with Filters.Offset rather
// than ever loading everything that matches at once.
const PageSize = 100

// Filters combine with AND; every field is optional and an empty Filters returns everything (paged).
type Filters struct {
	TimeFrom *time.Time `json:"timeFrom"`
	TimeTo   *time.Time `json:"timeTo"`
	Level    string     `json:"level"`
	// Fields covers every non-time-non-level column (msg included) — column name -> substring match.
	Fields map[string]string `json:"fields"`
	// Text matches anywhere in the original raw JSON line — covers fields that don't have their own
	// extracted column, since a wiretap's lines can carry arbitrary extra keys beyond its configured fields.
	Text   string `json:"text"`
	Offset int    `json:"offset"`
}

type LogRow struct {
	Time       *time.Time        `json:"time"`
	Level      *string           `json:"level"`
	Fields     map[string]string `json:"fields"`
	SourceFile string            `json:"sourceFile"`
	SourceLine int               `json:"sourceLine"`
}

type SearchResult struct {
	Rows    []LogRow `json:"rows"`
	HasMore bool     `json:"hasMore"`
}

func otherColumns(w Wiretap) []string {
	cols := make([]string, 0, len(w.Fields))
	for _, f := range w.Fields {
		if f.Column == "time" || f.Column == "level" {
			continue
		}
		cols = append(cols, f.Column)
	}
	return cols
}

// Search runs one parameterized query built from whichever filters are set — no SQL ever reaches the caller.
func (db *DB) Search(ctx context.Context, w Wiretap, f Filters) (res SearchResult, retErr error) {
	defer func() { db.heal(w.ID, retErr) }()
	var where []string
	var args []any

	if f.TimeFrom != nil {
		where = append(where, `"time" >= ?`)
		args = append(args, *f.TimeFrom)
	}
	if f.TimeTo != nil {
		where = append(where, `"time" <= ?`)
		args = append(args, *f.TimeTo)
	}
	if f.Level != "" {
		where = append(where, `"level" = ?`)
		args = append(args, f.Level)
	}

	known := map[string]bool{}
	for _, c := range otherColumns(w) {
		known[c] = true
	}
	for col, val := range f.Fields {
		if val == "" || !known[col] {
			continue
		}
		where = append(where, `"`+col+`" LIKE ?`)
		args = append(args, "%"+val+"%")
	}

	if f.Text != "" {
		where = append(where, `raw LIKE ?`)
		args = append(args, "%"+f.Text+"%")
	}

	others := otherColumns(w)
	selectCols := []string{`"time"`, `"level"`}
	for _, c := range others {
		selectCols = append(selectCols, `"`+c+`"`)
	}
	selectCols = append(selectCols, "source_file", "source_line")

	query := `SELECT ` + strings.Join(selectCols, ", ") + ` FROM "` + w.TableName + `"`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	query += fmt.Sprintf(` ORDER BY "time" DESC LIMIT %d OFFSET %d`, PageSize+1, offset)

	h, err := db.handle(w)
	if err != nil {
		return SearchResult{}, err
	}
	h.lockRead()
	defer h.unlockRead()
	rows, err := h.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return SearchResult{}, err
	}
	defer rows.Close()

	var result SearchResult
	for rows.Next() {
		var ts *time.Time
		var level *string
		otherVals := make([]*string, len(others))

		dest := make([]any, 0, len(selectCols))
		dest = append(dest, &ts, &level)
		for i := range otherVals {
			dest = append(dest, &otherVals[i])
		}
		var sourceFile string
		var sourceLine int
		dest = append(dest, &sourceFile, &sourceLine)

		if err := rows.Scan(dest...); err != nil {
			return SearchResult{}, err
		}

		row := LogRow{
			Time: ts, Level: level,
			Fields:     make(map[string]string, len(others)),
			SourceFile: sourceFile, SourceLine: sourceLine,
		}
		for i, c := range others {
			if otherVals[i] != nil {
				row.Fields[c] = *otherVals[i]
			}
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return SearchResult{}, err
	}

	if len(result.Rows) > PageSize {
		result.Rows = result.Rows[:PageSize]
		result.HasMore = true
	}
	return result, nil
}

// DistinctLevels feeds the level filter's dropdown from values already loaded for this wiretap.
func (db *DB) DistinctLevels(ctx context.Context, w Wiretap) (levels []string, retErr error) {
	h, err := db.handle(w)
	if err != nil {
		return nil, err
	}
	defer func() { db.heal(w.ID, retErr) }()
	h.lockRead()
	defer h.unlockRead()
	rows, err := h.sql.QueryContext(ctx,
		`SELECT DISTINCT "level" FROM "`+w.TableName+`" WHERE "level" IS NOT NULL ORDER BY "level"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// RawLine backs the "inspect full line" view; a row is identified by where it came from, (source_file, source_line).
func (db *DB) RawLine(ctx context.Context, w Wiretap, sourceFile string, sourceLine int) (line string, retErr error) {
	h, err := db.handle(w)
	if err != nil {
		return "", err
	}
	defer func() { db.heal(w.ID, retErr) }()
	h.lockRead()
	defer h.unlockRead()
	var raw string
	err = h.sql.QueryRowContext(ctx,
		`SELECT raw FROM "`+w.TableName+`" WHERE source_file = ? AND source_line = ?`, sourceFile, sourceLine,
	).Scan(&raw)
	return raw, err
}
