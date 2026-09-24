package parse

import (
	"testing"
	"time"
)

func TestLinePreservesSourceWallClockOffset(t *testing.T) {
	// The source wrote 09:01:28 in its own -06:00 zone. The stored/returned time must show
	// 09:01:28 verbatim — never converted to UTC or any other zone.
	raw := `{"time":"2026-07-25T09:01:28.314926-06:00","level":"DEBUG","msg":"hi"}`
	fields := []Field{
		{Column: "time", JSONKeys: []string{"time"}, Required: true},
		{Column: "level", JSONKeys: []string{"level"}, Required: true},
		{Column: "msg", JSONKeys: []string{"msg"}, Required: true},
	}

	ts, ok := Line(raw, fields, make([]*string, len(fields)))
	if !ok {
		t.Fatal("Line() returned ok=false for valid JSON")
	}
	if ts == nil {
		t.Fatal("ts is nil, want a parsed timestamp")
	}

	want := time.Date(2026, 7, 25, 9, 1, 28, 314926000, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("ts = %v, want %v (source wall-clock digits, offset discarded rather than converted)", ts, want)
	}
	if ts.Hour() != 9 {
		t.Errorf("ts.Hour() = %d, want 9 — the -06:00 offset must not shift the wall-clock hour", ts.Hour())
	}
}

func TestLineEpochIsUTC(t *testing.T) {
	// Unlike an offset string, a bare epoch number has no source-local wall clock to preserve —
	// UTC is the only correct interpretation.
	raw := `{"time":1704240000,"level":"INFO","msg":"hi"}`
	fields := DefaultTestFields()

	ts, ok := Line(raw, fields, make([]*string, len(fields)))
	if !ok {
		t.Fatal("Line() returned ok=false for valid JSON")
	}
	if ts == nil {
		t.Fatal("ts is nil, want a parsed timestamp")
	}
	want := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("ts = %v, want %v", ts, want)
	}
}

func DefaultTestFields() []Field {
	return []Field{
		{Column: "time", JSONKeys: []string{"time"}, Required: true},
		{Column: "level", JSONKeys: []string{"level"}, Required: true},
		{Column: "msg", JSONKeys: []string{"msg"}, Required: true},
	}
}
