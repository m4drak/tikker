// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/m4drak/tikker/internal/domain"
)

// Problem types of the API contract used by the middleware.
const (
	ProblemUnauthorized = "urn:tikker:problem:unauthorized"
	ProblemForbidden    = "urn:tikker:problem:forbidden"
	ProblemInternal     = "urn:tikker:problem:internal"
	ProblemRateLimited  = "urn:tikker:problem:rate-limited"
)

const realm = `Bearer realm="tikker"`

// dummyTokenHash is compared against when a token prefix is unknown, so
// that unknown and wrong tokens cost the same.
var dummyTokenHash = hashSecret(strings.Repeat("0", TokenSecretLen))

// AuthenticateToken validates a raw API token ("tkr_…", without "Bearer ")
// and returns its principal. Every failure (bad format, unknown prefix,
// wrong secret, expired, owner missing or disabled) yields
// [ErrInvalidToken]; other errors are infrastructure failures. On success
// last_used_at is updated, at most once per TouchInterval.
func (s *Service) AuthenticateToken(ctx context.Context, raw string) (Principal, error) {
	prefix, secret, err := ParseToken(raw)
	if err != nil {
		return Principal{}, ErrInvalidToken
	}
	t, err := s.st.Tokens().GetByPrefix(ctx, prefix)
	if errors.Is(err, domain.ErrNotFound) {
		_ = subtle.ConstantTimeCompare(hashSecret(secret), dummyTokenHash)
		return Principal{}, ErrInvalidToken
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: token lookup: %w", err)
	}
	if subtle.ConstantTimeCompare(hashSecret(secret), t.Hash) != 1 {
		s.log.Info("api token rejected", "prefix", prefix, "reason", "wrong secret")
		return Principal{}, ErrInvalidToken
	}
	now := s.clk.Now()
	if t.Expired(now) {
		s.log.Info("api token rejected", "token_id", string(t.ID), "reason", "expired")
		return Principal{}, ErrInvalidToken
	}
	u, err := s.st.Users().Get(ctx, t.UserID)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && u.Disabled) {
		s.log.Info("api token rejected", "token_id", string(t.ID), "reason", "owner disabled")
		return Principal{}, ErrInvalidToken
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: token owner: %w", err)
	}
	if now.Sub(t.LastUsedAt) >= s.touch {
		if err := s.st.Tokens().TouchLastUsed(ctx, t.ID, now); err != nil {
			s.log.Warn("token touch failed", "token_id", string(t.ID), "err", err.Error())
		} else {
			t.LastUsedAt = now
		}
	}
	t.Hash = nil
	return Principal{User: public(u), Token: &t}, nil
}

// bearer extracts the token from an Authorization header.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// RequireToken authenticates API requests with a Bearer token and requires
// any of scopes (none: any valid token), with the token's effective
// permissions being its scopes intersected with its owner's role
// (domain.TokenAllows). Responses follow api/openapi.yaml: 401 with
// WWW-Authenticate for missing or invalid tokens, 403 for a missing scope,
// as application/problem+json. The [Principal] is available to the next
// handler through [PrincipalFrom].
func (s *Service) RequireToken(scopes ...domain.Scope) func(http.Handler) http.Handler {
	for _, sc := range scopes {
		if !sc.Valid() {
			panic(fmt.Sprintf("auth: RequireToken with invalid scope %q", sc))
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearer(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", realm)
				WriteProblem(w, r, http.StatusUnauthorized, ProblemUnauthorized, "Missing, invalid, expired or revoked API token.")
				return
			}
			p, err := s.AuthenticateToken(r.Context(), raw)
			if errors.Is(err, ErrInvalidToken) {
				w.Header().Set("WWW-Authenticate", realm+`, error="invalid_token"`)
				WriteProblem(w, r, http.StatusUnauthorized, ProblemUnauthorized, "Missing, invalid, expired or revoked API token.")
				return
			}
			if err != nil {
				s.log.Error("token authentication failed", "err", err.Error())
				WriteProblem(w, r, http.StatusInternalServerError, ProblemInternal, "Internal server error.")
				return
			}
			if len(scopes) > 0 && !anyScope(p, scopes) {
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`%s, error="insufficient_scope", scope="%s"`, realm, scopesString(scopes)))
				WriteProblem(w, r, http.StatusForbidden, ProblemForbidden,
					fmt.Sprintf("Token lacks required scope %q.", string(scopes[0])))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func anyScope(p Principal, scopes []domain.Scope) bool {
	for _, sc := range scopes {
		if p.Can(sc) {
			return true
		}
	}
	return false
}

// RequireSession authenticates panel requests with the session cookie and
// requires at least role (empty: any logged-in user). Anonymous requests
// get, for htmx (HX-Request: true), 401 with an HX-Redirect header to the
// login page; for GET/HEAD navigation a 303 redirect to the login page
// with ?next=<original path>; otherwise 401. A logged-in user with too low
// a role gets 403.
func (s *Service) RequireSession(role domain.Role) func(http.Handler) http.Handler {
	if role != "" && !role.Valid() {
		panic(fmt.Sprintf("auth: RequireSession with invalid role %q", role))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := s.SessionPrincipal(w, r)
			if errors.Is(err, ErrUnauthenticated) {
				s.unauthenticated(w, r)
				return
			}
			if err != nil {
				s.log.Error("session authentication failed", "err", err.Error())
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			if role != "" && !p.User.Role.Allows(role) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			w.Header().Add("Vary", "Cookie")
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

func (s *Service) unauthenticated(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Header.Get("HX-Request") == "true":
		w.Header().Set("HX-Redirect", s.loginPath)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		http.Redirect(w, r, s.loginPath+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	default:
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}

// SafeRedirect returns next if it is a local path (for the ?next= parameter
// after login), otherwise "/". It rejects absolute and scheme-relative URLs
// ("//evil", "/\evil") to prevent open redirects.
func SafeRedirect(next string) string {
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

// problem is an RFC 9457 problem document (schema Problem).
type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// WriteProblem writes an application/problem+json response. instance is
// the request path.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type: typ, Title: http.StatusText(status), Status: status, Detail: detail, Instance: r.URL.Path,
	})
}

// WriteRateLimited writes the API 429 response for a *RateLimitError,
// with Retry-After.
func WriteRateLimited(w http.ResponseWriter, r *http.Request, e *RateLimitError) {
	w.Header().Set("Retry-After", fmt.Sprint(e.RetryAfterSeconds()))
	WriteProblem(w, r, http.StatusTooManyRequests, ProblemRateLimited,
		fmt.Sprintf("Too many login attempts; retry in %d s.", e.RetryAfterSeconds()))
}
