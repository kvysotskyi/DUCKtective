package store

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"io"
	"log"
	"runtime"
	"strconv"
	"strings"
	"time"

	duckdb "github.com/marcboeker/go-duckdb"
	"golang.org/x/sync/errgroup"

	"ducktective/internal/parse"
)

type IngestResult struct {
	RowsInserted int
	LinesSkipped int
}

// parseWorkers bounds how many goroutines parse log lines concurrently. Parsing is CPU-bound (JSON
// unmarshal per line), so cores+2 keeps every core busy without wildly oversubscribing it.
var parseWorkers = runtime.NumCPU() + 2

type parsedLine struct {
	blank  bool
	ok     bool
	values map[string]string
	ts     *time.Time
}

// parseLinesConcurrently splits lines into parseWorkers chunks, each parsed on its own goroutine —
// safe because parse.Line has no shared state. Results are written back by original index so line
// numbers (and therefore file_hash) come out identical to a sequential parse.
func parseLinesConcurrently(lines []string, fields []parse.Field) []parsedLine {
	results := make([]parsedLine, len(lines))
	if len(lines) == 0 {
		return results
	}

	workers := parseWorkers
	if workers > len(lines) {
		workers = len(lines)
	}
	chunkSize := (len(lines) + workers - 1) / workers

	var g errgroup.Group
	for start := 0; start < len(lines); start += chunkSize {
		end := start + chunkSize
		if end > len(lines) {
			end = len(lines)
		}
		start, end := start, end
		g.Go(func() error {
			for i := start; i < end; i++ {
				if strings.TrimSpace(lines[i]) == "" {
					results[i] = parsedLine{blank: true}
					continue
				}
				values, ts, ok := parse.Line(lines[i], fields)
				results[i] = parsedLine{ok: ok, values: values, ts: ts}
			}
			return nil
		})
	}
	g.Wait()
	return results
}

// LoadFile parses every line of objectName concurrently (see parseLinesConcurrently), then bulk-loads the
// parsed rows into the wiretap's table via DuckDB's native Appender. A prior version built batched
// multi-row "INSERT ... VALUES (...), (...)" statements instead; measured against a real ~90K-line file
// that ran at a flat ~600µs/row (batch size made no difference), which pointed at the SQL insert path
// itself — parsing/planning/binding through the driver — rather than row count as the bottleneck. The
// Appender writes columnar data chunks directly, bypassing the SQL layer entirely.
//
// LoadFile does NOT dedupe — calling it twice for the same file inserts every row twice. Callers must
// check IsFileLoaded first and skip files that are already loaded (see App.LoadFilesNow /
// App.autoLoadNewFiles); this is safe because the whole file loads inside one all-or-nothing
// transaction, so "already fully loaded" is the only duplicate scenario that can occur.
func (db *DB) LoadFile(ctx context.Context, w Wiretap, objectName string, r io.Reader) (IngestResult, error) {
	loadStart := time.Now()

	readStart := time.Now()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return IngestResult{}, err
	}
	readDur := time.Since(readStart)

	parseStart := time.Now()
	parsed := parseLinesConcurrently(lines, w.Fields)
	parseDur := time.Since(parseStart)

	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		return IngestResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			// Best-effort: the connection may already be broken, in which case the rollback is moot.
			conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var result IngestResult
	now := time.Now().UTC()
	insertStart := time.Now()

	err = conn.Raw(func(driverConn any) error {
		appender, err := duckdb.NewAppenderFromConn(driverConn.(driver.Conn), "", w.TableName)
		if err != nil {
			return err
		}
		defer appender.Close()

		row := make([]driver.Value, 0, len(w.Fields)+5)
		for i, p := range parsed {
			lineNo := i + 1
			if p.blank {
				continue
			}
			if !p.ok {
				result.LinesSkipped++
				continue
			}

			// Column order here must match the physical table layout (see CreateWiretap's DDL
			// comment): bookkeeping columns first, then fields in Wiretap.Fields order.
			row = row[:0]
			row = append(row, fileHash(w.ID, objectName, lineNo), lines[i], objectName, lineNo, now)
			for _, f := range w.Fields {
				if f.Column == parse.TimeColumn {
					row = append(row, timeArg(p.ts))
					continue
				}
				if v, present := p.values[f.Column]; present {
					row = append(row, v)
				} else {
					row = append(row, nil)
				}
			}

			if err := appender.AppendRow(row...); err != nil {
				return err
			}
			result.RowsInserted++
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	insertDur := time.Since(insertStart)

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO _meta_ingested_files (wiretap_id, file_name, ingested_at, rows_ingested)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (wiretap_id, file_name) DO UPDATE SET
			ingested_at = excluded.ingested_at,
			rows_ingested = excluded.rows_ingested
	`, w.ID, objectName, now, result.RowsInserted); err != nil {
		return result, err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return result, err
	}
	committed = true

	log.Printf("[ingest] %s: lines=%d rows=%d skipped=%d read=%s parse=%s(workers=%d) insert=%s total=%s",
		objectName, len(lines), result.RowsInserted, result.LinesSkipped,
		readDur, parseDur, parseWorkers, insertDur, time.Since(loadStart))
	return result, nil
}

func (db *DB) IsFileLoaded(ctx context.Context, wiretapID, objectName string) (bool, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM _meta_ingested_files WHERE wiretap_id = ? AND file_name = ?`,
		wiretapID, objectName,
	).Scan(&n)
	return n > 0, err
}

func fileHash(wiretapID, objectName string, lineNo int) string {
	h := sha256.Sum256([]byte(wiretapID + "/" + objectName + "#" + strconv.Itoa(lineNo)))
	return hex.EncodeToString(h[:])
}

func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
