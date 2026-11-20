// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
)

// authProblem maps errors of the auth service: validation failures to the
// 422 problem, repository sentinels to 404/409. It returns nil for other
// errors. conflictDetail describes a domain.ErrConflict of the operation.
func authProblem(err error, what, conflictDetail string) *apiError {
	var ve *auth.ValidationError
	if errors.As(err, &ve) {
		errs := make([]fieldError, len(ve.Errors))
		for i, f := range ve.Errors {
			errs[i] = fieldError{Field: f.Field, Code: f.Code, Detail: f.Detail}
		}
		if len(errs) == 1 {
			return validationFailed(errs[0].Detail, errs...)
		}
		return validationFailed("The request is invalid.", errs...)
	}
	return mapStoreError(err, what, conflictDetail)
}

// tokenCreate is schema TokenCreate.
type tokenCreate struct {
	Name      *string  `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expires_at"`
	UserID    *string  `json:"user_id"`
}

// listTokens implements GET /tokens, newest first.
func (a *API) listTokens(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	q := newQuery(r, "user_id", "cursor", "limit")
	userID := q.id("user_id")
	after, limit := q.cursor(), q.limit()
	if err := q.err(); err != nil {
		return err
	}
	toks, err := a.st.Tokens().List(r.Context(), userID)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	page, next := pageOf(toks, func(t domain.APIToken) domain.ID { return t.ID }, after, limit, true)
	items := make([]tokenDTO, len(page))
	for i, t := range page {
		items[i] = toTokenDTO(t)
	}
	writeJSON(w, http.StatusOK, newPage(items, next))
	return nil
}

// getToken implements GET /tokens/{id}.
func (a *API) getToken(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	toks, err := a.st.Tokens().List(r.Context(), "")
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	for _, t := range toks {
		if t.ID == id {
			writeJSON(w, http.StatusOK, toTokenDTO(t))
			return nil
		}
	}
	return notFound("Token")
}

// createToken implements POST /tokens. The secret is in this response only.
func (a *API) createToken(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	raw, err := readBody(w, r, a.opts.MaxBodyBytes)
	if err != nil {
		return err
	}
	var req tokenCreate
	if err := decodeStrict(raw, &req); err != nil {
		return err
	}
	var v validation
	in := auth.NewToken{UserID: p.User.ID}
	if req.Name != nil { // a missing name is reported by the auth service
		in.Name = *req.Name
	}
	for _, s := range req.Scopes {
		in.Scopes = append(in.Scopes, domain.Scope(s))
	}
	if req.ExpiresAt != nil {
		t, ok := parseTimestamp(*req.ExpiresAt)
		if !ok {
			v.add("/expires_at", codeInvalidFormat, "Must be an RFC 3339 timestamp.")
		}
		in.ExpiresAt = t
	}
	if req.UserID != nil {
		if *req.UserID == "" || len(*req.UserID) > maxIDLen {
			v.add("/user_id", codeInvalidFormat, "An ID has 1 to %d characters.", maxIDLen)
		}
		in.UserID = domain.ID(*req.UserID)
	}
	if err := v.err(); err != nil {
		return err
	}

	t, secret, err := a.auth.CreateToken(r.Context(), actorOf(p), in)
	if e := authProblem(err, "User", "A token with this prefix already exists; retry."); e != nil {
		return e
	}
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", Prefix+"/tokens/"+string(t.ID))
	writeJSON(w, http.StatusCreated, tokenCreatedDTO{tokenDTO: toTokenDTO(t), Token: secret})
	return nil
}

// revokeToken implements DELETE /tokens/{id}. A token may revoke itself.
func (a *API) revokeToken(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	err = a.auth.DeleteToken(r.Context(), actorOf(p), id)
	if errors.Is(err, domain.ErrNotFound) {
		return notFound("Token")
	}
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
