package store

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"time"

	"logviewer/internal/parse"
	"logviewer/internal/rules"
)

type IngestResult struct {
	RowsInserted int
	LinesSkipped int
}

// LoadFile streams objectName line by line, parsing and inserting each into bucket's table. Re-loading the
// same file+line is a no-op (file_hash is the dedupe key), so calling this twice never duplicates rows.
func (db *DB) LoadFile(ctx context.Context, bucket, objectName string, r io.Reader) (IngestResult, error) {
	table, err := db.TableForBucket(bucket)
	if err != nil {
		return IngestResult{}, err
	}

	mapping, err := rules.Load(bucket)
	if err != nil {
		return IngestResult{}, err
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return IngestResult{}, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO "`+table+`" (
			file_hash, time, colon_time, level, msg, colon_topic,
			accession, study_uid, raw, source_file, source_line, ingested_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (file_hash) DO NOTHING
	`)
	if err != nil {
		return IngestResult{}, err
	}
	defer stmt.Close()

	var result IngestResult
	now := time.Now().UTC()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		row, ok := parse.Line(line, mapping)
		if !ok {
			result.LinesSkipped++
			continue
		}

		_, err := stmt.ExecContext(ctx,
			fileHash(bucket, objectName, lineNo),
			timeArg(row.Time), timeArg(row.ColonTime),
			strArg(row.Level), strArg(row.Msg), strArg(row.ColonTopic),
			strArg(row.Accession), strArg(row.StudyUID),
			row.Raw, objectName, lineNo, now,
		)
		if err != nil {
			return result, err
		}
		result.RowsInserted++
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO _meta_ingested_files (bucket_name, file_name, ingested_at, rows_ingested)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (bucket_name, file_name) DO UPDATE SET
			ingested_at = excluded.ingested_at,
			rows_ingested = excluded.rows_ingested
	`, bucket, objectName, now, result.RowsInserted); err != nil {
		return result, err
	}

	return result, tx.Commit()
}

func (db *DB) IsFileLoaded(bucket, objectName string) (bool, error) {
	var n int
	err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM _meta_ingested_files WHERE bucket_name = ? AND file_name = ?`,
		bucket, objectName,
	).Scan(&n)
	return n > 0, err
}

func fileHash(bucket, objectName string, lineNo int) string {
	h := sha256.Sum256([]byte(bucket + "/" + objectName + "#" + strconv.Itoa(lineNo)))
	return hex.EncodeToString(h[:])
}

func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func strArg(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
