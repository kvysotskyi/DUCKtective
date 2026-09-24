package store

import "context"

// CompactWiretap rewrites the wiretap's table into a fresh one holding only its current rows and swaps it in — unlike DELETE/VACUUM, this reliably reclaims disk space DuckDB otherwise never returns to the OS (see internal/store/CLAUDE.md).
func (db *DB) CompactWiretap(ctx context.Context, w Wiretap) error {
	tmp := w.TableName + "_compact_tmp"

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS "`+tmp+`"`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE "`+tmp+`" AS SELECT * FROM "`+w.TableName+`"`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE "`+w.TableName+`"`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE "`+tmp+`" RENAME TO "`+w.TableName+`"`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true

	_, err = db.sql.ExecContext(ctx, `CHECKPOINT`)
	return err
}
