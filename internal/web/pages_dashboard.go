// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/queue"
	"github.com/m4drak/tikker/internal/store"
)

// recentErrors is the number of failed messages the dashboard lists.
const recentErrors = 5

// dashboardData is the data of the dashboard page and of its htmx fragment.
type dashboardData struct {
	Channels []channelRow
	// Queued and Sending are the outbound messages waiting and in flight
	// (from the database, so they survive restarts).
	Queued  int
	Sending int
	// Counters are the dispatcher's totals since it started.
	Counters queue.Counters
	Errors   []errorRow
}

// channelRow is one channel of the dashboard.
type channelRow struct {
	ID         domain.ID
	Name       string
	Device     string
	Port       string
	Tags       []string
	Enabled    bool
	State      domain.ChannelState
	StateKey   string
	Reason     string
	Operator   string
	Technology string
	Signal     string
	Checked    time.Time
	Sent       int64
	Failed     int64
	Sending    bool
}

// errorRow is a recently failed message.
type errorRow struct {
	ID      domain.ID
	At      time.Time
	Peer    string
	Channel string
	Error   string
}

func (s *Server) registerDashboard() {
	s.HandleFunc("GET /{$}", domain.RoleViewer, s.dashboard)
	s.HandleFunc("GET /dashboard/status", domain.RoleViewer, s.dashboardStatus)
}

// dashboard renders the whole page.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadDashboard(r.Context())
	if err != nil {
		s.log.Error("web: dashboard failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	s.Render(w, r, http.StatusOK, "dashboard", data)
}

// dashboardStatus renders the fragment htmx polls every 10 seconds.
func (s *Server) dashboardStatus(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadDashboard(r.Context())
	if err != nil {
		s.log.Error("web: dashboard failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	s.RenderBlock(w, r, http.StatusOK, "dashboard", "dashboard_status", data)
}

// loadDashboard gathers what the dashboard shows: stored channels overlaid
// with the live health and the dispatcher's per-channel counters, the queue
// length and the latest failed messages.
func (s *Server) loadDashboard(ctx context.Context) (dashboardData, error) {
	var d dashboardData
	chs, err := s.st.Channels().List(ctx, store.ChannelFilter{})
	if err != nil {
		return d, fmt.Errorf("list channels: %w", err)
	}
	devs, err := s.st.Devices().List(ctx)
	if err != nil {
		return d, fmt.Errorf("list devices: %w", err)
	}
	devNames := make(map[domain.ID]string, len(devs))
	for _, dev := range devs {
		devNames[dev.ID] = dev.Name
	}
	live := map[domain.ID]int{}
	snap := s.health.Snapshot()
	for i, st := range snap {
		live[st.ChannelID] = i
	}
	qs := s.queue.Stats()
	d.Counters = qs.Totals
	perChannel := make(map[domain.ID]queue.ChannelStats, len(qs.Channels))
	for _, c := range qs.Channels {
		perChannel[c.ChannelID] = c
	}

	chNames := make(map[domain.ID]string, len(chs))
	for _, c := range chs {
		chNames[c.ID] = channelName(c)
		row := channelRow{
			ID: c.ID, Name: channelName(c), Device: devNames[c.DeviceID], Port: c.Port, Tags: c.Tags,
			Enabled: c.Enabled, State: c.State, Reason: c.StateReason, Operator: c.Operator,
			Signal: signalText(c.Signal), Checked: c.LastHealthAt,
		}
		if i, ok := live[c.ID]; ok {
			st := snap[i]
			row.State, row.Reason, row.Operator, row.Technology = st.State, st.Reason, st.Operator, st.Technology
			row.Signal, row.Checked = signalText(st.Signal), st.LastCheckAt
			if row.Reason == "" {
				row.Reason = st.LastError
			}
		}
		if !c.Enabled {
			row.State = domain.ChannelDisabled
		}
		row.StateKey = stateKey(row.State)
		if cs, ok := perChannel[c.ID]; ok {
			row.Sent, row.Failed, row.Sending = cs.Sent, cs.Failed, cs.Sending
		}
		d.Channels = append(d.Channels, row)
	}

	out := domain.DirectionOut
	if d.Queued, err = s.st.Messages().Count(ctx, store.MessageFilter{Direction: out, Statuses: []domain.MessageStatus{domain.StatusQueued}}); err != nil {
		return d, fmt.Errorf("count queued: %w", err)
	}
	if d.Sending, err = s.st.Messages().Count(ctx, store.MessageFilter{Direction: out, Statuses: []domain.MessageStatus{domain.StatusSending}}); err != nil {
		return d, fmt.Errorf("count sending: %w", err)
	}
	failed, err := s.st.Messages().List(ctx,
		store.MessageFilter{Direction: out, Statuses: []domain.MessageStatus{domain.StatusFailed}},
		store.PageRequest{Limit: recentErrors})
	if err != nil {
		return d, fmt.Errorf("list failed messages: %w", err)
	}
	for _, m := range failed.Items {
		d.Errors = append(d.Errors, errorRow{ID: m.ID, At: m.CreatedAt, Peer: m.Peer, Channel: chNames[m.ChannelID], Error: m.LastError})
	}
	return d, nil
}

// channelName is the name shown for a channel: its own, else its port.
func channelName(c domain.Channel) string {
	if c.Name != "" {
		return c.Name
	}
	return c.Port
}
