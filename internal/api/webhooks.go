// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/webhook"
)

const conflictWebhookDup = "A webhook with this name already exists."

// webhookDTO is schema Webhook. The signing secret is never part of it.
type webhookDTO struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	CreatedAt string   `json:"created_at"`
}

// webhookCreatedDTO is schema WebhookCreated: the secret is shown once.
type webhookCreatedDTO struct {
	webhookDTO
	Secret string `json:"secret"`
}

func toWebhookDTO(w domain.Webhook) webhookDTO {
	evs := make([]string, len(w.Events))
	for i, e := range w.Events {
		evs[i] = string(e)
	}
	return webhookDTO{ID: string(w.ID), Name: w.Name, URL: w.URL, Events: evs, Enabled: w.Enabled, CreatedAt: formatTime(w.CreatedAt)}
}

// webhookCreate is schema WebhookCreate.
type webhookCreate struct {
	Name    *string  `json:"name"`
	URL     *string  `json:"url"`
	Secret  *string  `json:"secret"`
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled"`
}

// webhookProblem maps the errors of the webhook service; nil for others.
func webhookProblem(err error) *apiError {
	var ve *webhook.ValidationError
	if errors.As(err, &ve) {
		var v validation
		for _, f := range ve.Errors {
			v.add(f.Field, f.Code, "%s", f.Detail)
		}
		var ae *apiError
		_ = errors.As(v.err(), &ae)
		return ae
	}
	return mapStoreError(err, "Webhook", conflictWebhookDup)
}

// listWebhooks implements GET /webhooks.
func (a *API) listWebhooks(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	q := newQuery(r, "cursor", "limit")
	after, limit := q.cursor(), q.limit()
	if err := q.err(); err != nil {
		return err
	}
	all, err := a.webhooks.List(r.Context())
	if err != nil {
		return fmt.Errorf("list webhooks: %w", err)
	}
	page, next := pageOf(all, func(w domain.Webhook) domain.ID { return w.ID }, after, limit, false)
	items := make([]webhookDTO, len(page))
	for i, h := range page {
		items[i] = toWebhookDTO(h)
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}

// getWebhook implements GET /webhooks/{id}.
func (a *API) getWebhook(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	h, err := a.webhooks.Get(r.Context(), id)
	if e := webhookProblem(err); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("get webhook: %w", err)
	}
	writeJSON(w, http.StatusOK, toWebhookDTO(h))
	return nil
}

// createWebhook implements POST /webhooks.
func (a *API) createWebhook(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	var req webhookCreate
	if err := decodeStrict(raw, &req); err != nil {
		return err
	}
	h, secret, err := a.webhooks.Create(r.Context(), actorOf(p), webhook.Create(req))
	if e := webhookProblem(err); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("create webhook: %w", err)
	}
	w.Header().Set("Cache-Control", "no-store") // the response carries the secret
	w.Header().Set("Location", Prefix+"/webhooks/"+string(h.ID))
	writeJSON(w, http.StatusCreated, webhookCreatedDTO{webhookDTO: toWebhookDTO(h), Secret: secret})
	return nil
}

var webhookPatchFields = []string{"name", "url", "secret", "events", "enabled"}

// updateWebhook implements PATCH /webhooks/{id} (JSON Merge Patch).
func (a *API) updateWebhook(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	patch, err := decodePatch(raw, webhookPatchFields...)
	if err != nil {
		return err
	}
	var v validation
	if len(patch) == 0 {
		v.add("", codeRequired, "At least one property is required.")
		return v.err()
	}
	var ch webhook.Changes
	if s, ok, err := patchValue[string](patch, "name", &v); err != nil {
		return err
	} else if ok {
		ch.Name = &s
	}
	if s, ok, err := patchValue[string](patch, "url", &v); err != nil {
		return err
	} else if ok {
		ch.URL = &s
	}
	if s, ok, err := patchValue[string](patch, "secret", &v); err != nil {
		return err
	} else if ok {
		ch.Secret = &s
	}
	if evs, ok, err := patchValue[[]string](patch, "events", &v); err != nil {
		return err
	} else if ok {
		ch.Events = &evs
	}
	if en, ok, err := patchValue[bool](patch, "enabled", &v); err != nil {
		return err
	} else if ok {
		ch.Enabled = &en
	}
	if !v.ok() {
		return v.err()
	}
	h, err := a.webhooks.Update(r.Context(), actorOf(p), id, ch)
	if e := webhookProblem(err); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("update webhook: %w", err)
	}
	writeJSON(w, http.StatusOK, toWebhookDTO(h))
	return nil
}

// deleteWebhook implements DELETE /webhooks/{id}.
func (a *API) deleteWebhook(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	err = a.webhooks.Delete(r.Context(), actorOf(p), id)
	if e := webhookProblem(err); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
