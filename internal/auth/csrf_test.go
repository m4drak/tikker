// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/domain"
)

func TestCSRF(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.BaseURL = "http://sms.example.org"
		c.TrustedOrigins = []string{"https://admin.example.org"}
	})
	e.user("alice", domain.RoleOperator)
	cookie := e.login("alice")
	token := csrfFor(cookie.Value)
	other := e.login("alice")

	tests := []struct {
		name    string
		method  string
		session bool
		headers map[string]string
		form    url.Values
		status  int
	}{
		{"get without anything", http.MethodGet, true, nil, nil, 204},
		{"get cross-site", http.MethodGet, true, map[string]string{"Sec-Fetch-Site": "cross-site"}, nil, 204},
		{"head", http.MethodHead, true, nil, nil, 204},
		{"options", http.MethodOptions, true, nil, nil, 204},

		// Session present: the token is required.
		{"post no token", http.MethodPost, true, nil, nil, 403},
		{"post wrong token", http.MethodPost, true, map[string]string{CSRFHeader: "nope"}, nil, 403},
		{"post token of other session", http.MethodPost, true, map[string]string{CSRFHeader: csrfFor(other.Value)}, nil, 403},
		{"post session id as token", http.MethodPost, true, map[string]string{CSRFHeader: sessionID(cookie.Value)}, nil, 403},
		{"post cookie as token", http.MethodPost, true, map[string]string{CSRFHeader: cookie.Value}, nil, 403},
		{"post header token", http.MethodPost, true, map[string]string{CSRFHeader: token}, nil, 204},
		{"delete header token", http.MethodDelete, true, map[string]string{CSRFHeader: token}, nil, 204},
		{"post form token", http.MethodPost, true, nil, url.Values{CSRFFormField: {token}, "to": {"+48600000000"}}, 204},
		{"post form wrong token", http.MethodPost, true, nil, url.Values{CSRFFormField: {"x"}}, 403},
		{"post same-origin fetch", http.MethodPost, true,
			map[string]string{CSRFHeader: token, "Sec-Fetch-Site": "same-origin", "Origin": "http://sms.example.org"}, nil, 204},

		// Cross-origin requests fail even with a valid token.
		{"cross-site fetch", http.MethodPost, true, map[string]string{CSRFHeader: token, "Sec-Fetch-Site": "cross-site"}, nil, 403},
		{"same-site fetch", http.MethodPost, true, map[string]string{CSRFHeader: token, "Sec-Fetch-Site": "same-site"}, nil, 403},
		{"foreign origin", http.MethodPost, true, map[string]string{CSRFHeader: token, "Origin": "https://evil.example"}, nil, 403},
		{"null origin", http.MethodPost, true, map[string]string{CSRFHeader: token, "Origin": "null"}, nil, 403},
		{"foreign referer", http.MethodPost, true, map[string]string{CSRFHeader: token, "Referer": "https://evil.example/x"}, nil, 403},
		{"bad referer", http.MethodPost, true, map[string]string{CSRFHeader: token, "Referer": "::::"}, nil, 403},
		{"same referer", http.MethodPost, true, map[string]string{CSRFHeader: token, "Referer": "http://sms.example.org/send"}, nil, 204},
		{"trusted origin", http.MethodPost, true, map[string]string{CSRFHeader: token, "Origin": "https://admin.example.org"}, nil, 204},
		{"trusted referer", http.MethodPost, true, map[string]string{CSRFHeader: token, "Referer": "https://admin.example.org/a"}, nil, 204},

		// No session (login form): only the origin check applies.
		{"login same origin", http.MethodPost, false, map[string]string{"Origin": "http://sms.example.org"}, nil, 204},
		{"login no headers", http.MethodPost, false, nil, nil, 204},
		{"login cross-site", http.MethodPost, false, map[string]string{"Origin": "https://evil.example"}, nil, 403},
		{"login cross-site fetch", http.MethodPost, false, map[string]string{"Sec-Fetch-Site": "cross-site"}, nil, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r *http.Request
			if tc.form != nil {
				r = httptest.NewRequest(tc.method, "http://sms.example.org/send", strings.NewReader(tc.form.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			} else {
				r = httptest.NewRequest(tc.method, "http://sms.example.org/send", nil)
			}
			if tc.session {
				r.AddCookie(cookie)
			}
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			h := &okHandler{}
			w := httptest.NewRecorder()
			e.svc.CSRF(h).ServeHTTP(w, r)
			if w.Code != tc.status || h.called != (tc.status == 204) {
				t.Fatalf("status %d (handler called %v), want %d: %s", w.Code, h.called, tc.status, w.Body)
			}
		})
	}
	e.assertNoSecrets(cookie.Value, token)
}

// TestCSRFWithSessionMiddleware wires the middleware as the panel will:
// RequireSession, then CSRF, and checks the token exposed to handlers.
func TestCSRFWithSessionMiddleware(t *testing.T) {
	e := newEnv(t)
	e.user("alice", domain.RoleOperator)
	cookie := e.login("alice")
	var seen string
	h := e.svc.RequireSession(domain.RoleOperator)(e.svc.CSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFrom(r.Context())
		seen = p.CSRFToken
		w.WriteHeader(http.StatusNoContent)
	})))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request(http.MethodGet, "/send", cookie))
	if w.Code != 204 || seen == "" {
		t.Fatalf("GET: %d %q", w.Code, seen)
	}
	r := request(http.MethodPost, "/send", cookie)
	r.Header.Set("HX-Request", "true")
	r.Header.Set(CSRFHeader, seen)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("POST with token: %d", w.Code)
	}
	// The token changes with the session.
	w = httptest.NewRecorder()
	_, err := e.svc.Login(w, request(http.MethodPost, "/login", cookie), "alice", goodPassword)
	e.ok(err)
	rotated := sessionCookie(t, w)
	r = request(http.MethodPost, "/send", rotated)
	r.Header.Set(CSRFHeader, seen)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("old token after rotation: %d", w.Code)
	}
	if e.svc.CSRFToken(request(http.MethodGet, "/login")) != "" {
		t.Fatal("token without session")
	}
}
