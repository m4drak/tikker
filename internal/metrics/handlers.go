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

// family is one metric family: HELP/TYPE are emitted once, then all samples.
type family struct {
	name    string
	help    string
	typ     string
	samples []string
}

// collector groups samples by family, preserving first-seen family order, so
// the output has exactly one HELP/TYPE per family and contiguous samples.
type collector struct {
	ns    string
	order []*family
	byKey map[string]*family
}

func (c *collector) add(name, help, typ, labels, v string) {
	full := c.ns + name
	f, ok := c.byKey[full]
	if !ok {
		f = &family{name: full, help: help, typ: typ}
		c.byKey[full] = f
		c.order = append(c.order, f)
	}
	if labels != "" {
		labels = "{" + labels + "}"
	}
	f.samples = append(f.samples, full+labels+" "+v+"\n")
}

// render builds the whole body in memory (fast, small) so a slow client can
// never block the snapshot source for long.
func (s *Server) render(snap Snapshot) string {
	c := &collector{ns: s.ns, byKey: map[string]*family{}}
	gauge := func(name, help, labels string, v float64) {
		c.add(name, help, "gauge", labels, formatFloat(v))
	}
	// Counter-style ints are monotonic totals; gauge-style are levels/sizes.
	counter := func(name, help, labels string, v int64) {
		c.add(name, help, "counter", labels, strconv.FormatInt(v, 10))
	}

	gauge("start_time_seconds", "Process start time as a Unix timestamp in seconds.", "", float64(snap.Started))

	t := snap.Queue.Totals
	counter("queue_sent_total", "Messages sent successfully.", "", t.Sent)
	counter("queue_failed_total", "Messages that failed permanently.", "", t.Failed)
	counter("queue_retried_total", "Send attempts scheduled for retry.", "", t.Retried)
	counter("queue_possible_duplicates_total", "Messages possibly delivered more than once.", "", t.PossibleDuplicates)
	counter("queue_lease_lost_total", "Leases lost before the send result was stored.", "", t.LeaseLost)
	counter("queue_store_errors_total", "Store errors encountered by the queue.", "", t.StoreErrors)
	counter("queue_auth_errors_total", "Authentication errors from the router.", "", t.AuthErrors)
	gauge("queue_configured_channels", "Number of configured channels.", "", float64(len(snap.Queue.Channels)))
	counter("queue_recovered_leases_total", "Expired leases recovered at startup or by the sweeper.", "", snap.Queue.RecoveredLeases)
	counter("queue_expired_total", "Messages expired before being sent.", "", snap.Queue.Expired)

	for _, ch := range snap.Queue.Channels {
		lbl := "channel_id=\"" + escapeLabelValue(string(ch.ChannelID)) + "\""
		counter("channel_sent_total", "Messages sent successfully per channel.", lbl, ch.Sent)
		counter("channel_failed_total", "Messages that failed permanently per channel.", lbl, ch.Failed)
		counter("channel_retried_total", "Send attempts scheduled for retry per channel.", lbl, ch.Retried)
		counter("channel_possible_duplicates_total", "Messages possibly delivered more than once per channel.", lbl, ch.PossibleDuplicates)
		counter("channel_auth_errors_total", "Authentication errors per channel.", lbl, ch.AuthErrors)
		gauge("channel_active", "Whether the channel worker is active (1) or not (0).", lbl, boolGauge(ch.Active))
		gauge("channel_sending", "Whether the channel is currently sending (1) or not (0).", lbl, boolGauge(ch.Sending))
	}

	for _, h := range snap.Health {
		lbl := "channel_id=\"" + escapeLabelValue(string(h.ChannelID)) + "\",port=\"" + escapeLabelValue(h.Port) + "\""
		gauge("channel_health_severity", "Channel health: 0 unknown, 1 ok, 2 degraded, 3 down.", lbl, severity(h))
		gauge("channel_checked", "Whether the channel health has been checked (1) or not (0).", lbl, boolGauge(h.Checked))
		gauge("channel_auth_pending", "Whether the channel awaits authentication (1) or not (0).", lbl, boolGauge(h.AuthPending))
		if h.Signal != nil && h.Signal.RSSI != nil {
			gauge("channel_signal_rssi_dbm", "Signal strength (RSSI) in dBm.", lbl, float64(*h.Signal.RSSI))
		}
	}

	var b strings.Builder
	for _, f := range c.order {
		b.WriteString("# HELP " + f.name + " " + escapeHelp(f.help) + "\n")
		b.WriteString("# TYPE " + f.name + " " + f.typ + "\n")
		for _, line := range f.samples {
			b.WriteString(line)
		}
	}
	return b.String()
}

// escapeLabelValue escapes a label value per the Prometheus text format:
// only backslash, double quote and newline are escaped.
func escapeLabelValue(v string) string {
	return labelEscaper.Replace(v)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// escapeHelp escapes HELP text (backslash and newline only).
func escapeHelp(v string) string {
	return helpEscaper.Replace(v)
}

var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

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
