// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/store"
)

// Limits of schemas DeviceCreate and DeviceUpdate.
const (
	maxDeviceName     = 64
	maxBaseURL        = 255
	maxUsername       = 64
	maxPassword       = 256
	maxCAPEM          = 16384
	minScrubbedSecret = 3
	conflictDeviceDup = "A device with this name already exists."
	conflictDeviceMsg = "A channel of the device has messages in status \"queued\" or \"sending\"."
)

var fingerprintPattern = regexp.MustCompile(`^([0-9A-Fa-f]{2}:?){31}[0-9A-Fa-f]{2}$`)

// hostAllowlist is the set of hosts a device may use with plain http://.
type hostAllowlist struct {
	prefixes []netip.Prefix
	names    map[string]bool
}

// parseHostAllowlist reads IP addresses, CIDR prefixes and host names.
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

// allows reports whether host (as in url.URL.Hostname) is on the list.
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

// checkName validates a device name.
func checkName(v *validation, name string) {
	if n := runeLen(name); n < 1 || n > maxDeviceName {
		v.add("/name", codeOutOfRange, "Must have 1 to %d characters.", maxDeviceName)
	}
}

// checkBaseURL validates base_url: http(s) with a host, http only for allowed
// hosts (SPEC §3.3).
func (a *API) checkBaseURL(v *validation, raw string) {
	u, err := url.Parse(raw)
	switch {
	case len(raw) > maxBaseURL:
		v.add("/base_url", codeOutOfRange, "At most %d characters.", maxBaseURL)
	case err != nil || u.Host == "" || u.Hostname() == "":
		v.add("/base_url", codeInvalidFormat, "Must be an absolute http:// or https:// URL.")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		v.add("/base_url", codeInvalidFormat, "Must not contain credentials, a query or a fragment.")
	case strings.EqualFold(u.Scheme, "https"):
	case strings.EqualFold(u.Scheme, "http"):
		if !a.allowHTTP.allows(u.Hostname()) {
			v.add("/base_url", codeInsecureTransport, "Plain http:// is only allowed for the configured local addresses; use https://.")
		}
	default:
		v.add("/base_url", codeInvalidFormat, "Must be an absolute http:// or https:// URL.")
	}
}

func checkUsername(v *validation, s string) {
	if n := runeLen(s); n < 1 || n > maxUsername {
		v.add("/username", codeOutOfRange, "Must have 1 to %d characters.", maxUsername)
	}
}

func checkPassword(v *validation, p secrets.Password) {
	if n := runeLen(p.Reveal()); n < 1 || n > maxPassword {
		v.add("/password", codeOutOfRange, "Must have 1 to %d characters.", maxPassword)
	}
}

// checkCAPEM validates a PEM bundle of at least one certificate.
func checkCAPEM(v *validation, s string) {
	if len(s) > maxCAPEM {
		v.add("/ca_pem", codeOutOfRange, "At most %d characters.", maxCAPEM)
		return
	}
	rest, certs := []byte(s), 0
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
			v.add("/ca_pem", codeInvalidFormat, "Contains a certificate that cannot be parsed.")
			return
		}
		certs++
	}
	if certs == 0 {
		v.add("/ca_pem", codeInvalidFormat, "Must contain at least one PEM encoded certificate.")
	}
}

// normalizeFingerprint returns the "AB:CD:..." form, or "" and false.
func normalizeFingerprint(s string) (string, bool) {
	if !fingerprintPattern.MatchString(s) {
		return "", false
	}
	return formatFingerprint(s), true
}

// checkTLSConsistency requires the material the TLS mode needs (on the merged
// values, so PATCH is checked like POST).
func checkTLSConsistency(v *validation, mode domain.TLSMode, caPEM, fingerprint string) {
	switch mode {
	case domain.TLSCustomCA:
		if caPEM == "" {
			v.add("/ca_pem", codeRequired, "Required when tls_mode is \"ca\".")
		}
	case domain.TLSFingerprint:
		if fingerprint == "" {
			v.add("/fingerprint", codeRequired, "Required when tls_mode is \"fingerprint\".")
		}
	}
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
	var v validation
	d := domain.Device{
		Connector: domain.ConnectorRouterOSREST, TLSMode: domain.TLSVerify, Enabled: req.Enabled == nil || *req.Enabled,
	}
	if req.Name == nil {
		v.add("/name", codeRequired, "A name is required.")
	} else {
		d.Name = *req.Name
		checkName(&v, d.Name)
	}
	if req.BaseURL == nil {
		v.add("/base_url", codeRequired, "A base URL is required.")
	} else {
		d.BaseURL = *req.BaseURL
		a.checkBaseURL(&v, d.BaseURL)
	}
	if req.Username == nil {
		v.add("/username", codeRequired, "A username is required.")
	} else {
		d.Username = *req.Username
		checkUsername(&v, d.Username)
	}
	if req.Password == nil {
		v.add("/password", codeRequired, "A password is required.")
	} else {
		checkPassword(&v, *req.Password)
	}
	if req.TLSMode != nil {
		m, err := domain.ParseTLSMode(*req.TLSMode)
		if err != nil {
			v.add("/tls_mode", codeInvalidFormat, "Must be one of verify, ca, fingerprint, insecure.")
		}
		d.TLSMode = m
	}
	if req.CAPEM != nil {
		d.CAPEM = *req.CAPEM
		if d.CAPEM != "" {
			checkCAPEM(&v, d.CAPEM)
		}
	}
	if req.Fingerprint != nil && *req.Fingerprint != "" {
		fp, ok := normalizeFingerprint(*req.Fingerprint)
		if !ok {
			v.add("/fingerprint", codeInvalidFormat, "Must be a SHA-256 fingerprint: 32 hex bytes, optionally separated by colons.")
		}
		d.Fingerprint = fp
	}
	if d.TLSMode.Valid() {
		checkTLSConsistency(&v, d.TLSMode, d.CAPEM, d.Fingerprint)
	}
	if err := v.err(); err != nil {
		return err
	}

	ctx := r.Context()
	d.ID, d.CreatedAt = a.ids.NewID(), a.clk.Now()
	if d.PasswordEnc, err = a.creds.Encrypt(d.ID, *req.Password); err != nil {
		return fmt.Errorf("encrypt device password: %w", err)
	}
	err = a.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Devices().Create(ctx, d); err != nil {
			return err
		}
		return a.audit(ctx, tx, actorOf(p), domain.AuditDeviceCreate, "device:"+string(d.ID),
			map[string]string{"name": d.Name, "base_url": d.BaseURL})
	})
	if errors.Is(err, domain.ErrConflict) {
		return conflict(conflictDeviceDup)
	}
	if err != nil {
		return fmt.Errorf("create device: %w", err)
	}
	a.reload(ctx)
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
	name, hasName, err := patchValue[string](patch, "name", &v)
	if err != nil {
		return err
	}
	baseURL, hasURL, err := patchValue[string](patch, "base_url", &v)
	if err != nil {
		return err
	}
	username, hasUser, err := patchValue[string](patch, "username", &v)
	if err != nil {
		return err
	}
	password, hasPass, err := patchValue[secrets.Password](patch, "password", &v)
	if err != nil {
		return err
	}
	tlsMode, hasMode, err := patchValue[string](patch, "tls_mode", &v)
	if err != nil {
		return err
	}
	enabled, hasEnabled, err := patchValue[bool](patch, "enabled", &v)
	if err != nil {
		return err
	}
	// ca_pem and fingerprint accept null: it removes the value.
	var caPEM, fingerprint string
	hasCA, hasFP := patch.has("ca_pem"), patch.has("fingerprint")
	if hasCA && !patch.isNull("ca_pem") {
		if err := patch.get("ca_pem", &caPEM); err != nil {
			return err
		}
	}
	if hasFP && !patch.isNull("fingerprint") {
		if err := patch.get("fingerprint", &fingerprint); err != nil {
			return err
		}
	}
	if hasName {
		checkName(&v, name)
	}
	if hasURL {
		a.checkBaseURL(&v, baseURL)
	}
	if hasUser {
		checkUsername(&v, username)
	}
	if hasPass {
		checkPassword(&v, password)
	}
	var mode domain.TLSMode
	if hasMode {
		m, err := domain.ParseTLSMode(tlsMode)
		if err != nil {
			v.add("/tls_mode", codeInvalidFormat, "Must be one of verify, ca, fingerprint, insecure.")
		}
		mode = m
	}
	if hasCA && caPEM != "" {
		checkCAPEM(&v, caPEM)
	}
	if hasFP && fingerprint != "" {
		fp, ok := normalizeFingerprint(fingerprint)
		if !ok {
			v.add("/fingerprint", codeInvalidFormat, "Must be a SHA-256 fingerprint: 32 hex bytes, optionally separated by colons.")
		}
		fingerprint = fp
	}
	if !v.ok() {
		return v.err()
	}

	ctx := r.Context()
	var updated domain.Device
	err = a.st.WithTx(ctx, func(tx store.Repos) error {
		d, err := tx.Devices().Get(ctx, id)
		if err != nil {
			return err
		}
		meta := map[string]string{}
		if hasName {
			d.Name = name
			meta["name"] = name
		}
		if hasURL {
			d.BaseURL = baseURL
			meta["base_url"] = baseURL
		}
		if hasUser {
			d.Username = username
			meta["username"] = "changed"
		}
		if hasMode {
			d.TLSMode = mode
			meta["tls_mode"] = string(mode)
		}
		if hasCA {
			d.CAPEM = caPEM
			meta["ca_pem"] = "changed"
		}
		if hasFP {
			d.Fingerprint = fingerprint
			meta["fingerprint"] = "changed"
		}
		if hasEnabled {
			d.Enabled = enabled
			meta["enabled"] = fmt.Sprint(enabled)
		}
		// The merged result must be consistent (contract: 422).
		var cv validation
		checkTLSConsistency(&cv, d.TLSMode, d.CAPEM, d.Fingerprint)
		if err := cv.err(); err != nil {
			return err
		}
		d.PasswordEnc = nil // nil keeps the stored password
		if hasPass {
			if d.PasswordEnc, err = a.creds.Encrypt(d.ID, password); err != nil {
				return fmt.Errorf("encrypt device password: %w", err)
			}
			meta["password"] = "changed"
		}
		if err := tx.Devices().Update(ctx, d); err != nil {
			return err
		}
		updated = d
		return a.audit(ctx, tx, actorOf(p), domain.AuditDeviceUpdate, "device:"+string(id), meta)
	})
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, domain.ErrNotFound):
		return notFound("Device")
	case errors.Is(err, domain.ErrConflict):
		return conflict(conflictDeviceDup)
	case err != nil:
		return fmt.Errorf("update device: %w", err)
	}
	a.reload(ctx)
	writeJSON(w, http.StatusOK, toDeviceDTO(updated))
	return nil
}

// deleteDevice implements DELETE /devices/{id}.
func (a *API) deleteDevice(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = a.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Devices().Delete(ctx, id); err != nil {
			return err
		}
		return a.audit(ctx, tx, actorOf(p), domain.AuditDeviceDelete, "device:"+string(id), nil)
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return notFound("Device")
	case errors.Is(err, domain.ErrConflict):
		return conflict(conflictDeviceMsg)
	case err != nil:
		return fmt.Errorf("delete device: %w", err)
	}
	a.reload(ctx)
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

// probeCodes are the error codes DeviceTestResult may carry.
var probeCodes = map[domain.ErrorCode]bool{
	domain.CodeUnreachable: true, domain.CodeTimeout: true, domain.CodeTLSError: true, domain.CodeAuthFailed: true,
	domain.CodeInsufficientPolicy: true, domain.CodeUnsupportedVersion: true, domain.CodeUnexpectedResponse: true,
}

const maxProbeDetail = 300

// testDevice implements POST /devices/{id}/test. A failed test is a 200 with
// ok=false; the device and its channels are not changed.
func (a *API) testDevice(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	d, err := a.deviceByID(ctx, id)
	if err != nil {
		return err
	}
	chans, err := a.st.Channels().List(ctx, store.ChannelFilter{DeviceID: d.ID})
	if err != nil {
		return fmt.Errorf("list channels of device: %w", err)
	}

	res := testResultDTO{CheckedAt: formatTime(a.clk.Now()), LTEPorts: []testPortDTO{}}
	fail := func(code domain.ErrorCode, detail string) error {
		res.Error = &testErrorDTO{Code: string(code), Detail: truncateRunes(detail, maxProbeDetail)}
		writeJSON(w, http.StatusOK, res)
		return nil
	}

	cfg, err := a.creds.ConnectorConfig(d)
	if err != nil {
		a.log.Warn("device password cannot be decrypted", slog.String("device_id", string(d.ID)), slog.Any("error", err))
		return fail(domain.CodeUnexpectedResponse, "The stored password cannot be decrypted; was the secret key changed?")
	}
	cfg.Timeout = a.opts.ProbeTimeout
	conn, err := a.conns.New(ctx, cfg)
	if err != nil {
		return fail(domain.CodeUnexpectedResponse, scrub("Connector cannot be created: "+err.Error(), cfg.Password))
	}
	defer func() { _ = conn.Close() }()

	pctx, cancel := context.WithTimeout(ctx, a.opts.ProbeTimeout)
	defer cancel()
	start := a.clk.Now()
	probe, perr := conn.Probe(pctx)
	latency := a.clk.Now().Sub(start).Milliseconds()

	res.OK = perr == nil
	res.RouterOSVersion = nullableString(probe.RouterOSVersion)
	res.BoardName = nullableString(probe.BoardName)
	if probe.TLS != nil {
		res.TLS = &testTLSDTO{
			Mode: string(probe.TLS.Mode), Verified: probe.TLS.Verified,
			PeerFingerprint: nullableString(formatFingerprint(probe.TLS.PeerFingerprint)),
		}
	}
	for _, pi := range probe.Ports {
		pd := testPortDTO{
			Port: pi.Port, Running: pi.Health.Running, Operator: nullableString(pi.Health.Operator),
			Registration: nullableString(pi.Health.Registration), Signal: toSignalDTO(pi.Health.Signal),
		}
		for _, c := range chans {
			if c.Port == pi.Port {
				cid := string(c.ID)
				pd.ChannelID = &cid
				break
			}
		}
		res.LTEPorts = append(res.LTEPorts, pd)
	}
	if perr == nil {
		l := latency
		res.LatencyMS = &l
		writeJSON(w, http.StatusOK, res)
		return nil
	}
	code := probeErrorCode(pctx, perr)
	if code != domain.CodeUnreachable && code != domain.CodeTimeout {
		l := latency
		res.LatencyMS = &l
	}
	return fail(code, scrub(perr.Error(), cfg.Password))
}

// probeErrorCode classifies a failed probe for DeviceTestResult.error.code.
func probeErrorCode(ctx context.Context, err error) domain.ErrorCode {
	if c := domain.CodeOf(err); probeCodes[c] {
		return c
	}
	switch {
	case domain.KindOf(err) == domain.KindAuth:
		return domain.CodeAuthFailed
	case ctx.Err() != nil:
		return domain.CodeTimeout
	}
	return domain.CodeUnexpectedResponse
}

// scrub removes secret from s; connectors do not put passwords into errors,
// this is the last line of defence.
func scrub(s, secret string) string {
	if len(secret) >= minScrubbedSecret {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}
