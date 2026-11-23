// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// DeviceConfig is everything a [Factory] needs to build a connector for one
// device. It is built from a domain.Device with the password already
// decrypted, so it must never be logged verbatim: String, GoString and
// LogValue redact the password.
type DeviceConfig struct {
	DeviceID    domain.ID
	Kind        domain.ConnectorKind
	BaseURL     string
	Username    string
	Password    string // plaintext; redacted in String/GoString/LogValue
	TLSMode     domain.TLSMode
	CAPEM       string
	Fingerprint string
	// AgentToken is the plaintext bearer token of an agent device
	// (domain.ConnectorAgent), empty otherwise; redacted like Password.
	AgentToken string
	// Timeout bounds a single request; zero means the connector's default.
	Timeout time.Duration
}

// DeviceConfigFromDevice copies the non-secret fields of d into a
// DeviceConfig and sets password.
func DeviceConfigFromDevice(d domain.Device, password string) DeviceConfig {
	return DeviceConfig{
		DeviceID:    d.ID,
		Kind:        d.Connector,
		BaseURL:     d.BaseURL,
		Username:    d.Username,
		Password:    password,
		TLSMode:     d.TLSMode,
		CAPEM:       d.CAPEM,
		Fingerprint: d.Fingerprint,
	}
}

const redacted = "[REDACTED]"

func (c DeviceConfig) redacted() DeviceConfig {
	if c.Password != "" {
		c.Password = redacted
	}
	if c.AgentToken != "" {
		c.AgentToken = redacted
	}
	return c
}

// String implements fmt.Stringer without revealing the password.
func (c DeviceConfig) String() string {
	type plain DeviceConfig // drop methods to avoid recursion
	return fmt.Sprintf("%+v", plain(c.redacted()))
}

// GoString implements fmt.GoStringer without revealing the password.
func (c DeviceConfig) GoString() string { return "connector.DeviceConfig" + c.String() }

// LogValue implements slog.LogValuer without revealing the password or the
// agent token.
func (c DeviceConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("device_id", c.DeviceID.String()),
		slog.String("kind", c.Kind.String()),
		slog.String("base_url", c.BaseURL),
		slog.String("username", c.Username),
		slog.String("tls_mode", c.TLSMode.String()),
	)
}

// Factory builds a connector for one device. It should validate cfg and set
// up clients but not contact the device; connectivity problems surface on
// the first call.
type Factory func(ctx context.Context, cfg DeviceConfig) (Connector, error)

// ErrUnknownKind is returned by [Registry.New] for a kind with no factory.
var ErrUnknownKind = errors.New("unknown connector kind")

// Registry maps connector kinds to factories. It is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	factories map[domain.ConnectorKind]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[domain.ConnectorKind]Factory)}
}

// Register adds the factory for kind. Registering an invalid kind, a nil
// factory or the same kind twice is an error.
func (r *Registry) Register(kind domain.ConnectorKind, f Factory) error {
	if !kind.Valid() {
		return fmt.Errorf("register connector: %w: %q", ErrUnknownKind, kind)
	}
	if f == nil {
		return fmt.Errorf("register connector %q: nil factory", kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[kind]; dup {
		return fmt.Errorf("register connector %q: already registered", kind)
	}
	r.factories[kind] = f
	return nil
}

// New builds a connector for cfg using the factory registered for cfg.Kind.
// An empty kind defaults to [domain.ConnectorRouterOSREST].
func (r *Registry) New(ctx context.Context, cfg DeviceConfig) (Connector, error) {
	if cfg.Kind == "" {
		cfg.Kind = domain.ConnectorRouterOSREST
	}
	r.mu.RLock()
	f, ok := r.factories[cfg.Kind]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("new connector for device %s: %w: %q", cfg.DeviceID, ErrUnknownKind, cfg.Kind)
	}
	return f(ctx, cfg)
}

// Kinds returns the registered kinds, sorted.
func (r *Registry) Kinds() []domain.ConnectorKind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	kinds := make([]domain.ConnectorKind, 0, len(r.factories))
	for k := range r.factories {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	return kinds
}
