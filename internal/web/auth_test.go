// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/auth"
)

func TestLoginPage(t *testing.T) {
	e := newEnv(t, allRoles)
	w := e.do(http.MethodGet, "/login?next=/messages", nil, withHeader("Accept-Language", "en"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`<html lang="en">`,
		`<label for="username">Username</label>`,
		`<label for="password">Password</label>`,
		`name="next" value="/messages"`,
		`autocomplete="current-password"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page lacks %q", want)
		}
	}
	if strings.Contains(body, "aria-invalid") {
		t.Error("fresh login page marks fields invalid")
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}

func TestLogin(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		next     string
		status   int
		location string
		errText  string
	}{
		{name: "ok", username: "viewer1", password: goodPassword, status: 303, location: "/"},
		{name: "ok with next", username: "admin1", password: goodPassword, next: "/devices", status: 303, location: "/devices"},
		{name: "wrong password", username: "viewer1", password: "wrong wrong wrong", status: 401, errText: "Invalid username or password."},
		{name: "unknown user", username: "nobody", password: goodPassword, status: 401, errText: "Invalid username or password."},
		{name: "empty", username: "", password: "", status: 401, errText: "Invalid username or password."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, allRoles)
			w := e.do(http.MethodPost, "/login",
				url.Values{"username": {tt.username}, "password": {tt.password}, "next": {tt.next}})
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d\n%s", w.Code, tt.status, w.Body)
			}
			if tt.status == http.StatusSeeOther {
				if got := w.Header().Get("Location"); got != tt.location {
					t.Errorf("Location = %q, want %q", got, tt.location)
				}
				if cookieNamed(w, auth.CookieName) == nil {
					t.Error("no session cookie")
				}
				return
			}
			if cookieNamed(w, auth.CookieName) != nil {
				t.Error("session cookie set on failure")
			}
			body := w.Body.String()
			if !strings.Contains(body, tt.errText) {
				t.Errorf("no error text %q in\n%s", tt.errText, body)
			}
			// The error is linked to the fields and announced.
			for _, want := range []string{`role="alert"`, `aria-invalid="true"`, `aria-describedby="login-error"`, `id="login-error"`} {
				if !strings.Contains(body, want) {
					t.Errorf("error page lacks %q", want)
				}
			}
			if strings.Contains(body, tt.password) && tt.password != "" {
				t.Error("password echoed in the page")
			}
		})
	}
}

// TestLoginErrorsAreIndistinguishable: unknown user and wrong password give
// the same page (apart from the echoed username).
func TestLoginErrorsAreIndistinguishable(t *testing.T) {
	e := newEnv(t, allRoles)
	a := e.do(http.MethodPost, "/login", url.Values{"username": {"viewer1"}, "password": {"nope nope nope"}})
	b := e.do(http.MethodPost, "/login", url.Values{"username": {"viewer1"}, "password": {"nope nope nope"}})
	c := e.do(http.MethodPost, "/login", url.Values{"username": {"ghost"}, "password": {"nope nope nope"}})
	norm := func(s string) string { return strings.ReplaceAll(s, "ghost", "viewer1") }
	if a.Body.String() != b.Body.String() || a.Body.String() != norm(c.Body.String()) || a.Code != c.Code {
		t.Error("responses differ between wrong password and unknown user")
	}
}

func TestLoginRateLimited(t *testing.T) {
	e := newEnv(t, allRoles)
	var w *httptest.ResponseRecorder
	for i := 0; i < 12; i++ {
		w = e.do(http.MethodPost, "/login", url.Values{"username": {"viewer1"}, "password": {"nope nope nope"}})
		if w.Code == http.StatusTooManyRequests {
			break
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d", i, w.Code)
		}
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatal("no 429 after 12 failed attempts")
	}
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
	if want := "Too many sign-in attempts. Try again in " + strconv.Itoa(secs) + " s."; !strings.Contains(w.Body.String(), want) {
		t.Errorf("no %q in\n%s", want, w.Body)
	}
	// Even the right password is refused while limited.
	w = e.do(http.MethodPost, "/login", url.Values{"username": {"viewer1"}, "password": {goodPassword}})
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("right password while limited: status %d", w.Code)
	}
	// Polish message.
	w = e.do(http.MethodPost, "/login", url.Values{"username": {"viewer1"}, "password": {"x"}}, withHeader("Accept-Language", "pl"))
	if !strings.Contains(w.Body.String(), "Zbyt wiele prób logowania") {
		t.Errorf("no Polish rate limit message:\n%s", w.Body)
	}
}

func TestLoginNextNoOpenRedirect(t *testing.T) {
	e := newEnv(t, allRoles)
	tests := []struct{ next, want string }{
		{"/messages", "/messages"},
		{"/messages?page=2", "/messages?page=2"},
		{"", "/"},
		{"//evil.example", "/"},
		{"https://evil.example/", "/"},
		{"/\\evil.example", "/"},
		{"javascript:alert(1)", "/"},
		{"evil.example", "/"},
		{"/ok\r\nSet-Cookie: x=y", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.next, func(t *testing.T) {
			w := e.do(http.MethodPost, "/login",
				url.Values{"username": {"viewer1"}, "password": {goodPassword}, "next": {tt.next}})
			if got := w.Header().Get("Location"); got != tt.want {
				t.Errorf("POST Location = %q, want %q", got, tt.want)
			}
			// The form carries only the sanitised value.
			w = e.do(http.MethodGet, "/login?next="+url.QueryEscape(tt.next), nil)
			if !strings.Contains(w.Body.String(), `name="next" value="`+tt.want+`"`) {
				t.Errorf("login form does not carry next=%q:\n%s", tt.want, w.Body)
			}
		})
	}
}

func TestLoginRedirectsWhenSignedIn(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("viewer1")
	w := e.do(http.MethodGet, "/login?next=/messages", nil, withCookies(c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/messages" {
		t.Errorf("status %d, Location %q", w.Code, w.Header().Get("Location"))
	}
}

func TestAnonymousRedirectedToLogin(t *testing.T) {
	e := newEnv(t, allRoles)
	w := e.do(http.MethodGet, "/devices", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next=%2Fdevices" {
		t.Errorf("status %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	// htmx requests get HX-Redirect instead of a redirect to follow.
	w = e.do(http.MethodGet, "/messages", nil, withHeader("HX-Request", "true"))
	if w.Code != http.StatusUnauthorized || w.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("htmx: status %d, HX-Redirect %q", w.Code, w.Header().Get("HX-Redirect"))
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")
	token := e.csrfToken(c)

	tests := []struct {
		name   string
		form   url.Values
		opts   []reqOpt
		status int
		ended  bool
	}{
		{name: "no token", form: url.Values{}, status: http.StatusForbidden},
		{name: "wrong token", form: url.Values{"csrf_token": {"nope"}}, status: http.StatusForbidden},
		{name: "cross-origin", form: url.Values{"csrf_token": {token}}, opts: []reqOpt{withHeader("Origin", "https://evil.example")}, status: http.StatusForbidden},
		{name: "ok", form: url.Values{"csrf_token": {token}}, status: http.StatusSeeOther, ended: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := append([]reqOpt{withCookies(c)}, tt.opts...)
			w := e.do(http.MethodPost, "/logout", tt.form, opts...)
			if w.Code != tt.status {
				t.Fatalf("status %d, want %d", w.Code, tt.status)
			}
			after := e.do(http.MethodGet, "/", nil, withCookies(c))
			if signedOut := after.Code == http.StatusSeeOther; signedOut != tt.ended {
				t.Errorf("session ended = %v, want %v (GET / -> %d)", signedOut, tt.ended, after.Code)
			}
			if tt.ended && w.Header().Get("Location") != "/login" {
				t.Errorf("Location = %q", w.Header().Get("Location"))
			}
		})
	}
}

func TestLogoutWithoutSession(t *testing.T) {
	e := newEnv(t, allRoles)
	w := e.do(http.MethodPost, "/logout", url.Values{})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("status %d, Location %q", w.Code, w.Header().Get("Location"))
	}
}

func TestSetupWizard(t *testing.T) {
	e := newEnv(t, nil)

	// Everything leads to the wizard while there is no user.
	w := e.do(http.MethodGet, "/login", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
		t.Errorf("GET /login: status %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	w = e.do(http.MethodGet, "/messages", nil)
	if w.Header().Get("Location") != "/login?next=%2Fmessages" {
		t.Errorf("GET /messages: Location %q", w.Header().Get("Location"))
	}
	w = e.do(http.MethodPost, "/login", url.Values{"username": {"a"}, "password": {"b"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
		t.Errorf("POST /login: status %d, Location %q", w.Code, w.Header().Get("Location"))
	}

	w = e.do(http.MethodGet, "/setup", nil, withHeader("Accept-Language", "pl"))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /setup: %d", w.Code)
	}
	for _, want := range []string{`<label for="username">`, `<label for="password">`, `<label for="confirm">`, `autocomplete="new-password"`, "Utwórz administratora"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("setup page lacks %q", want)
		}
	}

	invalid := []struct {
		name string
		form url.Values
		want []string
	}{
		{"short password", url.Values{"username": {"boss"}, "password": {"short"}, "confirm": {"short"}},
			[]string{`id="password-error"`, "at least 12 characters", `aria-invalid="true"`}},
		{"mismatch", url.Values{"username": {"boss"}, "password": {goodPassword}, "confirm": {goodPassword + "x"}},
			[]string{`id="confirm-error"`, "The passwords do not match."}},
		{"bad username", url.Values{"username": {"a b"}, "password": {goodPassword}, "confirm": {goodPassword}},
			[]string{`id="username-error"`, "3-64 characters"}},
		{"empty username", url.Values{"username": {""}, "password": {goodPassword}, "confirm": {goodPassword}},
			[]string{`id="username-error"`, "Enter a username."}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			w := e.do(http.MethodPost, "/setup", tt.form, withHeader("Accept-Language", "en"))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d\n%s", w.Code, w.Body)
			}
			for _, want := range tt.want {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("response lacks %q", want)
				}
			}
			if strings.Contains(w.Body.String(), goodPassword) {
				t.Error("password echoed")
			}
		})
	}
	if ok, _ := e.svc.SetupRequired(t.Context()); !ok {
		t.Fatal("a user was created by an invalid submission")
	}

	w = e.do(http.MethodPost, "/setup", url.Values{"username": {"boss"}, "password": {goodPassword}, "confirm": {goodPassword}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("create: status %d, Location %q\n%s", w.Code, w.Header().Get("Location"), w.Body)
	}
	c := cookieNamed(w, auth.CookieName)
	if c == nil {
		t.Fatal("the new admin is not signed in")
	}
	// The new admin may open admin pages.
	if w := e.do(http.MethodGet, "/users", nil, withCookies(c)); w.Code != http.StatusOK {
		t.Errorf("GET /users as the new admin: %d", w.Code)
	}

	// The wizard is gone.
	if w := e.do(http.MethodGet, "/setup", nil); w.Code != http.StatusNotFound {
		t.Errorf("GET /setup after setup: %d", w.Code)
	}
	w = e.do(http.MethodPost, "/setup", url.Values{"username": {"evil"}, "password": {goodPassword}, "confirm": {goodPassword}})
	if w.Code != http.StatusNotFound {
		t.Errorf("POST /setup after setup: %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/login", nil); w.Code != http.StatusOK {
		t.Errorf("GET /login after setup: %d", w.Code)
	}
}

func TestSetupRejectsCrossOrigin(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do(http.MethodPost, "/setup", url.Values{"username": {"boss"}, "password": {goodPassword}, "confirm": {goodPassword}},
		withHeader("Origin", "https://evil.example"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status %d", w.Code)
	}
	if ok, _ := e.svc.SetupRequired(t.Context()); !ok {
		t.Error("cross-origin request created the admin")
	}
}

func TestRoleProtection(t *testing.T) {
	e := newEnv(t, allRoles)
	cookies := map[string]*http.Cookie{}
	for name := range allRoles {
		cookies[name] = e.login(name)
	}
	paths := []string{"/", "/messages", "/send", "/devices", "/tokens", "/users", "/audit"}
	// Index of the first path each role may not open.
	allowed := map[string]int{"viewer1": 2, "operator1": 3, "admin1": len(paths)}
	for user, n := range allowed {
		for i, p := range paths {
			want := http.StatusOK
			if i >= n {
				want = http.StatusForbidden
			}
			t.Run(user+p, func(t *testing.T) {
				w := e.do(http.MethodGet, p, nil, withCookies(cookies[user]))
				if w.Code != want {
					t.Fatalf("status %d, want %d", w.Code, want)
				}
				if want == http.StatusForbidden && !strings.Contains(w.Body.String(), "Forbidden") {
					t.Errorf("403 without the localised page:\n%s", w.Body)
				}
			})
		}
	}
}

func TestNavigationByRole(t *testing.T) {
	e := newEnv(t, allRoles)
	tests := []struct {
		user    string
		visible []string
		hidden  []string
	}{
		{"viewer1", []string{"/", "/messages"}, []string{"/send", "/devices", "/tokens", "/users", "/audit"}},
		{"operator1", []string{"/", "/messages", "/send"}, []string{"/devices", "/tokens", "/users", "/audit"}},
		{"admin1", []string{"/", "/messages", "/send", "/devices", "/tokens", "/users", "/audit"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.user, func(t *testing.T) {
			c := e.login(tt.user)
			body := e.do(http.MethodGet, "/messages", nil, withCookies(c)).Body.String()
			for _, p := range tt.visible {
				if !strings.Contains(body, `<a href="`+p+`"`) {
					t.Errorf("nav lacks %s", p)
				}
			}
			for _, p := range tt.hidden {
				if strings.Contains(body, `<a href="`+p+`"`) {
					t.Errorf("nav shows %s", p)
				}
			}
			if !strings.Contains(body, `<a href="/messages" aria-current="page">`) {
				t.Error("current page not marked")
			}
			for _, want := range []string{`action="/logout"`, `name="csrf_token"`, tt.user, "github.com/m4drak/tikker"} {
				if !strings.Contains(body, want) {
					t.Errorf("page lacks %q", want)
				}
			}
		})
	}
}

func TestHTTPSBaseURL(t *testing.T) {
	e := newEnv(t, allRoles, func(o *Options, c *auth.Config) {
		o.BaseURL = "https://sms.example.org"
		c.BaseURL = "https://sms.example.org"
	})
	w := e.do(http.MethodGet, "/login", nil)
	if got := w.Header().Get("Strict-Transport-Security"); got == "" {
		t.Error("no HSTS with an https base URL")
	}
	w = e.do(http.MethodPost, "/lang", url.Values{"lang": {"pl"}}, withHeader("Origin", "https://sms.example.org"))
	c := cookieNamed(w, LangCookie)
	if c == nil || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("language cookie = %+v", c)
	}
	// Plain http: no HSTS.
	e = newEnv(t, allRoles)
	if e.do(http.MethodGet, "/login", nil).Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS over plain http")
	}
}

func TestNewRequiresAuth(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New without Auth succeeded")
	}
}
