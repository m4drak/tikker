// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package probes implements the liveness and readiness probes /healthz and
// /readyz (SPEC §5, api/openapi.yaml). It also provides the client side used
// by `tikker healthcheck`, the command behind the image HEALTHCHECK (T40).
//
// The package only supplies [http.Handler] values; the HTTP server and the
// router are assembled by the caller (T21). Liveness never looks at
// dependencies. Readiness runs the injected checks in parallel, each with its
// own timeout. Channel state does not affect readiness: the queue accepts
// messages even when no channel is healthy.
package probes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/queue"
)

// Names of the readiness checks in the response (api/openapi.yaml, Readiness).
const (
	CheckDatabase   = "database"
	CheckSecretKey  = "secret_key"
	CheckDispatcher = "dispatcher"
)

// DefaultTimeout bounds every readiness check when Options.Timeout is zero.
const DefaultTimeout = 2 * time.Second

// Response values from api/openapi.yaml.
const (
	statusLive     = "ok"
	statusReady    = "ready"
	statusNotReady = "not_ready"
	checkOK        = "ok"
	checkFail      = "fail"
)

// CheckFunc reports nil when the dependency is healthy. It should honour ctx,
// but Readyz does not depend on that: a check that ignores ctx is abandoned
// once its timeout expires.
type CheckFunc func(ctx context.Context) error

// Check is a named readiness check.
type Check struct {
	Name string
	Fn   CheckFunc
}

// Options configures [New].
type Options struct {
	// Checks are run on every readiness request. Names must be unique and
	// non-empty; an empty list makes the instance always ready.
	Checks []Check
	// Timeout bounds each check. Zero means DefaultTimeout.
	Timeout time.Duration
	// Clock drives the per-check timeout. Nil means the system clock.
	Clock clock.Clock
	// Logger receives the details of failed checks, which are not sent to
	// clients. Nil means slog.Default().
	Logger *slog.Logger
}

// Probes serves the health endpoints. Use [New] to create one.
type Probes struct {
	checks  []Check
	timeout time.Duration
	clock   clock.Clock
	logger  *slog.Logger
}

// New validates opts and returns the probes.
func New(opts Options) (*Probes, error) {
	seen := make(map[string]bool, len(opts.Checks))
	for i, c := range opts.Checks {
		if c.Name == "" {
			return nil, fmt.Errorf("probes: check %d has no name", i)
		}
		if c.Fn == nil {
			return nil, fmt.Errorf("probes: check %q has no function", c.Name)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("probes: duplicate check %q", c.Name)
		}
		seen[c.Name] = true
	}
	p := &Probes{
		checks:  append([]Check(nil), opts.Checks...),
		timeout: opts.Timeout,
		clock:   opts.Clock,
		logger:  opts.Logger,
	}
	if p.timeout <= 0 {
		p.timeout = DefaultTimeout
	}
	if p.clock == nil {
		p.clock = clock.System()
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	return p, nil
}

// Routes returns a mux with GET /healthz and GET /readyz registered. Other
// methods get 405 and other paths get 404 from the mux.
func (p *Probes) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", p.Healthz())
	mux.Handle("GET /readyz", p.Readyz())
	return mux
}

type liveBody struct {
	Status string `json:"status"`
}

type readyBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Healthz returns the liveness handler. It answers 200 while the process
// serves HTTP.
func (p *Probes) Healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.writeJSON(w, http.StatusOK, liveBody{Status: statusLive})
	})
}

// Readyz returns the readiness handler. It answers 200 with status "ready"
// when every check passes, and 503 with status "not_ready" otherwise. The
// body names the failed checks but never their error messages.
func (p *Probes) Readyz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errs := p.runChecks(r.Context())

		body := readyBody{Status: statusReady, Checks: make(map[string]string, len(p.checks))}
		code := http.StatusOK
		for i, c := range p.checks {
			if errs[i] != nil {
				p.logger.WarnContext(r.Context(), "readiness check failed", "check", c.Name, "err", errs[i])
				body.Checks[c.Name] = checkFail
				body.Status = statusNotReady
				code = http.StatusServiceUnavailable
				continue
			}
			body.Checks[c.Name] = checkOK
		}
		p.writeJSON(w, code, body)
	})
}

// runChecks runs all checks concurrently and returns their results in the
// order of p.checks. It returns once every check has finished or timed out.
func (p *Probes) runChecks(ctx context.Context) []error {
	errs := make([]error, len(p.checks))
	var wg sync.WaitGroup
	for i, c := range p.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = p.runCheck(ctx, c)
		}()
	}
	wg.Wait()
	return errs
}

// runCheck runs one check under the timeout. A check that returns after the
// timeout is abandoned: its result goes to a buffered channel and is dropped.
func (p *Probes) runCheck(parent context.Context, c Check) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("check %q panicked: %v", c.Name, r)
			}
		}()
		done <- c.Fn(ctx)
	}()

	select {
	case err := <-done:
		return err
	case <-p.clock.After(p.timeout):
		cancel()
		return fmt.Errorf("check %q timed out after %s", c.Name, p.timeout)
	case <-parent.Done():
		return fmt.Errorf("check %q: %w", c.Name, parent.Err())
	}
}

// writeJSON writes a probe response. Probes must not be cached.
func (p *Probes) writeJSON(w http.ResponseWriter, code int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		p.logger.Debug("writing probe response", "err", err)
	}
}

// DispatcherCheck reports the dispatcher as running: it has been started and
// has not been closed. Pass the Stats method of the dispatcher, for example
// DispatcherCheck(d.Stats).
func DispatcherCheck(stats func() queue.Stats) CheckFunc {
	return func(context.Context) error {
		if stats == nil {
			return errors.New("dispatcher not configured")
		}
		s := stats()
		if !s.Started || s.Closed {
			return errors.New("dispatcher not running")
		}
		return nil
	}
}
