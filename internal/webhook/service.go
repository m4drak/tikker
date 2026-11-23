// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// Limits and error codes of the administration API.
const (
	MaxName      = 100
	MaxURL       = 2048
	MinSecret    = 16
	MaxSecret    = 256
	CodeRequired = "required"
	CodeFormat   = "invalid_format"
	CodeRange    = "out_of_range"
	CodeInsecure = "insecure_transport_not_allowed"
	// CodeForbidden: the destination is an internal address (SSRF guard).
	CodeForbidden = "forbidden_destination"
	// SecretPrefix starts every generated secret.
	SecretPrefix = "whsec_"
)

// Audited actions (convention "<object>.<verb>"). Meta never holds the secret
// or the URL path, which may carry a token.
const (
	AuditCreate domain.AuditAction = "webhook.create"
	AuditUpdate domain.AuditAction = "webhook.update"
	AuditDelete domain.AuditAction = "webhook.delete"
)

// FieldError is one validation problem; Field is a JSON pointer.
type FieldError struct{ Field, Code, Detail string }

// ValidationError lists the problems of a request.
type ValidationError struct{ Errors []FieldError }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, f := range e.Errors {
		parts[i] = f.Field + ": " + f.Detail
	}
	return "webhook: invalid: " + strings.Join(parts, "; ")
}

// Sealer encrypts a webhook secret; *secrets.Keyring implements it.
type Sealer interface {
	Seal(purpose, recordID string, plaintext []byte) ([]byte, error)
}

// ServiceDeps are the collaborators of a [Service].
type ServiceDeps struct {
	Store  store.Store
	Sealer Sealer
	Clock  clock.Clock        // default system clock
	IDs    domain.IDGenerator // default ULIDs
	Waker  Waker              // optional; woken after a change
	// AllowHTTP reports whether a plain http:// URL may use host (the
	// administrator's allowlist, as for device URLs). Nil: http is never
	// allowed.
	AllowHTTP func(host string) bool
}

// Service administers webhooks: validation, secret handling and storage.
type Service struct {
	st    store.Store
	seal  Sealer
	clk   clock.Clock
	ids   domain.IDGenerator
	waker Waker
	allow func(string) bool
}

// NewService validates deps.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Store == nil || d.Sealer == nil {
		return nil, errors.New("webhook: ServiceDeps.Store and Sealer are required")
	}
	if d.Clock == nil {
		d.Clock = clock.System()
	}
	if d.IDs == nil {
		d.IDs = domain.NewULIDGenerator(d.Clock)
	}
	if d.AllowHTTP == nil {
		d.AllowHTTP = func(string) bool { return false }
	}
	return &Service{st: d.Store, seal: d.Sealer, clk: d.Clock, ids: d.IDs, waker: d.Waker, allow: d.AllowHTTP}, nil
}

// Create is the input of [Service.Create]; nil Secret generates one, nil
// Enabled means true.
type Create struct {
	Name    *string
	URL     *string
	Secret  *string
	Events  []string // nil: missing
	Enabled *bool
}

// Changes is the input of [Service.Update]; nil fields stay unchanged.
type Changes struct {
	Name    *string
	URL     *string
	Secret  *string
	Events  *[]string
	Enabled *bool
}

type validation struct{ errs []FieldError }

func (v *validation) add(field, code, format string, args ...any) {
	v.errs = append(v.errs, FieldError{Field: field, Code: code, Detail: fmt.Sprintf(format, args...)})
}

func (v *validation) err() error {
	if len(v.errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: v.errs}
}

func (s *Service) checkName(v *validation, name string) {
	if n := utf8.RuneCountInString(name); n < 1 || n > MaxName {
		v.add("/name", CodeRange, "Must have 1 to %d characters.", MaxName)
	}
}

func (s *Service) checkURL(v *validation, raw string) {
	u, err := url.Parse(raw)
	switch {
	case len(raw) > MaxURL:
		v.add("/url", CodeRange, "At most %d characters.", MaxURL)
	case err != nil || u.Hostname() == "":
		v.add("/url", CodeFormat, "Must be an absolute http:// or https:// URL.")
	case u.User != nil || u.Fragment != "":
		v.add("/url", CodeFormat, "Must not contain credentials or a fragment.")
	case s.internalLiteral(u.Hostname()):
		v.add("/url", CodeForbidden, "Internal, loopback and link-local addresses are not allowed as a destination.")
	case strings.EqualFold(u.Scheme, "https"):
	case strings.EqualFold(u.Scheme, "http"):
		if !s.allow(u.Hostname()) {
			v.add("/url", CodeInsecure, "Plain http:// is only allowed for the configured local addresses; use https://.")
		}
	default:
		v.add("/url", CodeFormat, "Must be an absolute http:// or https:// URL.")
	}
}

// internalLiteral reports a URL host that is an internal IP address not on the
// administrator's allowlist. Names are checked when the request is sent,
// after resolution (see Sender).
func (s *Service) internalLiteral(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && BlockedAddr(ip) && !s.allow(host)
}

func checkSecret(v *validation, secret string) {
	if n := len(secret); n < MinSecret || n > MaxSecret {
		v.add("/secret", CodeRange, "Must have %d to %d bytes.", MinSecret, MaxSecret)
	}
}

// parseEvents validates the event list: non-empty, known, duplicates dropped.
func parseEvents(v *validation, names []string) []domain.WebhookEvent {
	if len(names) == 0 {
		v.add("/events", CodeRequired, "At least one event is required.")
		return nil
	}
	var out []domain.WebhookEvent
	for _, n := range names {
		ev, err := domain.ParseWebhookEvent(n)
		if err != nil {
			v.add("/events", CodeFormat, "Unknown event %q; use incoming_sms or message_status.", n)
			continue
		}
		if !slices.Contains(out, ev) {
			out = append(out, ev)
		}
	}
	return out
}

// GenerateSecret returns a random signing secret.
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("webhook: generate secret: %w", err)
	}
	return SecretPrefix + hex.EncodeToString(b), nil
}

// Create validates and stores a webhook. It returns the plaintext secret,
// which is not retrievable afterwards. Problems yield *ValidationError, a
// duplicate name domain.ErrConflict.
func (s *Service) Create(ctx context.Context, actor domain.Actor, in Create) (domain.Webhook, string, error) {
	var v validation
	w := domain.Webhook{ID: s.ids.NewID(), Enabled: in.Enabled == nil || *in.Enabled, CreatedAt: s.clk.Now()}
	if in.Name == nil {
		v.add("/name", CodeRequired, "A name is required.")
	} else {
		w.Name = *in.Name
		s.checkName(&v, w.Name)
	}
	if in.URL == nil {
		v.add("/url", CodeRequired, "A URL is required.")
	} else {
		w.URL = *in.URL
		s.checkURL(&v, w.URL)
	}
	w.Events = parseEvents(&v, in.Events)
	var secret string
	if in.Secret != nil {
		secret = *in.Secret
		checkSecret(&v, secret)
	}
	if err := v.err(); err != nil {
		return domain.Webhook{}, "", err
	}
	if in.Secret == nil {
		var err error
		if secret, err = GenerateSecret(); err != nil {
			return domain.Webhook{}, "", err
		}
	}
	enc, err := s.seal.Seal(SecretPurpose, w.ID.String(), []byte(secret))
	if err != nil {
		return domain.Webhook{}, "", fmt.Errorf("webhook: seal secret: %w", err)
	}
	w.SecretEnc = enc
	err = s.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Webhooks().Create(ctx, w); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, AuditCreate, w, map[string]string{"events": eventNames(w.Events)})
	})
	if err != nil {
		return domain.Webhook{}, "", fmt.Errorf("webhook: create: %w", err)
	}
	return w, secret, nil
}

// Get returns one webhook (domain.ErrNotFound if unknown).
func (s *Service) Get(ctx context.Context, id domain.ID) (domain.Webhook, error) {
	w, err := s.st.Webhooks().Get(ctx, id)
	if err != nil {
		return domain.Webhook{}, fmt.Errorf("webhook: get: %w", err)
	}
	return w, nil
}

// List returns all webhooks.
func (s *Service) List(ctx context.Context) ([]domain.Webhook, error) {
	ws, err := s.st.Webhooks().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("webhook: list: %w", err)
	}
	return ws, nil
}

// Update applies ch to the webhook id.
func (s *Service) Update(ctx context.Context, actor domain.Actor, id domain.ID, ch Changes) (domain.Webhook, error) {
	var v validation
	if ch.Name != nil {
		s.checkName(&v, *ch.Name)
	}
	if ch.URL != nil {
		s.checkURL(&v, *ch.URL)
	}
	if ch.Secret != nil {
		checkSecret(&v, *ch.Secret)
	}
	var events []domain.WebhookEvent
	if ch.Events != nil {
		events = parseEvents(&v, *ch.Events)
	}
	if err := v.err(); err != nil {
		return domain.Webhook{}, err
	}
	var out domain.Webhook
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		w, err := tx.Webhooks().Get(ctx, id)
		if err != nil {
			return err
		}
		if ch.Name != nil {
			w.Name = *ch.Name
		}
		if ch.URL != nil {
			w.URL = *ch.URL
		}
		if ch.Events != nil {
			w.Events = events
		}
		if ch.Enabled != nil {
			w.Enabled = *ch.Enabled
		}
		stored := w.SecretEnc
		w.SecretEnc = nil // keep the stored secret unless replaced
		if ch.Secret != nil {
			if w.SecretEnc, err = s.seal.Seal(SecretPurpose, w.ID.String(), []byte(*ch.Secret)); err != nil {
				return fmt.Errorf("seal secret: %w", err)
			}
		}
		if err := tx.Webhooks().Update(ctx, w); err != nil {
			return err
		}
		if w.SecretEnc == nil {
			w.SecretEnc = stored
		}
		out = w
		return s.audit(ctx, tx, actor, AuditUpdate, w, map[string]string{
			"events": eventNames(w.Events), "enabled": strconv.FormatBool(w.Enabled),
			"secret_rotated": strconv.FormatBool(ch.Secret != nil),
		})
	})
	if err != nil {
		return domain.Webhook{}, fmt.Errorf("webhook: update: %w", err)
	}
	s.wake()
	return out, nil
}

// Delete removes the webhook and its pending deliveries.
func (s *Service) Delete(ctx context.Context, actor domain.Actor, id domain.ID) error {
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		w, err := tx.Webhooks().Get(ctx, id)
		if err != nil {
			return err
		}
		if err := tx.Webhooks().Delete(ctx, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, AuditDelete, w, nil)
	})
	if err != nil {
		return fmt.Errorf("webhook: delete: %w", err)
	}
	return nil
}

func eventNames(evs []domain.WebhookEvent) string {
	names := make([]string, len(evs))
	for i, e := range evs {
		names[i] = string(e)
	}
	return strings.Join(names, ",")
}

// audit appends an entry inside tx. Only the host of the URL is recorded.
func (s *Service) audit(ctx context.Context, tx store.Repos, actor domain.Actor, action domain.AuditAction,
	w domain.Webhook, meta map[string]string) error {
	if meta == nil {
		meta = map[string]string{}
	}
	meta["name"] = w.Name
	if u, err := url.Parse(w.URL); err == nil {
		meta["host"] = u.Host
	}
	return tx.Audit().Append(ctx, domain.AuditLog{
		ID: s.ids.NewID(), Actor: actor, Action: action, Target: "webhook:" + string(w.ID), Meta: meta, At: s.clk.Now(),
	})
}

func (s *Service) wake() {
	if s.waker != nil {
		s.waker.Wake()
	}
}
