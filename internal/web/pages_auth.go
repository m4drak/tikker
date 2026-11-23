// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/m4drak/tikker/internal/auth"
)

// Paths of the authentication pages.
const (
	loginPath = auth.DefaultLoginPath
	setupPath = "/setup"
)

type loginData struct {
	Next     string
	Username string
	Error    string // translated, "" when there is none
}

type setupData struct {
	Username string
	// NeedsSecret is true when TIKKER_SETUP_SECRET is set: the wizard then
	// requires the secret to be provided (F-01).
	NeedsSecret bool
	Errors      map[string]string // field ("username", "password", "confirm", "setup_secret") -> translated message
}

// limitBody bounds the body of a panel form.
func limitBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
}

// setupRequired reports whether the first-run wizard applies. A failure to
// ask is logged and treated as "not required", so that an error never opens
// the wizard.
func (s *Server) setupRequired(r *http.Request) bool {
	ok, err := s.auth.SetupRequired(r.Context())
	if err != nil {
		s.log.Error("web: setup check failed", "err", err.Error())
		return false
	}
	return ok
}

// loginForm shows the login page; it sends users to the wizard while no user
// exists and signed-in users onward.
func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	next := auth.SafeRedirect(r.URL.Query().Get("next"))
	if s.setupRequired(r) {
		http.Redirect(w, r, setupPath, http.StatusSeeOther)
		return
	}
	if _, ok := s.principal(w, r); ok {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.Render(w, r, http.StatusOK, "login", loginData{Next: next})
}

// loginSubmit checks the credentials. Failures never say which part was
// wrong; a rate limit answers 429 with Retry-After.
func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest)
		return
	}
	if s.setupRequired(r) {
		http.Redirect(w, r, setupPath, http.StatusSeeOther)
		return
	}
	username, password := r.PostFormValue("username"), r.PostFormValue("password")
	next := auth.SafeRedirect(r.PostFormValue("next"))
	data := loginData{Next: next, Username: username}
	lang := s.language(r)

	_, err := s.auth.Login(w, r, username, password)
	var rl *auth.RateLimitError
	switch {
	case err == nil:
		http.Redirect(w, r, next, http.StatusSeeOther)
	case errors.As(err, &rl):
		secs := rl.RetryAfterSeconds()
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		data.Error = s.i18n.T(lang, "login.rate_limited", secs)
		s.Render(w, r, http.StatusTooManyRequests, "login", data)
	case errors.Is(err, auth.ErrInvalidCredentials):
		data.Error = s.i18n.T(lang, "login.invalid")
		s.Render(w, r, http.StatusUnauthorized, "login", data)
	default:
		s.log.Error("web: login failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
	}
}

// logout ends the session (POST with CSRF, enforced by auth.CSRF) and
// returns to the login page.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(w, r); err != nil {
		s.log.Error("web: logout failed", "err", err.Error())
	}
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

// setupForm shows the first-run wizard, only while no user exists.
func (s *Server) setupForm(w http.ResponseWriter, r *http.Request) {
	if !s.setupRequired(r) {
		s.notFound(w, r)
		return
	}
	s.Render(w, r, http.StatusOK, "setup", setupData{NeedsSecret: s.setupSecret != ""})
}

// setupSubmit creates the first admin and signs them in. The service
// re-checks atomically that no user exists.
func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest)
		return
	}
	if !s.setupRequired(r) {
		s.notFound(w, r)
		return
	}
	username, password, confirm := r.PostFormValue("username"), r.PostFormValue("password"), r.PostFormValue("confirm")
	lang := s.language(r)
	data := setupData{Username: username, NeedsSecret: s.setupSecret != "", Errors: map[string]string{}}
	// When a setup secret is configured, the wizard is gated on it (F-01).
	// Constant-time compare: the wizard is only reachable before the first
	// admin exists, so a wrong secret is not rate limiting; still keep the
	// comparison timing-safe to avoid leaking the secret length.
	if s.setupSecret != "" && subtle.ConstantTimeCompare([]byte(r.PostFormValue("setup_secret")), []byte(s.setupSecret)) != 1 {
		data.Errors["setup_secret"] = s.i18n.T(lang, "setup.err.secret_invalid")
		s.Render(w, r, http.StatusUnprocessableEntity, "setup", data)
		return
	}
	if password != confirm {
		data.Errors["confirm"] = s.i18n.T(lang, "setup.err.mismatch")
		s.Render(w, r, http.StatusUnprocessableEntity, "setup", data)
		return
	}

	u, err := s.auth.CreateFirstAdmin(r.Context(), username, password)
	var verr *auth.ValidationError
	switch {
	case errors.As(err, &verr):
		for _, fe := range verr.Errors {
			field, key := setupFieldError(fe)
			if _, dup := data.Errors[field]; dup {
				continue
			}
			if key == "setup.err.password_length" {
				data.Errors[field] = s.i18n.T(lang, key, auth.MinPasswordLen)
			} else {
				data.Errors[field] = s.i18n.T(lang, key)
			}
		}
		s.Render(w, r, http.StatusUnprocessableEntity, "setup", data)
		return
	case errors.Is(err, auth.ErrSetupCompleted):
		s.notFound(w, r)
		return
	case err != nil:
		s.log.Error("web: first admin creation failed", "err", err.Error())
		s.errorPage(w, r, http.StatusInternalServerError)
		return
	}
	if _, err := s.auth.StartSession(w, r, u); err != nil {
		s.log.Error("web: starting the first session failed", "err", err.Error())
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// setupFieldError maps a validation error of the service (JSON pointer and
// code) to a form field and an i18n key.
func setupFieldError(fe auth.FieldError) (field, key string) {
	switch fe.Field {
	case "/username":
		if fe.Code == auth.CodeRequired {
			return "username", "setup.err.username_required"
		}
		return "username", "setup.err.username_format"
	case "/password":
		if fe.Code == auth.CodeOutOfRange {
			return "password", "setup.err.password_length"
		}
		return "password", "setup.err.password_invalid"
	}
	return "username", "setup.err.generic"
}

// setLanguage stores the language chosen in the switcher in a cookie and
// returns to the page the user came from (a local path only).
func (s *Server) setLanguage(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest)
		return
	}
	if lang := r.PostFormValue("lang"); s.i18n.Supported(lang) {
		http.SetCookie(w, &http.Cookie{
			Name: LangCookie, Value: lang, Path: "/", MaxAge: int(365 * 24 * time.Hour / time.Second),
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.https || r.TLS != nil,
		})
	}
	http.Redirect(w, r, auth.SafeRedirect(r.PostFormValue("next")), http.StatusSeeOther)
}
