// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package metrics

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/queue"
)

// Snapshot is the point-in-time data rendered on each scrape.
type Snapshot struct {
	Queue   queue.Stats
	Health  []health.ChannelStatus
	Started int64 // unix seconds
}

// Source supplies the snapshot for a scrape.
type Source interface{ Snapshot() (Snapshot, error) }

// Options configures [New].
type Options struct {
	Source    Source
	Logger    *slog.Logger
	Namespace string
}

// Server renders Prometheus text metrics.
type Server struct {
	src Source
	log *slog.Logger
	ns  string
}

// New builds the metrics server.
func New(opts Options) (*Server, error) {
	if opts.Source == nil {
		return nil, fmt.Errorf("metrics: Source is required")
	}
	ns := strings.TrimSuffix(opts.Namespace, "_")
	if ns != "" {
		ns += "_"
	}
	return &Server{src: opts.Source, log: opts.Logger, ns: ns}, nil
}

// Handler returns GET /metrics.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		snap, err := s.src.Snapshot()
		if err != nil {
			if s.log != nil {
				s.log.Error("metrics: snapshot failed", "err", err.Error())
			}
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, s.render(snap))
	})
}

// render builds the whole body in memory (fast, small) so a slow client can
// never block the snapshot source for long.
func (s *Server) render(snap Snapshot) string {
	var b strings.Builder
	gauge := func(name, labels string, v float64) {
		writeLine(&b, s.ns+name, labels, formatFloat(v), "gauge")
	}
	counter := func(name, labels string, v int64) {
		writeLine(&b, s.ns+name, labels, strconv.FormatInt(v, 10), "counter")
	}
	// Counter-style ints are monotonic totals; gauge-style are levels/sizes.

	gauge("start_time_seconds", "", float64(snap.Started))

	t := snap.Queue.Totals
	counter("queue_sent_total", "", t.Sent)
	counter("queue_failed_total", "", t.Failed)
	counter("queue_retried_total", "", t.Retried)
	counter("queue_possible_duplicates_total", "", t.PossibleDuplicates)
	counter("queue_lease_lost_total", "", t.LeaseLost)
	counter("queue_store_errors_total", "", t.StoreErrors)
	counter("queue_auth_errors_total", "", t.AuthErrors)
	gauge("queue_configured_channels", "", float64(len(snap.Queue.Channels)))
	counter("queue_recovered_leases_total", "", snap.Queue.RecoveredLeases)
	counter("queue_expired_total", "", snap.Queue.Expired)

	for _, ch := range snap.Queue.Channels {
		lbl := fmt.Sprintf("channel_id=%q", ch.ChannelID)
		counter("channel_sent_total", lbl, ch.Sent)
		counter("channel_failed_total", lbl, ch.Failed)
		counter("channel_retried_total", lbl, ch.Retried)
		counter("channel_possible_duplicates_total", lbl, ch.PossibleDuplicates)
		counter("channel_auth_errors_total", lbl, ch.AuthErrors)
		gauge("channel_active", lbl, boolGauge(ch.Active))
		gauge("channel_sending", lbl, boolGauge(ch.Sending))
	}

	for _, h := range snap.Health {
		lbl := fmt.Sprintf("channel_id=%q,port=%q", h.ChannelID, h.Port)
		gauge("channel_health_severity", lbl, severity(h))
		gauge("channel_checked", lbl, boolGauge(h.Checked))
		gauge("channel_auth_pending", lbl, boolGauge(h.AuthPending))
		if h.Signal != nil && h.Signal.RSSI != nil {
			gauge("channel_signal_rssi_dbm", lbl, float64(*h.Signal.RSSI))
		}
	}

	return b.String()
}

// writeLine emits one line of the Prometheus text exposition format:
//
//	# TYPE <name> gauge|counter
//	<name>{<labels>} <value>
func writeLine(b *strings.Builder, name, labels, v string, typ string) {
	b.WriteString("# TYPE " + name + " " + typ + "\n")
	if labels != "" {
		labels = "{" + labels + "}"
	}
	b.WriteString(name + labels + " " + v + "\n")
}

func severity(h health.ChannelStatus) float64 {
	switch h.State {
	case domain.ChannelOK:
		return 1
	case domain.ChannelDegraded:
		return 2
	case domain.ChannelDown:
		return 3
	default:
		return 0 // unknown / not checked
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// formatFloat renders a float64 in a Prometheus-valid plain literal (no
// exponent notation — Prometheus parsers reject 'e').
func formatFloat(v float64) string {
	if v == float64(int64(v)) && v < 1e12 && v > -1e12 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
