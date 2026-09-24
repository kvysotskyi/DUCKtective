package store

import (
	"context"
	"fmt"
	"time"
)

// RetentionResult reports one ApplyRetention pass.
type RetentionResult struct {
	RowsDeleted    int64 `json:"rowsDeleted"`
	Compacted      bool  `json:"compacted"`
	BytesReclaimed int64 `json:"bytesReclaimed"`
	SizeBytes      int64 `json:"sizeBytes"`
}

// compactAfterDeletedFraction: compact once a pass has deleted at least this share of what remains — below it the rewrite costs more than it frees.
const compactAfterDeletedFraction = 0.25

// A size-cap pass trims the oldest share of rows proportional to the overshoot (plus 10% slack) within these bounds, so one rewrite usually lands under the cap yet a badly oversized wiretap converges over bounded passes rather than losing everything at once.
const (
	sizeCapMinTrimFraction = 0.10
	sizeCapMaxTrimFraction = 0.50
	sizeCapMaxPasses       = 3
)

// DeleteOlderThan removes rows whose time — or, for rows with no parsable time, ingested_at — is before cutoff, reporting how many.
func (db *DB) DeleteOlderThan(ctx context.Context, w Wiretap, cutoff time.Time) (int64, error) {
	h, err := db.handle(w)
	if err != nil {
		return 0, err
	}
	h.lockWrite()
	defer h.unlockWrite()
	return deleteOlderThanLocked(ctx, h, w, cutoff)
}

func deleteOlderThanLocked(ctx context.Context, h *wiretapHandle, w Wiretap, cutoff time.Time) (int64, error) {
	where := ` WHERE "time" < ? OR ("time" IS NULL AND ingested_at < ?)`
	var count int64
	if err := h.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+w.TableName+`"`+where, cutoff, cutoff).Scan(&count); err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	if _, err := h.sql.ExecContext(ctx, `DELETE FROM "`+w.TableName+`"`+where, cutoff, cutoff); err != nil {
		return 0, err
	}
	return count, nil
}

func rowCount(ctx context.Context, h *wiretapHandle, w Wiretap) (int64, error) {
	var n int64
	err := h.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+w.TableName+`"`).Scan(&n)
	return n, err
}

// oldestCutoff returns a cutoff that deleteOlderThanLocked can use to drop the oldest fraction of rows (inclusive of the boundary row).
func oldestCutoff(ctx context.Context, h *wiretapHandle, w Wiretap, remaining int64, fraction float64) (time.Time, error) {
	offset := int64(float64(remaining) * fraction)
	if offset < 1 {
		offset = 1
	}
	if offset >= remaining {
		offset = remaining - 1
	}
	var t time.Time
	err := h.sql.QueryRowContext(ctx,
		`SELECT COALESCE("time", ingested_at) AS t FROM "`+w.TableName+`" ORDER BY t LIMIT 1 OFFSET ?`, offset,
	).Scan(&t)
	// DuckDB TIMESTAMP is microsecond-resolution, so "< t+1µs" includes every row stamped exactly t.
	return t.Add(time.Microsecond), err
}

// ApplyRetention runs the wiretap's whole policy in one locked pass: age expiry, then the size cap, then a compaction when enough was deleted to pay for it.
func (db *DB) ApplyRetention(ctx context.Context, w Wiretap, now time.Time) (RetentionResult, error) {
	if w.RetentionDays <= 0 && w.MaxSizeMB <= 0 {
		return RetentionResult{}, fmt.Errorf("wiretap has no retention configured")
	}
	h, err := db.handle(w)
	if err != nil {
		return RetentionResult{}, err
	}
	h.lockWrite()
	defer h.unlockWrite()

	var res RetentionResult
	if w.RetentionDays > 0 {
		n, err := deleteOlderThanLocked(ctx, h, w, now.AddDate(0, 0, -w.RetentionDays))
		if err != nil {
			return res, err
		}
		res.RowsDeleted += n
	}

	remaining, err := rowCount(ctx, h, w)
	if err != nil {
		return res, err
	}
	needCompact := res.RowsDeleted > 0 &&
		(remaining == 0 || float64(res.RowsDeleted) >= compactAfterDeletedFraction*float64(remaining))

	capBytes := int64(w.MaxSizeMB) * 1024 * 1024
	for pass := 0; w.MaxSizeMB > 0 && pass < sizeCapMaxPasses; pass++ {
		size := db.wiretapSize(w.ID)
		if size <= capBytes {
			break
		}
		if remaining > 1 {
			frac := min(max((1-float64(capBytes)/float64(size))*1.1, sizeCapMinTrimFraction), sizeCapMaxTrimFraction)
			cutoff, err := oldestCutoff(ctx, h, w, remaining, frac)
			if err != nil {
				return res, err
			}
			n, err := deleteOlderThanLocked(ctx, h, w, cutoff)
			if err != nil {
				return res, err
			}
			res.RowsDeleted += n
			remaining -= n
		}
		reclaimed, err := compactLocked(ctx, db, h, w)
		if err != nil {
			return res, err
		}
		res.Compacted = true
		res.BytesReclaimed += reclaimed
		needCompact = false
		if remaining <= 1 {
			break
		}
	}

	if needCompact {
		reclaimed, err := compactLocked(ctx, db, h, w)
		if err != nil {
			return res, err
		}
		res.Compacted = true
		res.BytesReclaimed += reclaimed
	}
	res.SizeBytes = db.wiretapSize(w.ID)
	return res, nil
}
