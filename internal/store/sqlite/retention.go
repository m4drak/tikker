// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/m4drak/tikker/internal/clock"
)

// RetentionResult reports what one retention pass removed.
type RetentionResult struct {
	BodiesPurged      int
	IdempotencyPurged int
}

// Retain runs one retention pass at now: it empties the body of terminal
// messages created more than bodyRetention ago (SPEC §7; zero or negative
// disables body purging) and deletes idempotency records older than
// [IdempotencyTTL]. Both statements are no-ops, and write nothing, when
// there is nothing to remove.
func (s *Store) Retain(ctx context.Context, now time.Time, bodyRetention time.Duration) (RetentionResult, error) {
	var res RetentionResult
	var err error
	if bodyRetention > 0 {
		if res.BodiesPurged, err = s.Messages().PurgeBodies(ctx, now.Add(-bodyRetention)); err != nil {
			return res, err
		}
	}
	if res.IdempotencyPurged, err = s.Messages().PurgeIdempotency(ctx, now.Add(-IdempotencyTTL)); err != nil {
		return res, err
	}
	return res, nil
}

// RunRetention calls [Store.Retain] immediately and then every interval until
// ctx is cancelled. Failures are logged and do not stop the loop.
func (s *Store) RunRetention(ctx context.Context, clk clock.Clock, interval, bodyRetention time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	for {
		res, err := s.Retain(ctx, clk.Now(), bodyRetention)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Error("retention failed", "err", fmt.Sprint(err))
		case res.BodiesPurged > 0 || res.IdempotencyPurged > 0:
			log.Info("retention", "bodies_purged", res.BodiesPurged, "idempotency_purged", res.IdempotencyPurged)
		}
		select {
		case <-ctx.Done():
			return
		case <-clk.After(interval):
		}
	}
}
