# internal/gcp

Application Default Credentials only — no service account key files, no
OAuth flow in-app. The user runs `gcloud auth application-default login`
once outside the app; everything here just checks for and uses whatever
ADC already resolves to.

## adc.go

`CheckADC(ctx) AuthStatus` never returns an error to its caller — a
missing/broken ADC setup is reported *in* the struct (`Available: false`
+ a message with the exact retry command), since the UI needs to render
that state, not crash on it. `cloudPlatformScope` (full
`cloud-platform` scope, not just `storage.ScopeReadOnly`) is required
because listing GCP projects needs Resource Manager access too — a
plain `gcloud auth application-default login` already grants this scope
by default, so this isn't asking for anything beyond a normal ADC setup.

## projects.go

`ListProjects(ctx) ([]Project, error)` — every `ACTIVE` project the ADC
identity can see, sorted by name, for the Wiretap-creation project
picker. A GCS bucket lives in exactly one project, and it isn't
necessarily whatever `gcloud config` currently has set as active — hence
this exists instead of just trusting the active config project.
