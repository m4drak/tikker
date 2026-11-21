// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/store"
)

// Limits and defaults of a channel.
const (
	MaxPortLen         = 32
	MaxChannelName     = 64
	MinRatePerMin      = 1
	MaxRatePerMin      = 600
	MinMaxSegments     = 1
	MaxMaxSegments     = 10
	DefaultRatePerMin  = 6
	DefaultMaxSegments = 3
	maxIDLen           = 64

	reasonNotCheckedYet = "not checked yet"
)

var portPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// EffectiveState is the state a client sees: the administrators' switches
// win over the last health check.
func EffectiveState(c domain.Channel, deviceEnabled bool) domain.ChannelState {
	if !c.Enabled || !deviceEnabled {
		return domain.ChannelDisabled
	}
	return c.State
}

// ChannelView is a channel with its live health and queue depth.
type ChannelView struct {
	Channel       domain.Channel // health overlaid when a checker is configured
	DeviceEnabled bool
	QueueDepth    int
}

// State is the effective state of the channel.
func (v ChannelView) State() domain.ChannelState { return EffectiveState(v.Channel, v.DeviceEnabled) }

// View builds the view of c.
func (s *Service) View(ctx context.Context, c domain.Channel, deviceEnabled bool) (ChannelView, error) {
	if s.hv != nil {
		s.hv.ApplyTo(&c)
	}
	depth, err := s.st.Messages().Count(ctx, store.MessageFilter{
		Direction: domain.DirectionOut, ChannelID: c.ID,
		Statuses: []domain.MessageStatus{domain.StatusQueued, domain.StatusSending},
	})
	if err != nil {
		return ChannelView{}, wrap("count queue of channel", err)
	}
	return ChannelView{Channel: c, DeviceEnabled: deviceEnabled, QueueDepth: depth}, nil
}

// Overlay applies the live health to c when a checker is configured.
func (s *Service) Overlay(c *domain.Channel) {
	if s.hv != nil {
		s.hv.ApplyTo(c)
	}
}

// GetChannel returns a channel and whether its device is enabled; an unknown
// ID yields domain.ErrNotFound.
func (s *Service) GetChannel(ctx context.Context, id domain.ID) (domain.Channel, bool, error) {
	c, err := s.st.Channels().Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Channel{}, false, fmt.Errorf("channel: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.Channel{}, false, wrap("get channel", err)
	}
	d, err := s.st.Devices().Get(ctx, c.DeviceID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.Channel{}, false, wrap("get device of channel", err)
	}
	return c, err == nil && d.Enabled, nil
}

func wrap(what string, err error) error { return fmt.Errorf("%s: %w", what, err) }

func toFieldErrors(v *validation, field string, tags []string) {
	for _, e := range sending.ValidateTags(field, tags) {
		v.add(e.Field, e.Code, "%s", e.Detail)
	}
}

// ChannelCreate is the input of [Service.CreateChannel]; nil means "not
// given" (defaults apply).
type ChannelCreate struct {
	DeviceID      *string
	Port          *string
	Name          *string
	Tags          []string
	RatePerMin    *int
	MaxSegments   *int
	Transliterate *bool
	Enabled       *bool
}

// CreateChannel validates and stores a channel, audits it and reloads the
// channel set. Problems yield *ValidationError, a second channel on the same
// port [ErrDuplicate]. Transliterate defaults to what the connector needs
// (see defaultTransliterate).
func (s *Service) CreateChannel(ctx context.Context, actor domain.Actor, req ChannelCreate) (domain.Channel, error) {
	var v validation
	var dev domain.Device
	switch {
	case req.DeviceID == nil || *req.DeviceID == "":
		v.add("/device_id", CodeRequired, "A device ID is required.")
	case len(*req.DeviceID) > maxIDLen:
		v.add("/device_id", CodeOutOfRange, "An ID has 1 to 64 characters.")
	default:
		d, err := s.st.Devices().Get(ctx, domain.ID(*req.DeviceID))
		switch {
		case errors.Is(err, domain.ErrNotFound):
			v.add("/device_id", CodeUnknownDevice, "Device does not exist.")
		case err != nil:
			return domain.Channel{}, wrap("get device", err)
		default:
			dev = d
		}
	}
	switch {
	case req.Port == nil || *req.Port == "":
		v.add("/port", CodeRequired, "A port is required.")
	case len(*req.Port) > MaxPortLen:
		v.add("/port", CodeOutOfRange, "At most %d characters.", MaxPortLen)
	case !portPattern.MatchString(*req.Port):
		v.add("/port", CodeInvalidFormat, "Letters, digits, '.', '_' and '-' only.")
	}
	if req.Name != nil && runeLen(*req.Name) > MaxChannelName {
		v.add("/name", CodeOutOfRange, "At most %d characters.", MaxChannelName)
	}
	toFieldErrors(&v, "/tags", req.Tags)
	rate, segs := DefaultRatePerMin, DefaultMaxSegments
	if req.RatePerMin != nil {
		if rate = *req.RatePerMin; rate < MinRatePerMin || rate > MaxRatePerMin {
			v.add("/rate_per_min", CodeOutOfRange, "Must be between %d and %d.", MinRatePerMin, MaxRatePerMin)
		}
	}
	if req.MaxSegments != nil {
		if segs = *req.MaxSegments; segs < MinMaxSegments || segs > MaxMaxSegments {
			v.add("/max_segments", CodeOutOfRange, "Must be between %d and %d.", MinMaxSegments, MaxMaxSegments)
		}
	}
	if err := v.err(); err != nil {
		return domain.Channel{}, err
	}

	var translit bool
	if req.Transliterate != nil {
		translit = *req.Transliterate
	} else {
		translit = s.DefaultTransliterate(ctx, dev)
	}
	c := domain.Channel{
		ID: s.ids.NewID(), DeviceID: dev.ID, Port: *req.Port, Tags: append([]string{}, req.Tags...),
		RatePerMin: rate, MaxSegments: segs, Transliterate: translit,
		Enabled: req.Enabled == nil || *req.Enabled,
		State:   domain.ChannelDown, StateReason: reasonNotCheckedYet,
	}
	if req.Name != nil && *req.Name != "" {
		c.Name = *req.Name
	} else {
		c.Name = dev.Name + "/" + c.Port
		if runeLen(c.Name) > MaxChannelName {
			c.Name = string([]rune(c.Name)[:MaxChannelName])
		}
	}
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Channels().Create(ctx, c); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, AuditChannelCreate, "channel:"+string(c.ID),
			map[string]string{"device": string(c.DeviceID), "port": c.Port})
	})
	if errors.Is(err, domain.ErrConflict) {
		return domain.Channel{}, ErrDuplicate
	}
	if err != nil {
		return domain.Channel{}, wrap("create channel", err)
	}
	s.reload(ctx)
	created, err := s.st.Channels().Get(ctx, c.ID)
	if err != nil {
		return domain.Channel{}, wrap("get created channel", err)
	}
	return created, nil
}

// DefaultTransliterate is the transliterate setting of a new channel of dev
// (SPEC §4.6): on when the device's connector cannot send UCS-2, so that
// Polish text is sendable out of the box (RouterOS sends GSM-7 only), off
// when it can. When the capabilities cannot be determined (the stored
// password does not decrypt, the connector cannot be built) the GSM-7-safe
// choice, on, is made.
func (s *Service) DefaultTransliterate(ctx context.Context, dev domain.Device) bool {
	cfg, err := s.creds.ConnectorConfig(dev)
	if err != nil {
		s.log.Warn("device credentials unusable; transliterate defaults to on",
			slog.String("device_id", string(dev.ID)), slog.Any("error", err))
		return true
	}
	// Building a connector only prepares a client; the device is not contacted.
	conn, err := s.conns.New(ctx, cfg)
	if err != nil {
		s.log.Warn("connector cannot be built; transliterate defaults to on",
			slog.String("device_id", string(dev.ID)), slog.Any("error", err))
		return true
	}
	defer func() { _ = conn.Close() }()
	return !conn.Capabilities().SupportsEncoding(domain.EncodingUCS2)
}

// ChannelChanges is the input of [Service.UpdateChannel]; nil fields are left
// unchanged. Tags non-nil replaces the tags.
type ChannelChanges struct {
	Name          *string
	Tags          *[]string
	RatePerMin    *int
	MaxSegments   *int
	Transliterate *bool
	Enabled       *bool
}

// ValidateChannelChanges checks ch alone; UpdateChannel runs it.
func ValidateChannelChanges(ch ChannelChanges) []FieldError {
	var v validation
	checkChannelChanges(&v, ch)
	return v.errs
}

func checkChannelChanges(v *validation, ch ChannelChanges) {
	if ch.Name != nil && (*ch.Name == "" || runeLen(*ch.Name) > MaxChannelName) {
		v.add("/name", CodeOutOfRange, "Must have 1 to %d characters.", MaxChannelName)
	}
	if ch.Tags != nil {
		toFieldErrors(v, "/tags", *ch.Tags)
	}
	if ch.RatePerMin != nil && (*ch.RatePerMin < MinRatePerMin || *ch.RatePerMin > MaxRatePerMin) {
		v.add("/rate_per_min", CodeOutOfRange, "Must be between %d and %d.", MinRatePerMin, MaxRatePerMin)
	}
	if ch.MaxSegments != nil && (*ch.MaxSegments < MinMaxSegments || *ch.MaxSegments > MaxMaxSegments) {
		v.add("/max_segments", CodeOutOfRange, "Must be between %d and %d.", MinMaxSegments, MaxMaxSegments)
	}
}

// UpdateChannel applies ch; only real changes are audited and reloaded.
// Problems yield *ValidationError, an unknown ID domain.ErrNotFound.
func (s *Service) UpdateChannel(ctx context.Context, actor domain.Actor, id domain.ID, ch ChannelChanges) (domain.Channel, error) {
	var v validation
	checkChannelChanges(&v, ch)
	if err := v.err(); err != nil {
		return domain.Channel{}, err
	}
	var updated domain.Channel
	var changed []string
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		c, err := tx.Channels().Get(ctx, id)
		if err != nil {
			return err
		}
		changed = changed[:0]
		set := func(field string, differs bool, apply func()) {
			if differs {
				apply()
				changed = append(changed, field)
			}
		}
		if ch.Name != nil {
			set("name", c.Name != *ch.Name, func() { c.Name = *ch.Name })
		}
		if ch.Tags != nil {
			tags := *ch.Tags
			if tags == nil {
				tags = []string{}
			}
			set("tags", !slices.Equal(c.Tags, tags), func() { c.Tags = tags })
		}
		if ch.RatePerMin != nil {
			set("rate_per_min", c.RatePerMin != *ch.RatePerMin, func() { c.RatePerMin = *ch.RatePerMin })
		}
		if ch.MaxSegments != nil {
			set("max_segments", c.MaxSegments != *ch.MaxSegments, func() { c.MaxSegments = *ch.MaxSegments })
		}
		if ch.Transliterate != nil {
			set("transliterate", c.Transliterate != *ch.Transliterate, func() { c.Transliterate = *ch.Transliterate })
		}
		if ch.Enabled != nil {
			set("enabled", c.Enabled != *ch.Enabled, func() { c.Enabled = *ch.Enabled })
		}
		updated = c
		if len(changed) == 0 {
			return nil
		}
		if err := tx.Channels().Update(ctx, c); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, domain.AuditChannelUpdate, "channel:"+string(id),
			map[string]string{"fields": strings.Join(changed, ",")})
	})
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Channel{}, fmt.Errorf("channel: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.Channel{}, wrap("update channel", err)
	}
	if len(changed) > 0 {
		s.reload(ctx)
	}
	return updated, nil
}

// DeleteChannel deletes a channel. Queued or sending messages yield
// *[ActiveMessagesError] with their number; an unknown ID
// domain.ErrNotFound.
func (s *Service) DeleteChannel(ctx context.Context, actor domain.Actor, id domain.ID) error {
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Channels().Delete(ctx, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, AuditChannelDelete, "channel:"+string(id), nil)
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("channel: %w", domain.ErrNotFound)
	case errors.Is(err, domain.ErrConflict):
		n, cerr := s.st.Messages().Count(ctx, store.MessageFilter{
			Direction: domain.DirectionOut, ChannelID: id,
			Statuses: []domain.MessageStatus{domain.StatusQueued, domain.StatusSending},
		})
		if cerr != nil {
			n = 0
		}
		return &ActiveMessagesError{Count: n}
	case err != nil:
		return wrap("delete channel", err)
	}
	s.reload(ctx)
	return nil
}

// ErrNoChecker is returned by [Service.CheckChannel] without a health checker.
var ErrNoChecker = errors.New("admin: no health checker configured")

// CheckChannel runs a health check of the channel now and returns its status.
func (s *Service) CheckChannel(ctx context.Context, id domain.ID) (health.ChannelStatus, error) {
	if s.chk == nil {
		return health.ChannelStatus{}, ErrNoChecker
	}
	if _, _, err := s.GetChannel(ctx, id); err != nil {
		return health.ChannelStatus{}, err
	}
	st, err := s.chk.CheckNow(ctx, id)
	if err != nil {
		return st, wrap("check channel", err)
	}
	return st, nil
}
