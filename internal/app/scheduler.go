package app

import (
	"context"
	"log"
	"time"

	"ducktective/internal/store"
)

// pollTick is how often the scheduler checks whether any wiretap is due — the actual per-wiretap
// cadence is governed by each wiretap's own PollIntervalMinutes.
const pollTick = time.Minute

// scheduler runs auto-load and auto-retention for every enabled wiretap while the app is open. There
// is no background-when-closed service — this goroutine simply stops when the app does.
type scheduler struct {
	app            *App
	ticker         *time.Ticker
	stopCh         chan struct{}
	reencodeFailed map[string]time.Time
}

func newScheduler(a *App) *scheduler {
	return &scheduler{app: a, reencodeFailed: map[string]time.Time{}}
}

func (s *scheduler) start(ctx context.Context) {
	s.ticker = time.NewTicker(pollTick)
	s.stopCh = make(chan struct{})
	go s.loop(ctx)
}

func (s *scheduler) stop() {
	if s.ticker != nil {
		s.ticker.Stop()
	}
	if s.stopCh != nil {
		close(s.stopCh)
	}
}

func (s *scheduler) loop(ctx context.Context) {
	for {
		select {
		case <-s.stopCh:
			return
		case <-s.ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *scheduler) tick(ctx context.Context) {
	wiretaps, err := s.app.db.ListWiretaps(ctx)
	if err != nil {
		println("scheduler: list wiretaps:", err.Error())
		return
	}

	now := time.Now().UTC()
	for _, w := range wiretaps {
		s.reencodeIfLegacy(ctx, w, now)
		hasRetention := w.RetentionDays > 0 || w.MaxSizeMB > 0
		if !w.AutoLoadEnabled && !hasRetention {
			continue
		}
		due := w.LastPolledAt == nil ||
			now.Sub(*w.LastPolledAt) >= time.Duration(w.PollIntervalMinutes)*time.Minute
		if !due {
			continue
		}

		if w.AutoLoadEnabled {
			if _, err := s.app.autoLoadNewFiles(ctx, w); err != nil {
				println("scheduler: auto-load for wiretap", w.ID, "failed:", err.Error())
			}
		}
		if hasRetention {
			if _, err := s.app.db.ApplyRetention(ctx, w, now); err != nil {
				println("scheduler: retention for wiretap", w.ID, "failed:", err.Error())
			}
		}
		if err := s.app.db.MarkPolled(ctx, w.ID, now); err != nil {
			println("scheduler: mark polled for wiretap", w.ID, "failed:", err.Error())
		}
	}
	s.app.db.CloseIdle(idleHandleTTL)
}

// idleHandleTTL is how long a wiretap's DuckDB instance stays open unused before its memory is released.
const idleHandleTTL = 10 * time.Minute

// reencodeRetryAfter spaces out retries of a failed legacy-format re-encode, which is a multi-minute rewrite on a big wiretap.
const reencodeRetryAfter = time.Hour

// reencodeIfLegacy rewrites a wiretap file created before ZSTD text storage (DuckDB ≤ 1.1) into the current format once; ~9× smaller on real logs.
func (s *scheduler) reencodeIfLegacy(ctx context.Context, w store.Wiretap, now time.Time) {
	if !s.app.db.NeedsReencode(w) || now.Sub(s.reencodeFailed[w.ID]) < reencodeRetryAfter {
		return
	}
	log.Printf("[reencode] %s: legacy storage format, rewriting with ZSTD text compression", w.Name)
	if _, err := s.app.db.CompactWiretap(ctx, w); err != nil {
		log.Printf("[reencode] %s failed (retry in %s): %v", w.Name, reencodeRetryAfter, err)
		s.reencodeFailed[w.ID] = now
	}
}
