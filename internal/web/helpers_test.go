// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

var t0 = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

// testParams are cheap argon2id parameters for fast tests.
var testParams = auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const goodPassword = "correct horse battery"

type env struct {
	t   *testing.T
	srv *Server
	h   http.Handler
	svc *auth.Service
	clk *clock.Fake
}

// newEnv builds a panel on an in-memory database. users maps a username to
// its role; the password of every user is goodPassword.
func newEnv(t *testing.T, users map[string]domain.Role, mods ...func(*Options, *auth.Config)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	st, err := sqlite.Open(context.Background(), sqlite.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	acfg := auth.Config{Store: st, Clock: clk, Params: testParams, Logger: log}
	opts := Options{Logger: log}
	for _, m := range mods {
		m(&opts, &acfg)
	}
	svc, err := auth.New(acfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, role := range users {
		if _, err := svc.CreateUser(context.Background(), domain.SystemActor,
			auth.NewUser{Username: name, Password: goodPassword, Role: role}); err != nil {
			t.Fatal(err)
		}
	}
	opts.Auth = svc
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, srv: srv, h: srv.Handler(), svc: svc, clk: clk}
}

var allRoles = map[string]domain.Role{
	"viewer1":   domain.RoleViewer,
	"operator1": domain.RoleOperator,
	"admin1":    domain.RoleAdmin,
}

// reqOpt modifies a request.
type reqOpt func(*http.Request)

func withCookies(cs ...*http.Cookie) reqOpt {
	return func(r *http.Request) {
		for _, c := range cs {
			r.AddCookie(c)
		}
	}
}

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// do performs a request against the panel. A non-nil form is sent as a
// urlencoded body.
func (e *env) do(method, target string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// login signs the user in and returns the session cookie.
func (e *env) login(username string) *http.Cookie {
	e.t.Helper()
	w := e.do(http.MethodPost, "/login", url.Values{"username": {username}, "password": {goodPassword}})
	if w.Code != http.StatusSeeOther {
		e.t.Fatalf("login %s: status %d, body %s", username, w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName || c.Name == auth.SecureCookieName {
			return c
		}
	}
	e.t.Fatal("login set no session cookie")
	return nil
}

var csrfMetaRE = regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)">`)

// csrfToken fetches the CSRF token of the session from the dashboard.
func (e *env) csrfToken(c *http.Cookie) string {
	e.t.Helper()
	w := e.do(http.MethodGet, "/", nil, withCookies(c))
	m := csrfMetaRE.FindStringSubmatch(w.Body.String())
	if m == nil {
		e.t.Fatalf("no csrf meta tag in %d response:\n%s", w.Code, w.Body)
	}
	return m[1]
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}
