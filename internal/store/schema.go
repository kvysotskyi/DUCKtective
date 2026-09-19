package store

import (
	"regexp"
	"strings"
)

var invalidIdentChars = regexp.MustCompile(`[^a-z0-9_]`)

// sanitizeIdent turns arbitrary user text into a safe SQL identifier fragment: lowercase,
// non-alphanumeric runs collapsed to underscores, prefixed if it would otherwise start with a digit.
func sanitizeIdent(s string) string {
	name := invalidIdentChars.ReplaceAllString(strings.ToLower(s), "_")
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		name = "x_" + name
	}
	return name
}
