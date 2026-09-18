package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// resultCap is the row cap the UI is told about — results beyond it are trimmed, not silently dropped.
const resultCap = 1000

// Filters combine with AND; every field is optional and an empty Filters returns everything (up to the cap).
type Filters struct {
	TimeFrom  *time.Time `json:"timeFrom"`
	TimeTo    *time.Time `json:"timeTo"`
	Level     string     `json:"level"`
	Msg       string     `json:"msg"`
	Topic     string     `json:"topic"`
	Accession string     `json:"accession"`
	StudyUID  string     `json:"studyUid"`
	// Text matches anywhere in the original raw JSON line — covers fields that don't have their own
	// extracted column, since a bucket's lines can carry arbitrary extra keys beyond the fixed schema.
	Text string `json:"text"`
}

type LogRow struct {
	FileHash    string     `json:"fileHash"`
	EffectiveTS *time.Time `json:"effectiveTs"`
	Level       *string    `json:"level"`
	Msg         *string    `json:"msg"`
	Topic       *string    `json:"topic"`
	Accession   *string    `json:"accession"`
	StudyUID    *string    `json:"studyUid"`
	SourceFile  string     `json:"sourceFile"`
	SourceLine  int        `json:"sourceLine"`
}

type SearchResult struct {
	Rows      []LogRow `json:"rows"`
	Truncated bool     `json:"truncated"`
}

// Search runs one parameterized query built from whichever filters are set — no SQL ever reaches the caller.
func (db *DB) Search(ctx context.Context, bucket string, f Filters) (SearchResult, error) {
	table, err := db.TableForBucket(bucket)
	if err != nil {
		return SearchResult{}, err
	}

	var where []string
	var args []any

	if f.TimeFrom != nil {
		where = append(where, "COALESCE(time, colon_time) >= ?")
		args = append(args, *f.TimeFrom)
	}
	if f.TimeTo != nil {
		where = append(where, "COALESCE(time, colon_time) <= ?")
		args = append(args, *f.TimeTo)
	}
	if f.Level != "" {
		where = append(where, "level = ?")
		args = append(args, f.Level)
	}
	addLike := func(col, val string) {
		if val != "" {
			where = append(where, col+" LIKE ?")
			args = append(args, "%"+val+"%")
		}
	}
	addLike("msg", f.Msg)
	addLike("colon_topic", f.Topic)
	addLike("accession", f.Accession)
	addLike("study_uid", f.StudyUID)
	addLike("raw", f.Text)

	query := `SELECT file_hash, COALESCE(time, colon_time) AS effective_ts, level, msg, colon_topic,
		accession, study_uid, source_file, source_line FROM "` + table + `"`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY effective_ts DESC LIMIT %d", resultCap+1)

	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return SearchResult{}, err
	}
	defer rows.Close()

	var result SearchResult
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.FileHash, &r.EffectiveTS, &r.Level, &r.Msg, &r.Topic,
			&r.Accession, &r.StudyUID, &r.SourceFile, &r.SourceLine); err != nil {
			return SearchResult{}, err
		}
		result.Rows = append(result.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return SearchResult{}, err
	}

	if len(result.Rows) > resultCap {
		result.Rows = result.Rows[:resultCap]
		result.Truncated = true
	}
	return result, nil
}

// DistinctLevels feeds the level filter's dropdown from values already loaded for this bucket.
func (db *DB) DistinctLevels(ctx context.Context, bucket string) ([]string, error) {
	table, err := db.TableForBucket(bucket)
	if err != nil {
		return nil, err
	}

	rows, err := db.sql.QueryContext(ctx,
		`SELECT DISTINCT level FROM "`+table+`" WHERE level IS NOT NULL ORDER BY level`)
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

// RawLine backs the "inspect full line" view.
func (db *DB) RawLine(ctx context.Context, bucket, fileHash string) (string, error) {
	table, err := db.TableForBucket(bucket)
	if err != nil {
		return "", err
	}

	var raw string
	err = db.sql.QueryRowContext(ctx, `SELECT raw FROM "`+table+`" WHERE file_hash = ?`, fileHash).Scan(&raw)
	return raw, err
}
