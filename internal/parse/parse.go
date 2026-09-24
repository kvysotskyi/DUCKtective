// Package parse turns one NDJSON log line into column values per a watcher's field definitions.
package parse

import (
	"encoding/json"
	"time"
)

// Field maps a column to an ordered list of candidate JSON keys — the first key present with the
// right-shaped value wins, so one field can absorb multiple producers' spellings of the same concept
// (e.g. "time" and ":time") without needing a separate column per spelling.
type Field struct {
	Column   string   `json:"column"`
	JSONKeys []string `json:"jsonKeys"`
	Required bool     `json:"required"`
}

// TimeColumn is the fixed column name for the one field parsed as a timestamp rather than a string.
const TimeColumn = "time"

// Line parses one NDJSON line against fields, filling values[i] (nil if absent) for fields[i] — the
// caller owns and can reuse/pool values, sized to len(fields); it is untouched when ok is false. ok is
// false only when the line isn't valid JSON at all — a missing or malformed individual field never
// drops the line, it just leaves that column's slot nil. ts is non-nil only when the "time" field
// (Column == TimeColumn) resolved to a parsable timestamp.
func Line(raw string, fields []Field, values []*string) (ts *time.Time, ok bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, false
	}

	for i, f := range fields {
		if f.Column == TimeColumn {
			values[i] = nil
			ts = firstTimestamp(obj, f.JSONKeys)
			continue
		}
		values[i] = firstString(obj, f.JSONKeys)
	}
	return ts, true
}

func firstString(obj map[string]json.RawMessage, keys []string) *string {
	for _, key := range keys {
		raw, present := obj[key]
		if !present {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return &s
		}
	}
	return nil
}

func firstTimestamp(obj map[string]json.RawMessage, keys []string) *time.Time {
	for _, key := range keys {
		raw, present := obj[key]
		if !present {
			continue
		}
		if t := parseTimestamp(raw); t != nil {
			return t
		}
	}
	return nil
}

var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
}

func parseTimestamp(raw json.RawMessage) *time.Time {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return parseTimeString(s)
	}

	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return parseEpoch(f)
	}

	return nil
}

func parseTimeString(s string) *time.Time {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			// Keep the literal wall-clock digits the source wrote, discarding whatever offset came
			// with them (e.g. "-06:00") — DuckDB's TIMESTAMP column has no timezone concept and
			// normalizes on write, so passing the offset through would silently convert every
			// timestamp to a different wall-clock time than what's actually in the log line.
			wallClock := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
			return &wallClock
		}
	}
	return nil
}

// parseEpoch treats values above 1e12 as milliseconds, otherwise seconds (with fractional seconds as nanos).
func parseEpoch(f float64) *time.Time {
	var t time.Time
	if f > 1e12 {
		t = time.UnixMilli(int64(f)).UTC()
	} else {
		sec := int64(f)
		nsec := int64((f - float64(sec)) * 1e9)
		t = time.Unix(sec, nsec).UTC()
	}
	return &t
}
