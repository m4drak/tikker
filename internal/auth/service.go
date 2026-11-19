// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package auth implements authentication and authorisation for tikker
// (SPEC §5–§7, task T20):
//
//   - panel users with argon2id password hashes (PHC strings), a password
//     policy and transparent rehashing when the parameters change;
//   - API tokens "tkr_<prefix>_<secret>" stored as SHA-256 of the secret,
//     checked in constant time, with expiry, last-use tracking and scopes
//     intersected with the owner's role;
//   - server-side panel sessions in an HttpOnly, SameSite=Lax cookie with
//     absolute and idle timeouts, ID rotation on login and invalidation on
//     password, role or status changes;
//   - CSRF protection (cross-origin check plus a token bound to the session)
//     and an in-memory login rate limiter per IP and per username;
//   - net/http middleware ([Service.RequireToken], [Service.RequireSession],
//     [Service.CSRF]) that put a [Principal] in the request context;
//   - bootstrap of the first admin and audit records for every security
//     relevant action.
//
// Secrets (passwords, tokens, session cookies) are never logged or written
// to the audit log.
package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// Defaults for [Config].
const (
	DefaultSessionIdleTimeout     = time.Hour
	DefaultSessionAbsoluteTimeout = 12 * time.Hour
	// DefaultTouchInterval is how often last_seen_at of a session and
	// last_used_at of a token are written at most (flash wear).
	DefaultTouchInterval   = time.Minute
	DefaultJanitorInterval = 10 * time.Minute
	DefaultLoginPath       = "/login"
	// BootstrapUsername is the name of the admin created from
	// TIKKER_ADMIN_PASSWORD.
	BootstrapUsername = "admin"
)

// Config configures [New]. Only Store is required.
type Config struct {
	Store  store.Store
	Clock  clock.Clock        // nil: system clock
	IDs    domain.IDGenerator // nil: ULIDs from Clock
	Logger *slog.Logger       // nil: slog.Default()
	Rand   io.Reader          // nil: crypto/rand

	// BaseURL is the public URL of the instance (TIKKER_BASE_URL). An
	// https URL makes the session cookie Secure (and __Host- prefixed) even
	// when TLS is terminated by a reverse proxy; its origin is also trusted
	// by the CSRF check.
	BaseURL string
	// TrustedOrigins are additional origins (scheme://host[:port]) allowed
	// to send state-changing requests to the panel.
	TrustedOrigins []string
	// TrustedProxies lists reverse proxies whose X-Forwarded-For header is
	// believed when determining the client IP for login limits and audit.
	TrustedProxies []netip.Prefix

	// Password hashing (zero: DefaultParams, DefaultHashConcurrency).
	Params          Params
	HashConcurrency int

	// Sessions (zero: defaults above).
	SessionIdleTimeout     time.Duration
	SessionAbsoluteTimeout time.Duration
	TouchInterval          time.Duration
	JanitorInterval        time.Duration
	// LoginPath is where RequireSession redirects anonymous browsers.
	LoginPath string

	// Login limits (zero: DefaultLoginPerIP, DefaultLoginPerUser,
	// DefaultMaxLimiterEntries).
	LoginPerIP        Limit
	LoginPerUser      Limit
	MaxLimiterEntries int
}

// Service is the authentication service. It is safe for concurrent use.
type Service struct {
	st      store.Store
	clk     clock.Clock
	ids     domain.IDGenerator
	log     *slog.Logger
	rand    io.Reader
	hasher  *Hasher
	limiter *loginLimiter
	cop     *crossOrigin

	secureCookies  bool
	trustedProxies []netip.Prefix
	idle, absolute time.Duration
	touch, janitor time.Duration
	loginPath      string
}

// New builds a service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("auth: Store is required")
	}
	s := &Service{
		st: cfg.Store, clk: cfg.Clock, ids: cfg.IDs, log: cfg.Logger, rand: cfg.Rand,
		trustedProxies: cfg.TrustedProxies,
		idle:           orDefault(cfg.SessionIdleTimeout, DefaultSessionIdleTimeout),
		absolute:       orDefault(cfg.SessionAbsoluteTimeout, DefaultSessionAbsoluteTimeout),
		touch:          orDefault(cfg.TouchInterval, DefaultTouchInterval),
		janitor:        orDefault(cfg.JanitorInterval, DefaultJanitorInterval),
		loginPath:      cfg.LoginPath,
	}
	if s.clk == nil {
		s.clk = clock.System()
	}
	if s.ids == nil {
		s.ids = domain.NewULIDGenerator(s.clk)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.rand == nil {
		s.rand = rand.Reader
	}
	if s.loginPath == "" {
		s.loginPath = DefaultLoginPath
	}
	if s.idle > s.absolute {
		return nil, fmt.Errorf("auth: session idle timeout %s exceeds absolute timeout %s", s.idle, s.absolute)
	}
	perIP, perUser := cfg.LoginPerIP, cfg.LoginPerUser
	if perIP == (Limit{}) {
		perIP = DefaultLoginPerIP
	}
	if perUser == (Limit{}) {
		perUser = DefaultLoginPerUser
	}
	for _, l := range []Limit{perIP, perUser} {
		if l.Burst < 1 || l.Every <= 0 {
			return nil, fmt.Errorf("auth: invalid login limit %+v", l)
		}
	}
	maxEntries := cfg.MaxLimiterEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxLimiterEntries
	}
	s.limiter = newLoginLimiter(perIP, perUser, maxEntries)

	origins := cfg.TrustedOrigins
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("auth: invalid base URL %q", cfg.BaseURL)
		}
		s.secureCookies = u.Scheme == "https"
		origins = append([]string{u.Scheme + "://" + u.Host}, origins...)
	}
	var err error
	if s.cop, err = newCrossOrigin(origins); err != nil {
		return nil, err
	}
	if s.hasher, err = NewHasher(cfg.Params, cfg.HashConcurrency, s.rand); err != nil {
		return nil, err
	}
	return s, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Hasher returns the password hasher (e.g. for tooling).
func (s *Service) Hasher() *Hasher { return s.hasher }

// Run purges expired sessions and idle limiter entries every
// JanitorInterval until ctx is cancelled. Failures are logged.
func (s *Service) Run(ctx context.Context) {
	for {
		if _, err := s.PurgeExpiredSessions(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("purging expired sessions failed", "err", err.Error())
		}
		s.limiter.sweep(s.clk.Now())
		select {
		case <-ctx.Done():
			return
		case <-s.clk.After(s.janitor):
		}
	}
}

// PurgeExpiredSessions deletes sessions past their absolute or idle timeout
// and returns how many it removed.
func (s *Service) PurgeExpiredSessions(ctx context.Context) (int, error) {
	now := s.clk.Now()
	n, err := s.st.Sessions().DeleteExpired(ctx, now, now.Add(-s.idle))
	if err != nil {
		return 0, fmt.Errorf("auth: purge sessions: %w", err)
	}
	if n > 0 {
		s.log.Debug("expired sessions purged", "count", n)
	}
	return n, nil
}

// audit appends one audit record through repos (the store or a
// transaction).
func (s *Service) audit(ctx context.Context, repos store.Repos, actor domain.Actor, action domain.AuditAction, target string, meta map[string]string) error {
	err := repos.Audit().Append(ctx, domain.AuditLog{
		ID: s.ids.NewID(), Actor: actor, Action: action, Target: target, Meta: meta, At: s.clk.Now(),
	})
	if err != nil {
		return fmt.Errorf("auth: audit %s: %w", action, err)
	}
	return nil
}

func userTarget(id domain.ID) string  { return "user:" + string(id) }
func tokenTarget(id domain.ID) string { return "token:" + string(id) }

// public strips the password hash before a user leaves the package.
func public(u domain.User) domain.User {
	u.PasswordHash = ""
	return u
}

// ---------------------------------------------------------------------------
// Bootstrap

// BootstrapResult says what [Service.Bootstrap] did.
type BootstrapResult int

// Bootstrap results.
const (
	// BootstrapNotNeeded: users already exist; nothing was changed.
	BootstrapNotNeeded BootstrapResult = iota
	// BootstrapCreated: the admin was created from the configured password.
	BootstrapCreated
	// BootstrapSetupRequired: no users and no password configured; the
	// panel must show the first-run wizard (T30, [Service.CreateFirstAdmin]).
	BootstrapSetupRequired
)

func (r BootstrapResult) String() string {
	switch r {
	case BootstrapNotNeeded:
		return "not_needed"
	case BootstrapCreated:
		return "created"
	case BootstrapSetupRequired:
		return "setup_required"
	}
	return fmt.Sprintf("BootstrapResult(%d)", int(r))
}

// Bootstrap prepares the first admin at startup (SPEC §6). When no user
// exists and adminPassword (TIKKER_ADMIN_PASSWORD) is set, it creates the
// admin [BootstrapUsername] with that password; without a password it
// reports [BootstrapSetupRequired]. Once any user exists it does nothing,
// so it is safe to call on every start.
func (s *Service) Bootstrap(ctx context.Context, adminPassword string) (BootstrapResult, error) {
	n, err := s.st.Users().Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("auth: bootstrap: %w", err)
	}
	if n > 0 {
		return BootstrapNotNeeded, nil
	}
	if adminPassword == "" {
		s.log.Warn("no users exist; open the panel to create the first admin")
		return BootstrapSetupRequired, nil
	}
	u, err := s.createFirstUser(ctx, BootstrapUsername, adminPassword, domain.SystemActor)
	if errors.Is(err, ErrSetupCompleted) {
		return BootstrapNotNeeded, nil
	}
	if err != nil {
		return 0, fmt.Errorf("auth: bootstrap: %w", err)
	}
	s.log.Info("bootstrap admin created", "user_id", string(u.ID), "username", u.Username)
	return BootstrapCreated, nil
}

// SetupRequired reports whether no user exists yet (first-run wizard).
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	n, err := s.st.Users().Count(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: count users: %w", err)
	}
	return n == 0, nil
}

// CreateFirstAdmin creates the first admin from the first-run wizard. It
// fails with [ErrSetupCompleted] when any user exists, atomically with the
// insert, so two concurrent wizards cannot both succeed. The caller then
// usually calls [Service.StartSession].
func (s *Service) CreateFirstAdmin(ctx context.Context, username, password string) (domain.User, error) {
	u, err := s.createFirstUser(ctx, username, password, domain.Actor{})
	if err != nil {
		return domain.User{}, err
	}
	s.log.Info("first admin created by setup wizard", "user_id", string(u.ID), "username", u.Username)
	return public(u), nil
}

// createFirstUser validates, hashes and inserts the first admin; actor zero
// means the user itself.
func (s *Service) createFirstUser(ctx context.Context, username, password string, actor domain.Actor) (domain.User, error) {
	var v validation
	validateUsername(&v, "/username", username)
	if err := ValidatePassword("/password", password); err != nil {
		var ve *ValidationError
		errors.As(err, &ve)
		v = append(v, ve.Errors...)
	}
	if err := v.err(); err != nil {
		return domain.User{}, err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return domain.User{}, err
	}
	u := domain.User{ID: s.ids.NewID(), Username: username, PasswordHash: hash, Role: domain.RoleAdmin, CreatedAt: s.clk.Now()}
	if actor.IsZero() {
		actor = domain.UserActor(u.ID)
	}
	err = s.st.WithTx(ctx, func(tx store.Repos) error {
		n, err := tx.Users().Count(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrSetupCompleted
		}
		if err := tx.Users().Create(ctx, u); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, domain.AuditUserCreate, userTarget(u.ID),
			map[string]string{"username": u.Username, "role": string(u.Role), "via": "bootstrap"})
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("auth: create first admin: %w", err)
	}
	return u, nil
}

// ---------------------------------------------------------------------------
// Users

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{3,64}$`)

func validateUsername(v *validation, field, username string) {
	if username == "" {
		v.add(field, CodeRequired, "username is required")
	} else if !usernameRE.MatchString(username) {
		v.add(field, CodeInvalidFormat, "username must be 3-64 characters of A-Z, a-z, 0-9, '.', '_' or '-'")
	}
}

// safeUsername returns username if it looks like a username, for logs and
// audit of failed logins (a password typed into the username field must not
// end up there), or "" otherwise.
func safeUsername(username string) string {
	if usernameRE.MatchString(username) {
		return username
	}
	return ""
}

// NewUser is the input of [Service.CreateUser].
type NewUser struct {
	Username string
	Password string
	Role     domain.Role
	Disabled bool
}

// CreateUser validates and stores a new user and audits it. A duplicate
// username yields domain.ErrConflict. The returned user has no
// PasswordHash.
func (s *Service) CreateUser(ctx context.Context, actor domain.Actor, in NewUser) (domain.User, error) {
	var v validation
	validateUsername(&v, "/username", in.Username)
	if err := ValidatePassword("/password", in.Password); err != nil {
		var ve *ValidationError
		errors.As(err, &ve)
		v = append(v, ve.Errors...)
	}
	if !in.Role.Valid() {
		v.add("/role", CodeInvalidFormat, "role must be admin, operator or viewer")
	}
	if err := v.err(); err != nil {
		return domain.User{}, err
	}
	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return domain.User{}, err
	}
	u := domain.User{ID: s.ids.NewID(), Username: in.Username, PasswordHash: hash, Role: in.Role,
		Disabled: in.Disabled, CreatedAt: s.clk.Now()}
	err = s.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Users().Create(ctx, u); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, domain.AuditUserCreate, userTarget(u.ID), map[string]string{
			"username": u.Username, "role": string(u.Role), "disabled": fmt.Sprint(u.Disabled),
		})
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("auth: create user: %w", err)
	}
	return public(u), nil
}

// UserChanges is the input of [Service.UpdateUser]; nil fields are left
// unchanged (JSON Merge Patch).
type UserChanges struct {
	Password *string
	Role     *domain.Role
	Disabled *bool
}

// UpdateUser applies changes to a user and audits them. A change of the
// password, role or status ends every session of the user (the caller may
// start a fresh one for the current user with [Service.StartSession]).
// Demoting or disabling the last enabled admin yields domain.ErrConflict;
// an unknown ID domain.ErrNotFound. The returned user has no PasswordHash.
func (s *Service) UpdateUser(ctx context.Context, actor domain.Actor, id domain.ID, ch UserChanges) (domain.User, error) {
	var v validation
	if ch.Password != nil {
		if err := ValidatePassword("/password", *ch.Password); err != nil {
			var ve *ValidationError
			errors.As(err, &ve)
			v = append(v, ve.Errors...)
		}
	}
	if ch.Role != nil && !ch.Role.Valid() {
		v.add("/role", CodeInvalidFormat, "role must be admin, operator or viewer")
	}
	if err := v.err(); err != nil {
		return domain.User{}, err
	}
	var hash string
	if ch.Password != nil {
		var err error
		if hash, err = s.hasher.Hash(ctx, *ch.Password); err != nil {
			return domain.User{}, err
		}
	}
	var out domain.User
	var ended int
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		u, err := tx.Users().Get(ctx, id)
		if err != nil {
			return err
		}
		meta := map[string]string{}
		if ch.Password != nil {
			u.PasswordHash = hash
			meta["password"] = "changed"
		}
		if ch.Role != nil && *ch.Role != u.Role {
			meta["role"] = string(u.Role) + "->" + string(*ch.Role)
			u.Role = *ch.Role
		}
		if ch.Disabled != nil && *ch.Disabled != u.Disabled {
			meta["disabled"] = fmt.Sprint(*ch.Disabled)
			u.Disabled = *ch.Disabled
		}
		out = u
		if len(meta) == 0 {
			return nil
		}
		if err := tx.Users().Update(ctx, u); err != nil {
			return err
		}
		if ended, err = tx.Sessions().DeleteByUser(ctx, u.ID); err != nil {
			return err
		}
		if ended > 0 {
			meta["sessions_ended"] = fmt.Sprint(ended)
		}
		return s.audit(ctx, tx, actor, domain.AuditUserUpdate, userTarget(u.ID), meta)
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("auth: update user: %w", err)
	}
	if ended > 0 {
		s.log.Info("sessions ended after account change", "user_id", string(id), "count", ended)
	}
	return public(out), nil
}

// ---------------------------------------------------------------------------
// API tokens

// NewToken is the input of [Service.CreateToken].
type NewToken struct {
	UserID    domain.ID // owner; the API defaults it to the caller's user
	Name      string
	Scopes    domain.Scopes
	ExpiresAt time.Time // zero: never expires
}

const maxTokenNameLen = 64

// CreateToken validates and stores a new API token and audits it. It
// returns the stored token and the full token string, which must be shown
// to the user once and never stored or logged. Scopes may not exceed the
// owner's role (code scope_exceeds_role).
func (s *Service) CreateToken(ctx context.Context, actor domain.Actor, in NewToken) (domain.APIToken, string, error) {
	now := s.clk.Now()
	var v validation
	if n := utf8.RuneCountInString(strings.TrimSpace(in.Name)); n == 0 {
		v.add("/name", CodeRequired, "name is required")
	} else if n > maxTokenNameLen || !utf8.ValidString(in.Name) {
		v.add("/name", CodeOutOfRange, "name must be at most %d characters", maxTokenNameLen)
	}
	if !in.ExpiresAt.IsZero() && !in.ExpiresAt.After(now) {
		v.add("/expires_at", CodeExpiresAtInPast, "expires_at must be in the future")
	}
	var owner domain.User
	if in.UserID.IsZero() {
		v.add("/user_id", CodeRequired, "owner is required")
	} else {
		var err error
		owner, err = s.st.Users().Get(ctx, in.UserID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			v.add("/user_id", CodeUnknownUser, "user does not exist")
		case err != nil:
			return domain.APIToken{}, "", fmt.Errorf("auth: create token: %w", err)
		}
	}
	if len(in.Scopes) == 0 {
		v.add("/scopes", CodeRequired, "at least one scope is required")
	}
	seen := map[domain.Scope]bool{}
	for i, sc := range in.Scopes {
		field := fmt.Sprintf("/scopes/%d", i)
		switch {
		case !sc.Valid():
			v.add(field, CodeInvalidFormat, "unknown scope")
		case seen[sc]:
			v.add(field, CodeInvalidFormat, "duplicate scope %q", sc)
		case owner.ID != "" && !owner.Role.Scopes().Allows(sc):
			v.add(field, CodeScopeExceedsRole, "scope %q exceeds the owner's role %q", sc, owner.Role)
		}
		seen[sc] = true
	}
	if err := v.err(); err != nil {
		return domain.APIToken{}, "", err
	}

	const attempts = 3 // prefix collisions are astronomically rare
	for i := 0; ; i++ {
		full, prefix, hash, err := newToken(s.rand)
		if err != nil {
			return domain.APIToken{}, "", err
		}
		t := domain.APIToken{ID: s.ids.NewID(), UserID: owner.ID, Name: in.Name, Prefix: prefix, Hash: hash,
			Scopes: append(domain.Scopes(nil), in.Scopes...), ExpiresAt: in.ExpiresAt, CreatedAt: now}
		err = s.st.WithTx(ctx, func(tx store.Repos) error {
			if err := tx.Tokens().Create(ctx, t); err != nil {
				return err
			}
			meta := map[string]string{"name": t.Name, "prefix": t.Prefix, "owner": string(owner.ID), "scopes": scopesString(t.Scopes)}
			if !t.ExpiresAt.IsZero() {
				meta["expires_at"] = t.ExpiresAt.UTC().Format(time.RFC3339)
			}
			return s.audit(ctx, tx, actor, domain.AuditTokenCreate, tokenTarget(t.ID), meta)
		})
		if errors.Is(err, domain.ErrConflict) && i+1 < attempts {
			continue
		}
		if err != nil {
			return domain.APIToken{}, "", fmt.Errorf("auth: create token: %w", err)
		}
		s.log.Info("api token created", "token_id", string(t.ID), "prefix", t.Prefix, "owner", string(owner.ID))
		return t, full, nil
	}
}

func scopesString(ss domain.Scopes) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = string(s)
	}
	return strings.Join(parts, " ")
}

// DeleteToken revokes a token and audits it; an unknown ID yields
// domain.ErrNotFound. Requests with the token fail from now on.
func (s *Service) DeleteToken(ctx context.Context, actor domain.Actor, id domain.ID) error {
	err := s.st.WithTx(ctx, func(tx store.Repos) error {
		if err := tx.Tokens().Delete(ctx, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, domain.AuditTokenDelete, tokenTarget(id), nil)
	})
	if err != nil {
		return fmt.Errorf("auth: delete token: %w", err)
	}
	s.log.Info("api token revoked", "token_id", string(id))
	return nil
}
