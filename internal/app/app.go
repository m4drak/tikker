// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package app assembles the tikker server (`tikker serve`) from its
// components and runs it until shutdown.
//
// # Start-up
//
//	config -> data directory -> secret key -> SQLite (migrations)
//	-> check that the key opens every stored device password
//	-> auth (bootstrap admin) -> connector registry (routeros)
//	-> health checker, dispatcher, channel sync, inbox pollers
//	-> /readyz checks -> HTTP server -> retention and session janitor
//
// The dispatcher, the health checker and the channel sync refer to each other
// (the health checker sets channel states in the dispatcher, the dispatcher
// reports send health to the checker and hands released connectors back to the
// sync), so the cycles are closed with small forwarding adapters.
//
// # Shutdown
//
// When the context given to [App.Run] ends: the HTTP server stops accepting
// requests and lets running ones finish, the inbox pollers stop, the dispatcher
// finishes in-flight sends (ADR 0002: what does not finish in time is requeued
// as a possible duplicate), the health checker flushes its state, connectors
// are closed and the database is closed. Every step is bounded by
// config.ShutdownTimeout in total.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/agent"
	"github.com/m4drak/tikker/internal/api"
	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/config"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/routeros"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/metrics"
	"github.com/m4drak/tikker/internal/probes"
	"github.com/m4drak/tikker/internal/queue"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/sms"
	"github.com/m4drak/tikker/internal/store/sqlite"
	"github.com/m4drak/tikker/internal/version"
	"github.com/m4drak/tikker/internal/web"
	"github.com/m4drak/tikker/internal/webhook"
)

// DatabaseFile is the SQLite file name inside the data directory.
const DatabaseFile = "tikker.db"

// HTTP server limits. Request handlers are short (the longest is a device
// test, bounded by api.DefaultProbeTimeout), so the write timeout leaves
// plenty of room.
const (
	ReadHeaderTimeout = 10 * time.Second
	ReadTimeout       = 30 * time.Second
	WriteTimeout      = 60 * time.Second
	IdleTimeout       = 120 * time.Second
	MaxHeaderBytes    = 64 << 10
)

// RetentionInterval is the period of the retention pass (SPEC §7).
const RetentionInterval = time.Hour

// ErrKeyMismatch is returned by [New] when the secret key cannot decrypt the
// stored device passwords (a lost or replaced TIKKER_SECRET_KEY/secret.key).
var ErrKeyMismatch = errors.New("the secret key does not decrypt the stored device passwords; " +
	"restore the original TIKKER_SECRET_KEY or secret.key")

// Options configures [New].
type Options struct {
	// Config is the validated configuration (config.Load).
	Config config.Config
	// Logger receives every component's logs (default slog.Default()).
	Logger *slog.Logger
	// Listener, if set, is served instead of listening on Config.Listen
	// (tests use a random port).
	Listener net.Listener
	// Clock drives every component (default the system clock).
	Clock clock.Clock
	// PasswordParams overrides the argon2id parameters (tests); zero means
	// auth.DefaultParams.
	PasswordParams auth.Params
	// Connector tunes the RouterOS connector; Logger and Clock are filled
	// in when empty.
	Connector routeros.Options
}

// App is an assembled tikker server. Create it with [New] and run it with
// [App.Run]; an App runs once.
type App struct {
	cfg config.Config
	log *slog.Logger
	clk clock.Clock

	store   *sqlite.Store
	auth    *auth.Service
	disp    *queue.Dispatcher
	health  *health.Checker
	sync    *api.ChannelSync
	pollers *pollerSupervisor

	webhookSender *webhook.Sender
	webhookPub    *webhook.Publisher
	webhooks      *webhook.Service
	agentHub      *agent.Hub
	metricsSrv    *metrics.Server
	ln            net.Listener
	srv           *http.Server
	startedAt     time.Time

	// runCtx is the context of the background loops; cancelled last.
	runCtx    context.Context
	runCancel context.CancelFunc
	bg        sync.WaitGroup
}

// New performs the start-up sequence up to and including opening the
// listener; nothing is served yet. On error everything opened so far is
// closed again.
func New(ctx context.Context, opts Options) (_ *App, err error) {
	cfg := opts.Config
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.System()
	}
	a := &App{cfg: cfg, log: log, clk: clk, startedAt: clk.Now()}
	a.runCtx, a.runCancel = context.WithCancel(context.WithoutCancel(ctx))

	var cleanup []func()
	defer func() {
		if err != nil {
			for i := len(cleanup) - 1; i >= 0; i-- {
				cleanup[i]()
			}
			a.runCancel()
		}
	}()

	// Data directory: database and key file.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("data directory: %w", err)
	}

	key, source, err := secrets.LoadKey(string(cfg.SecretKey), cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	ring, err := secrets.NewKeyring(key)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	log.Info("secret key loaded", slog.String("source", string(source)), slog.String("key_id", ring.KeyID()))
	if source == secrets.SourceFileGenerated {
		log.Warn("a new secret key was generated; back it up together with the database",
			slog.String("path", filepath.Join(cfg.DataDir, secrets.KeyFileName)))
	}
	creds := secrets.NewDeviceCredentials(ring)

	dbPath := filepath.Join(cfg.DataDir, DatabaseFile)
	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath, Synchronous: cfg.SQLiteSynchronous, Clock: clk})
	if err != nil {
		return nil, fmt.Errorf("database %s: %w", dbPath, err)
	}
	a.store = st
	cleanup = append(cleanup, func() { _ = st.Close() })
	log.Info("database opened", slog.String("path", dbPath))

	devices, err := st.Devices().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	if err := creds.Verify(devices); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeyMismatch, err)
	}

	if err := a.initAuth(ctx, opts); err != nil {
		return nil, err
	}

	reg := connector.NewRegistry()
	copts := opts.Connector
	if copts.Logger == nil {
		copts.Logger = log
	}
	if copts.Clock == nil {
		copts.Clock = clk
	}
	if err := routeros.Register(reg, copts); err != nil {
		return nil, fmt.Errorf("connector registry: %w", err)
	}
	// The pull-based agent connector (backlog #1): a RouterOS script polls
	// this server over HTTPS (works behind CGNAT). The hub owns the in-memory
	// mailboxes shared by its connectors and the /agent/v1 handler.
	a.agentHub = agent.NewHub(agentOptions(cfg.Agent, log, clk))
	if err := a.agentHub.Register(reg); err != nil {
		return nil, fmt.Errorf("connector registry: %w", err)
	}

	if err := a.initPipeline(creds, reg, ring); err != nil {
		return nil, err
	}
	// From here on the dispatcher and the checker run; stopping them is
	// part of shutdown.
	cleanup = append(cleanup, func() { _ = a.stopPipeline(context.Background()) })

	h, err := a.handler(creds, reg, ring)
	if err != nil {
		return nil, err
	}

	var tlsConfig *tls.Config
	if cfg.TLS.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls certificate: %w", err)
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}

	ln := opts.Listener
	if ln == nil {
		var lc net.ListenConfig
		ln, err = lc.Listen(ctx, "tcp", cfg.Listen)
		if err != nil {
			return nil, fmt.Errorf("listen %s: %w", cfg.Listen, err)
		}
	}
	a.ln = ln
	a.srv = &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return a.runCtx },
	}
	a.srv.TLSConfig = tlsConfig
	return a, nil
}

// initAuth creates the auth service and the bootstrap admin.
func (a *App) initAuth(ctx context.Context, opts Options) error {
	proxies, err := config.ParsePrefixes(a.cfg.TrustedProxies)
	if err != nil {
		return fmt.Errorf("trusted proxies: %w", err)
	}
	a.auth, err = auth.New(auth.Config{
		Store: a.store, Clock: a.clk, Logger: a.log,
		BaseURL: a.cfg.BaseURL, TrustedOrigins: a.cfg.TrustedOrigins, TrustedProxies: proxies,
		Params:                 opts.PasswordParams,
		HashConcurrency:        a.cfg.HashConcurrency,
		SessionIdleTimeout:     a.cfg.SessionIdleTimeout,
		SessionAbsoluteTimeout: a.cfg.SessionAbsoluteTimeout,
	})
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	res, err := a.auth.Bootstrap(ctx, string(a.cfg.AdminPassword))
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	switch res {
	case auth.BootstrapSetupRequired:
		a.log.Warn("no users exist: open the web panel to create the administrator (first-run wizard), " +
			"or set TIKKER_ADMIN_PASSWORD (at least 12 characters) and restart")
	case auth.BootstrapCreated, auth.BootstrapNotNeeded: // logged by auth
	}
	return nil
}

// initPipeline creates and starts the health checker, the dispatcher, the
// channel sync and the inbox pollers, and loads the channels.
func (a *App) initPipeline(creds *secrets.DeviceCredentials, reg *connector.Registry, ring *secrets.Keyring) error {
	cfg := a.cfg
	allowHTTP, err := admin.HostAllowlist(cfg.AllowedHTTPHosts)
	if err != nil {
		return fmt.Errorf("allowed http hosts: %w", err)
	}
	// Webhooks: the publisher stores events in the transactions of the
	// dispatcher and the inbox pollers; the sender delivers them.
	sender, err := webhook.NewSender(webhook.SenderOptions{
		Store: a.store.Webhooks(), Secrets: ring, Clock: a.clk, Logger: a.log,
		UserAgent: "tikker-webhook/" + version.Version, AllowPrivate: allowHTTP, PayloadRetention: cfg.Retention,
	})
	if err != nil {
		return fmt.Errorf("webhook sender: %w", err)
	}
	a.webhookSender = sender
	a.webhookPub = webhook.NewPublisher(a.clk, nil, sender)
	a.webhooks, err = webhook.NewService(webhook.ServiceDeps{
		Store: a.store, Sealer: ring, Clock: a.clk, Waker: sender, AllowHTTP: allowHTTP,
	})
	if err != nil {
		return fmt.Errorf("webhook service: %w", err)
	}
	// The checker sets states in the dispatcher and the dispatcher reports
	// to the checker; both adapters forward to the value set below.
	var disp *queue.Dispatcher
	var hc *health.Checker
	var cs *api.ChannelSync

	hc, err = health.New(health.Options{
		Channels: a.store.Channels(), Clock: a.clk, Logger: a.log,
		Dispatcher: stateSetterFunc(func(id domain.ID, s domain.ChannelState, reason string) bool {
			return disp.SetChannelState(id, s, reason)
		}),
		Interval: cfg.HealthInterval,
		// A pending state change (e.g. an agent that has not checked in
		// yet) is rechecked no slower than a healthy channel.
		RetryInterval: min(cfg.HealthInterval, health.DefaultRetryInterval),
	})
	if err != nil {
		return fmt.Errorf("health checker: %w", err)
	}
	disp, err = queue.New(queue.Options{
		Messages: a.store.Messages(), Clock: a.clk, Logger: a.log,
		Retry:               cfg.Retry.Policy(),
		Reporter:            hc,
		OnEvent:             hc.HandleQueueEvent,
		Events:              a.webhookPub,
		Tx:                  a.store,
		OnConnectorReleased: func(c connector.Connector) { cs.Release(c) },
	})
	if err != nil {
		return fmt.Errorf("dispatcher: %w", err)
	}
	a.health, a.disp = hc, disp

	a.pollers = newPollerSupervisor(a.runCtx, pollerBase{
		store: a.store, clock: a.clk, logger: a.log,
		interval: cfg.InboxPollInterval, reporter: hc, events: a.webhookPub,
	})

	cs, err = api.NewChannelSync(api.SyncOptions{
		Store: a.store, Credentials: creds, Connectors: reg, Health: hc, Logger: a.log,
		Sinks: []api.SpecSink{api.DispatcherSink(disp), api.HealthSink(hc), a.pollers},
	})
	if err != nil {
		return fmt.Errorf("channel sync: %w", err)
	}
	a.sync = cs

	if err := sender.Start(a.runCtx); err != nil {
		return fmt.Errorf("start webhook sender: %w", err)
	}
	if err := disp.Start(a.runCtx); err != nil {
		sender.Stop()
		return fmt.Errorf("start dispatcher: %w", err)
	}
	if err := hc.Start(a.runCtx); err != nil {
		_ = disp.Shutdown(context.Background())
		sender.Stop()
		return fmt.Errorf("start health checker: %w", err)
	}
	if err := cs.Reload(a.runCtx); err != nil {
		_ = a.stopPipeline(context.Background())
		return fmt.Errorf("load channels: %w", err)
	}
	a.log.Info("channels loaded", slog.Int("channels", len(disp.Stats().Channels)))
	return nil
}

// handler builds the API, the probes and the mux.
func (a *App) handler(creds *secrets.DeviceCredentials, reg *connector.Registry, ring *secrets.Keyring) (http.Handler, error) {
	strategy, err := sms.ParseStrategy(string(a.cfg.LongMessageStrategy))
	if err != nil {
		return nil, fmt.Errorf("long message strategy: %w", err)
	}
	rate := api.RateLimit{Requests: a.cfg.APIRateLimit, Window: time.Minute}
	if a.cfg.APIRateLimit == 0 {
		rate.Requests = -1 // disabled
	}
	// One sending service for the API and the web panel.
	snd, err := sending.New(sending.Deps{
		Store: a.store, Capabilities: a.sync, Notifier: a.disp, Clock: a.clk,
		Options: sending.Options{
			DefaultRegion: a.cfg.DefaultRegion, AllowShortNumbers: a.cfg.AllowShortNumbers,
			LongMessageStrategy: strategy,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("sending: %w", err)
	}
	adm, err := admin.New(admin.Deps{
		Store: a.store, Credentials: creds, Connectors: reg, Reloader: a.sync, Health: a.health, Checker: a.health,
		Clock: a.clk, Logger: a.log,
		Options: admin.Options{AllowedHTTPHosts: a.cfg.AllowedHTTPHosts},
	})
	if err != nil {
		return nil, fmt.Errorf("admin: %w", err)
	}
	apiH, err := api.New(api.Deps{
		Store: a.store, Auth: a.auth, Dispatcher: a.disp, Reloader: a.sync, Capabilities: a.sync, Sending: snd, Admin: adm,
		Health: a.health, Credentials: creds, Connectors: reg, Webhooks: a.webhooks,
		Clock: a.clk, Logger: a.log,
		Options: api.Options{
			DefaultRegion:       a.cfg.DefaultRegion,
			AllowShortNumbers:   a.cfg.AllowShortNumbers,
			LongMessageStrategy: strategy,
			AllowedHTTPHosts:    a.cfg.AllowedHTTPHosts,
			RateLimit:           rate,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}

	pr, err := probes.New(probes.Options{
		Clock: a.clk, Logger: a.log,
		Checks: []probes.Check{
			{Name: probes.CheckDatabase, Fn: a.store.Ping},
			{Name: probes.CheckSecretKey, Fn: keyCheck(ring)},
			{Name: probes.CheckDispatcher, Fn: probes.DispatcherCheck(a.disp.Stats)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("probes: %w", err)
	}
	panel, err := web.New(web.Options{
		Auth: a.auth, Logger: a.log, BaseURL: a.cfg.BaseURL, SetupSecret: string(a.cfg.SetupSecret),
		Store: a.store, Sending: snd, Admin: adm, Clock: a.clk, Queue: a.disp, Health: a.health,
	})
	if err != nil {
		return nil, fmt.Errorf("web panel: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle(api.Prefix+"/", apiH)
	routes := pr.Routes()
	mux.Handle("/healthz", routes)
	mux.Handle("/readyz", routes)
	// Prometheus metrics (backlog §5 #3); unauthenticated, read-only.
	m, err := metrics.New(metrics.Options{
		Source: metricsSource{stats: a.disp.Stats, snap: a.health.Snapshot, started: func() int64 { return a.startedAt.Unix() }},
		Logger: a.log, Namespace: "tikker",
	})
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	a.metricsSrv = m
	mux.Handle("GET /metrics", m.Handler())
	// The agent API lets a RouterOS script poll and report (backlog #1).
	mux.Handle(agent.Prefix+"/", a.agentHub.Handler())
	mux.Handle("/", panel.Handler()) // the web panel owns everything else
	return mux, nil
}

// metricsSource adapts the app's background state to a metrics.Snapshot.
type metricsSource struct {
	stats   func() queue.Stats
	snap    func() []health.ChannelStatus
	started func() int64
}

func (s metricsSource) Snapshot() (metrics.Snapshot, error) {
	return metrics.Snapshot{Queue: s.stats(), Health: s.snap(), Started: s.started()}, nil
}

// agentOptions maps the [agent] settings onto the agent hub. Settings the
// configuration does not expose keep the package defaults.
func agentOptions(c config.AgentConfig, log *slog.Logger, clk clock.Clock) agent.Options {
	enc := []domain.Encoding{domain.EncodingGSM7}
	if c.UCS2 {
		enc = append(enc, domain.EncodingUCS2)
	}
	return agent.Options{
		Logger: log, Clock: clk, SendTimeout: c.SendTimeout, StaleAfter: c.StaleAfter,
		MaxSegments: c.MaxSegments, Encodings: enc,
	}
}

// keyCheck proves that the loaded key encrypts and decrypts.
func keyCheck(ring *secrets.Keyring) probes.CheckFunc {
	return func(context.Context) error {
		const purpose, id = "readyz", "probe"
		sealed, err := ring.Seal(purpose, id, []byte("ok"))
		if err != nil {
			return fmt.Errorf("seal: %w", err)
		}
		if _, err := ring.Open(purpose, id, sealed); err != nil {
			return fmt.Errorf("open: %w", err)
		}
		return nil
	}
}

// Addr returns the address the server listens on.
func (a *App) Addr() net.Addr { return a.ln.Addr() }

// Run serves HTTP and runs the background loops until ctx ends, then shuts
// everything down (see the package documentation). It returns nil after a
// clean shutdown and an error when serving failed or a shutdown step did not
// finish in time.
func (a *App) Run(ctx context.Context) error {
	a.bg.Add(2)
	go func() {
		defer a.bg.Done()
		a.store.RunRetention(a.runCtx, a.clk, RetentionInterval, a.cfg.Retention, a.log)
	}()
	go func() {
		defer a.bg.Done()
		a.auth.Run(a.runCtx)
	}()

	// http.Server modifies TLSConfig while serving: read it before.
	useTLS := a.srv.TLSConfig != nil
	serveErr := make(chan error, 1)
	go func() {
		var err error
		if useTLS {
			err = a.srv.ServeTLS(a.ln, "", "") // certificate loaded in New
		} else {
			err = a.srv.Serve(a.ln)
		}
		serveErr <- err
	}()
	a.log.Info("tikker listening", slog.String("addr", a.ln.Addr().String()), slog.Bool("tls", useTLS))

	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutting down", slog.Duration("timeout", a.cfg.ShutdownTimeout))
	case err := <-serveErr:
		runErr = fmt.Errorf("http server: %w", err)
		a.log.Error("http server failed; shutting down", slog.Any("error", err))
		serveErr <- nil // the server is gone; Shutdown below returns at once
	}

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.ShutdownTimeout)
	defer cancel()

	if err := a.srv.Shutdown(sctx); err != nil {
		a.log.Warn("http requests did not finish in time", slog.Any("error", err))
		_ = a.srv.Close()
		runErr = errors.Join(runErr, fmt.Errorf("http shutdown: %w", err))
	} else {
		a.log.Info("http server stopped")
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		runErr = errors.Join(runErr, fmt.Errorf("http server: %w", err))
	}

	runErr = errors.Join(runErr, a.stopPipeline(sctx))

	a.runCancel()
	a.bg.Wait()
	a.log.Info("background tasks stopped")

	if err := a.store.Close(); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("close database: %w", err))
	} else {
		a.log.Info("database closed")
	}
	return runErr
}

// stopPipeline stops the pollers, the dispatcher and the checker and closes
// the connectors, in this order.
func (a *App) stopPipeline(ctx context.Context) error {
	var errs []error
	a.pollers.Stop()
	a.log.Info("inbox pollers stopped")

	if err := a.disp.Shutdown(ctx); err != nil {
		a.log.Warn("dispatcher stopped with sends interrupted; they will be retried", slog.Any("error", err))
		errs = append(errs, fmt.Errorf("dispatcher shutdown: %w", err))
	}
	// After the dispatcher and the pollers: they may still publish events.
	if err := a.webhookSender.Shutdown(ctx); err != nil {
		a.log.Warn("webhook sender stopped with attempts interrupted; they will be retried", slog.Any("error", err))
		errs = append(errs, fmt.Errorf("webhook shutdown: %w", err))
	} else {
		a.log.Info("webhook sender stopped")
	}
	if err := a.health.Shutdown(ctx); err != nil {
		a.log.Warn("health checker did not flush in time", slog.Any("error", err))
		errs = append(errs, fmt.Errorf("health shutdown: %w", err))
	}
	if err := a.sync.Close(); err != nil {
		a.log.Warn("closing connectors failed", slog.Any("error", err))
	} else {
		a.log.Info("connectors closed")
	}
	return errors.Join(errs...)
}

// stateSetterFunc adapts a function to health.StateSetter.
type stateSetterFunc func(id domain.ID, state domain.ChannelState, reason string) bool

func (f stateSetterFunc) SetChannelState(id domain.ID, state domain.ChannelState, reason string) bool {
	return f(id, state, reason)
}
