// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/api"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

// stubConn is a connector with an always empty inbox that counts fetches.
// Only the methods a poller calls are implemented.
type stubConn struct {
	connector.Connector
	fetches atomic.Int64
}

func (c *stubConn) FetchInbox(context.Context, connector.ChannelRef) ([]connector.InboundSMS, error) {
	c.fetches.Add(1)
	return nil, nil
}

func (c *stubConn) Capabilities() connector.Capabilities { return connector.Capabilities{} }

func (c *stubConn) DeleteInbox(context.Context, connector.ChannelRef, []string) error { return nil }

func TestPollerSupervisor(t *testing.T) {
	st, err := sqlite.Open(context.Background(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	sup := newPollerSupervisor(context.Background(), pollerBase{
		store: st, clock: clock.System(), logger: slog.New(slog.DiscardHandler), interval: time.Hour,
	})
	defer sup.Stop()

	connA, connB := &stubConn{}, &stubConn{}
	spec := func(id domain.ID, port string, enabled bool, conn connector.Connector) api.ChannelSpec {
		return api.ChannelSpec{Channel: domain.Channel{ID: id, DeviceID: "dev", Port: port, Enabled: enabled}, Connector: conn}
	}
	running := func() *pollerRun {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return sup.running["ch1"]
	}

	tests := []struct {
		name      string
		specs     []api.ChannelSpec
		wantCount int
		wantConn  *stubConn // whose inbox is polled next; nil: none
		restarted bool      // the poller of ch1 is a new one
	}{
		{name: "new channel starts a poller", specs: []api.ChannelSpec{spec("ch1", "lte1", true, connA)}, wantCount: 1, wantConn: connA, restarted: true},
		{name: "unchanged spec keeps it", specs: []api.ChannelSpec{spec("ch1", "lte1", true, connA)}, wantCount: 1},
		{name: "disabled or connector-less channels have none", specs: []api.ChannelSpec{
			spec("ch1", "lte1", true, connA), spec("ch2", "lte2", false, connA), spec("ch3", "lte3", true, nil),
		}, wantCount: 1},
		{name: "new connector restarts it", specs: []api.ChannelSpec{spec("ch1", "lte1", true, connB)}, wantCount: 1, wantConn: connB, restarted: true},
		{name: "new port restarts it", specs: []api.ChannelSpec{spec("ch1", "lte2", true, connB)}, wantCount: 1, wantConn: connB, restarted: true},
		{name: "removed channel stops it", specs: nil, wantCount: 0},
	}
	var prev *pollerRun
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var before int64
			if tt.wantConn != nil {
				before = tt.wantConn.fetches.Load()
			}
			sup.SetSpecs(tt.specs)
			if got := sup.count(); got != tt.wantCount {
				t.Fatalf("count = %d, want %d", got, tt.wantCount)
			}
			cur := running()
			if tt.restarted && (cur == nil || cur == prev) {
				t.Error("poller was not restarted")
			}
			if !tt.restarted && tt.wantCount > 0 && cur != prev {
				t.Error("poller was restarted")
			}
			if prev != nil && cur != prev {
				waitFor(t, "previous poller exit", func() bool {
					select {
					case <-prev.done:
						return true
					default:
						return false
					}
				})
			}
			if tt.wantConn != nil {
				waitFor(t, "a poll", func() bool { return tt.wantConn.fetches.Load() > before })
			}
			prev = cur
		})
	}

	sup.Stop()
	sup.SetSpecs([]api.ChannelSpec{spec("ch1", "lte1", true, connA)})
	if got := sup.count(); got != 0 {
		t.Errorf("count after Stop = %d", got)
	}
}
