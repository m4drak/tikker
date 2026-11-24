// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package metrics

import (
	"errors"
	"fmt"
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

func TestRenderFamiliesOncePerMetricAndContiguous(t *testing.T) {
	src := &stubSource{snap: Snapshot{
		Started: 1,
		Queue: queue.Stats{Channels: []queue.ChannelStats{
			{ChannelID: "A", Counters: queue.Counters{Sent: 1}}, {ChannelID: "B", Counters: queue.Counters{Sent: 2}}, {ChannelID: "C", Counters: queue.Counters{Sent: 3}},
		}},
		Health: []health.ChannelStatus{
			{ChannelID: "A", Port: "lte1", State: domain.ChannelOK},
			{ChannelID: "B", Port: "lte2", State: domain.ChannelDown},
		},
	}}
	srv, err := New(Options{Source: src, Namespace: "tikker"})
	if err != nil {
		t.Fatal(err)
	}
	body := srv.render(src.snap)
	if err := validateExposition(body); err != nil {
		t.Fatalf("invalid exposition: %v\n%s", err, body)
	}
	for _, want := range []string{
		`tikker_channel_sent_total{channel_id="A"} 1`,
		`tikker_channel_sent_total{channel_id="C"} 3`,
		`tikker_channel_health_severity{channel_id="B",port="lte2"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestEscapeLabelValue(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "abc", "abc"},
		{"quote", `a"b`, `a\"b`},
		{"backslash", `a\b`, `a\\b`},
		{"newline", "a\nb", `a\nb`},
		{"unicode kept", "zażółć", "zażółć"},
		{"control not hex-escaped", "a\x01b", "a\x01b"},
		{"all", "\\\"\n", `\\\"\n`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeLabelValue(tc.in); got != tc.want {
				t.Errorf("escapeLabelValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderEscapesLabels(t *testing.T) {
	src := &stubSource{snap: Snapshot{
		Queue: queue.Stats{Channels: []queue.ChannelStats{{ChannelID: "a\"b\\c\nd\x01"}}},
	}}
	srv, err := New(Options{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	body := srv.render(src.snap)
	if !strings.Contains(body, `channel_id="a\"b\\c\nd`+"\x01"+`"`) {
		t.Errorf("label not escaped per exposition format:\n%q", body)
	}
	if strings.Contains(body, `\x01`) {
		t.Errorf("unexpected \\x escape:\n%q", body)
	}
	if err := validateExposition(body); err != nil {
		t.Errorf("invalid exposition: %v", err)
	}
}

// validateExposition is a tiny strict check: one HELP and one TYPE per
// family, and samples of a family contiguous right after its TYPE line.
func validateExposition(body string) error {
	help := map[string]bool{}
	typ := map[string]bool{}
	cur := ""
	for i, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "):
			n := strings.Fields(line)[2]
			if help[n] {
				return fmt.Errorf("line %d: duplicate HELP for %s", i+1, n)
			}
			help[n] = true
		case strings.HasPrefix(line, "# TYPE "):
			n := strings.Fields(line)[2]
			if typ[n] {
				return fmt.Errorf("line %d: duplicate TYPE for %s", i+1, n)
			}
			typ[n] = true
			cur = n
		default:
			name := line
			if j := strings.IndexAny(line, "{ "); j >= 0 {
				name = line[:j]
			}
			if name != cur {
				return fmt.Errorf("line %d: sample %s outside its family (current %q)", i+1, name, cur)
			}
		}
	}
	return nil
}

func TestRenderMailNotify(t *testing.T) {
	tests := []struct {
		name string
		mail *MailNotifyStats
		want []string
		not  []string
	}{
		{name: "disabled", not: []string{"mail_notify"}},
		{name: "enabled", mail: &MailNotifyStats{Pending: 4, Sent: 7, Failed: 1, Retried: 3, Deferred: 2}, want: []string{
			"tikker_mail_notify_pending 4", "tikker_mail_notify_sent_total 7", "tikker_mail_notify_failed_total 1",
			"tikker_mail_notify_retried_total 3", "tikker_mail_notify_deferred_total 2",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &stubSource{snap: Snapshot{Started: 1, MailNotify: tt.mail}}
			srv, err := New(Options{Source: src, Namespace: "tikker"})
			if err != nil {
				t.Fatal(err)
			}
			body := srv.render(src.snap)
			if err := validateExposition(body); err != nil {
				t.Fatalf("invalid exposition: %v\n%s", err, body)
			}
			for _, w := range tt.want {
				if !strings.Contains(body, w) {
					t.Errorf("body missing %q", w)
				}
			}
			for _, w := range tt.not {
				if strings.Contains(body, w) {
					t.Errorf("body has %q", w)
				}
			}
		})
	}
}
