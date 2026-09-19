package store

import (
	"context"
	"time"
)

// DeleteOlderThan removes every row in the wiretap's table with time before cutoff, reporting how many.
// Counts first and deletes second rather than relying on the driver's RowsAffected for DELETE.
func (db *DB) DeleteOlderThan(ctx context.Context, w Wiretap, cutoff time.Time) (int64, error) {
	var count int64
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "`+w.TableName+`" WHERE "time" < ?`, cutoff,
	).Scan(&count); err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	if _, err := db.sql.ExecContext(ctx,
		`DELETE FROM "`+w.TableName+`" WHERE "time" < ?`, cutoff,
	); err != nil {
		return 0, err
	}
	return count, nil
}
