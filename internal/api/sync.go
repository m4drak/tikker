// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/queue"
	"github.com/m4drak/tikker/internal/store"
)

// ChannelSpec is one channel with the connector of its device, as the queue,
// the health checker and the inbox pollers need it. Channel.Enabled already
// accounts for the device (channel and device both enabled); Connector is nil
// when the device is disabled or its connector cannot be built.
type ChannelSpec struct {
	Channel   domain.Channel
	Connector connector.Connector
}

// SpecSink receives the complete set of channel specs after every reload.
type SpecSink interface {
	SetSpecs(specs []ChannelSpec)
}

// SpecSinkFunc adapts a function to [SpecSink].
type SpecSinkFunc func(specs []ChannelSpec)

// SetSpecs implements [SpecSink].
func (f SpecSinkFunc) SetSpecs(specs []ChannelSpec) { f(specs) }

// DispatcherSink feeds specs to the queue dispatcher.
func DispatcherSink(d *queue.Dispatcher) SpecSink {
	return SpecSinkFunc(func(specs []ChannelSpec) {
		out := make([]queue.ChannelSpec, len(specs))
		for i, s := range specs {
			out[i] = queue.ChannelSpec(s)
		}
		d.SetChannels(out)
	})
}

// HealthSink feeds specs to the health checker.
func HealthSink(c *health.Checker) SpecSink {
	return SpecSinkFunc(func(specs []ChannelSpec) {
		out := make([]health.ChannelSpec, len(specs))
		for i, s := range specs {
			out[i] = health.ChannelSpec(s)
		}
		c.SetChannels(out)
	})
}

// SyncOptions are the dependencies of [NewChannelSync].
type SyncOptions struct {
	Store       store.Store
	Credentials Credentials
	Connectors  ConnectorFactory
	// Health, if set, overlays the live state on the stored channels before
	// they are handed to the sinks (health.Checker.ApplyTo).
	Health HealthView
	// Sinks receive the specs after every Reload, in order (typically the
	// dispatcher, the health checker and the inbox supervisor).
	Sinks  []SpecSink
	Logger *slog.Logger
}

// ChannelSync keeps one connector per enabled device and tells the sinks about
// the channels. It implements [ChannelReloader] and [CapabilityProvider].
//
// A device's connector is rebuilt only when its stored settings change.
// Replaced connectors that the sinks may still hold are not closed at once,
// because the dispatcher may still run a send on them: pass
// [ChannelSync.Release] as queue.Options OnConnectorReleased so a retired
// connector is closed when nobody uses it any more, and call
// [ChannelSync.Close] at shutdown. A retired connector the sinks do not hold
// (a device without channels, or whose channels were removed and released
// earlier) is closed during Reload, since no Release will ever come for it.
type ChannelSync struct {
	opts SyncOptions
	log  *slog.Logger

	mu      sync.Mutex // serialises Reload
	devices map[domain.ID]*deviceConn

	capMu sync.RWMutex
	caps  map[domain.ID]connector.Capabilities // by channel ID

	retiredMu sync.Mutex
	retired   map[connector.Connector]bool
	// held are connectors handed to the sinks and not released since;
	// inSpecs are those of the latest specs. Guarded by retiredMu.
	held    map[connector.Connector]bool
	inSpecs map[connector.Connector]bool
}

type deviceConn struct {
	key  string // digest of the stored settings the connector was built from
	conn connector.Connector
}

var (
	_ ChannelReloader    = (*ChannelSync)(nil)
	_ CapabilityProvider = (*ChannelSync)(nil)
)

// NewChannelSync validates opts. Call Reload once at start-up.
func NewChannelSync(opts SyncOptions) (*ChannelSync, error) {
	if opts.Store == nil || opts.Credentials == nil || opts.Connectors == nil {
		return nil, errors.New("api: ChannelSync needs Store, Credentials and Connectors")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &ChannelSync{
		opts: opts, log: log.With(slog.String("component", "channel-sync")),
		devices: map[domain.ID]*deviceConn{}, caps: map[domain.ID]connector.Capabilities{},
		retired: map[connector.Connector]bool{}, held: map[connector.Connector]bool{},
		inSpecs: map[connector.Connector]bool{},
	}, nil
}

// deviceKey digests everything a connector is built from.
func deviceKey(d domain.Device) string {
	h := sha256.New()
	for _, s := range []string{
		string(d.Connector), d.BaseURL, d.Username, string(d.TLSMode), d.CAPEM, d.Fingerprint, string(d.PasswordEnc),
		string(d.AgentTokenEnc),
	} {
		fmt.Fprintf(h, "%d:%s|", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Reload reads devices and channels from the store, builds connectors for new
// or changed enabled devices and passes the resulting specs to the sinks.
func (s *ChannelSync) Reload(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	devs, err := s.opts.Store.Devices().List(ctx)
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}
	chans, err := s.opts.Store.Channels().List(ctx, store.ChannelFilter{})
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}

	next := make(map[domain.ID]*deviceConn, len(devs))
	byID := make(map[domain.ID]domain.Device, len(devs))
	for _, d := range devs {
		byID[d.ID] = d
		if !d.Enabled {
			continue
		}
		key := deviceKey(d)
		if cur := s.devices[d.ID]; cur != nil && cur.key == key {
			next[d.ID] = cur
			continue
		}
		cfg, err := s.opts.Credentials.ConnectorConfig(d)
		if err != nil {
			s.log.Error("device credentials unusable; its channels stay idle", slog.String("device_id", string(d.ID)), slog.Any("error", err))
			continue
		}
		conn, err := s.opts.Connectors.New(ctx, cfg)
		if err != nil {
			s.log.Error("connector cannot be built; its channels stay idle", slog.String("device_id", string(d.ID)), slog.Any("error", err))
			continue
		}
		next[d.ID] = &deviceConn{key: key, conn: conn}
	}

	// Connectors that are no longer current are retired, not closed: a send
	// may still be running on them.
	var retire []connector.Connector
	for id, cur := range s.devices {
		if n := next[id]; n == nil || n.conn != cur.conn {
			retire = append(retire, cur.conn)
		}
	}
	s.devices = next

	specs := make([]ChannelSpec, 0, len(chans))
	caps := make(map[domain.ID]connector.Capabilities, len(chans))
	for _, c := range chans {
		d := byID[c.DeviceID]
		c.Enabled = c.Enabled && d.Enabled
		if s.opts.Health != nil {
			s.opts.Health.ApplyTo(&c)
		}
		spec := ChannelSpec{Channel: c}
		if dc := next[c.DeviceID]; dc != nil {
			spec.Connector = dc.conn
			caps[c.ID] = dc.conn.Capabilities()
		}
		specs = append(specs, spec)
	}
	s.capMu.Lock()
	s.caps = caps
	s.capMu.Unlock()

	// Mark what the sinks are about to hold before handing the specs over,
	// so a Release racing with SetSpecs cannot unmark it.
	inSpecs := make(map[connector.Connector]bool, len(next))
	for _, sp := range specs {
		if sp.Connector != nil {
			inSpecs[sp.Connector] = true
		}
	}
	var closeNow []connector.Connector
	s.retiredMu.Lock()
	s.inSpecs = inSpecs
	for c := range inSpecs {
		s.held[c] = true
	}
	for _, c := range retire {
		if s.held[c] {
			s.retired[c] = true // closed by Release
		} else {
			closeNow = append(closeNow, c)
		}
	}
	s.retiredMu.Unlock()
	for _, c := range closeNow {
		if err := c.Close(); err != nil {
			s.log.Warn("closing a retired connector failed", slog.Any("error", err))
		}
	}

	for _, sink := range s.opts.Sinks {
		sink.SetSpecs(specs)
	}
	return nil
}

// ChannelCapabilities implements [CapabilityProvider]. Channels of disabled
// devices, or whose connector could not be built, are unknown.
func (s *ChannelSync) ChannelCapabilities(id domain.ID) (connector.Capabilities, bool) {
	s.capMu.RLock()
	defer s.capMu.RUnlock()
	c, ok := s.caps[id]
	return c, ok
}

// Connector returns the current connector of a device, for components that
// run their own loops per channel (inbox pollers).
func (s *ChannelSync) Connector(deviceID domain.ID) (connector.Connector, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dc := s.devices[deviceID]
	if dc == nil {
		return nil, false
	}
	return dc.conn, true
}

// Release closes conn if it was replaced by a newer connector. It matches
// queue.Options.OnConnectorReleased. Connectors that are still current are
// left open; one that is no longer in the specs is remembered as released,
// so that its later retirement closes it at once.
func (s *ChannelSync) Release(conn connector.Connector) {
	s.retiredMu.Lock()
	ok := s.retired[conn]
	delete(s.retired, conn)
	if ok || !s.inSpecs[conn] {
		delete(s.held, conn)
	}
	s.retiredMu.Unlock()
	if !ok {
		return
	}
	if err := conn.Close(); err != nil {
		s.log.Warn("closing a retired connector failed", slog.Any("error", err))
	}
}

// Close closes every connector, current and retired. Call it after the
// dispatcher, the health checker and the pollers have stopped.
func (s *ChannelSync) Close() error {
	s.mu.Lock()
	cur := s.devices
	s.devices = map[domain.ID]*deviceConn{}
	s.mu.Unlock()
	s.retiredMu.Lock()
	old := s.retired
	s.retired = map[connector.Connector]bool{}
	s.held, s.inSpecs = map[connector.Connector]bool{}, map[connector.Connector]bool{}
	s.retiredMu.Unlock()

	var errs []error
	for _, dc := range cur {
		errs = append(errs, dc.conn.Close())
	}
	for c := range old {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}
