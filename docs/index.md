# Search GCS logs locally, without shipping them anywhere

Structured (NDJSON) logs in a GCS bucket are easy to store and hard to
search — `gsutil cat | grep` doesn't scale past a handful of files, and
standing up a hosted logging platform for occasional debugging is
overkill. Ducktective fills that gap: a desktop app that loads the log
files you point it at into a local embedded database and gives you a
real point-and-click search UI, entirely on your machine.

## What this tool does

Ducktective browses a GCS bucket/prefix, downloads the NDJSON files you
select (or lets it auto-discover), and parses whichever JSON fields you
configure into columns in an embedded [DuckDB](https://duckdb.org)
database — one file per user, nothing external to run. No SQL is ever
exposed to the UI; every filter is built from typed inputs.

It is not a replacement for a production observability stack. It's
for the case where you have log files sitting in a bucket and need to
search them without provisioning anything.

## Prerequisites

Before installing, ensure you have:

- macOS or Windows
- The [Google Cloud CLI](https://cloud.google.com/sdk/docs/install) (`gcloud`) installed
- Application Default Credentials configured (see below)
- Read access to the GCS bucket(s) you want to search

## Installation

1. Download the latest release from [GitHub Releases](https://github.com/kvysotskyi/Ducktective/releases)
2. Extract the archive
3. Move the app to your Applications folder (macOS) or wherever you keep installed apps (Windows)
4. Launch the application

## First launch & authentication

Ducktective uses your existing `gcloud` configuration — it never asks
for or stores credentials itself. Before first use:

```bash
gcloud auth application-default login
```

The app reads Application Default Credentials (ADC) the same way
`gcloud` does. If ADC isn't set up, the app's title bar shows a
credentials warning with the exact command to run; fix it and click
retry rather than restarting the app.

## Core concept: Wiretaps

A **Wiretap** is one configured log source — a GCS project, bucket, and
prefix, the fields to extract from each JSON line, and its own retention
and auto-load schedule. Create one Wiretap per bucket or log stream you
want to search.

## UI walkthrough

### Wiretaps list

The Wiretaps tab lists every configured source: its bucket, prefix,
retention policy, auto-load status, and when it was last polled.

![Wiretaps list showing a configured source](screenshots/wiretapsList.png)

### Creating or editing a Wiretap

Pick a GCP project and bucket — both searchable, since real accounts can
have dozens of each — set a prefix, and define the fields to extract.
`time`, `level`, and `msg` are required and pre-filled with sensible
defaults; add as many additional fields as your logs carry. Each field
can list multiple candidate JSON keys (first one present in a given line
wins), so one field survives producers that spell the same concept
differently.

![New/Edit Wiretap form with field mapping](screenshots/wiretapForm.png)

Below the form, **Browse & load files now** lists the objects under the
Wiretap's prefix and lets you load specific ones on demand — useful for
backfilling history before auto-load takes over.

### Searching

Select a Wiretap, then filter by free text (matched against the entire
raw JSON line, not just the fields you extracted), level, any configured
field, and a date/time range. Results load newest-first with infinite
scroll — no page-number pagination. Click any row to expand its parsed
fields and the original raw line.

![Search results with an expanded row showing parsed fields and raw JSON](screenshots/searchResults.png)

### Date/time filtering

The date/time picker is a custom-built calendar plus 24-hour numeric
spinners, not a native browser date input — native `<input type="date">`
controls render using OS locale (month/day order, AM/PM) with no way to
force a consistent, unambiguous format. Dates are always ISO
(`YYYY-MM-DD`) and times are always 24-hour.

![Custom ISO-format date picker popup](screenshots/datePicker.png)

## Auto-load and retention

A Wiretap can automatically discover and load new files under its prefix
on a poll interval you set, and/or delete rows older than a retention
period — both run on a background scheduler while the app is open, and
can also be triggered manually from the Wiretaps list ("Load now" /
"Run retention").

## Timestamps are never converted

A log line's own `time` field is stored and displayed using the literal
wall-clock digits the source wrote — Ducktective never converts it to
your local timezone or to UTC. DuckDB's `TIMESTAMP` type has no timezone
concept, so naively storing an offset-aware timestamp would silently
shift the displayed hour; Ducktective strips the offset at parse time
and keeps the original digits everywhere they're displayed. This applies
only to log content — operational timestamps like "last polled" use your
local time, since those are genuinely about your machine's clock.

## Why not just grep the files, or use a hosted logging platform?

**Direct file access (`gsutil cat`, `grep`) advantages:**
- No app to install
- Works in scripts and CI

**Direct file access disadvantages:**
- No structured filtering — you're grepping text, not querying fields
- No persistence — every search re-downloads and re-scans everything
- Painful across many files or a date range

**Hosted logging platform advantages:**
- Built for scale, multi-user, alerting, dashboards

**Hosted logging platform disadvantages:**
- Overkill for occasional debugging of a handful of buckets
- Your logs leave your machine
- Ongoing cost and setup for something you might use twice a week

**Ducktective is positioned for:** searching structured logs already
sitting in GCS, on your own machine, without provisioning anything or
sending data anywhere beyond the original GCS read.

## Security model

- **Authentication**: uses `gcloud auth application-default login` credentials
- **Authorization**: whatever IAM roles your ADC identity already has on the bucket (standard GCS object read access)
- **Credential storage**: none — Ducktective never stores or caches credentials; it reads ADC fresh each time
- **Data storage**: log data you load is stored locally, in a DuckDB file under your OS's per-user config directory, and never transmitted anywhere by the app itself

## Limitations

- **GCS only, for now**: the log-source layer is pluggable (see the repo's [internal/source/CLAUDE.md](https://github.com/kvysotskyi/Ducktective/blob/main/internal/source/CLAUDE.md)), but GCS is the only implemented source type today
- **Not a multi-user platform**: one local database per user, no sharing/collaboration features
- **Not officially supported**: this is an independent project, not affiliated with Google Cloud or DuckDB

## FAQ

### Does this replace a hosted logging platform like Elasticsearch or Datadog?

No. It's for searching log files already sitting in a GCS bucket without
provisioning infrastructure — not for production-scale observability,
alerting, or multi-user dashboards.

### Where is my data stored?

Locally, in a DuckDB file under your OS's per-user config directory.
Nothing is sent anywhere except the original reads from your GCS bucket.

### Can I search buckets from multiple GCP projects?

Yes. Each Wiretap picks its own project and bucket independently, so you
can have Wiretaps across as many projects as your ADC identity can access.

### Does it support log sources other than GCS?

Not yet. The source layer is built as a pluggable interface specifically
so other types (local files, S3, ...) can be added without reworking the
rest of the app — see the repository's `internal/source` package.

### Why not just use `gsutil` or a shell script?

You can, for a quick one-off. Ducktective is for when you're searching
the same buckets repeatedly and want field-aware filtering, a date range
picker, and persistence across sessions instead of re-downloading and
re-grepping every time.

### Does Ducktective modify or delete anything in my GCS bucket?

No. It only reads objects. Retention cleanup only deletes rows from its
own local database, never from the source bucket.

## Links

- [GitHub repository](https://github.com/kvysotskyi/Ducktective)
- [Releases](https://github.com/kvysotskyi/Ducktective/releases)
- [README](../README.md)
