// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package web is the tikker web panel: server-rendered HTML (html/template)
// with htmx and Pico CSS embedded in the binary, no external requests.
//
// # Layout of the package
//
//	web.go          Server, Options, routing, Handle
//	render.go       templates, View, Render/RenderBlock, error pages
//	nav.go          navigation items and their minimum roles
//	pages_auth.go   login, logout, first-run wizard, language switch
//	pages.go        registerPages: one register function per page file
//	pages_dashboard.go  dashboard and its htmx fragment (T31)
//	pages_send.go   send form, live segment preview (T31)
//	pages_messages.go   message list, details, cancel (T31)
//	pages_admin.go  helpers of the admin pages: field errors, confirmation page
//	pages_devices.go    devices: list, form, connection test, pin fingerprint (T32)
//	pages_channels.go   channels: edit, toggle, check now, delete (T32)
//	pages_tokens.go     API tokens, one-time display of the secret (T32)
//	pages_users.go      users, roles, own profile and password (T32)
//	pages_audit.go      audit log with filters (T32)
//	format.go       helpers shared by the pages (status labels, signal)
//	security.go     security headers (CSP, X-Frame-Options, ...)
//	static.go       /static/ (embedded assets, ETag, immutable cache)
//	templates/      layout/*.html (base + partials), pages/*.html (one per page)
//	static/         htmx, Pico CSS, app.css, app.js, LICENSES/, VENDOR.md
//	i18n/           pl.json, en.json
//
// # Adding a page (T31, T32)
//
//  1. Write a handler (http.Handler or func(w, r)) in a new file. Read the
//     signed-in user with [auth.PrincipalFrom]; render with [Server.Render]
//     (whole page) or [Server.RenderBlock] (a named template, for htmx
//     fragments). Read the CSRF token from the View, never build it by hand.
//  2. Write templates/pages/<name>.html. It defines the templates "title" and
//     "content" (see templates/pages/confirm.html); inside, `.` is the [View],
//     `.Data` is what the handler passed. Use {{t "key"}} for every text and
//     {{.CSRF}} in a hidden "csrf_token" field of every POST form. Extra
//     named templates defined in the file (for htmx fragments) are available
//     to RenderBlock. Shared pieces go to templates/layout/.
//  3. Add the Polish and English texts to i18n/pl.json and i18n/en.json (the
//     tests fail when a language lacks a key or a template uses an unknown
//     key).
//  4. Register the route from a register function called by registerPages
//     (pages.go): s.Handle("GET /messages", domain.RoleViewer, handler).
//     Registering a pattern again replaces the earlier handler.
//     The route is wrapped in [auth.Service.RequireSession] (anonymous users
//     are redirected to the login page, or get HX-Redirect for htmx) and a
//     role check that answers 403 with a localised page. State-changing
//     routes (POST, PUT, PATCH, DELETE) are covered by [auth.Service.CSRF]
//     automatically; htmx requests get the X-CSRF-Token header from app.js.
//  5. Navigation entries live in nav.go; the own profile (/profile, any
//     role) is linked from the user name in the header.
//
// Pages that change devices and channels use [admin.Service], the one the
// REST API uses; users and tokens go through [auth.Service]. Forms are plain
// POSTs answered by a redirect (success) or the page again with the typed
// values and a message per field (the password fields are always empty).
// htmx only swaps fragments (RenderBlock): the device test result, the
// pre-filled settings form of "pin this fingerprint", the channel check.
//
// Content-Security-Policy forbids inline scripts, inline styles and
// javascript: URLs: put scripts in static/ and styles in static/app.css, and
// use no style="..." or on...="..." attributes. htmx runs with allowEval and
// allowScriptTags off, so hx-on:*, hx-vals="js:..." and responses with
// <script> do not work.
package web

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/queue"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/version"
	"github.com/m4drak/tikker/internal/web/i18n"
)

// DefaultSourceURL is the link to the source code shown in the footer (the
// GPL asks for an offer of the source of a network service's users).
const DefaultSourceURL = "https://github.com/m4drak/tikker"

// maxFormBytes bounds the body of the panel's own forms.
const maxFormBytes = 64 << 10

// QueueStats gives the dashboard the dispatcher's counters;
// *queue.Dispatcher implements it.
type QueueStats interface{ Stats() queue.Stats }

// HealthSnapshot gives the dashboard the live health of the channels;
// *health.Checker implements it.
type HealthSnapshot interface{ Snapshot() []health.ChannelStatus }

// Options configures [New].
type Options struct {
	// Auth is the authentication service (required).
	Auth *auth.Service
	// Store is read by the dashboard and the message pages (required).
	Store store.Store
	// Sending sends and cancels messages and previews texts; the API uses
	// the same service (required).
	Sending *sending.Service
	// Admin administers devices and channels; the API uses the same service
	// (required).
	Admin *admin.Service
	// Clock is the time source of the admin pages (default: system clock).
	Clock clock.Clock
	// Queue and Health feed the dashboard (required).
	Queue  QueueStats
	Health HealthSnapshot
	// Logger receives the panel's logs (default slog.Default()).
	Logger *slog.Logger
	// BaseURL is the public URL of the instance (TIKKER_BASE_URL). An https
	// URL marks the language cookie Secure and enables HSTS.
	BaseURL string
	// SourceURL overrides [DefaultSourceURL].
	SourceURL string
}

// Server is the web panel. Create it with [New] and mount [Server.Handler]
// at "/".
type Server struct {
	auth      *auth.Service
	st        store.Store
	send      *sending.Service
	admin     *admin.Service
	clk       clock.Clock
	queue     QueueStats
	health    HealthSnapshot
	log       *slog.Logger
	i18n      *i18n.Bundle
	pages     map[string]map[string]*template.Template // language -> page -> template set
	assets    *assets
	https     bool
	sourceURL string
	build     string

	routes []route
	mux    *http.ServeMux
}

type route struct {
	pattern string
	role    domain.Role
	handler http.Handler
}

// New builds the panel: it loads and checks the templates, the translations
// and the embedded assets and registers the routes.
func New(opts Options) (*Server, error) {
	for _, m := range []struct {
		name string
		nil  bool
	}{
		{"Auth", opts.Auth == nil}, {"Store", opts.Store == nil}, {"Sending", opts.Sending == nil}, {"Admin", opts.Admin == nil},
		{"Queue", opts.Queue == nil}, {"Health", opts.Health == nil},
	} {
		if m.nil {
			return nil, fmt.Errorf("web: %s is required", m.name)
		}
	}
	s := &Server{
		auth:      opts.Auth,
		st:        opts.Store,
		send:      opts.Sending,
		admin:     opts.Admin,
		clk:       opts.Clock,
		queue:     opts.Queue,
		health:    opts.Health,
		log:       opts.Logger,
		https:     strings.HasPrefix(strings.ToLower(opts.BaseURL), "https://"),
		sourceURL: opts.SourceURL,
		build:     buildString(),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.clk == nil {
		s.clk = clock.System()
	}
	if s.sourceURL == "" {
		s.sourceURL = DefaultSourceURL
	}
	var err error
	if s.i18n, err = i18n.Load(); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	if s.assets, err = loadAssets(); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	if s.pages, err = s.loadTemplates(); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	s.registerPages()
	s.mux = s.buildMux()
	return s, nil
}

// buildString is the footer's version: "dev" or "1.2.3 (abc1234)".
func buildString() string {
	if version.Commit == "" || version.Commit == "none" {
		return version.Version
	}
	return fmt.Sprintf("%s (%s)", version.Version, version.Commit)
}

// Handle registers a panel page. pattern is a net/http ServeMux pattern with
// method ("GET /messages"); role is the minimum role ("" for any signed-in
// user). Registering a pattern again replaces the earlier handler (this is
// how the placeholders are replaced). It must be called before [New]
// returns, i.e. from registerPages.
func (s *Server) Handle(pattern string, role domain.Role, h http.Handler) {
	for i := range s.routes {
		if s.routes[i].pattern == pattern {
			s.routes[i] = route{pattern, role, h}
			return
		}
	}
	s.routes = append(s.routes, route{pattern, role, h})
}

// HandleFunc is [Server.Handle] for a function.
func (s *Server) HandleFunc(pattern string, role domain.Role, h http.HandlerFunc) {
	s.Handle(pattern, role, h)
}

// Handler returns the panel: every response carries the security headers and
// state-changing requests pass the CSRF check.
func (s *Server) Handler() http.Handler {
	return s.securityHeaders(limitPanelBody(s.auth.CSRF(s.mux)))
}

func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/", s.serveStatic)
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /setup", s.setupForm)
	mux.HandleFunc("POST /setup", s.setupSubmit)
	mux.HandleFunc("POST /lang", s.setLanguage)
	for _, rt := range s.routes {
		mux.Handle(rt.pattern, s.protect(rt.role, rt.handler))
	}
	mux.HandleFunc("/", s.notFound)
	return mux
}

// protect requires a session and at least role. A signed-in user with too
// low a role gets a localised 403 page.
func (s *Server) protect(role domain.Role, h http.Handler) http.Handler {
	return s.auth.RequireSession("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.PrincipalFrom(r.Context()); !ok || (role != "" && !p.User.Role.Allows(role)) {
			s.errorPage(w, r, http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	}))
}

// limitPanelBody bounds every request body before the CSRF middleware parses
// the form, so oversized POSTs cannot bypass maxFormBytes.
func limitPanelBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		}
		next.ServeHTTP(w, r)
	})
}
