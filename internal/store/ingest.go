package store

import (
	"bufio"
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	duckdb "github.com/marcboeker/go-duckdb/v2"
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
// numbers come out identical to a sequential parse.
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

// ingestChunkLines bounds how many lines are held per chunk and how many rows one transaction (and
// therefore one checkpoint) covers — a var so tests can shrink it.
var ingestChunkLines = 10_000

// pendingChunkDepth is how many parsed chunks a PendingFile buffers ahead of CommitFile before its reader blocks.
const pendingChunkDepth = 2

// parseSlot lets one PendingFile parse at a time — overlapping downloads is the win, overlapping parses would just multiply parseWorkers goroutines.
var parseSlot = make(chan struct{}, 1)

type ingestChunk struct {
	lines       []string
	parsed      []parsedLine
	firstLineNo int
}

// PendingFile is a file whose download and parse are already streaming in the background, waiting for CommitFile to append it under the wiretap's write lock.
type PendingFile struct {
	w          Wiretap
	objectName string
	chunks     chan ingestChunk
	cancel     context.CancelFunc
	done       chan struct{}
	start      time.Time

	// Written only by the reader goroutine; read by CommitFile after <-done.
	readErr           error
	lines             int
	readDur, parseDur time.Duration
}

// PrepareFile starts reading and parsing r into bounded chunks right away, so this file's network and JSON work overlaps the database work for files ahead of it; CommitFile must follow exactly once.
func (db *DB) PrepareFile(ctx context.Context, w Wiretap, objectName string, r io.Reader) *PendingFile {
	ctx, cancel := context.WithCancel(ctx)
	p := &PendingFile{
		w:          w,
		objectName: objectName,
		chunks:     make(chan ingestChunk, pendingChunkDepth),
		cancel:     cancel,
		done:       make(chan struct{}),
		start:      time.Now(),
	}
	go p.read(ctx, r)
	return p
}

func (p *PendingFile) read(ctx context.Context, r io.Reader) {
	defer close(p.done)
	defer close(p.chunks)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	firstLineNo := 1
	for {
		readStart := time.Now()
		lines := make([]string, 0, ingestChunkLines)
		for len(lines) < ingestChunkLines && scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			p.readErr = err
			return
		}
		p.readDur += time.Since(readStart)
		if len(lines) == 0 {
			return
		}

		parseStart := time.Now()
		parseSlot <- struct{}{}
		parsed := parseLinesConcurrently(lines, p.w.Fields)
		<-parseSlot
		p.parseDur += time.Since(parseStart)

		select {
		case p.chunks <- ingestChunk{lines: lines, parsed: parsed, firstLineNo: firstLineNo}:
		case <-ctx.Done():
			p.readErr = ctx.Err()
			return
		}
		firstLineNo += len(lines)
		p.lines += len(lines)
	}
}

// LoadFile is PrepareFile followed immediately by CommitFile — the single-file case.
func (db *DB) LoadFile(ctx context.Context, w Wiretap, objectName string, r io.Reader) (IngestResult, error) {
	return db.CommitFile(ctx, db.PrepareFile(ctx, w, objectName, r))
}

// CommitFile appends a prepared file under the wiretap's write lock, one transaction per chunk, then marks it loaded; it first deletes any rows an earlier attempt left for this file, so re-loading is idempotent (see internal/store/CLAUDE.md). On error the reader is cancelled and the caller's Close of the body unblocks it.
func (db *DB) CommitFile(ctx context.Context, p *PendingFile) (result IngestResult, retErr error) {
	w := p.w
	h, err := db.handle(w)
	if err != nil {
		p.cancel()
		return IngestResult{}, err
	}
	defer func() {
		if retErr != nil {
			p.cancel()
		}
		db.heal(w.ID, retErr)
	}()
	h.lockWrite()
	defer h.unlockWrite()

	conn, err := h.sql.Conn(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	defer conn.Close()

	for _, q := range []string{
		`DELETE FROM "` + w.TableName + `" WHERE source_file = ?`,
		`DELETE FROM _meta_ingested_files WHERE file_name = ?`,
	} {
		if _, err := conn.ExecContext(ctx, q, p.objectName); err != nil {
			return IngestResult{}, err
		}
	}

	now := time.Now().UTC()
	row := make([]driver.Value, 0, len(w.Fields)+4)
	var insertDur time.Duration
	for c := range p.chunks {
		insertStart := time.Now()
		if err := appendChunk(ctx, conn, w, p.objectName, now, row, c, &result); err != nil {
			return result, err
		}
		insertDur += time.Since(insertStart)
	}
	<-p.done
	if p.readErr != nil {
		return result, p.readErr
	}

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO _meta_ingested_files (wiretap_id, file_name, ingested_at, rows_ingested)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (wiretap_id, file_name) DO UPDATE SET
			ingested_at = excluded.ingested_at,
			rows_ingested = excluded.rows_ingested
	`, w.ID, p.objectName, now, result.RowsInserted); err != nil {
		return result, err
	}

	log.Printf("[ingest] %s: lines=%d rows=%d skipped=%d read=%s parse=%s(workers=%d) insert=%s total=%s",
		p.objectName, p.lines, result.RowsInserted, result.LinesSkipped,
		p.readDur, p.parseDur, parseWorkers, insertDur, time.Since(p.start))
	return result, nil
}

// appendChunk writes one chunk in its own transaction through the Appender; a chunk is the unit a checkpoint has to hold in memory.
func appendChunk(ctx context.Context, conn *sql.Conn, w Wiretap, objectName string, now time.Time, row []driver.Value, c ingestChunk, result *IngestResult) error {
	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		return err
	}
	err := conn.Raw(func(driverConn any) error {
		appender, err := duckdb.NewAppenderFromConn(driverConn.(driver.Conn), "", w.TableName)
		if err != nil {
			return err
		}
		for i, p := range c.parsed {
			lineNo := c.firstLineNo + i
			if p.blank {
				continue
			}
			if !p.ok {
				result.LinesSkipped++
				continue
			}

			// Column order here must match the physical table layout (see createWiretapTable's DDL
			// comment): bookkeeping columns first, then fields in Wiretap.Fields order.
			row = row[:0]
			row = append(row, c.lines[i], objectName, lineNo, now)
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
				appender.Close()
				return err
			}
			result.RowsInserted++
		}
		return appender.Close()
	})
	if err != nil {
		conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// IsFileLoaded reports whether a completed CommitFile has marked objectName done — callers skip such files before downloading them.
func (db *DB) IsFileLoaded(ctx context.Context, w Wiretap, objectName string) (loaded bool, retErr error) {
	h, err := db.handle(w)
	if err != nil {
		return false, err
	}
	defer func() { db.heal(w.ID, retErr) }()
	h.lockRead()
	defer h.unlockRead()
	var n int
	err = h.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM _meta_ingested_files WHERE wiretap_id = ? AND file_name = ?`,
		w.ID, objectName,
	).Scan(&n)
	return n > 0, err
}

func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
