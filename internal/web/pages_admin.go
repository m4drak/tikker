// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
)

// Shared helpers of the administration pages (devices, channels, tokens,
// users, audit).

// fieldErrKeys maps "<form field>/<code>" of a validation problem to its
// translation key; fieldErrByCode is the fallback by code alone.
var (
	fieldErrKeys = map[string]string{
		"base_url/insecure_transport_not_allowed": "err.insecure_transport",
		"base_url/invalid_format":                 "err.base_url",
		"fingerprint/invalid_format":              "err.fingerprint",
		"ca_pem/invalid_format":                   "err.ca_pem",
		"ca_pem/required":                         "err.ca_required",
		"fingerprint/required":                    "err.fingerprint_required",
		"password/out_of_range":                   "err.password_length",
		"agent_token/out_of_range":                "err.agent_token_length",
		"agent_token/invalid_format":              "err.agent_token",
		"agent_token/required":                    "err.agent_token_length",
		"scopes/scope_exceeds_role":               "err.scope_exceeds_role",
		"expires_at/expires_at_in_past":           "err.expires_past",
		"expires_at/invalid_format":               "err.expires_format",
		"tags/invalid_format":                     "err.tags_invalid",
		"tags/out_of_range":                       "err.tags_invalid",
		"port/invalid_format":                     "err.port",
		"username/invalid_format":                 "err.username_format",
		"username/out_of_range":                   "err.username_format",
	}
	fieldErrByCode = map[string]string{
		"required":       "err.required",
		"invalid_format": "err.invalid",
		"out_of_range":   "err.range",
	}
)

// problem is a field error from the admin or auth services.
type problem struct{ Field, Code string }

// formField reduces a JSON pointer ("/tags/0") to its form field ("tags").
func formField(pointer string) string {
	f := strings.TrimPrefix(pointer, "/")
	f, _, _ = strings.Cut(f, "/")
	return f
}

// fieldMessages translates problems into one message per form field (the
// first wins).
func (s *Server) fieldMessages(lang string, ps []problem) map[string]string {
	out := map[string]string{}
	for _, p := range ps {
		field := formField(p.Field)
		if _, dup := out[field]; dup {
			continue
		}
		key, ok := fieldErrKeys[field+"/"+p.Code]
		if !ok {
			if key, ok = fieldErrByCode[p.Code]; !ok {
				key = "err.generic"
			}
		}
		out[field] = s.i18n.T(lang, key)
	}
	return out
}

// problemsOf extracts the field problems of a validation error of either
// service; ok is false for other errors.
func problemsOf(err error) (ps []problem, ok bool) {
	var ae *admin.ValidationError
	if errors.As(err, &ae) {
		for _, f := range ae.Errors {
			ps = append(ps, problem{f.Field, f.Code})
		}
		return ps, true
	}
	var ue *auth.ValidationError
	if errors.As(err, &ue) {
		for _, f := range ue.Errors {
			ps = append(ps, problem{f.Field, f.Code})
		}
		return ps, true
	}
	return nil, false
}

// confirmData is the data of the confirmation page of a destructive action.
type confirmData struct {
	Heading string
	Text    string
	Action  string // POST target
	Back    string // link of the cancel button
	Error   string // translated, "" when there is none
	Button  string // translated label of the confirm button
}

// notice returns n when it is one of the allowed values.
func notice(n string, allowed ...string) string {
	for _, a := range allowed {
		if n == a {
			return n
		}
	}
	return ""
}

// parseID reads a path value as an ID.
func pathID(r *http.Request, name string) (domain.ID, bool) {
	id, err := domain.ParseID(r.PathValue(name))
	return id, err == nil
}

// postForm parses a bounded urlencoded form; false means the 400 page was
// written.
func (s *Server) postForm(w http.ResponseWriter, r *http.Request) bool {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest)
		return false
	}
	return true
}

// actor returns the signed-in principal of an admin page.
func (s *Server) actor(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		s.errorPage(w, r, http.StatusForbidden)
	}
	return p, ok
}

// internalError logs err and answers with the 500 page.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.Error("web: "+what, "err", err.Error())
	s.errorPage(w, r, http.StatusInternalServerError)
}
