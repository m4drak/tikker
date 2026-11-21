// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

func TestLoginCookie(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		tls        bool
		wantName   string
		wantSecure bool
	}{
		{"plain http", "", false, CookieName, false},
		{"http base url", "http://sms.lan:8080", false, CookieName, false},
		{"https base url behind proxy", "https://sms.example.org", false, SecureCookieName, true},
		{"built-in tls", "", true, SecureCookieName, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(c *Config) { c.BaseURL = tc.baseURL })
			u := e.user("alice", domain.RoleOperator)
			r := request(http.MethodPost, "/login")
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			w := httptest.NewRecorder()
			p, err := e.svc.Login(w, r, "Alice", goodPassword) // usernames are case-insensitive
			e.ok(err)
			c := sessionCookie(t, w)
			if c.Name != tc.wantName || c.Secure != tc.wantSecure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode ||
				c.Path != "/" || c.Domain != "" || c.MaxAge != int(DefaultSessionAbsoluteTimeout/time.Second) ||
				len(c.Value) != sessionTokenLen {
				t.Fatalf("cookie = %+v", c)
			}
			if p.User.ID != u.ID || p.User.PasswordHash != "" || p.Session == nil || p.CSRFToken == "" || p.Token != nil {
				t.Fatalf("principal = %+v", p)
			}
			if p.Actor() != domain.UserActor(u.ID) {
				t.Fatalf("actor = %v", p.Actor())
			}
			// Only the hash of the cookie is stored.
			sess, err := e.st.Sessions().Get(e.ctx, sessionID(c.Value))
			e.ok(err)
			if sess.ID == c.Value || sess.UserID != u.ID || !sess.ExpiresAt.Equal(t0.Add(DefaultSessionAbsoluteTimeout)) ||
				sess.IP != "192.0.2.0/24" {
				t.Fatalf("stored session = %+v", sess)
			}
			if _, err := e.st.Sessions().Get(e.ctx, c.Value); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("raw cookie value usable as session ID")
			}
			// The cookie authenticates the next request (same scheme).
			r2 := request(http.MethodGet, "/", c)
			r2.TLS = r.TLS
			got, err := e.svc.SessionPrincipal(httptest.NewRecorder(), r2)
			e.ok(err)
			if got.User.ID != u.ID || got.CSRFToken != p.CSRFToken || e.svc.CSRFToken(r2) != p.CSRFToken {
				t.Fatalf("resolved = %+v", got)
			}
			a := e.lastAudit()
			if a.Action != domain.AuditLoginSuccess || a.Actor != domain.UserActor(u.ID) || a.Meta["ip"] != "192.0.2.10" {
				t.Fatalf("audit = %+v", a)
			}
			e.assertNoSecrets(goodPassword, c.Value)
		})
	}
}

func TestLoginFailures(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.LoginPerUser = Limit{Burst: 100, Every: time.Second}
		c.LoginPerIP = Limit{Burst: 100, Every: time.Second}
	})
	u := e.user("alice", domain.RoleOperator)
	off := e.user("bob", domain.RoleOperator)
	e.user("keeper", domain.RoleAdmin)
	e.disable(off)
	// A user whose stored hash is corrupt.
	broken := e.user("carol", domain.RoleViewer)
	bu, _ := e.st.Users().Get(e.ctx, broken.ID)
	bu.PasswordHash = "$2a$10$notargon"
	e.ok(e.st.Users().Update(e.ctx, bu))

	tests := []struct {
		name, user, pw, reason string
		target                 domain.ID
	}{
		{"unknown user", "mallory", goodPassword, "unknown_user", ""},
		{"wrong password", "alice", "wrong password!!", "bad_password", u.ID},
		{"empty password", "alice", "", "bad_password", u.ID},
		{"disabled", "bob", goodPassword, "disabled", off.ID},
		{"corrupt hash", "carol", goodPassword, "bad_hash", broken.ID},
		{"empty username", "", goodPassword, "unknown_user", ""},
		{"password in username field", "my secret pw!", "x", "unknown_user", ""},
		{"huge password", "alice", strings.Repeat("p", 10*MaxPasswordLen), "bad_password", u.ID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			p, err := e.svc.Login(w, request(http.MethodPost, "/login"), tc.user, tc.pw)
			if !errors.Is(err, ErrInvalidCredentials) || err.Error() != ErrInvalidCredentials.Error() {
				t.Fatalf("err = %v", err)
			}
			if p.Session != nil || len(w.Result().Cookies()) != 0 {
				t.Fatal("session started")
			}
			a := e.lastAudit()
			if a.Action != domain.AuditLoginFailure || a.Actor != domain.SystemActor || a.Meta["reason"] != tc.reason ||
				a.Meta["ip"] != "192.0.2.10" {
				t.Fatalf("audit = %+v", a)
			}
			if tc.target != "" && a.Target != "user:"+string(tc.target) || tc.target == "" && a.Target != "" {
				t.Fatalf("audit target = %q", a.Target)
			}
			if safeUsername(tc.user) == "" && a.Meta["username"] != "" {
				t.Fatalf("non-username stored: %+v", a.Meta)
			}
		})
	}
	e.assertNoSecrets(goodPassword, "wrong password!!", "my secret pw!")
	if !strings.Contains(e.logs.String(), "stored password hash is malformed") {
		t.Fatal("corrupt hash not reported")
	}
}

// TestLoginEqualWork checks that an unknown username costs one argon2id
// verification, like a wrong password: both block on the hashing slot.
func TestLoginEqualWork(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.HashConcurrency = 1 })
	e.user("alice", domain.RoleOperator)
	for _, name := range []string{"alice", "nobody"} {
		e.svc.hasher.sem <- struct{}{} // saturate hashing
		r := request(http.MethodPost, "/login")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := e.svc.Login(httptest.NewRecorder(), r.WithContext(ctx), name, "wrong password!!")
		cancel()
		<-e.svc.hasher.sem
		if errors.Is(err, ErrInvalidCredentials) || err == nil {
			t.Fatalf("%s: login did not wait for hashing: %v", name, err)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.LoginPerUser = Limit{Burst: 3, Every: time.Minute}
		c.LoginPerIP = Limit{Burst: 5, Every: 10 * time.Second}
	})
	e.user("alice", domain.RoleOperator)
	e.user("bobby", domain.RoleOperator)
	try := func(ip, user, pw string) error {
		r := request(http.MethodPost, "/login")
		r.RemoteAddr = ip + ":1234"
		_, err := e.svc.Login(httptest.NewRecorder(), r, user, pw)
		return err
	}
	// Per username: 3 failures, then 429 even with the right password and
	// from another IP.
	for i := 0; i < 3; i++ {
		if err := try("198.51.100.1", "alice", "bad password!!"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	audits := len(e.audits())
	err := try("198.51.100.2", "ALICE", goodPassword)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != time.Minute || rl.RetryAfterSeconds() != 60 {
		t.Fatalf("4th attempt = %v", err)
	}
	if len(e.audits()) != audits {
		t.Fatal("rate-limited attempt written to the audit log")
	}
	e.clk.Advance(time.Minute)
	if err := try("198.51.100.2", "alice", goodPassword); err != nil {
		t.Fatalf("after waiting: %v", err)
	}
	// Success reset alice's bucket.
	for i := 0; i < 3; i++ {
		if err := try("198.51.100.3", "alice", "bad password!!"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("after reset %d: %v", i, err)
		}
	}

	// Per IP: spraying usernames from one address.
	for i, name := range []string{"u1x", "u2x", "u3x", "u4x", "u5x"} {
		if err := try("203.0.113.9", name, "bad password!!"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("spray %d: %v", i, err)
		}
	}
	if err := try("203.0.113.9", "bobby", goodPassword); !errors.As(err, &rl) || rl.RetryAfterSeconds() != 10 {
		t.Fatalf("spray limit = %v", err)
	}
	// Another address in the same IPv6 /64 shares the limit.
	for i := 0; i < 5; i++ {
		_ = try("2001:db8::1", "v6user"+string(rune('a'+i)), "bad password!!")
	}
	if err := try("2001:db8::ffff", "bobby", goodPassword); !errors.As(err, &rl) {
		t.Fatalf("ipv6 /64 limit = %v", err)
	}
	if err := try("2001:db8:0:1::1", "bobby", goodPassword); err != nil {
		t.Fatalf("other /64: %v", err)
	}
	e.assertNoSecrets(goodPassword, "bad password!!")
}

func TestSessionRotationAndLogout(t *testing.T) {
	e := newEnv(t)
	alice := e.user("alice", domain.RoleOperator)
	e.user("bobby", domain.RoleViewer)
	first := e.login("alice")

	// Logging in again with the old cookie rotates the session ID.
	w := httptest.NewRecorder()
	_, err := e.svc.Login(w, request(http.MethodPost, "/login", first), "bobby", goodPassword)
	e.ok(err)
	second := sessionCookie(t, w)
	if second.Value == first.Value {
		t.Fatal("session ID not rotated")
	}
	if _, err := e.svc.SessionPrincipal(httptest.NewRecorder(), request(http.MethodGet, "/", first)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old session still valid: %v", err)
	}
	// StartSession rotates too (e.g. after a password change).
	w = httptest.NewRecorder()
	p, err := e.svc.StartSession(w, request(http.MethodPost, "/account", second), alice)
	e.ok(err)
	third := sessionCookie(t, w)
	if p.User.ID != alice.ID || third.Value == second.Value {
		t.Fatal("StartSession")
	}
	if _, err := e.svc.SessionPrincipal(httptest.NewRecorder(), request(http.MethodGet, "/", second)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("session before StartSession still valid")
	}

	// Logout.
	w = httptest.NewRecorder()
	e.ok(e.svc.Logout(w, request(http.MethodPost, "/logout", third)))
	if c := sessionCookie(t, w); c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("logout cookie = %+v", c)
	}
	if _, err := e.svc.SessionPrincipal(httptest.NewRecorder(), request(http.MethodGet, "/", third)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("session valid after logout")
	}
	a := e.lastAudit()
	if a.Action != domain.AuditLogout || a.Actor != domain.UserActor(alice.ID) {
		t.Fatalf("audit = %+v", a)
	}
	// Logout without or with a stale session is a no-op.
	e.ok(e.svc.Logout(httptest.NewRecorder(), request(http.MethodPost, "/logout")))
	e.ok(e.svc.Logout(httptest.NewRecorder(), request(http.MethodPost, "/logout", third)))

	e.svc.rand = failReader{}
	if _, err := e.svc.StartSession(httptest.NewRecorder(), request(http.MethodPost, "/"), alice); err == nil {
		t.Fatal("session without entropy")
	}
}

func TestSessionTimeouts(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.SessionIdleTimeout = 30 * time.Minute
		c.SessionAbsoluteTimeout = 2 * time.Hour
	})
	e.user("alice", domain.RoleOperator)
	resolve := func(c *http.Cookie) error {
		_, err := e.svc.SessionPrincipal(httptest.NewRecorder(), request(http.MethodGet, "/", c))
		return err
	}

	// Idle timeout.
	c := e.login("alice")
	e.clk.Advance(29 * time.Minute)
	e.ok(resolve(c)) // touches last_seen_at
	e.clk.Advance(29 * time.Minute)
	e.ok(resolve(c)) // 58 min after login, 29 min idle
	e.clk.Advance(30 * time.Minute)
	if err := resolve(c); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("idle session = %v", err)
	}
	if _, err := e.st.Sessions().Get(e.ctx, sessionID(c.Value)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("idle session not deleted")
	}

	// Absolute timeout despite activity.
	c = e.login("alice")
	for i := 0; i < 7; i++ {
		e.clk.Advance(17 * time.Minute)
		e.ok(resolve(c))
	}
	e.clk.Advance(2*time.Hour - 7*17*time.Minute)
	if err := resolve(c); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("session past absolute timeout = %v", err)
	}

	// Purge removes what nobody resolved.
	c1 := e.login("alice")
	e.clk.Advance(31 * time.Minute)
	n, err := e.svc.PurgeExpiredSessions(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if err := resolve(c1); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("purged session valid")
	}
}

func TestSessionTouchCoalesced(t *testing.T) {
	e := newEnv(t)
	e.user("alice", domain.RoleOperator)
	c := e.login("alice")
	seen := func() time.Time {
		s, err := e.st.Sessions().Get(e.ctx, sessionID(c.Value))
		e.ok(err)
		return s.LastSeenAt
	}
	resolve := func() {
		_, err := e.svc.SessionPrincipal(httptest.NewRecorder(), request(http.MethodGet, "/", c))
		e.ok(err)
	}
	for i := 0; i < 5; i++ {
		e.clk.Advance(10 * time.Second)
		resolve()
	}
	if !seen().Equal(t0) {
		t.Fatalf("last_seen_at written within a minute: %v", seen())
	}
	e.clk.Advance(10 * time.Second) // 60 s
	resolve()
	if !seen().Equal(t0.Add(time.Minute)) {
		t.Fatalf("last_seen_at = %v", seen())
	}
}

func TestSessionInvalidCookies(t *testing.T) {
	e := newEnv(t)
	e.user("alice", domain.RoleOperator)
	valid := e.login("alice")
	tests := []struct {
		name  string
		value string
		clear bool // a Set-Cookie clearing it is expected
	}{
		{"garbage", "garbage", true},
		{"right length bad base64", strings.Repeat("!", sessionTokenLen), true},
		{"unknown token", strings.Repeat("A", sessionTokenLen), true},
		{"hash of real session", sessionID(valid.Value)[:sessionTokenLen], true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			_, err := e.svc.SessionPrincipal(w, request(http.MethodGet, "/", &http.Cookie{Name: CookieName, Value: tc.value}))
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v", err)
			}
			if cleared := len(w.Result().Cookies()) == 1; cleared != tc.clear {
				t.Fatalf("cleared = %v", cleared)
			}
		})
	}
	// No cookie: nothing to clear.
	w := httptest.NewRecorder()
	if _, err := e.svc.SessionPrincipal(w, request(http.MethodGet, "/")); !errors.Is(err, ErrUnauthenticated) || len(w.Result().Cookies()) != 0 {
		t.Fatalf("no cookie: %v", err)
	}
	// The secure cookie name is not accepted over plain HTTP and vice versa.
	if _, err := e.svc.SessionPrincipal(httptest.NewRecorder(),
		request(http.MethodGet, "/", &http.Cookie{Name: SecureCookieName, Value: valid.Value})); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("__Host- cookie accepted over http")
	}
}

func TestSessionEndsWhenUserDisabled(t *testing.T) {
	e := newEnv(t)
	e.user("admin", domain.RoleAdmin)
	bob := e.user("bobby", domain.RoleOperator)
	c := e.login("bobby")
	// Disable directly in the store (as another instance might): the next
	// request notices it.
	u, _ := e.st.Users().Get(e.ctx, bob.ID)
	u.Disabled = true
	e.ok(e.st.Users().Update(e.ctx, u))
	if _, err := e.svc.SessionPrincipal(httptest.NewRecorder(), request(http.MethodGet, "/", c)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disabled user session = %v", err)
	}
	if l, _ := e.st.Sessions().List(e.ctx, bob.ID); len(l) != 0 {
		t.Fatal("session of disabled user kept")
	}
}

func TestSessionDeletedConcurrently(t *testing.T) {
	e := newEnv(t)
	e.user("alice", domain.RoleOperator)
	c := e.login("alice")
	e.clk.Advance(2 * time.Minute) // a touch is due
	sess, _ := e.st.Sessions().Get(e.ctx, sessionID(c.Value))
	s := *e.svc
	s.st = &racyStore{Store: e.st, sess: sess}
	if _, err := s.SessionPrincipal(httptest.NewRecorder(), request(http.MethodGet, "/", c)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
}

// racyStore returns a session from Get that is already gone.
type racyStore struct {
	store.Store
	sess store.Session
}

func (r *racyStore) Sessions() store.SessionRepo { return racySessions{r.Store.Sessions(), r} }

type racySessions struct {
	store.SessionRepo
	r *racyStore
}

func (s racySessions) Get(ctx context.Context, id string) (store.Session, error) {
	_ = s.Delete(ctx, id)
	return s.r.sess, nil
}

func TestRequireSession(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LoginPath = "/signin" })
	e.user("admin", domain.RoleAdmin)
	e.user("viewer", domain.RoleViewer)
	adminC, viewerC := e.login("admin"), e.login("viewer")

	tests := []struct {
		name     string
		method   string
		cookie   *http.Cookie
		htmx     bool
		role     domain.Role
		status   int
		location string
		hxRedir  string
	}{
		{"anonymous navigation", http.MethodGet, nil, false, "", 303, "/signin?next=%2Fmessages%3Fstatus%3Dfailed", ""},
		{"anonymous htmx", http.MethodGet, nil, true, "", 401, "", "/signin"},
		{"anonymous post", http.MethodPost, nil, false, "", 401, "", ""},
		{"viewer any", http.MethodGet, viewerC, false, "", 204, "", ""},
		{"viewer viewer", http.MethodGet, viewerC, false, domain.RoleViewer, 204, "", ""},
		{"viewer operator page", http.MethodGet, viewerC, false, domain.RoleOperator, 403, "", ""},
		{"viewer admin htmx", http.MethodPost, viewerC, true, domain.RoleAdmin, 403, "", ""},
		{"admin operator page", http.MethodGet, adminC, false, domain.RoleOperator, 204, "", ""},
		{"admin admin page", http.MethodGet, adminC, false, domain.RoleAdmin, 204, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cookies []*http.Cookie
			if tc.cookie != nil {
				cookies = append(cookies, tc.cookie)
			}
			r := request(tc.method, "/messages?status=failed", cookies...)
			if tc.htmx {
				r.Header.Set("HX-Request", "true")
			}
			h := &okHandler{}
			w := httptest.NewRecorder()
			e.svc.RequireSession(tc.role)(h).ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Location") != tc.location || w.Header().Get("HX-Redirect") != tc.hxRedir {
				t.Fatalf("status %d, Location %q, HX-Redirect %q", w.Code, w.Header().Get("Location"), w.Header().Get("HX-Redirect"))
			}
			if h.called != (tc.status == 204) {
				t.Fatalf("handler called = %v", h.called)
			}
			if h.called && (h.p.Session == nil || h.p.CSRFToken == "" || w.Header().Get("Cache-Control") != "no-store") {
				t.Fatalf("principal = %+v", h.p)
			}
		})
	}
	defer func() {
		if recover() == nil {
			t.Fatal("invalid role accepted")
		}
	}()
	e.svc.RequireSession("root")
}

func TestClientIP(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	})
	tests := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"direct", "192.0.2.1:1234", nil, "192.0.2.1"},
		{"direct ignores xff", "192.0.2.1:1234", []string{"198.51.100.7"}, "192.0.2.1"},
		{"proxy", "10.1.1.1:1234", []string{"198.51.100.7"}, "198.51.100.7"},
		{"proxy chain", "10.1.1.1:1234", []string{"203.0.113.5, 198.51.100.7, 10.2.2.2"}, "198.51.100.7"},
		{"proxy spoof attempt", "10.1.1.1:1234", []string{"1.2.3.4", "198.51.100.7"}, "198.51.100.7"},
		{"proxy bad xff", "10.1.1.1:1234", []string{"garbage"}, "10.1.1.1"},
		{"proxy no xff", "10.1.1.1:1234", nil, "10.1.1.1"},
		{"ipv6 proxy", "[::1]:80", []string{"2001:db8::5"}, "2001:db8::5"},
		{"mapped", "[::ffff:192.0.2.1]:1", nil, "192.0.2.1"},
		{"bad remote", "nonsense", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := request(http.MethodGet, "/")
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := e.svc.ClientIP(r); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
	if networkOf("2001:db8:1:2::5") != "2001:db8:1::/48" || networkOf("") != "" {
		t.Fatal("networkOf")
	}
}

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"zażółć", 3, "za"}, // "ż" is two bytes
		{"a\xffb", 5, "ab"},
	}
	for _, tc := range tests {
		if got := truncateUTF8(tc.in, tc.n); got != tc.want {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestRehashOnLogin(t *testing.T) {
	e := newEnv(t)
	u := e.user("alice", domain.RoleOperator)
	old, _ := e.st.Users().Get(e.ctx, u.ID)
	stronger := Params{Memory: 128, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}
	s2, err := New(Config{Store: e.st, Clock: e.clk, Params: stronger, Logger: e.svc.log})
	e.ok(err)
	_, err = s2.Login(httptest.NewRecorder(), request(http.MethodPost, "/login"), "alice", goodPassword)
	e.ok(err)
	now, _ := e.st.Users().Get(e.ctx, u.ID)
	if now.PasswordHash == old.PasswordHash || !strings.Contains(now.PasswordHash, "m=128,t=2,p=1") {
		t.Fatalf("not rehashed: %s", now.PasswordHash)
	}
	// Both services still accept the password.
	for _, s := range []*Service{e.svc, s2} {
		if _, err := s.Login(httptest.NewRecorder(), request(http.MethodPost, "/login"), "alice", goodPassword); err != nil {
			t.Fatal(err)
		}
	}
	// A failing rehash is logged and the login still succeeds.
	s3, err := New(Config{Store: e.st, Clock: e.clk, Params: testParams, Logger: e.svc.log})
	e.ok(err)
	s3.hasher.rand = failReader{}
	if _, err := s3.Login(httptest.NewRecorder(), request(http.MethodPost, "/login"), "alice", goodPassword); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.logs.String(), "password rehash failed") {
		t.Fatal("rehash failure not logged")
	}
}
