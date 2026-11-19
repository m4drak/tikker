// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

func TestNewConfig(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no store", Config{}},
		{"idle > absolute", Config{Store: e.st, SessionIdleTimeout: 2 * time.Hour, SessionAbsoluteTimeout: time.Hour}},
		{"bad base url", Config{Store: e.st, BaseURL: "ftp://x"}},
		{"base url without host", Config{Store: e.st, BaseURL: "https://"}},
		{"bad origin", Config{Store: e.st, TrustedOrigins: []string{"not an origin"}}},
		{"bad limit", Config{Store: e.st, LoginPerIP: Limit{Burst: 0, Every: time.Second}}},
		{"bad params", Config{Store: e.st, Params: Params{Memory: 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// Defaults.
	s, err := New(Config{Store: e.st, Params: testParams, BaseURL: "https://sms.example.org/"})
	if err != nil {
		t.Fatal(err)
	}
	if s.idle != DefaultSessionIdleTimeout || s.absolute != DefaultSessionAbsoluteTimeout || s.touch != DefaultTouchInterval ||
		s.loginPath != DefaultLoginPath || !s.secureCookies || s.hasher == nil || s.Hasher() != s.hasher {
		t.Fatalf("defaults not applied: %+v", s)
	}
	if s.limiter.perIP.limit != DefaultLoginPerIP || s.limiter.perUsr.limit != DefaultLoginPerUser ||
		s.limiter.perIP.max != DefaultMaxLimiterEntries {
		t.Fatal("limiter defaults")
	}
}

func TestBootstrap(t *testing.T) {
	tests := []struct {
		name     string
		existing bool
		password string
		want     BootstrapResult
		wantErr  error
	}{
		{"creates admin", false, "s3cret-bootstrap", BootstrapCreated, nil},
		{"setup required", false, "", BootstrapSetupRequired, nil},
		{"users exist", true, "s3cret-bootstrap", BootstrapNotNeeded, nil},
		{"users exist no password", true, "", BootstrapNotNeeded, nil},
		{"short password", false, "short", 0, ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.existing {
				e.user("someone", domain.RoleViewer)
			}
			got, err := e.svc.Bootstrap(e.ctx, tc.password)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			e.ok(err)
			if got != tc.want {
				t.Fatalf("Bootstrap = %v, want %v", got, tc.want)
			}
			if tc.want != BootstrapCreated {
				return
			}
			u, err := e.st.Users().GetByUsername(e.ctx, BootstrapUsername)
			e.ok(err)
			if u.Role != domain.RoleAdmin || u.Disabled || !strings.HasPrefix(u.PasswordHash, "$argon2id$") {
				t.Fatalf("admin = %+v", u)
			}
			a := e.lastAudit()
			if a.Action != domain.AuditUserCreate || a.Actor != domain.SystemActor || a.Meta["via"] != "bootstrap" {
				t.Fatalf("audit = %+v", a)
			}
			// Idempotent.
			if again, err := e.svc.Bootstrap(e.ctx, tc.password); err != nil || again != BootstrapNotNeeded {
				t.Fatalf("second Bootstrap = %v, %v", again, err)
			}
			if n, _ := e.st.Users().Count(e.ctx); n != 1 {
				t.Fatalf("users = %d", n)
			}
			// The bootstrap password logs in.
			w := newRecorder()
			if _, err := e.svc.Login(w, request("POST", "/login"), "ADMIN", tc.password); err != nil {
				t.Fatalf("login with bootstrap password: %v", err)
			}
			e.assertNoSecrets(tc.password)
		})
	}
	e := newEnv(t)
	for _, r := range []BootstrapResult{BootstrapNotNeeded, BootstrapCreated, BootstrapSetupRequired, 7} {
		if r.String() == "" {
			t.Fatal("empty String")
		}
	}
	_ = e
}

func TestCreateFirstAdmin(t *testing.T) {
	e := newEnv(t)
	req, err := e.svc.SetupRequired(e.ctx)
	if err != nil || !req {
		t.Fatalf("SetupRequired = %v, %v", req, err)
	}
	if _, err := e.svc.CreateFirstAdmin(e.ctx, "x", "short"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid input: %v", err)
	} else {
		var ve *ValidationError
		errors.As(err, &ve)
		if len(ve.Errors) != 2 {
			t.Fatalf("errors = %+v", ve.Errors)
		}
	}

	// Concurrent wizards: exactly one wins.
	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = e.svc.CreateFirstAdmin(e.ctx, "boss"+string(rune('a'+i)), goodPassword)
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrSetupCompleted) || !errors.Is(err, domain.ErrConflict):
			t.Fatalf("loser err = %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d wizards succeeded", wins)
	}
	if req, _ := e.svc.SetupRequired(e.ctx); req {
		t.Fatal("still requires setup")
	}
	a := e.lastAudit()
	if a.Action != domain.AuditUserCreate || a.Actor.Type != domain.ActorUser || a.Target != "user:"+string(a.Actor.ID) {
		t.Fatalf("audit = %+v", a)
	}
	e.assertNoSecrets(goodPassword)
}

func TestCreateUser(t *testing.T) {
	e := newEnv(t)
	admin := domain.UserActor("01JZZZZZZZZZZZZZZZZZZZZZZA")
	tests := []struct {
		name   string
		in     NewUser
		fields []string // expected error fields, nil = ok
	}{
		{"ok", NewUser{Username: "op.1", Password: goodPassword, Role: domain.RoleOperator}, nil},
		{"ok disabled", NewUser{Username: "old_user", Password: goodPassword, Role: domain.RoleViewer, Disabled: true}, nil},
		{"missing all", NewUser{}, []string{"/username", "/password", "/role"}},
		{"bad username", NewUser{Username: "a b", Password: goodPassword, Role: domain.RoleViewer}, []string{"/username"}},
		{"short username", NewUser{Username: "ab", Password: goodPassword, Role: domain.RoleViewer}, []string{"/username"}},
		{"bad role", NewUser{Username: "abc", Password: goodPassword, Role: "root"}, []string{"/role"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := e.svc.CreateUser(e.ctx, admin, tc.in)
			if tc.fields == nil {
				e.ok(err)
				if u.PasswordHash != "" || u.Username != tc.in.Username || u.Disabled != tc.in.Disabled {
					t.Fatalf("user = %+v", u)
				}
				stored, _ := e.st.Users().Get(e.ctx, u.ID)
				if !strings.HasPrefix(stored.PasswordHash, "$argon2id$") {
					t.Fatalf("stored hash %q", stored.PasswordHash)
				}
				a := e.lastAudit()
				if a.Action != domain.AuditUserCreate || a.Actor != admin || a.Target != "user:"+string(u.ID) {
					t.Fatalf("audit = %+v", a)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || len(ve.Errors) != len(tc.fields) {
				t.Fatalf("err = %v", err)
			}
			for i, f := range tc.fields {
				if ve.Errors[i].Field != f {
					t.Fatalf("field %d = %q, want %q", i, ve.Errors[i].Field, f)
				}
			}
		})
	}
	if _, err := e.svc.CreateUser(e.ctx, admin, NewUser{Username: "OP.1", Password: goodPassword, Role: domain.RoleViewer}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	e.assertNoSecrets(goodPassword)
}

func TestUpdateUser(t *testing.T) {
	e := newEnv(t)
	admin := e.user("admin", domain.RoleAdmin)
	op := e.user("opr", domain.RoleOperator)
	actor := domain.UserActor(admin.ID)
	newPw := "brand new password"
	viewer, operator, adminRole := domain.RoleViewer, domain.RoleOperator, domain.RoleAdmin
	yes, no := true, false
	short := "short"
	passwords := map[domain.ID]string{admin.ID: goodPassword, op.ID: goodPassword}

	tests := []struct {
		name     string
		id       domain.ID
		ch       UserChanges
		err      error
		endsSess bool
		wantMeta map[string]string
		wantRole domain.Role
	}{
		{"no changes", op.ID, UserChanges{}, nil, false, nil, domain.RoleOperator},
		{"same role", op.ID, UserChanges{Role: &operator, Disabled: &no}, nil, false, nil, domain.RoleOperator},
		{"password", op.ID, UserChanges{Password: &newPw}, nil, true, map[string]string{"password": "changed"}, domain.RoleOperator},
		{"role", op.ID, UserChanges{Role: &viewer}, nil, true, map[string]string{"role": "operator->viewer"}, domain.RoleViewer},
		{"bad password", op.ID, UserChanges{Password: &short}, ErrValidation, false, nil, ""},
		{"bad role", op.ID, UserChanges{Role: new(domain.Role)}, ErrValidation, false, nil, ""},
		{"unknown", "01JZZZZZZZZZZZZZZZZZZZZZZB", UserChanges{Role: &viewer}, domain.ErrNotFound, false, nil, ""},
		{"last admin demote", admin.ID, UserChanges{Role: &viewer}, domain.ErrConflict, false, nil, ""},
		{"last admin disable", admin.ID, UserChanges{Disabled: &yes}, domain.ErrConflict, false, nil, ""},
		{"promote", op.ID, UserChanges{Role: &adminRole}, nil, true, map[string]string{"role": "viewer->admin"}, domain.RoleAdmin},
		{"disable", op.ID, UserChanges{Disabled: &yes}, nil, true, map[string]string{"disabled": "true"}, domain.RoleAdmin},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Each case starts with a live session of the target user.
			var cookie *http.Cookie
			if u, err := e.st.Users().Get(e.ctx, tc.id); err == nil {
				w := newRecorder()
				_, err := e.svc.Login(w, request("POST", "/login"), u.Username, passwords[tc.id])
				e.ok(err)
				cookie = sessionCookie(t, w)
			}
			before := len(e.audits())
			u, err := e.svc.UpdateUser(e.ctx, actor, tc.id, tc.ch)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				if len(e.audits()) != before {
					t.Fatal("failed update audited")
				}
				if cookie != nil {
					if _, err := e.svc.SessionPrincipal(newRecorder(), request("GET", "/", cookie)); err != nil {
						t.Fatalf("session ended by failed update: %v", err)
					}
				}
				return
			}
			e.ok(err)
			if u.PasswordHash != "" || u.Role != tc.wantRole {
				t.Fatalf("user = %+v", u)
			}
			if tc.ch.Password != nil {
				passwords[tc.id] = *tc.ch.Password
			}
			_, err = e.svc.SessionPrincipal(newRecorder(), request("GET", "/", cookie))
			if ended := errors.Is(err, ErrUnauthenticated); ended != tc.endsSess {
				t.Fatalf("session ended = %v, want %v (%v)", ended, tc.endsSess, err)
			}
			audits := e.audits()
			if tc.wantMeta == nil {
				if len(audits) != before {
					t.Fatal("no-op update audited")
				}
				return
			}
			a := audits[len(audits)-1]
			if a.Action != domain.AuditUserUpdate || a.Actor != actor || a.Target != "user:"+string(tc.id) ||
				a.Meta["sessions_ended"] == "" {
				t.Fatalf("audit = %+v", a)
			}
			for k, v := range tc.wantMeta {
				if a.Meta[k] != v {
					t.Fatalf("audit meta = %v, want %s=%s", a.Meta, k, v)
				}
			}
		})
	}
	// The disabled user cannot log in and gets the generic error.
	if _, err := e.svc.Login(newRecorder(), request("POST", "/login"), "opr", newPw); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled login = %v", err)
	}
	e.assertNoSecrets(goodPassword, newPw)
}

func TestCreateToken(t *testing.T) {
	e := newEnv(t)
	admin := e.user("admin", domain.RoleAdmin)
	op := e.user("opr", domain.RoleOperator)
	viewer := e.user("viewer", domain.RoleViewer)
	actor := domain.UserActor(admin.ID)
	send, read, adm := domain.ScopeSMSSend, domain.ScopeSMSRead, domain.ScopeAdmin

	tests := []struct {
		name   string
		in     NewToken
		fields map[string]string // field -> code; nil = ok
	}{
		{"admin all", NewToken{UserID: admin.ID, Name: "ci", Scopes: domain.Scopes{adm}}, nil},
		{"operator send", NewToken{UserID: op.ID, Name: "crm", Scopes: domain.Scopes{send, read}, ExpiresAt: t0.Add(time.Hour)}, nil},
		{"viewer read", NewToken{UserID: viewer.ID, Name: "ro", Scopes: domain.Scopes{read}}, nil},
		{"viewer send", NewToken{UserID: viewer.ID, Name: "x", Scopes: domain.Scopes{read, send}},
			map[string]string{"/scopes/1": CodeScopeExceedsRole}},
		{"operator admin", NewToken{UserID: op.ID, Name: "x", Scopes: domain.Scopes{adm}},
			map[string]string{"/scopes/0": CodeScopeExceedsRole}},
		{"unknown scope", NewToken{UserID: op.ID, Name: "x", Scopes: domain.Scopes{"sms:all"}},
			map[string]string{"/scopes/0": CodeInvalidFormat}},
		{"duplicate scope", NewToken{UserID: op.ID, Name: "x", Scopes: domain.Scopes{read, read}},
			map[string]string{"/scopes/1": CodeInvalidFormat}},
		{"no scopes", NewToken{UserID: op.ID, Name: "x"}, map[string]string{"/scopes": CodeRequired}},
		{"no name", NewToken{UserID: op.ID, Name: "  ", Scopes: domain.Scopes{read}}, map[string]string{"/name": CodeRequired}},
		{"long name", NewToken{UserID: op.ID, Name: strings.Repeat("n", 65), Scopes: domain.Scopes{read}},
			map[string]string{"/name": CodeOutOfRange}},
		{"past expiry", NewToken{UserID: op.ID, Name: "x", Scopes: domain.Scopes{read}, ExpiresAt: t0},
			map[string]string{"/expires_at": CodeExpiresAtInPast}},
		{"no owner", NewToken{Name: "x", Scopes: domain.Scopes{read}}, map[string]string{"/user_id": CodeRequired}},
		{"unknown owner", NewToken{UserID: "01JZZZZZZZZZZZZZZZZZZZZZZB", Name: "x", Scopes: domain.Scopes{read}},
			map[string]string{"/user_id": CodeUnknownUser}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok, full, err := e.svc.CreateToken(e.ctx, actor, tc.in)
			if tc.fields == nil {
				e.ok(err)
				prefix, _, perr := ParseToken(full)
				if perr != nil || prefix != tok.Prefix || len(tok.Hash) != 32 || tok.UserID != tc.in.UserID {
					t.Fatalf("token %+v / %q", tok, full)
				}
				p, err := e.svc.AuthenticateToken(e.ctx, full)
				e.ok(err)
				if p.Token.ID != tok.ID {
					t.Fatalf("principal = %+v", p)
				}
				a := e.lastAudit()
				if a.Action != domain.AuditTokenCreate || a.Actor != actor || a.Target != "token:"+string(tok.ID) ||
					a.Meta["prefix"] != tok.Prefix || a.Meta["name"] != tc.in.Name {
					t.Fatalf("audit = %+v", a)
				}
				if strings.Contains(e.logs.String(), full) || strings.Contains(strings.Join(mapValues(a.Meta), " "), full[13:]) {
					t.Fatal("token secret leaked to logs or audit")
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v", err)
			}
			got := map[string]string{}
			for _, f := range ve.Errors {
				got[f.Field] = f.Code
			}
			for f, c := range tc.fields {
				if got[f] != c {
					t.Fatalf("errors = %+v, want %s=%s", ve.Errors, f, c)
				}
			}
		})
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// conflictStore makes the first token insert fail with a prefix conflict.
type conflictStore struct {
	store.Store
	mu    sync.Mutex
	fails int
}

func (c *conflictStore) WithTx(ctx context.Context, fn func(store.Repos) error) error {
	return c.Store.WithTx(ctx, func(tx store.Repos) error { return fn(&conflictRepos{Repos: tx, c: c}) })
}

type conflictRepos struct {
	store.Repos
	c *conflictStore
}

func (r *conflictRepos) Tokens() store.TokenRepo {
	return &conflictTokens{TokenRepo: r.Repos.Tokens(), c: r.c}
}

type conflictTokens struct {
	store.TokenRepo
	c *conflictStore
}

func (t *conflictTokens) Create(ctx context.Context, tok domain.APIToken) error {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.c.fails > 0 {
		t.c.fails--
		return domain.ErrConflict
	}
	return t.TokenRepo.Create(ctx, tok)
}

func TestCreateTokenPrefixCollision(t *testing.T) {
	base := newEnv(t)
	admin := base.user("admin", domain.RoleAdmin)
	cs := &conflictStore{Store: base.st, fails: 2}
	svc, err := New(Config{Store: cs, Clock: base.clk, Params: testParams, Logger: slog.New(slog.DiscardHandler)})
	base.ok(err)
	in := NewToken{UserID: admin.ID, Name: "x", Scopes: domain.Scopes{domain.ScopeAdmin}}
	if _, _, err := svc.CreateToken(base.ctx, domain.SystemActor, in); err != nil {
		t.Fatalf("retry after collisions failed: %v", err)
	}
	cs.fails = 3
	if _, _, err := svc.CreateToken(base.ctx, domain.SystemActor, in); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("persistent collision = %v", err)
	}
	svc.rand = failReader{}
	if _, _, err := svc.CreateToken(base.ctx, domain.SystemActor, in); err == nil {
		t.Fatal("token without entropy")
	}
}

func TestDeleteToken(t *testing.T) {
	e := newEnv(t)
	admin := e.user("admin", domain.RoleAdmin)
	tok, full := e.token(admin, domain.ScopeAdmin)
	if _, err := e.svc.AuthenticateToken(e.ctx, full); err != nil {
		t.Fatal(err)
	}
	actor := domain.UserActor(admin.ID)
	e.ok(e.svc.DeleteToken(e.ctx, actor, tok.ID))
	if _, err := e.svc.AuthenticateToken(e.ctx, full); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token = %v", err)
	}
	a := e.lastAudit()
	if a.Action != domain.AuditTokenDelete || a.Actor != actor || a.Target != "token:"+string(tok.ID) {
		t.Fatalf("audit = %+v", a)
	}
	if err := e.svc.DeleteToken(e.ctx, actor, tok.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

func TestRunPurgesSessions(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.JanitorInterval = time.Minute })
	e.user("alice", domain.RoleViewer)
	cookie := e.login("alice")
	e.ok(e.svc.limiterSizeNonZero())

	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { e.svc.Run(ctx); close(done) }()
	e.clk.BlockUntil(1)
	e.clk.Advance(DefaultSessionIdleTimeout + time.Minute) // idle expiry
	e.clk.BlockUntil(1)
	cancel()
	<-done
	if _, err := e.st.Sessions().Get(e.ctx, sessionID(cookie.Value)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired session not purged: %v", err)
	}
	if ips, users := e.svc.limiter.size(); ips != 0 || users != 0 {
		t.Fatalf("limiter not swept: %d %d", ips, users)
	}
}

// limiterSizeNonZero is a test hook: after a successful login only the IP
// refund happens, so make one failed attempt to have state to sweep.
func (s *Service) limiterSizeNonZero() error {
	if _, ok := s.limiter.allow("198.51.100.1", "x", s.clk.Now()); !ok {
		return errors.New("limiter refused")
	}
	return nil
}
