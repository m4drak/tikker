// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

func TestRequireToken(t *testing.T) {
	e := newEnv(t)
	admin := e.user("admin", domain.RoleAdmin)
	op := e.user("operator", domain.RoleOperator)
	viewer := e.user("viewer", domain.RoleViewer)
	gone := e.user("gone", domain.RoleOperator)

	_, adminTok := e.token(admin, domain.ScopeAdmin)
	opTokRec, opTok := e.token(op, domain.ScopeSMSSend, domain.ScopeSMSRead)
	_, readTok := e.token(op, domain.ScopeSMSRead)
	_, viewerTok := e.token(viewer, domain.ScopeSMSRead)
	_, goneTok := e.token(gone, domain.ScopeSMSSend)
	expRec, expTok, err := e.svc.CreateToken(e.ctx, domain.SystemActor,
		NewToken{UserID: op.ID, Name: "exp", Scopes: domain.Scopes{domain.ScopeSMSRead}, ExpiresAt: t0.Add(time.Hour)})
	e.ok(err)
	revRec, revTok := e.token(op, domain.ScopeSMSRead)
	e.ok(e.svc.DeleteToken(e.ctx, domain.SystemActor, revRec.ID))
	e.disable(gone)
	// An operator token whose owner is later demoted to viewer.
	demoted := e.user("demoted", domain.RoleOperator)
	_, demotedTok := e.token(demoted, domain.ScopeSMSSend, domain.ScopeSMSRead)
	v := domain.RoleViewer
	_, err = e.svc.UpdateUser(e.ctx, domain.SystemActor, demoted.ID, UserChanges{Role: &v})
	e.ok(err)
	e.clk.Advance(2 * time.Hour) // expTok expires
	_ = expRec

	wrongSecret := opTok[:len(opTok)-1] + flip(opTok[len(opTok)-1])
	unknownPrefix := "tkr_ZZZZZZZZ_" + opTok[13:]

	tests := []struct {
		name   string
		header string
		scopes []domain.Scope
		status int
		wwwErr string // expected error= in WWW-Authenticate ("" = none)
	}{
		{"no header", "", []domain.Scope{domain.ScopeSMSRead}, 401, ""},
		{"basic scheme", "Basic dXNlcjpwYXNz", []domain.Scope{domain.ScopeSMSRead}, 401, ""},
		{"empty bearer", "Bearer ", []domain.Scope{domain.ScopeSMSRead}, 401, ""},
		{"bad format", "Bearer abc", []domain.Scope{domain.ScopeSMSRead}, 401, "invalid_token"},
		{"unknown prefix", "Bearer " + unknownPrefix, []domain.Scope{domain.ScopeSMSRead}, 401, "invalid_token"},
		{"wrong secret", "Bearer " + wrongSecret, []domain.Scope{domain.ScopeSMSRead}, 401, "invalid_token"},
		{"expired", "Bearer " + expTok, []domain.Scope{domain.ScopeSMSRead}, 401, "invalid_token"},
		{"revoked", "Bearer " + revTok, []domain.Scope{domain.ScopeSMSRead}, 401, "invalid_token"},
		{"disabled owner", "Bearer " + goneTok, []domain.Scope{domain.ScopeSMSSend}, 401, "invalid_token"},
		{"missing scope", "Bearer " + readTok, []domain.Scope{domain.ScopeSMSSend}, 403, "insufficient_scope"},
		{"viewer admin", "Bearer " + viewerTok, []domain.Scope{domain.ScopeAdmin}, 403, "insufficient_scope"},
		{"role narrower than scope", "Bearer " + demotedTok, []domain.Scope{domain.ScopeSMSSend}, 403, "insufficient_scope"},
		{"demoted still reads", "Bearer " + demotedTok, []domain.Scope{domain.ScopeSMSRead}, 204, ""},
		{"ok send", "Bearer " + opTok, []domain.Scope{domain.ScopeSMSSend}, 204, ""},
		{"ok lowercase scheme", "bearer " + opTok, []domain.Scope{domain.ScopeSMSRead}, 204, ""},
		{"ok any of", "Bearer " + readTok, []domain.Scope{domain.ScopeSMSSend, domain.ScopeSMSRead}, 204, ""},
		{"admin implies send", "Bearer " + adminTok, []domain.Scope{domain.ScopeSMSSend}, 204, ""},
		{"admin", "Bearer " + adminTok, []domain.Scope{domain.ScopeAdmin}, 204, ""},
		{"no scope required", "Bearer " + viewerTok, nil, 204, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &okHandler{}
			r := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			e.svc.RequireToken(tc.scopes...)(h).ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			if tc.status == 204 {
				if !h.called || h.p.Token == nil || h.p.User.PasswordHash != "" || h.p.Token.Hash != nil {
					t.Fatalf("principal = %+v", h.p)
				}
				if a := h.p.Actor(); a.Type != domain.ActorToken || a.ID != h.p.Token.ID {
					t.Fatalf("actor = %v", a)
				}
				return
			}
			if h.called {
				t.Fatal("next handler called")
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("Content-Type = %q", ct)
			}
			var p map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			wantType := ProblemUnauthorized
			if tc.status == 403 {
				wantType = ProblemForbidden
			}
			if p["type"] != wantType || p["status"] != float64(tc.status) || p["title"] == "" || p["instance"] != "/api/v1/messages" {
				t.Fatalf("problem = %v", p)
			}
			www := w.Header().Get("WWW-Authenticate")
			if !strings.HasPrefix(www, `Bearer realm="tikker"`) {
				t.Fatalf("WWW-Authenticate = %q", www)
			}
			if tc.wwwErr == "" && strings.Contains(www, "error=") || tc.wwwErr != "" && !strings.Contains(www, `error="`+tc.wwwErr+`"`) {
				t.Fatalf("WWW-Authenticate = %q, want error %q", www, tc.wwwErr)
			}
		})
	}
	e.assertNoSecrets(adminTok, opTok, readTok, viewerTok, goneTok, expTok, revTok, demotedTok, wrongSecret,
		opTok[13:], wrongSecret[13:])
	_ = opTokRec
}

func flip(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}

func TestRequireTokenInvalidScopePanics(t *testing.T) {
	e := newEnv(t)
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	e.svc.RequireToken("sms:everything")
}

func TestTokenTouchCoalesced(t *testing.T) {
	e := newEnv(t)
	u := e.user("admin", domain.RoleAdmin)
	tok, full := e.token(u, domain.ScopeAdmin)
	lastUsed := func() time.Time {
		got, err := e.st.Tokens().GetByPrefix(e.ctx, tok.Prefix)
		e.ok(err)
		return got.LastUsedAt
	}
	use := func() {
		p, err := e.svc.AuthenticateToken(e.ctx, full)
		e.ok(err)
		if p.Token.LastUsedAt.IsZero() {
			t.Fatal("principal LastUsedAt not set")
		}
	}
	use()
	if !lastUsed().Equal(t0) {
		t.Fatalf("first use: %v", lastUsed())
	}
	e.clk.Advance(30 * time.Second)
	use()
	if !lastUsed().Equal(t0) {
		t.Fatalf("touched within the interval: %v", lastUsed())
	}
	e.clk.Advance(31 * time.Second)
	use()
	if !lastUsed().Equal(t0.Add(61 * time.Second)) {
		t.Fatalf("not touched after the interval: %v", lastUsed())
	}
}

// failingStore fails selected calls with an infrastructure error.
type failingStore struct {
	store.Store
	tokens, users, sessions, audit bool
}

var errDB = errors.New("disk I/O error")

func (f *failingStore) Tokens() store.TokenRepo {
	if f.tokens {
		return failTokens{}
	}
	return f.Store.Tokens()
}

func (f *failingStore) Users() store.UserRepo {
	if f.users {
		return failUsers{f.Store.Users()}
	}
	return f.Store.Users()
}

func (f *failingStore) Sessions() store.SessionRepo {
	if f.sessions {
		return failSessions{f.Store.Sessions()}
	}
	return f.Store.Sessions()
}

func (f *failingStore) Audit() store.AuditRepo {
	if f.audit {
		return failAudit{}
	}
	return f.Store.Audit()
}

type failTokens struct{ store.TokenRepo }

func (failTokens) GetByPrefix(context.Context, string) (domain.APIToken, error) {
	return domain.APIToken{}, errDB
}

type failUsers struct{ store.UserRepo }

func (failUsers) Get(context.Context, domain.ID) (domain.User, error) { return domain.User{}, errDB }
func (failUsers) GetByUsername(context.Context, string) (domain.User, error) {
	return domain.User{}, errDB
}
func (failUsers) Count(context.Context) (int, error) { return 0, errDB }

type failSessions struct{ store.SessionRepo }

func (failSessions) Get(context.Context, string) (store.Session, error) {
	return store.Session{}, errDB
}
func (failSessions) DeleteExpired(context.Context, time.Time, time.Time) (int, error) {
	return 0, errDB
}

type failAudit struct{ store.AuditRepo }

func (failAudit) Append(context.Context, domain.AuditLog) error { return errDB }

func TestInfrastructureErrors(t *testing.T) {
	e := newEnv(t)
	u := e.user("admin", domain.RoleAdmin)
	_, full := e.token(u, domain.ScopeAdmin)
	cookie := e.login("admin")
	mk := func(fs *failingStore) *Service {
		fs.Store = e.st
		s := *e.svc
		s.st = fs
		return &s
	}

	t.Run("token lookup", func(t *testing.T) {
		s := mk(&failingStore{tokens: true})
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
		r.Header.Set("Authorization", "Bearer "+full)
		s.RequireToken(domain.ScopeAdmin)(&okHandler{}).ServeHTTP(w, r)
		if w.Code != 500 || !strings.Contains(w.Body.String(), ProblemInternal) {
			t.Fatalf("status %d %s", w.Code, w.Body)
		}
	})
	t.Run("token owner", func(t *testing.T) {
		s := mk(&failingStore{users: true})
		if _, err := s.AuthenticateToken(e.ctx, full); err == nil || errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("session lookup", func(t *testing.T) {
		s := mk(&failingStore{sessions: true})
		w := httptest.NewRecorder()
		s.RequireSession("")(&okHandler{}).ServeHTTP(w, request(http.MethodGet, "/", cookie))
		if w.Code != 500 {
			t.Fatalf("status %d", w.Code)
		}
		if _, err := s.PurgeExpiredSessions(e.ctx); !errors.Is(err, errDB) {
			t.Fatalf("purge = %v", err)
		}
		if err := s.Logout(httptest.NewRecorder(), request(http.MethodPost, "/logout", cookie)); !errors.Is(err, errDB) {
			t.Fatalf("logout = %v", err)
		}
	})
	t.Run("session user", func(t *testing.T) {
		s := mk(&failingStore{users: true})
		if _, err := s.SessionPrincipal(httptest.NewRecorder(), request(http.MethodGet, "/", cookie)); err == nil ||
			errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("login lookup", func(t *testing.T) {
		s := mk(&failingStore{users: true})
		if _, err := s.Login(httptest.NewRecorder(), request(http.MethodPost, "/login"), "admin", goodPassword); !errors.Is(err, errDB) {
			t.Fatalf("err = %v", err)
		}
		if _, err := s.Bootstrap(e.ctx, "x"); !errors.Is(err, errDB) {
			t.Fatalf("bootstrap = %v", err)
		}
		if _, err := s.SetupRequired(e.ctx); !errors.Is(err, errDB) {
			t.Fatalf("setup = %v", err)
		}
		if _, _, err := s.CreateToken(e.ctx, domain.SystemActor, NewToken{UserID: u.ID, Name: "x", Scopes: domain.Scopes{domain.ScopeAdmin}}); !errors.Is(err, errDB) {
			t.Fatalf("create token = %v", err)
		}
	})
	t.Run("failed login audit is best effort", func(t *testing.T) {
		s := mk(&failingStore{audit: true})
		if _, err := s.Login(httptest.NewRecorder(), request(http.MethodPost, "/login"), "admin", "wrong password!"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(e.logs.String(), "audit of failed login failed") {
			t.Fatal("audit failure not logged")
		}
	})
}

func TestSafeRedirect(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/messages?status=failed", "/messages?status=failed"},
		{"/", "/"},
		{"", "/"},
		{"messages", "/"},
		{"//evil.example", "/"},
		{"/\\evil.example", "/"},
		{"https://evil.example/", "/"},
		{"/ok\r\nSet-Cookie: x", "/"},
		{"/%zz", "/"},
	}
	for _, tc := range tests {
		if got := SafeRedirect(tc.in); got != tc.want {
			t.Errorf("SafeRedirect(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWriteRateLimited(t *testing.T) {
	w := httptest.NewRecorder()
	WriteRateLimited(w, httptest.NewRequest(http.MethodPost, "/login", nil), &RateLimitError{RetryAfter: 42 * time.Second})
	if w.Code != 429 || w.Header().Get("Retry-After") != "42" || !strings.Contains(w.Body.String(), ProblemRateLimited) {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
	}
}

func TestPrincipal(t *testing.T) {
	u := domain.User{ID: "01JZZZZZZZZZZZZZZZZZZZZZZA", Role: domain.RoleOperator}
	p := Principal{User: u}
	if p.Actor() != domain.UserActor(u.ID) || !p.Can(domain.ScopeSMSSend) || p.Can(domain.ScopeAdmin) {
		t.Fatalf("session principal %+v", p)
	}
	tok := &domain.APIToken{ID: "01JZZZZZZZZZZZZZZZZZZZZZZB", Scopes: domain.Scopes{domain.ScopeAdmin}}
	p.Token = tok
	if p.Actor() != domain.TokenActor(tok.ID) || !p.Can(domain.ScopeSMSSend) || p.Can(domain.ScopeAdmin) {
		t.Fatalf("token principal %+v", p)
	}
	ctx := WithPrincipal(context.Background(), p)
	if got, ok := PrincipalFrom(ctx); !ok || got.Token != tok {
		t.Fatal("PrincipalFrom")
	}
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("principal in empty context")
	}
}
