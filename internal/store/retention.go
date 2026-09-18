package store

import (
	"context"
	"time"
)

// DeleteOlderThan removes every row in bucket's table with effective_ts before cutoff, reporting how many.
// Counts first and deletes second rather than relying on the driver's RowsAffected for DELETE.
func (db *DB) DeleteOlderThan(ctx context.Context, bucket string, cutoff time.Time) (int64, error) {
	table, err := db.TableForBucket(bucket)
	if err != nil {
		return 0, err
	}

	var count int64
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "`+table+`" WHERE COALESCE(time, colon_time) < ?`, cutoff,
	).Scan(&count); err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	if _, err := db.sql.ExecContext(ctx,
		`DELETE FROM "`+table+`" WHERE COALESCE(time, colon_time) < ?`, cutoff,
	); err != nil {
		return 0, err
	}
	return count, nil
}
