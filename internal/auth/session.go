// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// Session cookie names. Over HTTPS the __Host- prefix makes the browser
// insist on Secure, Path=/ and no Domain, so a sibling subdomain cannot
// plant or overwrite the cookie.
const (
	CookieName       = "tikker_session"
	SecureCookieName = "__Host-tikker_session"
)

// Session tokens are 32 random bytes, base64url without padding.
const (
	sessionTokenBytes = 32
	sessionTokenLen   = 43
	maxUserAgentLen   = 200
)

var b64url = base64.RawURLEncoding

// secure reports whether cookies for r must be Secure: the public base URL
// is https, or the request itself arrived over TLS.
func (s *Service) secure(r *http.Request) bool { return s.secureCookies || r.TLS != nil }

func (s *Service) cookieName(r *http.Request) string {
	if s.secure(r) {
		return SecureCookieName
	}
	return CookieName
}

// readCookie returns the raw session token of r if it is well-formed.
func (s *Service) readCookie(r *http.Request) (string, bool) {
	c, err := r.Cookie(s.cookieName(r))
	if err != nil || len(c.Value) != sessionTokenLen {
		return "", false
	}
	if _, err := b64url.DecodeString(c.Value); err != nil {
		return "", false
	}
	return c.Value, true
}

func (s *Service) setCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(r), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearCookie(w http.ResponseWriter, r *http.Request) { s.setCookie(w, r, "", -1) }

// sessionID is the stored identifier of a session token.
func sessionID(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// csrfLabel domain-separates the CSRF token from the session ID.
const csrfLabel = "tikker/csrf/v1"

// csrfFor derives the CSRF token of a session: HMAC-SHA256 keyed with the
// secret cookie value. It is bound to the session, changes when the
// session is rotated, needs no storage and cannot be computed without the
// HttpOnly cookie; publishing it in HTML reveals nothing about the cookie.
func csrfFor(raw string) string {
	m := hmac.New(sha256.New, []byte(raw))
	m.Write([]byte(csrfLabel))
	return b64url.EncodeToString(m.Sum(nil))
}

// CSRFToken returns the CSRF token for the session cookie of r, or "" when
// r has none. Pages that render forms outside [Service.RequireSession] (the
// login page) use it; inside, [Principal.CSRFToken] holds the same value.
func (s *Service) CSRFToken(r *http.Request) string {
	raw, ok := s.readCookie(r)
	if !ok {
		return ""
	}
	return csrfFor(raw)
}

// ---------------------------------------------------------------------------
// Login and logout

// Login checks username and password, applying the per-IP and per-username
// attempt limits, and on success starts a new session (rotating any
// previous one) and sets the cookie on w. Failures are audited and yield
// [ErrInvalidCredentials] whatever the cause (unknown user, wrong password,
// disabled account) after the same amount of hashing work, or a
// *[RateLimitError] (HTTP 429 with Retry-After) without any.
func (s *Service) Login(w http.ResponseWriter, r *http.Request, username, password string) (Principal, error) {
	ctx := r.Context()
	ip := s.ClientIP(r)
	ukey := userKey(username)
	now := s.clk.Now()
	if wait, ok := s.limiter.allow(limiterIP(ip), ukey, now); !ok {
		s.log.Warn("login rate limited", "ip", ip, "username", safeUsername(username), "retry_after", wait.String())
		return Principal{}, &RateLimitError{RetryAfter: wait}
	}

	u, reason, err := s.verify(ctx, username, password)
	if err != nil {
		return Principal{}, err
	}
	if reason != "" {
		s.log.Info("login failed", "ip", ip, "username", safeUsername(username), "reason", reason)
		meta := map[string]string{"ip": ip, "reason": reason}
		if name := safeUsername(username); name != "" {
			meta["username"] = name
		}
		target := ""
		if !u.ID.IsZero() {
			target = userTarget(u.ID)
		}
		if err := s.audit(ctx, s.st, domain.SystemActor, domain.AuditLoginFailure, target, meta); err != nil {
			s.log.Error("audit of failed login failed", "err", err.Error())
		}
		return Principal{}, ErrInvalidCredentials
	}

	s.limiter.succeeded(limiterIP(ip), ukey, s.clk.Now())
	p, err := s.startSession(w, r, u, func(tx store.Repos) error {
		return s.audit(ctx, tx, domain.UserActor(u.ID), domain.AuditLoginSuccess, userTarget(u.ID), map[string]string{"ip": ip})
	})
	if err != nil {
		return Principal{}, err
	}
	s.log.Info("login", "user_id", string(u.ID), "ip", ip)
	return p, nil
}

// verify checks the credentials. reason is empty on success, otherwise one
// of unknown_user, bad_password, disabled or bad_hash (for audit only). err
// is reserved for infrastructure failures.
func (s *Service) verify(ctx context.Context, username, password string) (domain.User, string, error) {
	if len(password) > MaxPasswordLen {
		password = password[:MaxPasswordLen] // bound the work; such a password never matches
	}
	u, err := s.st.Users().GetByUsername(ctx, username)
	if errors.Is(err, domain.ErrNotFound) {
		if err := s.hasher.VerifyDummy(ctx, password); err != nil {
			return domain.User{}, "", err
		}
		return domain.User{}, "unknown_user", nil
	}
	if err != nil {
		return domain.User{}, "", fmt.Errorf("auth: login: %w", err)
	}
	ok, rehash, err := s.hasher.Verify(ctx, password, u.PasswordHash)
	switch {
	case errors.Is(err, ErrMalformedHash):
		s.log.Error("stored password hash is malformed", "user_id", string(u.ID))
		if err := s.hasher.VerifyDummy(ctx, password); err != nil {
			return domain.User{}, "", err
		}
		return u, "bad_hash", nil
	case err != nil:
		return domain.User{}, "", err
	case !ok:
		return u, "bad_password", nil
	case u.Disabled:
		return u, "disabled", nil
	}
	if rehash {
		s.rehash(ctx, u, password)
	}
	return u, "", nil
}

// rehash stores a hash with the current parameters after a successful
// login. Failures are logged; the old hash keeps working.
func (s *Service) rehash(ctx context.Context, u domain.User, password string) {
	hash, err := s.hasher.Hash(ctx, password)
	if err == nil {
		u.PasswordHash = hash
		err = s.st.Users().Update(ctx, u)
	}
	if err != nil {
		s.log.Error("password rehash failed", "user_id", string(u.ID), "err", err.Error())
		return
	}
	s.log.Info("password rehashed with new parameters", "user_id", string(u.ID), "params", s.hasher.Params().String())
}

// StartSession starts a session for u (after the first-run wizard, or to
// keep the current user logged in after [Service.UpdateUser] ended their
// sessions) and sets the cookie. A session the request already carries is
// ended first, so the session ID always changes when the privilege level
// changes (no session fixation).
func (s *Service) StartSession(w http.ResponseWriter, r *http.Request, u domain.User) (Principal, error) {
	return s.startSession(w, r, u, nil)
}

func (s *Service) startSession(w http.ResponseWriter, r *http.Request, u domain.User, also func(tx store.Repos) error) (Principal, error) {
	ctx := r.Context()
	buf := make([]byte, sessionTokenBytes)
	if _, err := io.ReadFull(s.rand, buf); err != nil {
		return Principal{}, fmt.Errorf("auth: random session token: %w", err)
	}
	raw := b64url.EncodeToString(buf)
	now := s.clk.Now()
	sess := store.Session{
		ID: sessionID(raw), UserID: u.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(s.absolute),
		UserAgent: truncateUTF8(r.UserAgent(), maxUserAgentLen), IP: networkOf(s.ClientIP(r)),
	}
	old, hadOld := s.readCookie(r)
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		if hadOld {
			if err := tx.Sessions().Delete(ctx, sessionID(old)); err != nil && !errors.Is(err, domain.ErrNotFound) {
				return err
			}
		}
		if err := tx.Sessions().Create(ctx, sess); err != nil {
			return err
		}
		if also != nil {
			return also(tx)
		}
		return nil
	})
	if err != nil {
		return Principal{}, fmt.Errorf("auth: start session: %w", err)
	}
	s.setCookie(w, r, raw, int(s.absolute/time.Second))
	return Principal{User: public(u), Session: &sess, CSRFToken: csrfFor(raw)}, nil
}

// Logout ends the session of r (if any), audits it and clears the cookie.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	raw, ok := s.readCookie(r)
	s.clearCookie(w, r)
	if !ok {
		return nil
	}
	id := sessionID(raw)
	sess, err := s.st.Sessions().Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auth: logout: %w", err)
	}
	err = s.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Sessions().Delete(ctx, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, domain.UserActor(sess.UserID), domain.AuditLogout, userTarget(sess.UserID), nil)
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auth: logout: %w", err)
	}
	s.log.Info("logout", "user_id", string(sess.UserID))
	return nil
}

// SessionPrincipal resolves the session of r. It returns
// [ErrUnauthenticated] when r has no valid session, after clearing a stale
// cookie on w. Expired sessions are deleted; last_seen_at is written at most
// once per TouchInterval. The user's role and status are read afresh, so a
// disabled user is logged out at the next request.
func (s *Service) SessionPrincipal(w http.ResponseWriter, r *http.Request) (Principal, error) {
	ctx := r.Context()
	raw, ok := s.readCookie(r)
	if !ok {
		if _, err := r.Cookie(s.cookieName(r)); err == nil {
			s.clearCookie(w, r)
		}
		return Principal{}, ErrUnauthenticated
	}
	id := sessionID(raw)
	sess, err := s.st.Sessions().Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		s.clearCookie(w, r)
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: session: %w", err)
	}
	now := s.clk.Now()
	if !now.Before(sess.ExpiresAt) || now.Sub(sess.LastSeenAt) >= s.idle {
		return Principal{}, s.endSession(w, r, id, "expired")
	}
	u, err := s.st.Users().Get(ctx, sess.UserID)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && u.Disabled) {
		return Principal{}, s.endSession(w, r, id, "user disabled")
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: session user: %w", err)
	}
	if now.Sub(sess.LastSeenAt) >= s.touch {
		err := s.st.Sessions().Touch(ctx, id, now)
		switch {
		case errors.Is(err, domain.ErrNotFound): // ended concurrently (logout elsewhere)
			s.clearCookie(w, r)
			return Principal{}, ErrUnauthenticated
		case err != nil:
			s.log.Warn("session touch failed", "user_id", string(u.ID), "err", err.Error())
		default:
			sess.LastSeenAt = now
		}
	}
	return Principal{User: public(u), Session: &sess, CSRFToken: csrfFor(raw)}, nil
}

func (s *Service) endSession(w http.ResponseWriter, r *http.Request, id, why string) error {
	if err := s.st.Sessions().Delete(r.Context(), id); err != nil && !errors.Is(err, domain.ErrNotFound) {
		s.log.Warn("ending session failed", "reason", why, "err", err.Error())
	}
	s.clearCookie(w, r)
	return ErrUnauthenticated
}

// ---------------------------------------------------------------------------
// Client address

// ClientIP returns the client address of r: the TCP peer, or, when the
// peer is a trusted proxy, the right-most address in X-Forwarded-For that
// is not a trusted proxy. The result is a bare IP ("" if unknown).
func (s *Service) ClientIP(r *http.Request) string {
	peer := parseAddr(r.RemoteAddr)
	if !peer.IsValid() {
		return ""
	}
	if !s.trusted(peer) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a := parseAddr(strings.TrimSpace(hops[i]))
		if !a.IsValid() {
			break
		}
		if !s.trusted(a) {
			return a.String()
		}
	}
	return peer.String()
}

func (s *Service) trusted(a netip.Addr) bool {
	for _, p := range s.trustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseAddr accepts "ip", "ip:port" and "[ipv6]:port".
func parseAddr(s string) netip.Addr {
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap().WithZone("")
}

// limiterIP is the rate-limiting key of an address: the address for IPv4,
// its /64 for IPv6 (one subscriber usually gets a whole /64).
func limiterIP(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// networkOf truncates an address for storage with a session (/24 for IPv4,
// /48 for IPv6): enough to recognise a session, not to track a person.
func networkOf(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	bits := 24
	if a.Is6() {
		bits = 48
	}
	p, _ := a.Prefix(bits)
	return p.String()
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary and drops
// invalid UTF-8.
func truncateUTF8(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
