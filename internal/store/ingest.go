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
	"sync"
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

// valuesPool recycles the []*string a parsed line's field values land in — a 150K-line file used to
// mean 150K fresh map[string]string allocations; see internal/store/CLAUDE.md.
var valuesPool = sync.Pool{
	New: func() any { return make([]*string, 0, 8) },
}

func getValues(n int) []*string {
	v := valuesPool.Get().([]*string)
	if cap(v) < n {
		return make([]*string, n)
	}
	return v[:n]
}

func putValues(v []*string) {
	valuesPool.Put(v[:0])
}

type parsedLine struct {
	blank  bool
	ok     bool
	values []*string
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
				values := getValues(len(fields))
				ts, ok := parse.Line(lines[i], fields, values)
				if !ok {
					putValues(values)
					values = nil
				}
				results[i] = parsedLine{ok: ok, values: values, ts: ts}
			}
			return nil
		})
	}
	g.Wait()
	return results
}

// ingestChunkLines bounds how many lines LoadFile holds in memory at once (Go side) and how many rows the
// Appender buffers natively before a Flush — a var so tests can shrink it to exercise chunk boundaries.
var ingestChunkLines = 10_000

// LoadFile streams objectName through read→parse→append in ingestChunkLines-sized chunks inside one all-or-nothing transaction; it does NOT dedupe — callers check IsFileLoaded first (see internal/store/CLAUDE.md).
func (db *DB) LoadFile(ctx context.Context, w Wiretap, objectName string, r io.Reader) (IngestResult, error) {
	loadStart := time.Now()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	h, err := db.handle(w)
	if err != nil {
		return IngestResult{}, err
	}
	h.lockWrite()
	defer h.unlockWrite()

	conn, err := h.sql.Conn(ctx)
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
	var readDur, parseDur, insertDur time.Duration
	totalLines := 0

	err = conn.Raw(func(driverConn any) error {
		appender, err := duckdb.NewAppenderFromConn(driverConn.(driver.Conn), "", w.TableName)
		if err != nil {
			return err
		}
		defer appender.Close()

		row := make([]driver.Value, 0, len(w.Fields)+5)
		lines := make([]string, 0, ingestChunkLines)
		for {
			readStart := time.Now()
			lines = lines[:0]
			for len(lines) < ingestChunkLines && scanner.Scan() {
				lines = append(lines, scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				return err
			}
			readDur += time.Since(readStart)
			if len(lines) == 0 {
				return nil
			}

			parseStart := time.Now()
			parsed := parseLinesConcurrently(lines, w.Fields)
			parseDur += time.Since(parseStart)

			insertStart := time.Now()
			for i, p := range parsed {
				lineNo := totalLines + i + 1
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
				for fi, f := range w.Fields {
					if f.Column == parse.TimeColumn {
						row = append(row, timeArg(p.ts))
						continue
					}
					if v := p.values[fi]; v != nil {
						row = append(row, *v)
					} else {
						row = append(row, nil)
					}
				}
				putValues(p.values)

				if err := appender.AppendRow(row...); err != nil {
					return err
				}
				result.RowsInserted++
			}
			totalLines += len(lines)
			if err := appender.Flush(); err != nil {
				return err
			}
			insertDur += time.Since(insertStart)
		}
	})
	if err != nil {
		return result, err
	}

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
		objectName, totalLines, result.RowsInserted, result.LinesSkipped,
		readDur, parseDur, parseWorkers, insertDur, time.Since(loadStart))
	return result, nil
}

func (db *DB) IsFileLoaded(ctx context.Context, w Wiretap, objectName string) (bool, error) {
	h, err := db.handle(w)
	if err != nil {
		return false, err
	}
	h.lockRead()
	defer h.unlockRead()
	var n int
	err = h.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM _meta_ingested_files WHERE wiretap_id = ? AND file_name = ?`,
		w.ID, objectName,
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
