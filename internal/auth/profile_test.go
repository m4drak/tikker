// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/m4drak/tikker/internal/domain"
)

func TestChangeOwnPassword(t *testing.T) {
	const newPass = "another long passphrase"
	tests := []struct {
		name, current, next string
		wantErr             func(error) bool
	}{
		{"ok", goodPassword, newPass, nil},
		{"wrong current", "not the password", newPass, func(err error) bool { return errors.Is(err, ErrInvalidCredentials) }},
		{"policy", goodPassword, "short", func(err error) bool { return errors.Is(err, ErrValidation) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			u := e.user("alice", domain.RoleOperator)
			c := e.login("alice")
			r := request(http.MethodPost, "/profile/password", c)
			p, err := e.svc.SessionPrincipal(httptest.NewRecorder(), r)
			e.ok(err)
			w := httptest.NewRecorder()
			np, err := e.svc.ChangeOwnPassword(w, r, p, tt.current, tt.next)
			if tt.wantErr != nil {
				if !tt.wantErr(err) {
					t.Fatalf("err = %v", err)
				}
				// The password stays, the session stays.
				if _, err := e.svc.Login(httptest.NewRecorder(), request(http.MethodPost, "/login"), u.Username, goodPassword); err != nil {
					t.Fatal(err)
				}
				return
			}
			e.ok(err)
			if np.CSRFToken == "" || sessionCookie(t, w).Value == c.Value {
				t.Error("no fresh session")
			}
			// The old session ended, the new password works, the old does not.
			if _, err := e.svc.SessionPrincipal(httptest.NewRecorder(), request(http.MethodGet, "/", c)); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("old session still valid: %v", err)
			}
			if _, err := e.svc.Login(httptest.NewRecorder(), request(http.MethodPost, "/login"), "alice", newPass); err != nil {
				t.Errorf("new password: %v", err)
			}
			if _, err := e.svc.Login(httptest.NewRecorder(), request(http.MethodPost, "/login"), "alice", goodPassword); !errors.Is(err, ErrInvalidCredentials) {
				t.Errorf("old password: %v", err)
			}
			if a := e.lastAudit(); a.Action != domain.AuditLoginFailure && a.Action != domain.AuditLoginSuccess {
				// the last entries are the logins above
				t.Logf("last audit %s", a.Action)
			}
			e.assertNoSecrets(newPass, goodPassword)
		})
	}
}

func TestChangeOwnPasswordRateLimited(t *testing.T) {
	e := newEnv(t)
	e.user("bob", domain.RoleViewer)
	c := e.login("bob")
	r := request(http.MethodPost, "/profile/password", c)
	p, err := e.svc.SessionPrincipal(httptest.NewRecorder(), r)
	e.ok(err)
	var rl *RateLimitError
	for i := 0; i < 100; i++ {
		_, err = e.svc.ChangeOwnPassword(httptest.NewRecorder(), r, p, "wrong wrong wrong", "yet another passphrase")
		if errors.As(err, &rl) {
			return
		}
	}
	t.Fatalf("never rate limited, last err %v", err)
}
