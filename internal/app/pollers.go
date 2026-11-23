// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/api"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/inbox"
	"github.com/m4drak/tikker/internal/store"
)

// pollerBase holds the settings shared by every inbox poller.
type pollerBase struct {
	store    store.Store
	clock    clock.Clock
	ids      domain.IDGenerator
	logger   *slog.Logger
	interval time.Duration
	reporter inbox.ChannelStateReporter
	events   inbox.MessagePublisher // optional
}

// pollerSupervisor runs one inbox poller per enabled channel with a connector.
// It is an api.SpecSink: after every reload it starts pollers of new channels,
// stops pollers of removed or disabled ones and restarts a poller whose
// device, port or connector changed. Channel state does not matter: a poller
// of a channel that is down backs off on its own and its successes help the
// health checker.
type pollerSupervisor struct {
	ctx  context.Context
	base pollerBase
	log  *slog.Logger

	mu      sync.Mutex
	closed  bool
	running map[domain.ID]*pollerRun
	wg      sync.WaitGroup
}

// pollerKey is what a running poller was built from.
type pollerKey struct {
	deviceID domain.ID
	port     string
	conn     connector.Connector
}

type pollerRun struct {
	key    pollerKey
	cancel context.CancelFunc
	done   chan struct{}
}

var _ api.SpecSink = (*pollerSupervisor)(nil)

func newPollerSupervisor(ctx context.Context, base pollerBase) *pollerSupervisor {
	log := base.logger
	if log == nil {
		log = slog.Default()
	}
	return &pollerSupervisor{
		ctx: ctx, base: base, log: log.With(slog.String("component", "inbox-supervisor")),
		running: map[domain.ID]*pollerRun{},
	}
}

// SetSpecs implements api.SpecSink. It never blocks on a running poll: a
// replaced poller is cancelled and its successor waits for it to exit before
// it polls, so one channel never runs two cycles at once.
func (s *pollerSupervisor) SetSpecs(specs []api.ChannelSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	want := make(map[domain.ID]api.ChannelSpec, len(specs))
	for _, sp := range specs {
		if sp.Channel.Enabled && sp.Connector != nil {
			want[sp.Channel.ID] = sp
		}
	}

	for id, r := range s.running {
		sp, ok := want[id]
		if ok && r.key == keyOf(sp) {
			continue
		}
		r.cancel()
		if !ok {
			delete(s.running, id)
			s.log.Info("inbox poller stopped", slog.String("channel_id", string(id)))
		}
	}

	for id, sp := range want {
		prev := s.running[id]
		if prev != nil && prev.key == keyOf(sp) {
			continue
		}
		var prevDone <-chan struct{}
		if prev != nil {
			prevDone = prev.done
		}
		s.start(sp, prevDone)
	}
}

func keyOf(sp api.ChannelSpec) pollerKey {
	return pollerKey{deviceID: sp.Channel.DeviceID, port: sp.Channel.Port, conn: sp.Connector}
}

// start launches the poller of sp; it waits for prevDone first. Callers
// hold s.mu.
func (s *pollerSupervisor) start(sp api.ChannelSpec, prevDone <-chan struct{}) {
	p, err := inbox.New(inbox.Config{
		Channel: sp.Channel, Conn: sp.Connector, Store: s.base.store,
		Clock: s.base.clock, IDs: s.base.ids, Logger: s.base.logger,
		Interval: s.base.interval, Reporter: s.base.reporter, Events: s.base.events,
	})
	if err != nil {
		s.log.Error("inbox poller cannot be created", slog.String("channel_id", string(sp.Channel.ID)), slog.Any("error", err))
		delete(s.running, sp.Channel.ID)
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	r := &pollerRun{key: keyOf(sp), cancel: cancel, done: make(chan struct{})}
	s.running[sp.Channel.ID] = r
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(r.done)
		defer cancel()
		if prevDone != nil {
			select {
			case <-prevDone:
			case <-ctx.Done():
				return
			}
		}
		_ = p.Run(ctx) // returns ctx.Err()
	}()
	s.log.Info("inbox poller started", slog.String("channel_id", string(sp.Channel.ID)), slog.String("port", sp.Channel.Port))
}

// Stop cancels every poller and waits for them. Later SetSpecs calls are
// ignored.
func (s *pollerSupervisor) Stop() {
	s.mu.Lock()
	s.closed = true
	for _, r := range s.running {
		r.cancel()
	}
	s.running = map[domain.ID]*pollerRun{}
	s.mu.Unlock()
	s.wg.Wait()
}

// count returns the number of running pollers (tests).
func (s *pollerSupervisor) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}
