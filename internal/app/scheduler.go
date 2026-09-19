package app

import (
	"context"
	"time"
)

// pollTick is how often the scheduler checks whether any wiretap is due — the actual per-wiretap
// cadence is governed by each wiretap's own PollIntervalMinutes.
const pollTick = time.Minute

// scheduler runs auto-load and auto-retention for every enabled wiretap while the app is open. There
// is no background-when-closed service — this goroutine simply stops when the app does.
type scheduler struct {
	app    *App
	ticker *time.Ticker
	stopCh chan struct{}
}

func newScheduler(a *App) *scheduler {
	return &scheduler{app: a}
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
		if !w.AutoLoadEnabled {
			continue
		}
		due := w.LastPolledAt == nil ||
			now.Sub(*w.LastPolledAt) >= time.Duration(w.PollIntervalMinutes)*time.Minute
		if !due {
			continue
		}

		if _, err := s.app.autoLoadNewFiles(ctx, w); err != nil {
			println("scheduler: auto-load for wiretap", w.ID, "failed:", err.Error())
		}
		if w.RetentionDays > 0 {
			cutoff := now.AddDate(0, 0, -w.RetentionDays)
			if _, err := s.app.db.DeleteOlderThan(ctx, w, cutoff); err != nil {
				println("scheduler: retention for wiretap", w.ID, "failed:", err.Error())
			}
		}
		if err := s.app.db.MarkPolled(ctx, w.ID, now); err != nil {
			println("scheduler: mark polled for wiretap", w.ID, "failed:", err.Error())
		}
	}
}
