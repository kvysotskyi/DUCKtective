// Package parse turns one NDJSON log line into typed columns per the bucket's field mapping.
package parse

import (
	"encoding/json"
	"time"

	"logviewer/internal/rules"
)

// Row holds the typed columns for a single line — every field is optional, nil when absent or unparsable.
type Row struct {
	Time       *time.Time
	ColonTime  *time.Time
	Level      *string
	Msg        *string
	ColonTopic *string
	Accession  *string
	StudyUID   *string
	Raw        string
}

// Line parses one NDJSON line. ok is false only when the line isn't valid JSON at all — a missing or
// malformed individual field never drops the line, it just leaves that column nil.
func Line(raw string, m rules.Mapping) (Row, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return Row{}, false
	}

	return Row{
		Time:       parseTimestamp(obj[m.Time]),
		ColonTime:  parseTimestamp(obj[m.ColonTime]),
		Level:      parseString(obj[m.Level]),
		Msg:        parseString(obj[m.Msg]),
		ColonTopic: parseString(obj[m.ColonTopic]),
		Accession:  parseString(obj[m.Accession]),
		StudyUID:   parseString(obj[m.StudyUID]),
		Raw:        raw,
	}, true
}

func parseString(raw json.RawMessage) *string {
	if raw == nil {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return &s
}

var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
}

func parseTimestamp(raw json.RawMessage) *time.Time {
	if raw == nil {
		return nil
	}

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
			return &t
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
