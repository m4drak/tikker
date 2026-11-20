// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// Limits of schemas ChannelCreate and ChannelUpdate.
const (
	maxPortLen            = 32
	maxChannelName        = 64
	minRatePerMin         = 1
	maxRatePerMin         = 600
	minMaxSegments        = 1
	maxMaxSegments        = 10
	defaultRatePerMin     = 6
	defaultMaxSegments    = 3
	reasonNotCheckedYet   = "not checked yet"
	conflictChannelActive = "Channel has messages in status \"queued\" or \"sending\"."
)

var portPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// effectiveState is the state a client sees: administrators' switches win
// over the last health check.
func effectiveState(c domain.Channel, deviceEnabled bool) domain.ChannelState {
	if !c.Enabled || !deviceEnabled {
		return domain.ChannelDisabled
	}
	return c.State
}

// devicesByID reads all devices.
func (a *API) devicesByID(ctx context.Context) (map[domain.ID]domain.Device, error) {
	devs, err := a.st.Devices().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	m := make(map[domain.ID]domain.Device, len(devs))
	for _, d := range devs {
		m[d.ID] = d
	}
	return m, nil
}

// overlay applies the live health, when a checker is configured.
func (a *API) overlay(c *domain.Channel) {
	if a.hv != nil {
		a.hv.ApplyTo(c)
	}
}

// channelView builds the DTO of c including its queue depth.
func (a *API) channelView(ctx context.Context, c domain.Channel, deviceEnabled bool) (channelDTO, error) {
	a.overlay(&c)
	depth, err := a.st.Messages().Count(ctx, store.MessageFilter{
		Direction: domain.DirectionOut, ChannelID: c.ID,
		Statuses: []domain.MessageStatus{domain.StatusQueued, domain.StatusSending},
	})
	if err != nil {
		return channelDTO{}, fmt.Errorf("count queue of channel: %w", err)
	}
	return toChannelDTO(c, deviceEnabled, depth), nil
}

// listChannels implements GET /channels.
func (a *API) listChannels(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	q := newQuery(r, "device_id", "state", "cursor", "limit")
	deviceID := q.id("device_id")
	var state domain.ChannelState
	if s := q.str("state"); s != "" {
		st, err := domain.ParseChannelState(s)
		if err != nil {
			q.fail("state", codeInvalidFormat, "Must be one of ok, degraded, down, disabled.")
		}
		state = st
	}
	after := q.cursor()
	limit := q.limit()
	if err := q.err(); err != nil {
		return err
	}
	ctx := r.Context()
	chs, err := a.st.Channels().List(ctx, store.ChannelFilter{DeviceID: deviceID})
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}
	devs, err := a.devicesByID(ctx)
	if err != nil {
		return err
	}
	matching := chs[:0:0]
	for _, c := range chs {
		a.overlay(&c)
		if state != "" && effectiveState(c, devs[c.DeviceID].Enabled) != state {
			continue
		}
		matching = append(matching, c)
	}
	page, next := pageOf(matching, func(c domain.Channel) domain.ID { return c.ID }, after, limit, false)
	items := make([]channelDTO, len(page))
	for i, c := range page {
		if items[i], err = a.channelView(ctx, c, devs[c.DeviceID].Enabled); err != nil {
			return err
		}
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}

// pathID reads the {id} path parameter of resources with opaque IDs.
func pathID(r *http.Request) (domain.ID, error) {
	id := r.PathValue("id")
	if id == "" || len(id) > maxIDLen {
		return "", malformed("Invalid ID.", fieldError{Field: "id", Code: codeInvalidFormat, Detail: "An ID has 1 to 64 characters."})
	}
	return domain.ID(id), nil
}

// getChannelAndDevice returns the channel and whether its device is enabled.
func (a *API) getChannelAndDevice(ctx context.Context, id domain.ID) (domain.Channel, bool, error) {
	c, err := a.st.Channels().Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Channel{}, false, notFound("Channel")
	}
	if err != nil {
		return domain.Channel{}, false, fmt.Errorf("get channel: %w", err)
	}
	d, err := a.st.Devices().Get(ctx, c.DeviceID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.Channel{}, false, fmt.Errorf("get device of channel: %w", err)
	}
	return c, err == nil && d.Enabled, nil
}

// getChannel implements GET /channels/{id}.
func (a *API) getChannel(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	c, devEnabled, err := a.getChannelAndDevice(r.Context(), id)
	if err != nil {
		return err
	}
	dto, err := a.channelView(r.Context(), c, devEnabled)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, dto)
	return nil
}

// channelCreate is schema ChannelCreate.
type channelCreate struct {
	DeviceID      *string  `json:"device_id"`
	Port          *string  `json:"port"`
	Name          *string  `json:"name"`
	Tags          []string `json:"tags"`
	RatePerMin    *int     `json:"rate_per_min"`
	MaxSegments   *int     `json:"max_segments"`
	Transliterate *bool    `json:"transliterate"`
	Enabled       *bool    `json:"enabled"`
}

// createChannel implements POST /channels.
func (a *API) createChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	var req channelCreate
	if err := decodeStrict(raw, &req); err != nil {
		return err
	}
	ctx := r.Context()
	var v validation
	var dev domain.Device
	switch {
	case req.DeviceID == nil || *req.DeviceID == "":
		v.add("/device_id", codeRequired, "A device ID is required.")
	case len(*req.DeviceID) > maxIDLen:
		v.add("/device_id", codeOutOfRange, "An ID has 1 to 64 characters.")
	default:
		d, err := a.st.Devices().Get(ctx, domain.ID(*req.DeviceID))
		switch {
		case errors.Is(err, domain.ErrNotFound):
			v.add("/device_id", codeUnknownDevice, "Device does not exist.")
		case err != nil:
			return fmt.Errorf("get device: %w", err)
		default:
			dev = d
		}
	}
	switch {
	case req.Port == nil || *req.Port == "":
		v.add("/port", codeRequired, "A port is required.")
	case len(*req.Port) > maxPortLen:
		v.add("/port", codeOutOfRange, "At most %d characters.", maxPortLen)
	case !portPattern.MatchString(*req.Port):
		v.add("/port", codeInvalidFormat, "Letters, digits, '.', '_' and '-' only.")
	}
	if req.Name != nil && runeLen(*req.Name) > maxChannelName {
		v.add("/name", codeOutOfRange, "At most %d characters.", maxChannelName)
	}
	validateTags(&v, "/tags", req.Tags)
	rate, segs := defaultRatePerMin, defaultMaxSegments
	if req.RatePerMin != nil {
		if rate = *req.RatePerMin; rate < minRatePerMin || rate > maxRatePerMin {
			v.add("/rate_per_min", codeOutOfRange, "Must be between %d and %d.", minRatePerMin, maxRatePerMin)
		}
	}
	if req.MaxSegments != nil {
		if segs = *req.MaxSegments; segs < minMaxSegments || segs > maxMaxSegments {
			v.add("/max_segments", codeOutOfRange, "Must be between %d and %d.", minMaxSegments, maxMaxSegments)
		}
	}
	if err := v.err(); err != nil {
		return err
	}

	var translit bool
	if req.Transliterate != nil {
		translit = *req.Transliterate
	} else {
		translit = a.defaultTransliterate(ctx, dev)
	}
	c := domain.Channel{
		ID: a.ids.NewID(), DeviceID: dev.ID, Port: *req.Port, Tags: append([]string{}, req.Tags...),
		RatePerMin: rate, MaxSegments: segs, Transliterate: translit,
		Enabled: req.Enabled == nil || *req.Enabled,
		State:   domain.ChannelDown, StateReason: reasonNotCheckedYet,
	}
	if req.Name != nil && *req.Name != "" {
		c.Name = *req.Name
	} else {
		c.Name = dev.Name + "/" + c.Port
		if runeLen(c.Name) > maxChannelName {
			c.Name = string([]rune(c.Name)[:maxChannelName])
		}
	}
	err = a.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Channels().Create(ctx, c); err != nil {
			return err
		}
		return a.audit(ctx, tx, actorOf(p), auditChannelCreate, "channel:"+string(c.ID),
			map[string]string{"device": string(c.DeviceID), "port": c.Port})
	})
	if errors.Is(err, domain.ErrConflict) {
		return conflict("A channel for this device and port already exists.")
	}
	if err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	a.reload(ctx)
	created, err := a.st.Channels().Get(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("get created channel: %w", err)
	}
	dto, err := a.channelView(ctx, created, dev.Enabled)
	if err != nil {
		return err
	}
	w.Header().Set("Location", Prefix+"/channels/"+dto.ID)
	writeJSON(w, http.StatusCreated, dto)
	return nil
}

// defaultTransliterate is the transliterate setting of a new channel whose
// request leaves it out (SPEC §4.6): on when the device's connector cannot
// send UCS-2, so that Polish text is sendable out of the box (RouterOS sends
// GSM-7 only), off when it can. When the capabilities cannot be determined
// (the stored password does not decrypt, the connector cannot be built) the
// GSM-7-safe choice, on, is made; an administrator can change it with PATCH.
func (a *API) defaultTransliterate(ctx context.Context, dev domain.Device) bool {
	cfg, err := a.creds.ConnectorConfig(dev)
	if err != nil {
		a.log.Warn("device credentials unusable; transliterate defaults to on",
			slog.String("device_id", string(dev.ID)), slog.Any("error", err))
		return true
	}
	// Building a connector only prepares a client; the device is not contacted.
	conn, err := a.conns.New(ctx, cfg)
	if err != nil {
		a.log.Warn("connector cannot be built; transliterate defaults to on",
			slog.String("device_id", string(dev.ID)), slog.Any("error", err))
		return true
	}
	defer func() { _ = conn.Close() }()
	return !conn.Capabilities().SupportsEncoding(domain.EncodingUCS2)
}

var channelPatchFields = []string{"name", "tags", "rate_per_min", "max_segments", "transliterate", "enabled"}

// patchValue decodes member key of patch into a *T, reporting null (the
// contract has no nullable members here) as a validation error.
func patchValue[T any](p mergePatch, key string, v *validation) (val T, set bool, err error) {
	if !p.has(key) {
		return val, false, nil
	}
	if p.isNull(key) {
		v.add("/"+key, codeInvalidFormat, "Must not be null.")
		return val, false, nil
	}
	if err := p.get(key, &val); err != nil {
		return val, false, err
	}
	return val, true, nil
}

// updateChannel implements PATCH /channels/{id} (JSON Merge Patch).
func (a *API) updateChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	patch, err := decodePatch(raw, channelPatchFields...)
	if err != nil {
		return err
	}
	var v validation
	if len(patch) == 0 {
		v.add("", codeRequired, "At least one property is required.")
		return v.err()
	}
	name, hasName, err := patchValue[string](patch, "name", &v)
	if err != nil {
		return err
	}
	tags, hasTags, err := patchValue[[]string](patch, "tags", &v)
	if err != nil {
		return err
	}
	rate, hasRate, err := patchValue[int](patch, "rate_per_min", &v)
	if err != nil {
		return err
	}
	segs, hasSegs, err := patchValue[int](patch, "max_segments", &v)
	if err != nil {
		return err
	}
	translit, hasTranslit, err := patchValue[bool](patch, "transliterate", &v)
	if err != nil {
		return err
	}
	enabled, hasEnabled, err := patchValue[bool](patch, "enabled", &v)
	if err != nil {
		return err
	}
	if hasName && (name == "" || runeLen(name) > maxChannelName) {
		v.add("/name", codeOutOfRange, "Must have 1 to %d characters.", maxChannelName)
	}
	if hasTags {
		validateTags(&v, "/tags", tags)
	}
	if hasRate && (rate < minRatePerMin || rate > maxRatePerMin) {
		v.add("/rate_per_min", codeOutOfRange, "Must be between %d and %d.", minRatePerMin, maxRatePerMin)
	}
	if hasSegs && (segs < minMaxSegments || segs > maxMaxSegments) {
		v.add("/max_segments", codeOutOfRange, "Must be between %d and %d.", minMaxSegments, maxMaxSegments)
	}
	if err := v.err(); err != nil {
		return err
	}

	ctx := r.Context()
	var updated domain.Channel
	var changed []string
	err = a.st.WithTx(ctx, func(tx store.Repos) error {
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
		if hasName {
			set("name", c.Name != name, func() { c.Name = name })
		}
		if hasTags {
			if tags == nil {
				tags = []string{}
			}
			set("tags", !slices.Equal(c.Tags, tags), func() { c.Tags = tags })
		}
		if hasRate {
			set("rate_per_min", c.RatePerMin != rate, func() { c.RatePerMin = rate })
		}
		if hasSegs {
			set("max_segments", c.MaxSegments != segs, func() { c.MaxSegments = segs })
		}
		if hasTranslit {
			set("transliterate", c.Transliterate != translit, func() { c.Transliterate = translit })
		}
		if hasEnabled {
			set("enabled", c.Enabled != enabled, func() { c.Enabled = enabled })
		}
		updated = c
		if len(changed) == 0 {
			return nil
		}
		if err := tx.Channels().Update(ctx, c); err != nil {
			return err
		}
		return a.audit(ctx, tx, actorOf(p), domain.AuditChannelUpdate, "channel:"+string(id),
			map[string]string{"fields": strings.Join(changed, ",")})
	})
	if errors.Is(err, domain.ErrNotFound) {
		return notFound("Channel")
	}
	if err != nil {
		return fmt.Errorf("update channel: %w", err)
	}
	if len(changed) > 0 {
		a.reload(ctx)
	}
	c, devEnabled, err := a.getChannelAndDevice(ctx, updated.ID)
	if err != nil {
		return err
	}
	dto, err := a.channelView(ctx, c, devEnabled)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, dto)
	return nil
}

// deleteChannel implements DELETE /channels/{id}.
func (a *API) deleteChannel(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = a.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Channels().Delete(ctx, id); err != nil {
			return err
		}
		return a.audit(ctx, tx, actorOf(p), auditChannelDelete, "channel:"+string(id), nil)
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return notFound("Channel")
	case errors.Is(err, domain.ErrConflict):
		n, cerr := a.st.Messages().Count(ctx, store.MessageFilter{
			Direction: domain.DirectionOut, ChannelID: id,
			Statuses: []domain.MessageStatus{domain.StatusQueued, domain.StatusSending},
		})
		if cerr != nil || n == 0 {
			return conflict(conflictChannelActive)
		}
		return conflict(fmt.Sprintf("Channel has %d messages in status \"queued\" or \"sending\".", n))
	case err != nil:
		return fmt.Errorf("delete channel: %w", err)
	}
	a.reload(ctx)
	w.WriteHeader(http.StatusNoContent)
	return nil
}
