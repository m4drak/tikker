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
	"github.com/m4drak/tikker/internal/secrets"
)

const (
	conflictDeviceDup = "A device with this name already exists."
	conflictDeviceMsg = "A channel of the device has messages in status \"queued\" or \"sending\"."
)

// adminProblem maps the errors of the admin service: validation failures to
// the 422 problem, the sentinels to 404/409. It returns nil for other errors.
func adminProblem(err error, what, conflictDetail string) *apiError {
	var ve *admin.ValidationError
	if errors.As(err, &ve) {
		var v validation
		for _, f := range ve.Errors {
			v.add(f.Field, f.Code, "%s", f.Detail)
		}
		var ae *apiError
		_ = errors.As(v.err(), &ae)
		return ae
	}
	return mapStoreError(err, what, conflictDetail)
}

// deviceCreate is schema DeviceCreate.
type deviceCreate struct {
	Name        *string           `json:"name"`
	BaseURL     *string           `json:"base_url"`
	Username    *string           `json:"username"`
	Password    *secrets.Password `json:"password"`
	TLSMode     *string           `json:"tls_mode"`
	CAPEM       *string           `json:"ca_pem"`
	Fingerprint *string           `json:"fingerprint"`
	Enabled     *bool             `json:"enabled"`
}

// listDevices implements GET /devices.
func (a *API) listDevices(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	q := newQuery(r, "cursor", "limit")
	after, limit := q.cursor(), q.limit()
	if err := q.err(); err != nil {
		return err
	}
	devs, err := a.st.Devices().List(r.Context())
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}
	page, next := pageOf(devs, func(d domain.Device) domain.ID { return d.ID }, after, limit, false)
	items := make([]deviceDTO, len(page))
	for i, d := range page {
		items[i] = toDeviceDTO(d)
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}

func (a *API) deviceByID(ctx context.Context, id domain.ID) (domain.Device, error) {
	d, err := a.st.Devices().Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Device{}, notFound("Device")
	}
	if err != nil {
		return domain.Device{}, fmt.Errorf("get device: %w", err)
	}
	return d, nil
}

// getDevice implements GET /devices/{id}.
func (a *API) getDevice(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	d, err := a.deviceByID(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toDeviceDTO(d))
	return nil
}

// createDevice implements POST /devices.
func (a *API) createDevice(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	var req deviceCreate
	if err := decodeStrict(raw, &req); err != nil {
		return err
	}
	d, err := a.admin.CreateDevice(r.Context(), actorOf(p), admin.DeviceCreate(req))
	if e := adminProblem(err, "Device", conflictDeviceDup); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("create device: %w", err)
	}
	w.Header().Set("Location", Prefix+"/devices/"+string(d.ID))
	writeJSON(w, http.StatusCreated, toDeviceDTO(d))
	return nil
}

var devicePatchFields = []string{"name", "base_url", "username", "password", "tls_mode", "ca_pem", "fingerprint", "enabled"}

// updateDevice implements PATCH /devices/{id} (JSON Merge Patch).
func (a *API) updateDevice(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	patch, err := decodePatch(raw, devicePatchFields...)
	if err != nil {
		return err
	}
	var v validation
	if len(patch) == 0 {
		v.add("", codeRequired, "At least one property is required.")
		return v.err()
	}
	var ch admin.DeviceChanges
	if name, ok, err := patchValue[string](patch, "name", &v); err != nil {
		return err
	} else if ok {
		ch.Name = &name
	}
	if u, ok, err := patchValue[string](patch, "base_url", &v); err != nil {
		return err
	} else if ok {
		ch.BaseURL = &u
	}
	if u, ok, err := patchValue[string](patch, "username", &v); err != nil {
		return err
	} else if ok {
		ch.Username = &u
	}
	if pw, ok, err := patchValue[secrets.Password](patch, "password", &v); err != nil {
		return err
	} else if ok {
		ch.Password = &pw
	}
	if m, ok, err := patchValue[string](patch, "tls_mode", &v); err != nil {
		return err
	} else if ok {
		ch.TLSMode = &m
	}
	if en, ok, err := patchValue[bool](patch, "enabled", &v); err != nil {
		return err
	} else if ok {
		ch.Enabled = &en
	}
	// ca_pem and fingerprint accept null: it removes the value.
	if patch.has("ca_pem") {
		var s string
		if !patch.isNull("ca_pem") {
			if err := patch.get("ca_pem", &s); err != nil {
				return err
			}
		}
		ch.CAPEM = &s
	}
	if patch.has("fingerprint") {
		var s string
		if !patch.isNull("fingerprint") {
			if err := patch.get("fingerprint", &s); err != nil {
				return err
			}
		}
		ch.Fingerprint = &s
	}
	if !v.ok() { // report the problems of the values together with the nulls
		for _, f := range a.admin.ValidateDeviceChanges(ch) {
			v.add(f.Field, f.Code, "%s", f.Detail)
		}
		return v.err()
	}
	d, err := a.admin.UpdateDevice(r.Context(), actorOf(p), id, ch)
	if e := adminProblem(err, "Device", conflictDeviceDup); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("update device: %w", err)
	}
	writeJSON(w, http.StatusOK, toDeviceDTO(d))
	return nil
}

// deleteDevice implements DELETE /devices/{id}.
func (a *API) deleteDevice(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	err = a.admin.DeleteDevice(r.Context(), actorOf(p), id)
	if e := adminProblem(err, "Device", conflictDeviceMsg); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("delete device: %w", err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------------------------------------------------------------------------
// POST /devices/{id}/test

type testTLSDTO struct {
	Mode            string  `json:"mode"`
	Verified        bool    `json:"verified"`
	PeerFingerprint *string `json:"peer_fingerprint"`
}

type testPortDTO struct {
	Port         string     `json:"port"`
	Running      bool       `json:"running"`
	Operator     *string    `json:"operator"`
	Registration *string    `json:"registration"`
	Signal       *signalDTO `json:"signal"`
	ChannelID    *string    `json:"channel_id"`
}

type testErrorDTO struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// testResultDTO is schema DeviceTestResult.
type testResultDTO struct {
	OK              bool          `json:"ok"`
	CheckedAt       string        `json:"checked_at"`
	LatencyMS       *int64        `json:"latency_ms"`
	RouterOSVersion *string       `json:"routeros_version"`
	BoardName       *string       `json:"board_name"`
	TLS             *testTLSDTO   `json:"tls"`
	LTEPorts        []testPortDTO `json:"lte_ports"`
	Error           *testErrorDTO `json:"error"`
}

// testDevice implements POST /devices/{id}/test. A failed test is a 200 with
// ok=false; the device and its channels are not changed.
func (a *API) testDevice(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	tr, err := a.admin.TestDevice(r.Context(), id)
	if e := adminProblem(err, "Device", ""); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("test device: %w", err)
	}
	res := testResultDTO{
		OK: tr.OK, CheckedAt: formatTime(tr.CheckedAt), LTEPorts: []testPortDTO{},
		RouterOSVersion: nullableString(tr.RouterOSVersion), BoardName: nullableString(tr.BoardName),
	}
	if tr.Latency != nil {
		l := tr.Latency.Milliseconds()
		res.LatencyMS = &l
	}
	if tr.TLS != nil {
		res.TLS = &testTLSDTO{Mode: tr.TLS.Mode, Verified: tr.TLS.Verified, PeerFingerprint: nullableString(tr.TLS.PeerFingerprint)}
	}
	for _, pi := range tr.Ports {
		pd := testPortDTO{
			Port: pi.Port, Running: pi.Running, Operator: nullableString(pi.Operator),
			Registration: nullableString(pi.Registration), Signal: toSignalDTO(pi.Signal),
		}
		if pi.ChannelID != "" {
			cid := string(pi.ChannelID)
			pd.ChannelID = &cid
		}
		res.LTEPorts = append(res.LTEPorts, pd)
	}
	if tr.Error != nil {
		res.Error = &testErrorDTO{Code: string(tr.Error.Code), Detail: tr.Error.Detail}
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}
