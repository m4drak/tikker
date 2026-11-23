// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/queue"
)

var errBoom = errors.New("boom")

type stubSource struct {
	snap Snapshot
	err  error
}

func (s *stubSource) Snapshot() (Snapshot, error) { return s.snap, s.err }

func TestHandlerRendersPrometheusText(t *testing.T) {
	src := &stubSource{snap: Snapshot{
		Started: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC).Unix(),
		Queue: queue.Stats{
			Started: true,
			Channels: []queue.ChannelStats{
				{ChannelID: "01ABCDEF", Sending: true},
			},
			Totals:          queue.Counters{Sent: 7, Failed: 1, PossibleDuplicates: 2},
			RecoveredLeases: 1,
		},
		Health: []health.ChannelStatus{
			{ChannelID: "01ABCDEF", Port: "lte1", State: domain.ChannelOK, Checked: true},
		},
	}}
	srv, err := New(Options{Source: src, Namespace: "tikker"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE tikker_start_time_seconds gauge",
		"tikker_start_time_seconds 1795435200",
		"# TYPE tikker_queue_sent_total counter",
		"tikker_queue_sent_total 7",
		"tikker_queue_failed_total 1",
		"tikker_queue_recovered_leases_total 1",
		"# TYPE tikker_channel_health_severity gauge",
		"channel_id=\"01ABCDEF\"",
		"tikker_channel_health_severity{channel_id=\"01ABCDEF\",port=\"lte1\"} 1",
	} {
		if !strings.Contains(body, want) && !strings.Contains(body, "--"+want) {
			// value may be part of label line; plain search of the metric line:
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q\n%s", want, body)
			}
		}
	}
	if !strings.Contains(body, "port=\"lte1\"") {
		t.Errorf("channel port label missing:\n%s", body)
	}
}

func TestHandlerSnapshotErrorIs500(t *testing.T) {
	srv, err := New(Options{Source: &stubSource{err: errBoom}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
}

func TestNewRequiresSource(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New() with nil Source should error")
	}
}
