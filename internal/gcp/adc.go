// Package gcp checks for local Application Default Credentials — no service account keys, gcloud/ADC only.
package gcp

import (
	"context"
	"os/exec"
	"strings"

	"golang.org/x/oauth2/google"
)

const RetryCommand = "gcloud auth application-default login"

// cloudPlatformScope covers both GCS and Resource Manager (project listing) — a plain
// `gcloud auth application-default login` grants this by default, so this isn't asking for
// anything beyond what a normal ADC setup already has.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// AuthStatus is what the UI renders on launch and after a retry.
type AuthStatus struct {
	Available bool   `json:"available"`
	Account   string `json:"account"`
	ProjectID string `json:"projectId"`
	Message   string `json:"message"`
}

// CheckADC never errors out to the caller — a missing/broken ADC setup is reported in the struct, not via error.
func CheckADC(ctx context.Context) AuthStatus {
	if _, err := google.FindDefaultCredentials(ctx, cloudPlatformScope); err != nil {
		return AuthStatus{
			Available: false,
			Message:   "No Application Default Credentials found. Run:\n" + RetryCommand,
		}
	}

	return AuthStatus{
		Available: true,
		Account:   gcloudConfigValue("account"),
		ProjectID: gcloudConfigValue("project"),
	}
}

func gcloudConfigValue(key string) string {
	out, err := exec.Command("gcloud", "config", "get-value", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
