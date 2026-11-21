// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"bytes"
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

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/connector/routeros"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/queue"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

var t0 = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

// testParams are cheap argon2id parameters for fast tests.
var testParams = auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const goodPassword = "correct horse battery"

type env struct {
	t     *testing.T
	srv   *Server
	h     http.Handler
	svc   *auth.Service
	clk   *clock.Fake
	st    *sqlite.Store
	caps  *fakeCaps
	queue *fakeQueue
	hs    *fakeHealth
	wake  *fakeNotifier

	admin   *admin.Service
	router  *fake.Server // the fake RouterOS behind the devices of the tests
	url     string       // its base URL
	reloads *countReloader
	checker *fakeChecker
}

const (
	routerUser = "tikker"
	routerPass = "r0uter-Secret-Pa55"
)

type countReloader struct{ n int }

func (c *countReloader) Reload(context.Context) error { c.n++; return nil }

// fakeChecker answers CheckNow with a canned status.
type fakeChecker struct {
	status health.ChannelStatus
	err    error
	calls  int
}

func (f *fakeChecker) CheckNow(_ context.Context, id domain.ID) (health.ChannelStatus, error) {
	f.calls++
	st := f.status
	st.ChannelID = id
	return st, f.err
}

// fakeCaps gives every channel the capabilities of a RouterOS modem (GSM-7
// only, one segment) unless a channel has its own entry.
type fakeCaps struct {
	def connector.Capabilities
	per map[domain.ID]connector.Capabilities
}

func (f *fakeCaps) ChannelCapabilities(id domain.ID) (connector.Capabilities, bool) {
	if c, ok := f.per[id]; ok {
		return c, true
	}
	return f.def, true
}

type fakeQueue struct{ stats queue.Stats }

func (f *fakeQueue) Stats() queue.Stats { return f.stats }

type fakeHealth struct{ snap []health.ChannelStatus }

func (f *fakeHealth) Snapshot() []health.ChannelStatus { return f.snap }

type fakeNotifier struct{ n int }

func (f *fakeNotifier) Notify() { f.n++ }

// newEnv builds a panel on an in-memory database. users maps a username to
// its role; the password of every user is goodPassword.
func newEnv(t *testing.T, users map[string]domain.Role, mods ...func(*Options, *auth.Config)) *env {
	t.Helper()
	return newEnvSending(t, sending.Options{DefaultRegion: "PL"}, users, mods...)
}

// newEnvSending is newEnv with the settings of the sending service.
func newEnvSending(t *testing.T, so sending.Options, users map[string]domain.Role, mods ...func(*Options, *auth.Config)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	st, err := sqlite.Open(context.Background(), sqlite.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	acfg := auth.Config{Store: st, Clock: clk, Params: testParams, Logger: log}
	e := &env{t: t, clk: clk, st: st,
		caps:  &fakeCaps{def: connector.Capabilities{Encodings: []domain.Encoding{domain.EncodingGSM7}, MaxSegments: 1}},
		queue: &fakeQueue{}, hs: &fakeHealth{}, wake: &fakeNotifier{}}
	snd, err := sending.New(sending.Deps{
		Store: st, Capabilities: e.caps, Notifier: e.wake, Clock: clk,
		Options: so,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.router = fake.New(fake.Options{User: routerUser, Password: routerPass, Ports: []string{"lte1", "lte2"}, Now: clk.Now})
	e.url = e.router.Start()
	t.Cleanup(e.router.Close)
	reg := connector.NewRegistry()
	if err := routeros.Register(reg, routeros.Options{Logger: log, Clock: clk}); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.reloads, e.checker = &countReloader{}, &fakeChecker{}
	e.admin, err = admin.New(admin.Deps{
		Store: st, Credentials: secrets.NewDeviceCredentials(ring), Connectors: reg, Reloader: e.reloads,
		Checker: e.checker, Clock: clk, Logger: log,
		Options: admin.Options{AllowedHTTPHosts: []string{"127.0.0.1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Logger: log, Store: st, Sending: snd, Admin: e.admin, Clock: clk, Queue: e.queue, Health: e.hs}
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
	e.srv, e.h, e.svc = srv, srv.Handler(), svc
	return e
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
