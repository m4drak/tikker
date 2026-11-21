// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

const conflictChannelActive = "Channel has messages in status \"queued\" or \"sending\"."

// effectiveState is the state a client sees: administrators' switches win
// over the last health check.
func effectiveState(c domain.Channel, deviceEnabled bool) domain.ChannelState {
	return admin.EffectiveState(c, deviceEnabled)
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
func (a *API) overlay(c *domain.Channel) { a.admin.Overlay(c) }

// channelView builds the DTO of c including its queue depth.
func (a *API) channelView(ctx context.Context, c domain.Channel, deviceEnabled bool) (channelDTO, error) {
	v, err := a.admin.View(ctx, c, deviceEnabled)
	if err != nil {
		return channelDTO{}, fmt.Errorf("channel view: %w", err)
	}
	return toChannelDTO(v.Channel, deviceEnabled, v.QueueDepth), nil
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
	c, en, err := a.admin.GetChannel(ctx, id)
	if e := adminProblem(err, "Channel", ""); e != nil {
		return domain.Channel{}, false, e
	}
	return c, en, err
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
	c, err := a.admin.CreateChannel(ctx, actorOf(p), admin.ChannelCreate(req))
	if e := adminProblem(err, "Device", "A channel for this device and port already exists."); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	_, devEnabled, err := a.getChannelAndDevice(ctx, c.ID)
	if err != nil {
		return err
	}
	dto, err := a.channelView(ctx, c, devEnabled)
	if err != nil {
		return err
	}
	w.Header().Set("Location", Prefix+"/channels/"+dto.ID)
	writeJSON(w, http.StatusCreated, dto)
	return nil
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
	var ch admin.ChannelChanges
	if x, ok, err := patchValue[string](patch, "name", &v); err != nil {
		return err
	} else if ok {
		ch.Name = &x
	}
	if x, ok, err := patchValue[[]string](patch, "tags", &v); err != nil {
		return err
	} else if ok {
		ch.Tags = &x
	}
	if x, ok, err := patchValue[int](patch, "rate_per_min", &v); err != nil {
		return err
	} else if ok {
		ch.RatePerMin = &x
	}
	if x, ok, err := patchValue[int](patch, "max_segments", &v); err != nil {
		return err
	} else if ok {
		ch.MaxSegments = &x
	}
	if x, ok, err := patchValue[bool](patch, "transliterate", &v); err != nil {
		return err
	} else if ok {
		ch.Transliterate = &x
	}
	if x, ok, err := patchValue[bool](patch, "enabled", &v); err != nil {
		return err
	} else if ok {
		ch.Enabled = &x
	}
	if !v.ok() {
		for _, f := range admin.ValidateChannelChanges(ch) {
			v.add(f.Field, f.Code, "%s", f.Detail)
		}
		return v.err()
	}
	ctx := r.Context()
	updated, err := a.admin.UpdateChannel(ctx, actorOf(p), id, ch)
	if e := adminProblem(err, "Channel", ""); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("update channel: %w", err)
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
	err = a.admin.DeleteChannel(r.Context(), actorOf(p), id)
	var am *admin.ActiveMessagesError
	if errors.As(err, &am) {
		if am.Count == 0 {
			return conflict(conflictChannelActive)
		}
		return conflict(fmt.Sprintf("Channel has %d messages in status \"queued\" or \"sending\".", am.Count))
	}
	if e := adminProblem(err, "Channel", conflictChannelActive); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
