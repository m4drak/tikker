// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
)

// Notices of the user pages.
const (
	noticeUserCreated  = "created"
	noticeUserSaved    = "saved"
	noticePasswordSet  = "password"
	noticePasswordSelf = "password_changed"
)

func (s *Server) registerUsers() {
	s.HandleFunc("GET /users", domain.RoleAdmin, s.users)
	s.HandleFunc("GET /users/new", domain.RoleAdmin, s.userNew)
	s.HandleFunc("POST /users", domain.RoleAdmin, s.userCreate)
	s.HandleFunc("GET /users/{id}", domain.RoleAdmin, s.userDetails)
	s.HandleFunc("POST /users/{id}/role", domain.RoleAdmin, s.userRole)
	s.HandleFunc("POST /users/{id}/status", domain.RoleAdmin, s.userStatus)
	s.HandleFunc("POST /users/{id}/password", domain.RoleAdmin, s.userPassword)
	s.HandleFunc("GET /profile", "", s.profile)
	s.HandleFunc("POST /profile/password", "", s.profilePassword)
}

var roles = []domain.Role{domain.RoleViewer, domain.RoleOperator, domain.RoleAdmin}

type userRow struct {
	ID       domain.ID
	Username string
	RoleKey  string
	Disabled bool
	Created  time.Time
	Self     bool
}

type usersData struct {
	Rows   []userRow
	Notice string
}

type userFormData struct {
	Username string
	Role     string
	Roles    []optionItem
	Errors   map[string]string
}

// userData is the data of the details page of one user.
type userData struct {
	U       userRow
	Roles   []optionItem
	Errors  map[string]string // by form: "role", "status", "password"
	Notice  string
	Alert   string // translated, e.g. the last-admin conflict
	CanSelf bool
}

func (s *Server) roleOptions(lang string, selected string) []optionItem {
	var out []optionItem
	for _, r := range roles {
		out = append(out, optionItem{Value: string(r), Label: s.i18n.T(lang, "role."+string(r)), Selected: string(r) == selected})
	}
	return out
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	us, err := s.st.Users().List(r.Context())
	if err != nil {
		s.internalError(w, r, "list users failed", err)
		return
	}
	data := usersData{Notice: notice(r.URL.Query().Get("notice"), noticeUserCreated, noticeUserSaved)}
	for _, u := range us {
		data.Rows = append(data.Rows, userRow{ID: u.ID, Username: u.Username, RoleKey: "role." + string(u.Role),
			Disabled: u.Disabled, Created: u.CreatedAt, Self: u.ID == p.User.ID})
	}
	s.Render(w, r, http.StatusOK, "users", data)
}

func (s *Server) userNew(w http.ResponseWriter, r *http.Request) {
	s.Render(w, r, http.StatusOK, "user_new", userFormData{Role: string(domain.RoleViewer), Roles: s.roleOptions(s.language(r), string(domain.RoleViewer))})
}

func (s *Server) userCreate(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	lang := s.language(r)
	f := userFormData{Username: strings.TrimSpace(r.PostFormValue("username")), Role: r.PostFormValue("role")}
	render := func(status int, errs map[string]string) {
		f.Roles, f.Errors = s.roleOptions(lang, f.Role), errs
		s.Render(w, r, status, "user_new", f)
	}
	password := r.PostFormValue("password")
	if password != r.PostFormValue("confirm") {
		render(http.StatusUnprocessableEntity, map[string]string{"confirm": s.i18n.T(lang, "setup.err.mismatch")})
		return
	}
	u, err := s.auth.CreateUser(r.Context(), p.Actor(), auth.NewUser{Username: f.Username, Password: password, Role: domain.Role(f.Role)})
	switch {
	case err == nil:
		http.Redirect(w, r, "/users/"+string(u.ID)+"?notice="+noticeUserCreated, http.StatusSeeOther)
		return
	case errors.Is(err, domain.ErrConflict):
		render(http.StatusConflict, map[string]string{"username": s.i18n.T(lang, "usr.err.duplicate")})
		return
	}
	if ps, ok := problemsOf(err); ok {
		render(http.StatusUnprocessableEntity, s.fieldMessages(lang, ps))
		return
	}
	s.internalError(w, r, "create user failed", err)
}

// loadUser reads the user of the path.
func (s *Server) loadUser(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return domain.User{}, false
	}
	u, err := s.st.Users().Get(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		s.notFound(w, r)
		return u, false
	}
	if err != nil {
		s.internalError(w, r, "get user failed", err)
		return u, false
	}
	return u, true
}

func (s *Server) userPage(w http.ResponseWriter, r *http.Request, status int, u domain.User, errs map[string]string, noticeValue, alert string) {
	p, _ := auth.PrincipalFrom(r.Context())
	s.Render(w, r, status, "user", userData{
		U:     userRow{ID: u.ID, Username: u.Username, RoleKey: "role." + string(u.Role), Disabled: u.Disabled, Created: u.CreatedAt, Self: u.ID == p.User.ID},
		Roles: s.roleOptions(s.language(r), string(u.Role)), Errors: errs, Notice: noticeValue, Alert: alert,
	})
}

func (s *Server) userDetails(w http.ResponseWriter, r *http.Request) {
	u, ok := s.loadUser(w, r)
	if !ok {
		return
	}
	s.userPage(w, r, http.StatusOK, u, nil, notice(r.URL.Query().Get("notice"), noticeUserCreated, noticeUserSaved, noticePasswordSet), "")
}

// updateUser applies changes of the admin pages. Changes end the sessions of
// the user; the signed-in admin who changed their own account gets a fresh
// session (or goes to the login page when they locked themselves out).
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, u domain.User, ch auth.UserChanges, form string, okNotice string) {
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	lang := s.language(r)
	updated, err := s.auth.UpdateUser(r.Context(), p.Actor(), u.ID, ch)
	switch {
	case err == nil:
	case errors.Is(err, domain.ErrNotFound):
		s.notFound(w, r)
		return
	case errors.Is(err, domain.ErrConflict):
		s.userPage(w, r, http.StatusConflict, u, nil, "", s.i18n.T(lang, "usr.err.last_admin"))
		return
	default:
		if ps, ok := problemsOf(err); ok {
			s.userPage(w, r, http.StatusUnprocessableEntity, u, map[string]string{form: firstMessage(s.fieldMessages(lang, ps))}, "", "")
			return
		}
		s.internalError(w, r, "update user failed", err)
		return
	}
	if u.ID == p.User.ID {
		if updated.Disabled {
			http.Redirect(w, r, loginPath, http.StatusSeeOther)
			return
		}
		if _, err := s.auth.StartSession(w, r, updated); err != nil {
			s.log.Error("web: restarting the session failed", "err", err.Error())
			http.Redirect(w, r, loginPath, http.StatusSeeOther)
			return
		}
		if !updated.Role.Allows(domain.RoleAdmin) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/users/"+string(u.ID)+"?notice="+okNotice, http.StatusSeeOther)
}

func (s *Server) userRole(w http.ResponseWriter, r *http.Request) {
	u, ok := s.loadUser(w, r)
	if !ok || !s.postForm(w, r) {
		return
	}
	role := domain.Role(r.PostFormValue("role"))
	s.updateUser(w, r, u, auth.UserChanges{Role: &role}, "role", noticeUserSaved)
}

func (s *Server) userStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := s.loadUser(w, r)
	if !ok || !s.postForm(w, r) {
		return
	}
	disabled := r.PostFormValue("disabled") == "true"
	s.updateUser(w, r, u, auth.UserChanges{Disabled: &disabled}, "status", noticeUserSaved)
}

// userPassword resets the password of a user (the admin sets a new one).
func (s *Server) userPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.loadUser(w, r)
	if !ok || !s.postForm(w, r) {
		return
	}
	pw := r.PostFormValue("password")
	if pw != r.PostFormValue("confirm") {
		s.userPage(w, r, http.StatusUnprocessableEntity, u, map[string]string{"password": s.i18n.T(s.language(r), "setup.err.mismatch")}, "", "")
		return
	}
	s.updateUser(w, r, u, auth.UserChanges{Password: &pw}, "password", noticePasswordSet)
}

// ---------------------------------------------------------------------------
// Own profile

type profileData struct {
	Errors map[string]string
	Notice string
}

func (s *Server) profile(w http.ResponseWriter, r *http.Request) {
	s.Render(w, r, http.StatusOK, "profile", profileData{Notice: notice(r.URL.Query().Get("notice"), noticePasswordSelf)})
}

// profilePassword changes the own password after checking the current one.
func (s *Server) profilePassword(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	p, ok := s.actor(w, r)
	if !ok {
		return
	}
	lang := s.language(r)
	fail := func(status int, field, key string) {
		s.Render(w, r, status, "profile", profileData{Errors: map[string]string{field: s.i18n.T(lang, key)}})
	}
	next := r.PostFormValue("password")
	if next != r.PostFormValue("confirm") {
		fail(http.StatusUnprocessableEntity, "confirm", "setup.err.mismatch")
		return
	}
	_, err := s.auth.ChangeOwnPassword(w, r, p, r.PostFormValue("current"), next)
	var rl *auth.RateLimitError
	switch {
	case err == nil:
		http.Redirect(w, r, "/profile?notice="+noticePasswordSelf, http.StatusSeeOther)
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", strconv.Itoa(rl.RetryAfterSeconds()))
		s.Render(w, r, http.StatusTooManyRequests, "profile", profileData{Errors: map[string]string{"current": s.i18n.T(lang, "login.rate_limited", rl.RetryAfterSeconds())}})
	case errors.Is(err, auth.ErrInvalidCredentials):
		fail(http.StatusUnprocessableEntity, "current", "profile.err.current")
	default:
		if ps, ok := problemsOf(err); ok {
			s.Render(w, r, http.StatusUnprocessableEntity, "profile", profileData{Errors: s.fieldMessages(lang, ps)})
			return
		}
		s.internalError(w, r, "change password failed", err)
	}
}

// firstMessage returns some message of the map (there is usually one).
func firstMessage(m map[string]string) string {
	for _, v := range m {
		return v
	}
	return ""
}
