// Package rules resolves the JSON-key-per-column mapping for a bucket, falling back to the default spelling.
package rules

import (
	"encoding/json"
	"os"
	"path/filepath"

	"logviewer/internal/appdir"
)

// Mapping is the JSON key each column is read from for one bucket.
type Mapping struct {
	Time       string `json:"time"`
	ColonTime  string `json:"colon_time"`
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	ColonTopic string `json:"colon_topic"`
	Accession  string `json:"accession"`
	StudyUID   string `json:"study_uid"`
}

func Default() Mapping {
	return Mapping{
		Time:       "time",
		ColonTime:  ":time",
		Level:      "level",
		Msg:        "msg",
		ColonTopic: ":topic",
		Accession:  "accession",
		StudyUID:   "study_uid",
	}
}

// Load reads <UserConfigDir>/logviewer/rules/<bucket>.json and overlays any fields it sets onto the default mapping.
// A missing override file, or one that can't be read, silently falls back to defaults.
func Load(bucket string) (Mapping, error) {
	m := Default()

	dir, err := appdir.Dir()
	if err != nil {
		return m, nil
	}

	data, err := os.ReadFile(filepath.Join(dir, "rules", bucket+".json"))
	if err != nil {
		return m, nil
	}

	var override Mapping
	if err := json.Unmarshal(data, &override); err != nil {
		return m, err
	}

	if override.Time != "" {
		m.Time = override.Time
	}
	if override.ColonTime != "" {
		m.ColonTime = override.ColonTime
	}
	if override.Level != "" {
		m.Level = override.Level
	}
	if override.Msg != "" {
		m.Msg = override.Msg
	}
	if override.ColonTopic != "" {
		m.ColonTopic = override.ColonTopic
	}
	if override.Accession != "" {
		m.Accession = override.Accession
	}
	if override.StudyUID != "" {
		m.StudyUID = override.StudyUID
	}

	return m, nil
}
