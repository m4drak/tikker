// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package admin holds the administration of devices and channels shared by
// the REST API and the web panel: validation, password encryption, the device
// test, the default transliteration of new channels, auditing and the reload
// of the channel set after a change. Both front ends map its errors to their
// own form (RFC 9457 problems, form messages).
//
// Users and tokens are administered through auth.Service.
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/store"
)

// Audit actions beyond the constants of the domain package.
const (
	AuditChannelCreate domain.AuditAction = "channel.create"
	AuditChannelDelete domain.AuditAction = "channel.delete"
)

// Field error codes (api/openapi.yaml).
const (
	CodeRequired          = "required"
	CodeInvalidFormat     = "invalid_format"
	CodeOutOfRange        = "out_of_range"
	CodeInsecureTransport = "insecure_transport_not_allowed"
	CodeUnknownDevice     = "unknown_device"
)

// DefaultProbeTimeout bounds [Service.TestDevice].
const DefaultProbeTimeout = 10 * time.Second

// Errors. Test them with errors.Is / errors.As.
var (
	// ErrDuplicate: a device name or a channel port is already used. It
	// wraps domain.ErrConflict.
	ErrDuplicate = fmt.Errorf("admin: already exists: %w", domain.ErrConflict)
	// ErrValidation matches every *ValidationError.
	ErrValidation = errors.New("admin: validation failed")
)

// ActiveMessagesError reports that a device or channel cannot be deleted
// while messages are queued or sending. Count is 0 when unknown. It matches
// domain.ErrConflict.
type ActiveMessagesError struct{ Count int }

func (e *ActiveMessagesError) Error() string {
	return "admin: queued or sending messages exist"
}

// Is makes errors.Is(err, domain.ErrConflict) true.
func (e *ActiveMessagesError) Is(target error) bool { return target == domain.ErrConflict }

// FieldError describes one invalid field; Field is a JSON pointer ("/name").
type FieldError struct {
	Field  string
	Code   string
	Detail string
}

// ValidationError lists invalid fields. It never contains secrets.
type ValidationError struct{ Errors []FieldError }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, f := range e.Errors {
		parts[i] = f.Field + ": " + f.Detail
	}
	return "admin: validation failed: " + strings.Join(parts, "; ")
}

// Is makes errors.Is(err, ErrValidation) true.
func (e *ValidationError) Is(target error) bool { return target == ErrValidation }

const maxReported = 100

type validation struct{ errs []FieldError }

func (v *validation) add(field, code, format string, args ...any) {
	if len(v.errs) >= maxReported {
		return
	}
	v.errs = append(v.errs, FieldError{Field: field, Code: code, Detail: fmt.Sprintf(format, args...)})
}

func (v *validation) err() error {
	if len(v.errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: v.errs}
}

// Credentials encrypts device passwords and builds connector configurations;
// *secrets.DeviceCredentials implements it.
type Credentials interface {
	Encrypt(id domain.ID, p secrets.Password) ([]byte, error)
	ConnectorConfig(d domain.Device) (connector.DeviceConfig, error)
}

// ConnectorFactory builds a connector for one device; *connector.Registry
// implements it.
type ConnectorFactory interface {
	New(ctx context.Context, cfg connector.DeviceConfig) (connector.Connector, error)
}

// ChannelReloader rebuilds what depends on the stored devices and channels;
// *api.ChannelSync implements it.
type ChannelReloader interface {
	Reload(ctx context.Context) error
}

// HealthView overlays the live health on a channel; *health.Checker
// implements it.
type HealthView interface {
	ApplyTo(ch *domain.Channel) bool
}

// ChannelChecker runs a health check on demand; *health.Checker implements
// it.
type ChannelChecker interface {
	CheckNow(ctx context.Context, id domain.ID) (health.ChannelStatus, error)
}

// Options are the settings of the service.
type Options struct {
	// AllowedHTTPHosts lists the hosts a device base URL may use with plain
	// http:// (IP addresses, CIDR prefixes or host names; SPEC §3.3).
	AllowedHTTPHosts []string
	// ProbeTimeout bounds the device test (default DefaultProbeTimeout).
	ProbeTimeout time.Duration
}

// Deps are the collaborators of [New]. Health and Checker are optional.
type Deps struct {
	Store       store.Store
	Credentials Credentials
	Connectors  ConnectorFactory
	Reloader    ChannelReloader
	Health      HealthView
	Checker     ChannelChecker
	Clock       clock.Clock
	IDs         domain.IDGenerator
	Logger      *slog.Logger
	Options     Options
}

// Service administers devices and channels.
type Service struct {
	st        store.Store
	creds     Credentials
	conns     ConnectorFactory
	reld      ChannelReloader
	hv        HealthView
	chk       ChannelChecker
	clk       clock.Clock
	ids       domain.IDGenerator
	log       *slog.Logger
	opts      Options
	allowHTTP hostAllowlist
}

// New validates deps.
func New(d Deps) (*Service, error) {
	if d.Store == nil || d.Credentials == nil || d.Connectors == nil || d.Reloader == nil {
		return nil, errors.New("admin: Store, Credentials, Connectors and Reloader are required")
	}
	s := &Service{st: d.Store, creds: d.Credentials, conns: d.Connectors, reld: d.Reloader,
		hv: d.Health, chk: d.Checker, clk: d.Clock, ids: d.IDs, log: d.Logger, opts: d.Options}
	if s.clk == nil {
		s.clk = clock.System()
	}
	if s.ids == nil {
		s.ids = domain.NewULIDGenerator(s.clk)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.log = s.log.With(slog.String("component", "admin"))
	if s.opts.ProbeTimeout <= 0 {
		s.opts.ProbeTimeout = DefaultProbeTimeout
	}
	var err error
	if s.allowHTTP, err = parseHostAllowlist(s.opts.AllowedHTTPHosts); err != nil {
		return nil, fmt.Errorf("AllowedHTTPHosts: %w", err)
	}
	return s, nil
}

// hostAllowlist is the set of hosts a device may use with plain http://.
type hostAllowlist struct {
	prefixes []netip.Prefix
	names    map[string]bool
}

func parseHostAllowlist(entries []string) (hostAllowlist, error) {
	l := hostAllowlist{names: map[string]bool{}}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		switch {
		case e == "":
			continue
		case strings.Contains(e, "/"):
			p, err := netip.ParsePrefix(e)
			if err != nil {
				return l, fmt.Errorf("invalid prefix %q: %w", e, err)
			}
			l.prefixes = append(l.prefixes, p.Masked())
		default:
			if ip, err := netip.ParseAddr(e); err == nil {
				l.prefixes = append(l.prefixes, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
				continue
			}
			l.names[strings.ToLower(e)] = true
		}
	}
	return l, nil
}

func (l hostAllowlist) allows(host string) bool {
	if l.names[strings.ToLower(host)] {
		return true
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, p := range l.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// audit appends an audit entry inside the transaction tx.
func (s *Service) audit(ctx context.Context, tx store.Repos, actor domain.Actor, action domain.AuditAction, target string, meta map[string]string) error {
	return tx.Audit().Append(ctx, domain.AuditLog{
		ID: s.ids.NewID(), Actor: actor, Action: action, Target: target, Meta: meta, At: s.clk.Now(),
	})
}

// reload rebuilds the channel set after a committed change; a failure is only
// logged (the change has succeeded).
func (s *Service) reload(ctx context.Context) {
	if err := s.reld.Reload(context.WithoutCancel(ctx)); err != nil {
		s.log.Error("reloading channels after a change failed", slog.Any("error", err))
	}
}
