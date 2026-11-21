// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
)

// Notices of the token page.
const noticeTokenRevoked = "revoked"

func (s *Server) registerTokens() {
	s.HandleFunc("GET /tokens", domain.RoleAdmin, s.tokens)
	s.HandleFunc("POST /tokens", domain.RoleAdmin, s.tokenCreate)
	s.HandleFunc("GET /tokens/{id}/delete", domain.RoleAdmin, s.tokenDeleteConfirm)
	s.HandleFunc("POST /tokens/{id}/delete", domain.RoleAdmin, s.tokenDelete)
}

// scopeKeys are the translation keys of the scopes.
var scopeKeys = map[domain.Scope]string{
	domain.ScopeSMSSend: "tok.scope.send",
	domain.ScopeSMSRead: "tok.scope.read",
	domain.ScopeAdmin:   "tok.scope.admin",
}

type scopeOption struct {
	Value    string
	LabelKey string
	Checked  bool
}

type tokenRow struct {
	ID        domain.ID
	Name      string
	Prefix    string
	Owner     string
	Scopes    []string // translation keys
	Expires   time.Time
	Expired   bool
	LastUsed  time.Time
	CreatedAt time.Time
}

type tokenForm struct {
	Name    string
	Owner   string
	Expires string // YYYY-MM-DD
	Scopes  []string
}

type tokensData struct {
	Rows    []tokenRow
	Form    tokenForm
	Owners  []optionItem
	Scopes  []scopeOption
	Errors  map[string]string
	Notice  string
	Created *createdToken
}

// createdToken is the one-time display of a new token.
type createdToken struct {
	Name   string
	Secret string
}

func (s *Server) scopeOptions(selected []string) []scopeOption {
	var out []scopeOption
	for _, sc := range []domain.Scope{domain.ScopeSMSRead, domain.ScopeSMSSend, domain.ScopeAdmin} {
		out = append(out, scopeOption{Value: string(sc), LabelKey: scopeKeys[sc], Checked: slices.Contains(selected, string(sc))})
	}
	return out
}

// tokensPage renders the list and the creation form.
func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request, status int, f tokenForm, errs map[string]string, noticeValue string) {
	ctx := r.Context()
	users, err := s.st.Users().List(ctx)
	if err != nil {
		s.internalError(w, r, "list users failed", err)
		return
	}
	toks, err := s.st.Tokens().List(ctx, "")
	if err != nil {
		s.internalError(w, r, "list tokens failed", err)
		return
	}
	names := make(map[domain.ID]string, len(users))
	data := tokensData{Form: f, Errors: errs, Notice: noticeValue, Scopes: s.scopeOptions(f.Scopes)}
	for _, u := range users {
		names[u.ID] = u.Username
		if !u.Disabled {
			data.Owners = append(data.Owners, optionItem{Value: string(u.ID), Label: u.Username + " (" + s.i18n.T(s.language(r), "role."+string(u.Role)) + ")",
				Selected: string(u.ID) == f.Owner})
		}
	}
	now := s.clk.Now()
	slices.SortFunc(toks, func(a, b domain.APIToken) int { return strings.Compare(string(b.ID), string(a.ID)) }) // newest first
	for _, t := range toks {
		row := tokenRow{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Owner: names[t.UserID], Expires: t.ExpiresAt,
			Expired: t.Expired(now), LastUsed: t.LastUsedAt, CreatedAt: t.CreatedAt}
		for _, sc := range t.Scopes {
			if k, ok := scopeKeys[sc]; ok {
				row.Scopes = append(row.Scopes, k)
			}
		}
		data.Rows = append(data.Rows, row)
	}
	s.Render(w, r, status, "tokens", data)
}

func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	s.tokensPage(w, r, http.StatusOK, tokenForm{Owner: string(p.User.ID), Scopes: []string{string(domain.ScopeSMSSend), string(domain.ScopeSMSRead)}}, nil,
		notice(r.URL.Query().Get("notice"), noticeTokenRevoked))
}

// tokenCreate creates a token and shows its secret once, in the response of
// the POST itself (never in a redirect or a URL), marked no-store.
func (s *Server) tokenCreate(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	f := tokenForm{
		Name: strings.TrimSpace(r.PostFormValue("name")), Owner: r.PostFormValue("owner"),
		Expires: strings.TrimSpace(r.PostFormValue("expires")), Scopes: r.PostForm["scope"],
	}
	lang := s.language(r)
	in := newToken(f)
	errs := map[string]string{}
	if f.Expires != "" {
		t, err := time.Parse(time.DateOnly, f.Expires)
		if err != nil {
			errs["expires_at"] = s.i18n.T(lang, "err.expires_format")
		} else {
			in.ExpiresAt = t.Add(24*time.Hour - time.Second) // the whole day, UTC
		}
	}
	if len(errs) > 0 {
		s.tokensPage(w, r, http.StatusUnprocessableEntity, f, errs, "")
		return
	}
	tok, secret, err := s.auth.CreateToken(r.Context(), p.Actor(), in)
	if err != nil {
		if ps, ok := problemsOf(err); ok {
			s.tokensPage(w, r, http.StatusUnprocessableEntity, f, s.fieldMessages(lang, mapTokenFields(ps)), "")
			return
		}
		s.internalError(w, r, "create token failed", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.Render(w, r, http.StatusCreated, "token_created", createdToken{Name: tok.Name, Secret: secret})
}

// mapTokenFields renames the pointers of the token problems to form fields.
func mapTokenFields(ps []problem) []problem {
	out := make([]problem, len(ps))
	for i, p := range ps {
		out[i] = p
		switch formField(p.Field) {
		case "user_id":
			out[i].Field = "/owner"
		case "scopes":
			out[i].Field = "/scopes"
		}
	}
	return out
}

func (s *Server) tokenDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	t, ok := s.loadToken(w, r)
	if !ok {
		return
	}
	lang := s.language(r)
	s.Render(w, r, http.StatusOK, "confirm", confirmData{
		Heading: s.i18n.T(lang, "tok.delete.heading", t.Name), Text: s.i18n.T(lang, "tok.delete.text"),
		Action: "/tokens/" + string(t.ID) + "/delete", Back: "/tokens", Button: s.i18n.T(lang, "tok.delete.button"),
	})
}

// loadToken finds the token of the path.
func (s *Server) loadToken(w http.ResponseWriter, r *http.Request) (domain.APIToken, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return domain.APIToken{}, false
	}
	toks, err := s.st.Tokens().List(r.Context(), "")
	if err != nil {
		s.internalError(w, r, "list tokens failed", err)
		return domain.APIToken{}, false
	}
	for _, t := range toks {
		if t.ID == id {
			return t, true
		}
	}
	s.notFound(w, r)
	return domain.APIToken{}, false
}

func (s *Server) tokenDelete(w http.ResponseWriter, r *http.Request) {
	t, ok := s.loadToken(w, r)
	if !ok {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	err := s.auth.DeleteToken(r.Context(), p.Actor(), t.ID)
	switch {
	case err == nil:
		http.Redirect(w, r, "/tokens?notice="+noticeTokenRevoked, http.StatusSeeOther)
	case errors.Is(err, domain.ErrNotFound):
		s.notFound(w, r)
	default:
		s.internalError(w, r, "delete token failed", err)
	}
}

// newToken converts the form to the input of the auth service.
func newToken(f tokenForm) auth.NewToken {
	in := auth.NewToken{UserID: domain.ID(f.Owner), Name: f.Name}
	for _, sc := range f.Scopes {
		in.Scopes = append(in.Scopes, domain.Scope(sc))
	}
	return in
}
