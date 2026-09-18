package store

import (
	"regexp"
	"strings"
)

var invalidTableChars = regexp.MustCompile(`[^a-z0-9_]`)

func sanitizeTableName(bucket string) string {
	name := invalidTableChars.ReplaceAllString(strings.ToLower(bucket), "_")
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		name = "b_" + name
	}
	return name
}

const bucketTableColumns = `
	file_hash TEXT PRIMARY KEY,
	time TIMESTAMP,
	colon_time TIMESTAMP,
	level TEXT,
	msg TEXT,
	colon_topic TEXT,
	accession TEXT,
	study_uid TEXT,
	raw TEXT,
	source_file TEXT,
	source_line INTEGER,
	ingested_at TIMESTAMP
`

// TableForBucket returns the sanitized table name for a bucket, creating both the table and its
// bucket-name→table-name metadata row if they don't exist yet.
func (db *DB) TableForBucket(bucket string) (string, error) {
	table := sanitizeTableName(bucket)

	if _, err := db.sql.Exec(
		`INSERT INTO _meta_bucket_tables (bucket_name, table_name) VALUES (?, ?)
		 ON CONFLICT (bucket_name) DO NOTHING`,
		bucket, table,
	); err != nil {
		return "", err
	}

	if _, err := db.sql.Exec(`CREATE TABLE IF NOT EXISTS "` + table + `" (` + bucketTableColumns + `)`); err != nil {
		return "", err
	}

	return table, nil
}

// LoadedBuckets lists every bucket that has a local table, real bucket names (not sanitized table names).
func (db *DB) LoadedBuckets() ([]string, error) {
	rows, err := db.sql.Query(`SELECT bucket_name FROM _meta_bucket_tables ORDER BY bucket_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
