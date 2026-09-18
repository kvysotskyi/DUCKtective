// Package appdir resolves the per-OS user-config directory the app stores its database and rule overrides under.
package appdir

import (
	"os"
	"path/filepath"
)

const appName = "logviewer"

// Dir returns <UserConfigDir>/logviewer, creating nothing itself.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appName), nil
}
