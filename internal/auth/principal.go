// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// Principal is the authenticated caller of a request: a panel user with a
// session, or an API token with its owner.
type Principal struct {
	// User is the session user or the token owner, as currently stored
	// (role and status are re-read on every request). PasswordHash is
	// always empty.
	User domain.User
	// Token is set for API requests; it has no Hash.
	Token *domain.APIToken
	// Session is set for panel requests.
	Session *store.Session
	// CSRFToken is the anti-CSRF token of the session, to be embedded in
	// forms (field "csrf_token") and htmx requests (header X-CSRF-Token).
	CSRFToken string
}

// Actor returns who acts: "token:<id>" for API requests (the API records
// created_by per token), "user:<id>" for panel requests.
func (p Principal) Actor() domain.Actor {
	if p.Token != nil {
		return domain.TokenActor(p.Token.ID)
	}
	return domain.UserActor(p.User.ID)
}

// Can reports whether the principal may exercise scope: for a token, the
// intersection of its scopes and its owner's role (domain.TokenAllows); for
// a session, the user's role.
func (p Principal) Can(scope domain.Scope) bool {
	if p.Token != nil {
		return domain.TokenAllows(p.Token.Scopes, p.User.Role, scope)
	}
	return p.User.Role.Scopes().Allows(scope)
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p. The middleware calls it; tests of
// other packages may use it to fake an authenticated request.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored by the middleware.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
