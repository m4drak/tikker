// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package admin

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/store"
)

// Limits of a device.
const (
	MaxDeviceName = 64
	MaxBaseURL    = 255
	MaxUsername   = 64
	MaxPassword   = 256
	MaxCAPEM      = 16384
)

var fingerprintPattern = regexp.MustCompile(`^([0-9A-Fa-f]{2}:?){31}[0-9A-Fa-f]{2}$`)

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// FormatFingerprint returns a hex fingerprint in the "AB:CD:..." form.
func FormatFingerprint(s string) string {
	s = strings.ToUpper(strings.ReplaceAll(s, ":", ""))
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i := 0; i+1 < len(s); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(s[i : i+2])
	}
	return b.String()
}

// NormalizeFingerprint returns the "AB:CD:..." form of a SHA-256
// fingerprint, or "" and false.
func NormalizeFingerprint(s string) (string, bool) {
	if !fingerprintPattern.MatchString(s) {
		return "", false
	}
	return FormatFingerprint(s), true
}

func checkName(v *validation, name string) {
	if n := runeLen(name); n < 1 || n > MaxDeviceName {
		v.add("/name", CodeOutOfRange, "Must have 1 to %d characters.", MaxDeviceName)
	}
}

func (s *Service) checkBaseURL(v *validation, raw string) {
	u, err := url.Parse(raw)
	switch {
	case len(raw) > MaxBaseURL:
		v.add("/base_url", CodeOutOfRange, "At most %d characters.", MaxBaseURL)
	case err != nil || u.Host == "" || u.Hostname() == "":
		v.add("/base_url", CodeInvalidFormat, "Must be an absolute http:// or https:// URL.")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		v.add("/base_url", CodeInvalidFormat, "Must not contain credentials, a query or a fragment.")
	case strings.EqualFold(u.Scheme, "https"):
	case strings.EqualFold(u.Scheme, "http"):
		if !s.allowHTTP.allows(u.Hostname()) {
			v.add("/base_url", CodeInsecureTransport, "Plain http:// is only allowed for the configured local addresses; use https://.")
		}
	default:
		v.add("/base_url", CodeInvalidFormat, "Must be an absolute http:// or https:// URL.")
	}
}

func checkUsername(v *validation, s string) {
	if n := runeLen(s); n < 1 || n > MaxUsername {
		v.add("/username", CodeOutOfRange, "Must have 1 to %d characters.", MaxUsername)
	}
}

func checkPassword(v *validation, p secrets.Password) {
	if n := runeLen(p.Reveal()); n < 1 || n > MaxPassword {
		v.add("/password", CodeOutOfRange, "Must have 1 to %d characters.", MaxPassword)
	}
}

func checkCAPEM(v *validation, s string) {
	if len(s) > MaxCAPEM {
		v.add("/ca_pem", CodeOutOfRange, "At most %d characters.", MaxCAPEM)
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
			v.add("/ca_pem", CodeInvalidFormat, "Contains a certificate that cannot be parsed.")
			return
		}
		certs++
	}
	if certs == 0 {
		v.add("/ca_pem", CodeInvalidFormat, "Must contain at least one PEM encoded certificate.")
	}
}

func checkTLSConsistency(v *validation, mode domain.TLSMode, caPEM, fingerprint string) {
	switch mode {
	case domain.TLSCustomCA:
		if caPEM == "" {
			v.add("/ca_pem", CodeRequired, "Required when tls_mode is \"ca\".")
		}
	case domain.TLSFingerprint:
		if fingerprint == "" {
			v.add("/fingerprint", CodeRequired, "Required when tls_mode is \"fingerprint\".")
		}
	}
}

// DeviceCreate is the input of [Service.CreateDevice]; nil means "not given".
type DeviceCreate struct {
	Name        *string
	BaseURL     *string
	Username    *string
	Password    *secrets.Password
	TLSMode     *string
	CAPEM       *string
	Fingerprint *string
	Enabled     *bool
}

// CreateDevice validates and stores a device and its encrypted password,
// audits it and reloads the channel set. Problems yield *ValidationError, a
// duplicate name [ErrDuplicate].
func (s *Service) CreateDevice(ctx context.Context, actor domain.Actor, req DeviceCreate) (domain.Device, error) {
	var v validation
	d := domain.Device{
		Connector: domain.ConnectorRouterOSREST, TLSMode: domain.TLSVerify, Enabled: req.Enabled == nil || *req.Enabled,
	}
	if req.Name == nil {
		v.add("/name", CodeRequired, "A name is required.")
	} else {
		d.Name = *req.Name
		checkName(&v, d.Name)
	}
	if req.BaseURL == nil {
		v.add("/base_url", CodeRequired, "A base URL is required.")
	} else {
		d.BaseURL = *req.BaseURL
		s.checkBaseURL(&v, d.BaseURL)
	}
	if req.Username == nil {
		v.add("/username", CodeRequired, "A username is required.")
	} else {
		d.Username = *req.Username
		checkUsername(&v, d.Username)
	}
	if req.Password == nil {
		v.add("/password", CodeRequired, "A password is required.")
	} else {
		checkPassword(&v, *req.Password)
	}
	if req.TLSMode != nil {
		m, err := domain.ParseTLSMode(*req.TLSMode)
		if err != nil {
			v.add("/tls_mode", CodeInvalidFormat, "Must be one of verify, ca, fingerprint, insecure.")
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
		fp, ok := NormalizeFingerprint(*req.Fingerprint)
		if !ok {
			v.add("/fingerprint", CodeInvalidFormat, "Must be a SHA-256 fingerprint: 32 hex bytes, optionally separated by colons.")
		}
		d.Fingerprint = fp
	}
	if d.TLSMode.Valid() {
		checkTLSConsistency(&v, d.TLSMode, d.CAPEM, d.Fingerprint)
	}
	if err := v.err(); err != nil {
		return domain.Device{}, err
	}

	var err error
	d.ID, d.CreatedAt = s.ids.NewID(), s.clk.Now()
	if d.PasswordEnc, err = s.creds.Encrypt(d.ID, *req.Password); err != nil {
		return domain.Device{}, fmt.Errorf("encrypt device password: %w", err)
	}
	err = s.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Devices().Create(ctx, d); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, domain.AuditDeviceCreate, "device:"+string(d.ID),
			map[string]string{"name": d.Name, "base_url": d.BaseURL})
	})
	if errors.Is(err, domain.ErrConflict) {
		return domain.Device{}, ErrDuplicate
	}
	if err != nil {
		return domain.Device{}, fmt.Errorf("create device: %w", err)
	}
	s.reload(ctx)
	return d, nil
}

// DeviceChanges is the input of [Service.UpdateDevice]; nil fields are left
// unchanged. A non-nil CAPEM or Fingerprint replaces the value ("" removes it).
type DeviceChanges struct {
	Name        *string
	BaseURL     *string
	Username    *string
	Password    *secrets.Password
	TLSMode     *string
	CAPEM       *string
	Fingerprint *string
	Enabled     *bool
}

// ValidateDeviceChanges checks the fields of ch that can be checked alone.
// UpdateDevice runs it; the API uses it to report these problems together
// with its own.
func (s *Service) ValidateDeviceChanges(ch DeviceChanges) []FieldError {
	var v validation
	s.checkDeviceChanges(&v, ch)
	return v.errs
}

func (s *Service) checkDeviceChanges(v *validation, ch DeviceChanges) {
	if ch.Name != nil {
		checkName(v, *ch.Name)
	}
	if ch.BaseURL != nil {
		s.checkBaseURL(v, *ch.BaseURL)
	}
	if ch.Username != nil {
		checkUsername(v, *ch.Username)
	}
	if ch.Password != nil {
		checkPassword(v, *ch.Password)
	}
	if ch.TLSMode != nil {
		if _, err := domain.ParseTLSMode(*ch.TLSMode); err != nil {
			v.add("/tls_mode", CodeInvalidFormat, "Must be one of verify, ca, fingerprint, insecure.")
		}
	}
	if ch.CAPEM != nil && *ch.CAPEM != "" {
		checkCAPEM(v, *ch.CAPEM)
	}
	if ch.Fingerprint != nil && *ch.Fingerprint != "" {
		if _, ok := NormalizeFingerprint(*ch.Fingerprint); !ok {
			v.add("/fingerprint", CodeInvalidFormat, "Must be a SHA-256 fingerprint: 32 hex bytes, optionally separated by colons.")
		}
	}
}

// UpdateDevice applies ch, audits it (without secrets) and reloads the
// channel set. Problems yield *ValidationError (also when the merged TLS
// settings are inconsistent), an unknown ID domain.ErrNotFound, a duplicate
// name [ErrDuplicate].
func (s *Service) UpdateDevice(ctx context.Context, actor domain.Actor, id domain.ID, ch DeviceChanges) (domain.Device, error) {
	var v validation
	s.checkDeviceChanges(&v, ch)
	if err := v.err(); err != nil {
		return domain.Device{}, err
	}
	var mode domain.TLSMode
	if ch.TLSMode != nil {
		mode, _ = domain.ParseTLSMode(*ch.TLSMode)
	}
	var fp string
	if ch.Fingerprint != nil && *ch.Fingerprint != "" {
		fp, _ = NormalizeFingerprint(*ch.Fingerprint)
	}

	var updated domain.Device
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		d, err := tx.Devices().Get(ctx, id)
		if err != nil {
			return err
		}
		meta := map[string]string{}
		if ch.Name != nil {
			d.Name = *ch.Name
			meta["name"] = d.Name
		}
		if ch.BaseURL != nil {
			d.BaseURL = *ch.BaseURL
			meta["base_url"] = d.BaseURL
		}
		if ch.Username != nil {
			d.Username = *ch.Username
			meta["username"] = "changed"
		}
		if ch.TLSMode != nil {
			d.TLSMode = mode
			meta["tls_mode"] = string(mode)
		}
		if ch.CAPEM != nil {
			d.CAPEM = *ch.CAPEM
			meta["ca_pem"] = "changed"
		}
		if ch.Fingerprint != nil {
			d.Fingerprint = fp
			meta["fingerprint"] = "changed"
		}
		if ch.Enabled != nil {
			d.Enabled = *ch.Enabled
			meta["enabled"] = fmt.Sprint(*ch.Enabled)
		}
		// The merged result must be consistent (contract: 422).
		var cv validation
		checkTLSConsistency(&cv, d.TLSMode, d.CAPEM, d.Fingerprint)
		if err := cv.err(); err != nil {
			return err
		}
		d.PasswordEnc = nil // nil keeps the stored password
		if ch.Password != nil {
			if d.PasswordEnc, err = s.creds.Encrypt(d.ID, *ch.Password); err != nil {
				return fmt.Errorf("encrypt device password: %w", err)
			}
			meta["password"] = "changed"
		}
		if err := tx.Devices().Update(ctx, d); err != nil {
			return err
		}
		updated = d
		return s.audit(ctx, tx, actor, domain.AuditDeviceUpdate, "device:"+string(id), meta)
	})
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		return domain.Device{}, ve
	case errors.Is(err, domain.ErrNotFound):
		return domain.Device{}, fmt.Errorf("device: %w", domain.ErrNotFound)
	case errors.Is(err, domain.ErrConflict):
		return domain.Device{}, ErrDuplicate
	case err != nil:
		return domain.Device{}, fmt.Errorf("update device: %w", err)
	}
	s.reload(ctx)
	return updated, nil
}

// DeleteDevice deletes a device with its channels. Queued or sending messages
// yield *[ActiveMessagesError]; an unknown ID domain.ErrNotFound.
func (s *Service) DeleteDevice(ctx context.Context, actor domain.Actor, id domain.ID) error {
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Devices().Delete(ctx, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, domain.AuditDeviceDelete, "device:"+string(id), nil)
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("device: %w", domain.ErrNotFound)
	case errors.Is(err, domain.ErrConflict):
		return &ActiveMessagesError{}
	case err != nil:
		return fmt.Errorf("delete device: %w", err)
	}
	s.reload(ctx)
	return nil
}

// GetDevice returns a device; an unknown ID yields domain.ErrNotFound.
func (s *Service) GetDevice(ctx context.Context, id domain.ID) (domain.Device, error) {
	d, err := s.st.Devices().Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Device{}, fmt.Errorf("device: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.Device{}, fmt.Errorf("get device: %w", err)
	}
	return d, nil
}
