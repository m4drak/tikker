// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/web/i18n"
)

func TestLanguageSelection(t *testing.T) {
	e := newEnv(t, allRoles)
	tests := []struct {
		name    string
		opts    []reqOpt
		lang    string
		heading string
	}{
		{"no header", nil, "en", "Sign in to tikker"},
		{"accept pl", []reqOpt{withHeader("Accept-Language", "pl-PL,pl;q=0.9,en;q=0.8")}, "pl", "Zaloguj się do tikkera"},
		{"accept en", []reqOpt{withHeader("Accept-Language", "en-GB,en;q=0.9")}, "en", "Sign in to tikker"},
		{"accept other", []reqOpt{withHeader("Accept-Language", "de-DE,de;q=0.9")}, "en", "Sign in to tikker"},
		{"cookie beats header", []reqOpt{
			withHeader("Accept-Language", "pl"),
			withCookies(&http.Cookie{Name: LangCookie, Value: "en"})}, "en", "Sign in to tikker"},
		{"cookie pl", []reqOpt{
			withHeader("Accept-Language", "en"),
			withCookies(&http.Cookie{Name: LangCookie, Value: "pl"})}, "pl", "Zaloguj się do tikkera"},
		{"bad cookie ignored", []reqOpt{
			withHeader("Accept-Language", "pl"),
			withCookies(&http.Cookie{Name: LangCookie, Value: "xx"})}, "pl", "Zaloguj się do tikkera"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := e.do(http.MethodGet, "/login", nil, tt.opts...)
			body := w.Body.String()
			if !strings.Contains(body, `<html lang="`+tt.lang+`">`) {
				t.Errorf("no lang=%q in\n%s", tt.lang, body)
			}
			if !strings.Contains(body, tt.heading) {
				t.Errorf("no heading %q", tt.heading)
			}
			if got := w.Header().Get("Content-Language"); got != tt.lang {
				t.Errorf("Content-Language = %q", got)
			}
		})
	}
}

func TestLanguageSwitch(t *testing.T) {
	e := newEnv(t, allRoles)

	// Anonymous: the footer form posts to /lang and the cookie then wins.
	body := e.do(http.MethodGet, "/login", nil).Body.String()
	for _, want := range []string{`action="/lang"`, `name="lang" value="pl"`, `name="lang" value="en"`, `aria-current="true"`, ">Polski<", ">English<"} {
		if !strings.Contains(body, want) {
			t.Errorf("switcher lacks %q", want)
		}
	}
	w := e.do(http.MethodPost, "/lang", url.Values{"lang": {"pl"}, "next": {"/login"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("status %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	c := cookieNamed(w, LangCookie)
	if c == nil || c.Value != "pl" || c.Path != "/" || !c.HttpOnly || c.MaxAge <= 0 {
		t.Fatalf("cookie = %+v", c)
	}
	if !strings.Contains(e.do(http.MethodGet, "/login", nil, withCookies(c)).Body.String(), "Zaloguj się") {
		t.Error("cookie does not switch the language")
	}

	// Unsupported language: no cookie. Open redirect: refused.
	w = e.do(http.MethodPost, "/lang", url.Values{"lang": {"xx"}, "next": {"//evil.example"}})
	if cookieNamed(w, LangCookie) != nil || w.Header().Get("Location") != "/" {
		t.Errorf("cookie %v, Location %q", cookieNamed(w, LangCookie), w.Header().Get("Location"))
	}

	// Signed in: the form needs the CSRF token like any POST.
	s := e.login("viewer1")
	if w := e.do(http.MethodPost, "/lang", url.Values{"lang": {"pl"}}, withCookies(s)); w.Code != http.StatusForbidden {
		t.Errorf("without token: %d", w.Code)
	}
	w = e.do(http.MethodPost, "/lang", url.Values{"lang": {"pl"}, "csrf_token": {e.csrfToken(s)}, "next": {"/messages"}}, withCookies(s))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/messages" || cookieNamed(w, LangCookie) == nil {
		t.Errorf("with token: %d, Location %q", w.Code, w.Header().Get("Location"))
	}

	// Pages are fully translated: the dashboard stub in both languages.
	pl := e.do(http.MethodGet, "/messages", nil, withCookies(s, &http.Cookie{Name: LangCookie, Value: "pl"})).Body.String()
	en := e.do(http.MethodGet, "/messages", nil, withCookies(s, &http.Cookie{Name: LangCookie, Value: "en"})).Body.String()
	for _, want := range []string{"Wiadomości", "Ta strona będzie dostępna wkrótce.", "Wyloguj", "podgląd", "Kod źródłowy"} {
		if !strings.Contains(pl, want) {
			t.Errorf("pl page lacks %q", want)
		}
	}
	for _, want := range []string{"Messages", "This page is coming soon.", "Sign out", "viewer", "Source code"} {
		if !strings.Contains(en, want) {
			t.Errorf("en page lacks %q", want)
		}
	}
}

func TestErrorPages(t *testing.T) {
	e := newEnv(t, allRoles)
	v := e.login("viewer1")
	tests := []struct {
		path   string
		opts   []reqOpt
		status int
		want   string
	}{
		{"/nope", []reqOpt{withHeader("Accept-Language", "en")}, 404, "This page does not exist."},
		{"/nope", []reqOpt{withHeader("Accept-Language", "pl")}, 404, "Ta strona nie istnieje."},
		{"/users", []reqOpt{withCookies(v), withHeader("Accept-Language", "en")}, 403, "Your role does not allow you to open this page."},
		{"/users", []reqOpt{withCookies(v), withHeader("Accept-Language", "pl")}, 403, "Twoja rola nie pozwala otworzyć tej strony."},
	}
	for _, tt := range tests {
		w := e.do(http.MethodGet, tt.path, nil, tt.opts...)
		if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.want) {
			t.Errorf("%s: status %d, want %d with %q", tt.path, w.Code, tt.status, tt.want)
		}
	}
	// A signed-in user gets the navigation on error pages.
	w := e.do(http.MethodGet, "/nope", nil, withCookies(v))
	if !strings.Contains(w.Body.String(), `action="/logout"`) {
		t.Error("404 page of a signed-in user has no navigation")
	}
}

func TestHandleReplacesPlaceholderAndRenderBlock(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("operator1")
	e.srv.HandleFunc("GET /send", domain.RoleOperator, func(w http.ResponseWriter, r *http.Request) {
		e.srv.RenderBlock(w, r, http.StatusAccepted, "error", "content", errorData{Status: 404, TitleKey: "error.404.title", TextKey: "error.404.text"})
	})
	e.srv.HandleFunc("GET /boom", "", func(w http.ResponseWriter, r *http.Request) {
		e.srv.Render(w, r, http.StatusOK, "no-such-page", nil)
	})
	e.srv.HandleFunc("GET /badblock", "", func(w http.ResponseWriter, r *http.Request) {
		e.srv.RenderBlock(w, r, http.StatusOK, "error", "no-such-block", nil)
	})
	e.srv.mux = e.srv.buildMux()
	e.h = e.srv.Handler()

	w := e.do(http.MethodGet, "/send", nil, withCookies(c), withHeader("Accept-Language", "en"))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "This page does not exist.") || strings.Contains(w.Body.String(), "<html") {
		t.Errorf("block render: status %d\n%s", w.Code, w.Body)
	}
	// The replacement keeps its role: a viewer is refused.
	if w := e.do(http.MethodGet, "/send", nil, withCookies(e.login("viewer1"))); w.Code != http.StatusForbidden {
		t.Errorf("viewer on replaced /send: %d", w.Code)
	}
	for _, p := range []string{"/boom", "/badblock"} {
		w = e.do(http.MethodGet, p, nil, withCookies(c))
		if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "<html") {
			t.Errorf("%s: status %d", p, w.Code)
		}
	}
}

// TestTemplateKeysExist checks that every literal translation key used in a
// template or in Go code exists, and that every key is used somewhere.
func TestTemplateKeysExist(t *testing.T) {
	bundle, err := i18n.Load()
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}

	tplKey := regexp.MustCompile(`\{\{-?\s*(?:[^{}]*?\()?t "([^"]+)"`)
	inlineKey := regexp.MustCompile(`\(t "([^"]+)"`)
	err = fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := templateFS.ReadFile(p)
		if err != nil {
			return err
		}
		for _, re := range []*regexp.Regexp{tplKey, inlineKey, regexp.MustCompile(`\bt "([^"]+)"`)} {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				if !bundle.Has(m[1]) {
					t.Errorf("%s uses unknown key %q", p, m[1])
				}
				used[m[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Keys in Go code: any string literal that is a key (dotted lower-case).
	keyLit := regexp.MustCompile(`"([a-z]+(?:\.[a-z0-9_]+)+)"`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range keyLit.FindAllStringSubmatch(string(data), -1) {
			if bundle.Has(m[1]) {
				used[m[1]] = true
			}
		}
	}
	for _, it := range navItems {
		if !bundle.Has(it.LabelKey) {
			t.Errorf("nav key %q missing", it.LabelKey)
		}
		used[it.LabelKey] = true
	}
	for _, role := range []domain.Role{domain.RoleViewer, domain.RoleOperator, domain.RoleAdmin} {
		used["role."+string(role)] = true
	}
	for _, status := range []string{"400", "403", "404", "500"} {
		used["error."+status+".title"], used["error."+status+".text"] = true, true
	}
	for _, k := range bundle.Keys() {
		if !used[k] {
			t.Errorf("translation key %q is not used", k)
		}
	}
}

func TestBuildString(t *testing.T) {
	if got := buildString(); got == "" {
		t.Error("empty build string")
	}
}
