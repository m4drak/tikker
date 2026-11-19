// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

// t0 is the start of the fake clock (whole milliseconds, as stored).
var t0 = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

// testParams are cheap argon2id parameters for fast tests.
var testParams = Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const goodPassword = "correct horse battery"

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t    *testing.T
	ctx  context.Context
	st   *sqlite.Store
	clk  *clock.Fake
	svc  *Service
	logs *syncBuffer
}

func newEnv(t *testing.T, mod ...func(*Config)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	st, err := sqlite.Open(context.Background(), sqlite.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logs := &syncBuffer{}
	cfg := Config{
		Store: st, Clock: clk, Params: testParams,
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, m := range mod {
		m(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: context.Background(), st: st, clk: clk, svc: svc, logs: logs}
}

func (e *env) ok(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("unexpected error: %v", err)
	}
}

func (e *env) user(name string, role domain.Role) domain.User {
	e.t.Helper()
	u, err := e.svc.CreateUser(e.ctx, domain.SystemActor, NewUser{Username: name, Password: goodPassword, Role: role})
	e.ok(err)
	return u
}

func (e *env) token(owner domain.User, scopes ...domain.Scope) (domain.APIToken, string) {
	e.t.Helper()
	tok, full, err := e.svc.CreateToken(e.ctx, domain.SystemActor, NewToken{UserID: owner.ID, Name: "t", Scopes: scopes})
	e.ok(err)
	return tok, full
}

func (e *env) disable(u domain.User) {
	e.t.Helper()
	yes := true
	_, err := e.svc.UpdateUser(e.ctx, domain.SystemActor, u.ID, UserChanges{Disabled: &yes})
	e.ok(err)
}

// audits returns the audit actions in chronological order.
func (e *env) audits() []domain.AuditLog {
	e.t.Helper()
	p, err := e.st.Audit().List(e.ctx, store.AuditFilter{}, store.PageRequest{Limit: 500})
	e.ok(err)
	out := make([]domain.AuditLog, 0, len(p.Items))
	for i := len(p.Items) - 1; i >= 0; i-- {
		out = append(out, p.Items[i])
	}
	return out
}

func (e *env) lastAudit() domain.AuditLog {
	e.t.Helper()
	a := e.audits()
	if len(a) == 0 {
		e.t.Fatal("no audit entries")
	}
	return a[len(a)-1]
}

// request builds a panel request from 192.0.2.10, optionally with cookies.
func request(method, target string, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "192.0.2.10:5555"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

// login performs a successful login and returns the session cookie.
func (e *env) login(name string) *http.Cookie {
	e.t.Helper()
	w := httptest.NewRecorder()
	_, err := e.svc.Login(w, request(http.MethodPost, "/login"), name, goodPassword)
	e.ok(err)
	return sessionCookie(e.t, w)
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if strings.HasSuffix(c.Name, CookieName) {
			return c
		}
	}
	t.Fatal("no session cookie set")
	return nil
}

// okHandler records the principal it was called with.
type okHandler struct {
	called bool
	p      Principal
}

func (h *okHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	h.p, _ = PrincipalFrom(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// failReader fails every read.
type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("entropy exhausted") }

// assertNoSecrets checks that none of secrets appears in the logs.
func (e *env) assertNoSecrets(secrets ...string) {
	e.t.Helper()
	logs := e.logs.String()
	for _, s := range secrets {
		if s != "" && strings.Contains(logs, s) {
			e.t.Fatalf("secret %q found in logs:\n%s", s, logs)
		}
	}
}
