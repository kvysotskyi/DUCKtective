package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"ducktective/internal/parse"
)

// SourceTypeGCS is the only supported source type today. Adding a new one means: a new SourceType
// constant, a new optional config struct on Wiretap, a case in internal/app's sourceFor, and a
// matching UI sub-form — nothing else in this file needs to change.
const SourceTypeGCS = "gcs"

type GCSSourceConfig struct {
	ProjectID string `json:"projectId"`
	Bucket    string `json:"bucket"`
}

// Wiretap bundles a source (where its files live), the fields to extract from its NDJSON lines, and
// its own retention/auto-load schedule. Its table holds exactly the columns in Fields plus the fixed
// bookkeeping columns (file_hash, raw, source_file, source_line, ingested_at).
type Wiretap struct {
	ID                  string           `json:"id"`
	Name                string           `json:"name"`
	SourceType          string           `json:"sourceType"`
	GCS                 *GCSSourceConfig `json:"gcs,omitempty"`
	Prefix              string           `json:"prefix"`
	TableName           string           `json:"tableName"`
	Fields              []parse.Field    `json:"fields"`
	RetentionDays       int              `json:"retentionDays"`
	AutoLoadEnabled     bool             `json:"autoLoadEnabled"`
	PollIntervalMinutes int              `json:"pollIntervalMinutes"`
	// LoadDaysBack limits loading to files modified in the last N days (0 = no limit).
	LoadDaysBack int        `json:"loadDaysBack"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastPolledAt *time.Time `json:"lastPolledAt"`
}

// WiretapInput is what the UI submits to create or update a wiretap.
type WiretapInput struct {
	Name                string           `json:"name"`
	SourceType          string           `json:"sourceType"`
	GCS                 *GCSSourceConfig `json:"gcs,omitempty"`
	Prefix              string           `json:"prefix"`
	Fields              []parse.Field    `json:"fields"`
	RetentionDays       int              `json:"retentionDays"`
	AutoLoadEnabled     bool             `json:"autoLoadEnabled"`
	PollIntervalMinutes int              `json:"pollIntervalMinutes"`
	LoadDaysBack        int              `json:"loadDaysBack"`
}

// DefaultFields pre-fills a new wiretap with the fields the original fixed schema always extracted.
func DefaultFields() []parse.Field {
	return []parse.Field{
		{Column: "time", JSONKeys: []string{"time", ":time"}, Required: true},
		{Column: "level", JSONKeys: []string{"level"}, Required: true},
		{Column: "msg", JSONKeys: []string{"msg"}, Required: true},
		{Column: "topic", JSONKeys: []string{":topic", "topic"}, Required: false},
		{Column: "accession", JSONKeys: []string{"accession"}, Required: false},
		{Column: "study_uid", JSONKeys: []string{"study_uid"}, Required: false},
	}
}

var reservedColumns = map[string]bool{
	"file_hash": true, "raw": true, "source_file": true, "source_line": true, "ingested_at": true,
}

// validColumn matches a safe SQL identifier — enforced because, unlike the old fixed schema, column
// names now come from user input and get concatenated directly into DDL/DML as quoted identifiers.
var validColumn = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func validateFields(fields []parse.Field) error {
	seen := map[string]bool{}
	requiredSeen := map[string]bool{"time": false, "level": false, "msg": false}

	for _, f := range fields {
		if !validColumn.MatchString(f.Column) {
			return fmt.Errorf("field column %q must be lowercase letters, digits, or underscores, starting with a letter", f.Column)
		}
		if len(f.JSONKeys) == 0 {
			return fmt.Errorf("field %q needs at least one JSON key", f.Column)
		}
		if slices.Contains(f.JSONKeys, "") {
			return fmt.Errorf("field %q has an empty JSON key", f.Column)
		}
		if seen[f.Column] {
			return fmt.Errorf("duplicate field column %q", f.Column)
		}
		seen[f.Column] = true

		if _, required := requiredSeen[f.Column]; required {
			requiredSeen[f.Column] = true
		} else if reservedColumns[f.Column] {
			return fmt.Errorf("%q is a reserved column name", f.Column)
		}
	}

	for col, present := range requiredSeen {
		if !present {
			return fmt.Errorf("missing required field %q", col)
		}
	}
	return nil
}

func validateSource(sourceType string, gcsCfg *GCSSourceConfig) error {
	switch sourceType {
	case SourceTypeGCS:
		if gcsCfg == nil || gcsCfg.ProjectID == "" || gcsCfg.Bucket == "" {
			return fmt.Errorf("gcs source requires a project and a bucket")
		}
		return nil
	default:
		return fmt.Errorf("unsupported source type %q", sourceType)
	}
}

func columnType(column string) string {
	if column == parse.TimeColumn {
		return "TIMESTAMP"
	}
	return "TEXT"
}

// CreateWiretap validates the input, creates the wiretap's dynamic table, and records it.
func (db *DB) CreateWiretap(ctx context.Context, in WiretapInput) (Wiretap, error) {
	if in.Name == "" {
		return Wiretap{}, fmt.Errorf("name is required")
	}
	if err := validateSource(in.SourceType, in.GCS); err != nil {
		return Wiretap{}, err
	}
	if err := validateFields(in.Fields); err != nil {
		return Wiretap{}, err
	}

	id := sanitizeIdent(in.Name)
	var exists int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM _meta_wiretaps WHERE id = ?`, id).Scan(&exists); err != nil {
		return Wiretap{}, err
	}
	if exists > 0 {
		return Wiretap{}, fmt.Errorf("a wiretap named %q (or producing the same id) already exists", in.Name)
	}

	w := Wiretap{
		ID:                  id,
		Name:                in.Name,
		SourceType:          in.SourceType,
		GCS:                 in.GCS,
		Prefix:              in.Prefix,
		TableName:           "w_" + id,
		Fields:              in.Fields,
		RetentionDays:       in.RetentionDays,
		AutoLoadEnabled:     in.AutoLoadEnabled,
		PollIntervalMinutes: in.PollIntervalMinutes,
		LoadDaysBack:        in.LoadDaysBack,
		CreatedAt:           time.Now().UTC(),
	}
	if w.PollIntervalMinutes <= 0 {
		w.PollIntervalMinutes = 15
	}

	var ddl strings.Builder
	ddl.WriteString(`CREATE TABLE "`)
	ddl.WriteString(w.TableName)
	// file_hash has no uniqueness constraint (and LoadFile no ON CONFLICT) deliberately — DuckDB's
	// per-row conflict-check path is drastically slower than a plain vectorized bulk insert. Duplicate
	// prevention lives at the file level instead: callers must check IsFileLoaded before calling
	// LoadFile, since each file loads inside one all-or-nothing transaction and can't leave partial
	// duplicate rows behind on its own.
	//
	// Bookkeeping columns come before the field columns, not after: LoadFile appends rows via DuckDB's
	// Appender, which binds positionally by physical column order, not by name. ALTER TABLE ADD COLUMN
	// (see UpdateWiretap) always appends new columns at the very end of the table, and UpdateWiretap
	// appends new fields at the end of Wiretap.Fields to match — so field columns must be the last
	// section of the table for those two "append at the end" behaviors to stay in sync.
	ddl.WriteString(`" (file_hash TEXT, raw TEXT, source_file TEXT, source_line INTEGER, ingested_at TIMESTAMP`)
	for _, f := range w.Fields {
		ddl.WriteString(`, "`)
		ddl.WriteString(f.Column)
		ddl.WriteString(`" `)
		ddl.WriteString(columnType(f.Column))
	}
	ddl.WriteString(`)`)
	if _, err := db.sql.ExecContext(ctx, ddl.String()); err != nil {
		return Wiretap{}, err
	}

	fieldsJSON, err := json.Marshal(w.Fields)
	if err != nil {
		return Wiretap{}, err
	}
	gcsJSON, err := json.Marshal(w.GCS)
	if err != nil {
		return Wiretap{}, err
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO _meta_wiretaps (
			id, name, source_type, gcs_config_json, prefix, table_name, fields_json,
			retention_days, auto_load_enabled, poll_interval_minutes, load_days_back, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Name, w.SourceType, string(gcsJSON), w.Prefix, w.TableName, string(fieldsJSON),
		w.RetentionDays, w.AutoLoadEnabled, w.PollIntervalMinutes, w.LoadDaysBack, w.CreatedAt,
	)
	if err != nil {
		return Wiretap{}, err
	}
	return w, nil
}

func scanWiretap(row interface{ Scan(...any) error }) (Wiretap, error) {
	var w Wiretap
	var fieldsJSON, gcsJSON string
	if err := row.Scan(
		&w.ID, &w.Name, &w.SourceType, &gcsJSON, &w.Prefix, &w.TableName, &fieldsJSON,
		&w.RetentionDays, &w.AutoLoadEnabled, &w.PollIntervalMinutes, &w.LoadDaysBack, &w.CreatedAt, &w.LastPolledAt,
	); err != nil {
		return Wiretap{}, err
	}
	if err := json.Unmarshal([]byte(fieldsJSON), &w.Fields); err != nil {
		return Wiretap{}, err
	}
	if gcsJSON != "" && gcsJSON != "null" {
		if err := json.Unmarshal([]byte(gcsJSON), &w.GCS); err != nil {
			return Wiretap{}, err
		}
	}
	return w, nil
}

const wiretapColumns = `id, name, source_type, gcs_config_json, prefix, table_name, fields_json,
	retention_days, auto_load_enabled, poll_interval_minutes, load_days_back, created_at, last_polled_at`

func (db *DB) ListWiretaps(ctx context.Context) ([]Wiretap, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+wiretapColumns+` FROM _meta_wiretaps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Wiretap
	for rows.Next() {
		w, err := scanWiretap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (db *DB) GetWiretap(ctx context.Context, id string) (Wiretap, error) {
	row := db.sql.QueryRowContext(ctx, `SELECT `+wiretapColumns+` FROM _meta_wiretaps WHERE id = ?`, id)
	return scanWiretap(row)
}

// UpdateWiretap updates source config/prefix/retention/auto-load/poll-interval/name, updates JSON
// keys on existing fields, and appends any genuinely new fields (via ALTER TABLE ADD COLUMN). It
// never removes or renames an existing column.
func (db *DB) UpdateWiretap(ctx context.Context, id string, in WiretapInput) (Wiretap, error) {
	existing, err := db.GetWiretap(ctx, id)
	if err != nil {
		return Wiretap{}, err
	}
	sourceType := in.SourceType
	if sourceType == "" {
		sourceType = existing.SourceType
	}
	gcsCfg := in.GCS
	if gcsCfg == nil {
		gcsCfg = existing.GCS
	}
	if err := validateSource(sourceType, gcsCfg); err != nil {
		return Wiretap{}, err
	}
	if err := validateFields(in.Fields); err != nil {
		return Wiretap{}, err
	}

	byColumn := make(map[string]int, len(existing.Fields))
	merged := append([]parse.Field(nil), existing.Fields...)
	for i, f := range merged {
		byColumn[f.Column] = i
	}

	for _, nf := range in.Fields {
		if i, ok := byColumn[nf.Column]; ok {
			merged[i].JSONKeys = nf.JSONKeys
			continue
		}
		if _, err := db.sql.ExecContext(ctx,
			`ALTER TABLE "`+existing.TableName+`" ADD COLUMN "`+nf.Column+`" `+columnType(nf.Column),
		); err != nil {
			return Wiretap{}, err
		}
		nf.Required = false
		merged = append(merged, nf)
		byColumn[nf.Column] = len(merged) - 1
	}

	fieldsJSON, err := json.Marshal(merged)
	if err != nil {
		return Wiretap{}, err
	}
	gcsJSON, err := json.Marshal(gcsCfg)
	if err != nil {
		return Wiretap{}, err
	}

	name := in.Name
	if name == "" {
		name = existing.Name
	}
	pollInterval := in.PollIntervalMinutes
	if pollInterval <= 0 {
		pollInterval = existing.PollIntervalMinutes
	}

	_, err = db.sql.ExecContext(ctx, `
		UPDATE _meta_wiretaps SET
			name = ?, source_type = ?, gcs_config_json = ?, prefix = ?, fields_json = ?,
			retention_days = ?, auto_load_enabled = ?, poll_interval_minutes = ?, load_days_back = ?
		WHERE id = ?`,
		name, sourceType, string(gcsJSON), in.Prefix, string(fieldsJSON),
		in.RetentionDays, in.AutoLoadEnabled, pollInterval, in.LoadDaysBack, id,
	)
	if err != nil {
		return Wiretap{}, err
	}
	return db.GetWiretap(ctx, id)
}

func (db *DB) DeleteWiretap(ctx context.Context, id string) error {
	w, err := db.GetWiretap(ctx, id)
	if err != nil {
		return err
	}
	if _, err := db.sql.ExecContext(ctx, `DROP TABLE IF EXISTS "`+w.TableName+`"`); err != nil {
		return err
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM _meta_ingested_files WHERE wiretap_id = ?`, id); err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, `DELETE FROM _meta_wiretaps WHERE id = ?`, id)
	return err
}

func (db *DB) MarkPolled(ctx context.Context, id string, at time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE _meta_wiretaps SET last_polled_at = ? WHERE id = ?`, at, id)
	return err
}
