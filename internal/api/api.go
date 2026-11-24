// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package api implements the REST API of tikker, /api/v1 (SPEC §5), as
// defined by api/openapi.yaml, which is the source of truth: handlers adapt to
// the contract, never the other way round. It uses only net/http (Go 1.22
// method patterns).
//
// # Wiring
//
//	h, err := api.New(api.Deps{
//		Store: st, Auth: authSvc, Clock: clk, IDs: ids, Logger: log,
//		Dispatcher: disp,       // *queue.Dispatcher: Notify() after Enqueue
//		Reloader: sync,         // *api.ChannelSync: Reload(ctx) after device/channel changes
//		Capabilities: sync,     // *api.ChannelSync: connector capabilities per channel
//		Health: checker,        // optional, *health.Checker: live state of channels
//		Credentials: creds,     // *secrets.DeviceCredentials
//		Connectors: registry,   // *connector.Registry, for POST /devices/{id}/test
//		Options: api.Options{DefaultRegion: cfg.DefaultRegion, ...},
//	})
//	mux.Handle("/api/v1/", h)   // h expects the full path, including /api/v1
//
// /healthz and /readyz are not served here (package probes).
//
// # Conventions
//
//   - Errors are RFC 9457 problems (problem.go) with the types and codes of
//     the contract; internal errors are logged and reported without details.
//   - Every request needs a Bearer token (auth.Service.RequireToken). The
//     per-token request budget (ratelimit.go) is charged after authentication
//     and before the scope check; every response of an authenticated request
//     carries RateLimit-* headers.
//   - Request bodies are decoded strictly (unknown members are 400, like
//     additionalProperties: false); PATCH is JSON Merge Patch (RFC 7396).
//   - Responses are built from DTOs mapped field by field from the domain
//     entities (dto.go); entities are never serialised, so secrets
//     (password hashes, token hashes, encrypted router passwords) cannot leak.
//
// # Choosing the channel capabilities for a message (POST /messages)
//
// Validation, channel selection and planning of the text are done by package
// sending, which the web panel uses too: the text is planned once, at write
// time, with sms.Plan, and the plan must be valid for every channel that may
// finally send the message (see the package documentation of sending for the
// routing rules and the strictest-capabilities rule).
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/sms"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/webhook"
)

// Prefix is the path prefix of the API; the handler returned by [New]
// expects it in request paths.
const Prefix = "/api/v1"

// Defaults of [Options].
const (
	DefaultMaxBodyBytes   = 2 << 20 // 100 messages of 1600 characters, escaped
	DefaultIdempotencyTTL = 24 * time.Hour
	DefaultProbeTimeout   = 10 * time.Second
	// MaxBatchSize is the largest POST /messages/batch (contract).
	MaxBatchSize = 100
)

// Auth is the part of auth.Service the API uses; *auth.Service implements it.
type Auth interface {
	// RequireToken authenticates Bearer tokens (401 problems) and puts the
	// auth.Principal into the request context. It is called without scopes:
	// the API checks the scope itself, after rate limiting.
	RequireToken(scopes ...domain.Scope) func(http.Handler) http.Handler
	CreateUser(ctx context.Context, actor domain.Actor, in auth.NewUser) (domain.User, error)
	UpdateUser(ctx context.Context, actor domain.Actor, id domain.ID, ch auth.UserChanges) (domain.User, error)
	CreateToken(ctx context.Context, actor domain.Actor, in auth.NewToken) (domain.APIToken, string, error)
	DeleteToken(ctx context.Context, actor domain.Actor, id domain.ID) error
}

// Notifier wakes the queue after messages were enqueued; *queue.Dispatcher
// implements it.
type Notifier interface{ Notify() }

// ChannelReloader rebuilds what depends on the stored devices and channels
// (the dispatcher's and the health checker's channel specs, connectors, inbox
// pollers) after a device or channel was created, changed or deleted. The API
// calls it after the change was committed; a failure is logged and does not
// fail the request. *ChannelSync implements it.
type ChannelReloader interface {
	Reload(ctx context.Context) error
}

// CapabilityProvider reports what the connector that serves a channel can
// send. ok is false when the channel has no usable connector (for example its
// stored password does not decrypt). *ChannelSync implements it.
type CapabilityProvider interface {
	ChannelCapabilities(id domain.ID) (caps connector.Capabilities, ok bool)
}

// HealthView overlays the live health on a stored channel; *health.Checker
// implements it. Optional.
type HealthView interface {
	ApplyTo(ch *domain.Channel) bool
}

// Credentials encrypts device passwords and builds connector configurations;
// *secrets.DeviceCredentials implements it.
type Credentials interface {
	Encrypt(id domain.ID, p secrets.Password) ([]byte, error)
	EncryptAgentToken(id domain.ID, token secrets.Password) ([]byte, error)
	DecryptAgentToken(d domain.Device) (secrets.Password, error)
	ConnectorConfig(d domain.Device) (connector.DeviceConfig, error)
}

// ConnectorFactory builds a connector for one device (used by
// POST /devices/{id}/test); *connector.Registry implements it.
type ConnectorFactory interface {
	New(ctx context.Context, cfg connector.DeviceConfig) (connector.Connector, error)
}

// Deps are the collaborators of the API. Every field except Health, IDs,
// Clock and Logger is required.
type Deps struct {
	Store        store.Store
	Auth         Auth
	Dispatcher   Notifier
	Reloader     ChannelReloader
	Capabilities CapabilityProvider
	Health       HealthView // optional
	Credentials  Credentials
	Connectors   ConnectorFactory

	// Sending is the outbound message service; when nil the API builds its
	// own from the other dependencies and Options. The application passes
	// the one it shares with the web panel.
	Sending *sending.Service

	// Admin administers devices and channels; when nil the API builds its
	// own from the other dependencies and Options. The application passes
	// the one it shares with the web panel.
	Admin *admin.Service

	// Webhooks administers webhooks. Optional: without it the /webhooks
	// routes are not served.
	Webhooks *webhook.Service
	// Limiter, if set, is the per-token budget shared with other entry
	// points (the SMTP gateway); nil: a limiter from Options.RateLimit.
	Limiter *TokenLimiter

	Clock  clock.Clock        // default: system clock
	IDs    domain.IDGenerator // default: ULIDs from Clock
	Logger *slog.Logger       // default: slog.Default()

	Options Options
}

// Options are the settings of the API (mostly from internal/config).
type Options struct {
	// DefaultRegion is the region of numbers without a country prefix
	// (TIKKER_DEFAULT_REGION); empty means "PL".
	DefaultRegion string
	// AllowShortNumbers accepts short codes and alphanumeric numbers
	// (TIKKER_ALLOW_SHORT_NUMBERS).
	AllowShortNumbers bool
	// LongMessageStrategy is TIKKER_LONG_MESSAGE_STRATEGY; empty means reject.
	LongMessageStrategy sms.Strategy
	// AllowedHTTPHosts lists the hosts a device base URL may use with plain
	// http:// (SPEC §3.3: the veth gateway of container mode). Entries are
	// IP addresses, CIDR prefixes or host names; names match literally, no DNS
	// lookup is made. Empty: http:// is never allowed.
	AllowedHTTPHosts []string
	// RateLimit is the per-token request budget; the zero value means
	// 120 requests per minute, a negative Requests disables the limit.
	RateLimit RateLimit
	// MaxBodyBytes limits request bodies (default DefaultMaxBodyBytes).
	MaxBodyBytes int64
	// IdempotencyTTL is how long an Idempotency-Key is remembered
	// (default 24 h, contract).
	IdempotencyTTL time.Duration
	// ProbeTimeout bounds POST /devices/{id}/test (default 10 s, contract).
	ProbeTimeout time.Duration
}

// API serves /api/v1.
type API struct {
	st    store.Store
	auth  Auth
	send  *sending.Service
	admin *admin.Service

	webhooks *webhook.Service // nil: no /webhooks routes
	hv       HealthView
	creds    Credentials
	conns    ConnectorFactory
	clk      clock.Clock
	ids      domain.IDGenerator
	log      *slog.Logger
	opts     Options

	limiter  *limiter
	inflight *inflight
	mux      *http.ServeMux
}

// New validates deps and returns the handler of /api/v1.
func New(deps Deps) (http.Handler, error) {
	a, err := newAPI(deps)
	if err != nil {
		return nil, err
	}
	return a.recoverer(a.mux), nil
}

func newAPI(deps Deps) (*API, error) {
	var missing []string
	for _, d := range []struct {
		name string
		nil  bool
	}{
		{"Store", deps.Store == nil}, {"Auth", deps.Auth == nil}, {"Dispatcher", deps.Dispatcher == nil},
		{"Reloader", deps.Reloader == nil}, {"Capabilities", deps.Capabilities == nil},
		{"Credentials", deps.Credentials == nil}, {"Connectors", deps.Connectors == nil},
	} {
		if d.nil {
			missing = append(missing, d.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("api: missing dependencies: %s", strings.Join(missing, ", "))
	}
	a := &API{
		st: deps.Store, auth: deps.Auth, admin: deps.Admin, webhooks: deps.Webhooks,
		hv: deps.Health, creds: deps.Credentials, conns: deps.Connectors,
		clk: deps.Clock, ids: deps.IDs, log: deps.Logger, opts: deps.Options,
		inflight: newInflight(),
	}
	if a.clk == nil {
		a.clk = clock.System()
	}
	if a.ids == nil {
		a.ids = domain.NewULIDGenerator(a.clk)
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	a.log = a.log.With(slog.String("component", "api"))
	if a.opts.DefaultRegion == "" {
		a.opts.DefaultRegion = "PL"
	}
	if a.opts.MaxBodyBytes <= 0 {
		a.opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if a.opts.IdempotencyTTL <= 0 {
		a.opts.IdempotencyTTL = DefaultIdempotencyTTL
	}
	if a.opts.ProbeTimeout <= 0 {
		a.opts.ProbeTimeout = DefaultProbeTimeout
	}
	if _, err := sms.ParseStrategy(string(a.opts.LongMessageStrategy)); err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	var err error
	if a.admin == nil {
		a.admin, err = admin.New(admin.Deps{
			Store: a.st, Credentials: a.creds, Connectors: a.conns, Reloader: deps.Reloader, Health: deps.Health,
			Clock: a.clk, IDs: a.ids, Logger: a.log,
			Options: admin.Options{AllowedHTTPHosts: a.opts.AllowedHTTPHosts, ProbeTimeout: a.opts.ProbeTimeout},
		})
		if err != nil {
			return nil, fmt.Errorf("api: %w", err)
		}
	}
	if a.send = deps.Sending; a.send == nil {
		a.send, err = sending.New(sending.Deps{
			Store: a.st, Capabilities: deps.Capabilities, Notifier: deps.Dispatcher, Clock: a.clk, IDs: a.ids,
			Options: sending.Options{
				DefaultRegion: a.opts.DefaultRegion, AllowShortNumbers: a.opts.AllowShortNumbers,
				LongMessageStrategy: a.opts.LongMessageStrategy,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("api: %w", err)
		}
	}
	if deps.Limiter != nil {
		a.limiter = deps.Limiter.l
	} else {
		a.limiter = newLimiter(a.opts.RateLimit, a.clk)
	}
	a.mux = http.NewServeMux()
	a.routes()
	return a, nil
}

// handlerFunc is an operation: it returns an error to report a problem.
type handlerFunc func(w http.ResponseWriter, r *http.Request, p auth.Principal) error

func (a *API) routes() {
	send, read, admin := domain.ScopeSMSSend, domain.ScopeSMSRead, domain.ScopeAdmin
	r := func(pattern string, scope domain.Scope, h handlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		a.mux.Handle(method+" "+Prefix+path, a.route(scope, h))
	}
	r("POST /messages", send, a.sendMessage)
	r("POST /messages/batch", send, a.sendBatch)
	r("GET /messages", read, a.listMessages)
	r("GET /messages/{id}", read, a.getMessage)
	r("DELETE /messages/{id}", send, a.cancelMessage)

	r("GET /channels", read, a.listChannels)
	r("POST /channels", admin, a.createChannel)
	r("GET /channels/{id}", read, a.getChannel)
	r("PATCH /channels/{id}", admin, a.updateChannel)
	r("DELETE /channels/{id}", admin, a.deleteChannel)

	r("GET /devices", admin, a.listDevices)
	r("POST /devices", admin, a.createDevice)
	r("GET /devices/{id}", admin, a.getDevice)
	r("PATCH /devices/{id}", admin, a.updateDevice)
	r("DELETE /devices/{id}", admin, a.deleteDevice)
	r("POST /devices/{id}/test", admin, a.testDevice)

	r("GET /tokens", admin, a.listTokens)
	r("POST /tokens", admin, a.createToken)
	r("GET /tokens/{id}", admin, a.getToken)
	r("DELETE /tokens/{id}", admin, a.revokeToken)

	r("GET /users", admin, a.listUsers)
	r("POST /users", admin, a.createUser)
	r("GET /users/{id}", admin, a.getUser)
	r("PATCH /users/{id}", admin, a.updateUser)

	if a.webhooks != nil {
		r("GET /webhooks", admin, a.listWebhooks)
		r("POST /webhooks", admin, a.createWebhook)
		r("GET /webhooks/{id}", admin, a.getWebhook)
		r("PATCH /webhooks/{id}", admin, a.updateWebhook)
		r("DELETE /webhooks/{id}", admin, a.deleteWebhook)
	}

	a.mux.HandleFunc("/", a.notFoundOrMethod)
}

// route wraps h: token authentication, request budget, scope check, error
// reporting. Order matters: a request with an invalid token is 401 and costs
// nothing; a valid token is charged even if it lacks the scope, so scanning
// forbidden endpoints is limited too.
func (a *API) route(scope domain.Scope, h handlerFunc) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p.Token == nil {
			writeProblem(w, r, newProblem(http.StatusUnauthorized, problemUnauthorized, "Missing, invalid, expired or revoked API token."))
			return
		}
		st := a.limiter.take(p.Token.ID)
		if st.Limit > 0 {
			hd := w.Header()
			hd.Set("RateLimit-Limit", strconv.Itoa(st.Limit))
			hd.Set("RateLimit-Remaining", strconv.Itoa(st.Remaining))
			hd.Set("RateLimit-Reset", strconv.Itoa(st.ResetSecs))
		}
		if !st.Allowed {
			e := newProblem(http.StatusTooManyRequests, problemRateLimited,
				fmt.Sprintf("Rate limit of %d requests per %d s exceeded; retry in %d s.",
					st.Limit, int(a.limiter.cfg.Window.Seconds()), st.RetryAfter))
			writeProblem(w, r, e.withHeader("Retry-After", strconv.Itoa(st.RetryAfter)))
			return
		}
		if !p.Can(scope) {
			writeProblem(w, r, newProblem(http.StatusForbidden, problemForbidden,
				fmt.Sprintf("Token lacks required scope %q.", string(scope))))
			return
		}
		if err := h(w, r, p); err != nil {
			a.fail(w, r, err)
		}
	})
	return a.auth.RequireToken()(inner)
}

// fail reports err: problems as they are, everything else as a 500 whose
// details only reach the log.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		if ae.status >= 500 {
			a.log.Error("request failed", slog.String("path", r.URL.Path), slog.Any("error", err))
		}
		writeProblem(w, r, ae)
		return
	}
	reqID := a.ids.NewID()
	a.log.Error("internal error", slog.String("request_id", string(reqID)),
		slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("error", err))
	e := newProblem(http.StatusInternalServerError, problemInternal, "")
	e.instance = requestInstancePrefix + string(reqID)
	writeProblem(w, r, e)
}

// recoverer turns a panic of a handler into a logged 500.
func (a *API) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel panic value, compared by identity
					panic(v)
				}
				a.log.Error("panic in handler", slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
				a.fail(w, r, fmt.Errorf("panic: %v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// notFoundOrMethod answers what the mux did not route: 405 with Allow for a
// known path with the wrong method, 404 otherwise.
func (a *API) notFoundOrMethod(w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		if m == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = m
		if _, pattern := a.mux.Handler(probe); pattern != "" && pattern != "/" {
			allow = append(allow, m)
		}
	}
	if len(allow) > 0 {
		e := newProblem(http.StatusMethodNotAllowed, problemMethodNotAllow, "Method "+r.Method+" is not allowed for this resource.")
		writeProblem(w, r, e.withHeader("Allow", strings.Join(allow, ", ")))
		return
	}
	writeProblem(w, r, notFound("Resource"))
}

// actorOf is the actor recorded for audit entries and message authorship.
func actorOf(p auth.Principal) domain.Actor { return p.Actor() }
