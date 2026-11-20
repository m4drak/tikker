// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// checkSecurityHeaders asserts the headers every panel response carries.
func checkSecurityHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	h := w.Header()
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*"} {
		if strings.Contains(csp, bad) {
			t.Errorf("CSP contains %q: %s", bad, csp)
		}
	}
	for k, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if h.Get("Permissions-Policy") == "" {
		t.Error("no Permissions-Policy")
	}
}

func TestSecurityHeadersOnEveryPage(t *testing.T) {
	e := newEnv(t, allRoles)
	viewer := e.login("viewer1")
	admin := e.login("admin1")
	token := e.csrfToken(viewer)

	tests := []struct {
		name   string
		method string
		path   string
		form   url.Values
		opts   []reqOpt
		status int
	}{
		{"login", "GET", "/login", nil, nil, 200},
		{"dashboard", "GET", "/", nil, []reqOpt{withCookies(admin)}, 200},
		{"messages", "GET", "/messages", nil, []reqOpt{withCookies(viewer)}, 200},
		{"forbidden", "GET", "/devices", nil, []reqOpt{withCookies(viewer)}, 403},
		{"not found", "GET", "/nope", nil, nil, 404},
		{"anonymous redirect", "GET", "/devices", nil, nil, 303},
		{"bad login", "POST", "/login", url.Values{"username": {"x"}, "password": {"y"}}, nil, 401},
		{"csrf failure", "POST", "/logout", url.Values{}, []reqOpt{withCookies(viewer)}, 403},
		{"lang", "POST", "/lang", url.Values{"lang": {"pl"}, "csrf_token": {token}}, []reqOpt{withCookies(viewer)}, 303},
		{"static css", "GET", "/static/app.css", nil, nil, 200},
		{"static js", "GET", "/static/htmx.min.js", nil, nil, 200},
		{"static missing", "GET", "/static/none.js", nil, nil, 404},
		{"setup absent", "GET", "/setup", nil, nil, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := e.do(tt.method, tt.path, tt.form, tt.opts...)
			if w.Code != tt.status {
				t.Fatalf("status %d, want %d", w.Code, tt.status)
			}
			checkSecurityHeaders(t, w)
		})
	}

	// The wizard page and the 429 page.
	e2 := newEnv(t, nil)
	w := e2.do(http.MethodGet, "/setup", nil)
	if w.Code != 200 {
		t.Fatalf("setup: %d", w.Code)
	}
	checkSecurityHeaders(t, w)
	for i := 0; i < 12 && w.Code != http.StatusTooManyRequests; i++ {
		w = e.do(http.MethodPost, "/login", url.Values{"username": {"viewer1"}, "password": {"nope nope nope"}})
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatal("no 429")
	}
	checkSecurityHeaders(t, w)
}

// An htmx request refused by CSRF (no token) is a 403, with headers.
func TestHTMXRequestsNeedCSRFToken(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")
	token := e.csrfToken(c)
	e.srv.HandleFunc("POST /ping", "", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("pong")) })
	e.srv.mux = e.srv.buildMux()
	e.h = e.srv.Handler()

	w := e.do(http.MethodPost, "/ping", nil, withCookies(c), withHeader("HX-Request", "true"))
	if w.Code != http.StatusForbidden {
		t.Errorf("without token: %d", w.Code)
	}
	w = e.do(http.MethodPost, "/ping", nil, withCookies(c), withHeader("HX-Request", "true"), withHeader("X-CSRF-Token", token))
	if w.Code != http.StatusOK || w.Body.String() != "pong" {
		t.Errorf("with token: %d %q", w.Code, w.Body)
	}
	w = e.do(http.MethodPost, "/ping", nil, withHeader("HX-Request", "true"))
	if w.Code != http.StatusUnauthorized || w.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("anonymous: %d, HX-Redirect %q", w.Code, w.Header().Get("HX-Redirect"))
	}
}

var (
	tagRE        = regexp.MustCompile(`(?is)<(/?)([a-z][a-z0-9-]*)((?:\s+[^<>]*?)?)\s*/?>`)
	attrRE       = regexp.MustCompile(`(?is)([a-z_:][-a-z0-9_:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	inlineCodeRE = regexp.MustCompile(`(?i)^\s*(javascript|data|vbscript):`)
)

// inlineCode lists everything in html that the CSP would block: <style>
// elements, <script> without src or with a body, style="" and on*=""
// attributes, javascript: URLs.
func inlineCode(html string) []string {
	var found []string
	for _, m := range tagRE.FindAllStringSubmatch(html, -1) {
		closing, name, attrs := m[1] == "/", strings.ToLower(m[2]), m[3]
		if closing {
			continue
		}
		if name == "style" {
			found = append(found, "<style> element")
		}
		hasSrc := false
		for _, a := range attrRE.FindAllStringSubmatch(attrs, -1) {
			key := strings.ToLower(a[1])
			val := a[2] + a[3] + a[4]
			switch {
			case key == "style":
				found = append(found, "style attribute on <"+name+">")
			case strings.HasPrefix(key, "on"):
				found = append(found, key+" handler on <"+name+">")
			case strings.HasPrefix(key, "hx-on"):
				found = append(found, key+" (htmx inline code) on <"+name+">")
			case key == "src" && name == "script":
				hasSrc = true
			case (key == "href" || key == "src" || key == "action" || key == "formaction") && inlineCodeRE.MatchString(val):
				found = append(found, key+"="+val)
			}
		}
		if name == "script" {
			if !hasSrc {
				found = append(found, "inline <script>")
			}
		}
	}
	return found
}

func TestInlineCodeDetector(t *testing.T) {
	bad := []string{
		`<script>alert(1)</script>`,
		`<style>a{}</style>`,
		`<p style="color:red">x</p>`,
		`<button onclick="x()">`,
		`<a href="javascript:x()">`,
		`<div hx-on:click="x()">`,
		`<SCRIPT >x</SCRIPT>`,
	}
	for _, b := range bad {
		if len(inlineCode(b)) == 0 {
			t.Errorf("not detected: %s", b)
		}
	}
	good := `<script src="/static/a.js" defer></script><a href="/x" class="y">z</a><meta name="htmx-config" content='{"a":1}'>`
	if got := inlineCode(good); len(got) != 0 {
		t.Errorf("false positive: %v", got)
	}
}

func TestNoInlineScriptsOrStyles(t *testing.T) {
	e := newEnv(t, allRoles)
	admin := e.login("admin1")
	pages := []struct {
		name string
		path string
		opts []reqOpt
	}{
		{"login", "/login", nil},
		{"dashboard", "/", []reqOpt{withCookies(admin)}},
		{"messages", "/messages", []reqOpt{withCookies(admin)}},
		{"send", "/send", []reqOpt{withCookies(admin)}},
		{"devices", "/devices", []reqOpt{withCookies(admin)}},
		{"tokens", "/tokens", []reqOpt{withCookies(admin)}},
		{"users", "/users", []reqOpt{withCookies(admin)}},
		{"audit", "/audit", []reqOpt{withCookies(admin)}},
		{"404", "/nope", nil},
	}
	for _, lang := range []string{"pl", "en"} {
		for _, p := range pages {
			t.Run(lang+"/"+p.name, func(t *testing.T) {
				opts := append([]reqOpt{withHeader("Accept-Language", lang)}, p.opts...)
				w := e.do(http.MethodGet, p.path, nil, opts...)
				if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
					t.Fatalf("Content-Type = %q (status %d)", ct, w.Code)
				}
				if found := inlineCode(w.Body.String()); len(found) != 0 {
					t.Errorf("inline code: %v", found)
				}
				// Only own scripts and styles, no external hosts.
				for _, m := range regexp.MustCompile(`(?:src|href)="(https?:)?//[^"]+"`).FindAllString(w.Body.String(), -1) {
					if !strings.Contains(m, "github.com/m4drak/tikker") {
						t.Errorf("external reference %s", m)
					}
				}
				if !strings.Contains(w.Body.String(), `<meta name="htmx-config" content='{"includeIndicatorStyles":false,"allowEval":false,"allowScriptTags":false,"historyEnabled":false}'>`) {
					t.Error("htmx hardening meta tag missing")
				}
			})
		}
	}
}

func TestCSRFWiringInPages(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("viewer1")
	token := e.csrfToken(c)
	body := e.do(http.MethodGet, "/", nil, withCookies(c)).Body.String()
	if got := strings.Count(body, `name="csrf_token" value="`+token+`"`); got != 2 { // logout + language forms
		t.Errorf("%d forms carry the token, want 2", got)
	}
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"htmx:configRequest", "X-CSRF-Token", "csrf-token"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// Anonymous pages have no meta token (there is no session to protect).
	if strings.Contains(e.do(http.MethodGet, "/login", nil).Body.String(), `name="csrf-token"`) {
		t.Error("anonymous page has a csrf meta tag")
	}
}
