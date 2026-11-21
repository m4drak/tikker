// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package probes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/queue"
)

var errSecret = errors.New("sqlite: open /data/secret.key: password=hunter2")

func okCheck(name string) Check {
	return Check{Name: name, Fn: func(context.Context) error { return nil }}
}

func failCheck(name string) Check {
	return Check{Name: name, Fn: func(context.Context) error { return errSecret }}
}

func newProbes(t *testing.T, opts Options) *Probes {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name    string
		checks  []Check
		wantErr string
	}{
		{name: "no checks is valid"},
		{name: "unique checks", checks: []Check{okCheck("a"), okCheck("b")}},
		{name: "empty name", checks: []Check{okCheck("")}, wantErr: "has no name"},
		{name: "nil function", checks: []Check{{Name: "a"}}, wantErr: `"a" has no function`},
		{name: "duplicate name", checks: []Check{okCheck("a"), okCheck("a")}, wantErr: `duplicate check "a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(Options{Checks: tt.checks})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New: unexpected error %v", err)
				}
				if p.timeout != DefaultTimeout || p.clock == nil || p.logger == nil {
					t.Errorf("defaults not applied: timeout=%s", p.timeout)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewCopiesChecks(t *testing.T) {
	checks := []Check{okCheck("a")}
	p := newProbes(t, Options{Checks: checks})
	checks[0].Name = "changed"
	if p.checks[0].Name != "a" {
		t.Errorf("checks slice aliased the caller's slice")
	}
}

func TestHealthz(t *testing.T) {
	// Liveness must answer 200 even when a readiness check would fail.
	p := newProbes(t, Options{Checks: []Check{failCheck(CheckDatabase)}})

	rec := get(t, p.Healthz(), http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertHeaders(t, rec)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["status"] != "ok" || len(body) != 1 {
		t.Errorf("body = %v, want exactly {status: ok}", body)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		checks     []Check
		wantCode   int
		wantStatus string
		wantChecks map[string]string
	}{
		{
			name:       "no checks is ready",
			wantCode:   http.StatusOK,
			wantStatus: "ready",
			wantChecks: map[string]string{},
		},
		{
			name:       "all ok",
			checks:     []Check{okCheck(CheckDatabase), okCheck(CheckSecretKey), okCheck(CheckDispatcher)},
			wantCode:   http.StatusOK,
			wantStatus: "ready",
			wantChecks: map[string]string{CheckDatabase: "ok", CheckSecretKey: "ok", CheckDispatcher: "ok"},
		},
		{
			name:       "database fails",
			checks:     []Check{failCheck(CheckDatabase), okCheck(CheckSecretKey), okCheck(CheckDispatcher)},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not_ready",
			wantChecks: map[string]string{CheckDatabase: "fail", CheckSecretKey: "ok", CheckDispatcher: "ok"},
		},
		{
			name:       "all fail",
			checks:     []Check{failCheck(CheckDatabase), failCheck(CheckSecretKey)},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "not_ready",
			wantChecks: map[string]string{CheckDatabase: "fail", CheckSecretKey: "fail"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProbes(t, Options{Checks: tt.checks})
			rec := get(t, p.Readyz(), http.MethodGet, "/readyz")
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			assertHeaders(t, rec)
			assertReadyBody(t, rec.Body.String(), tt.wantStatus, tt.wantChecks)
		})
	}
}

// assertReadyBody checks the readiness body against the schema in
// api/openapi.yaml: status is ready|not_ready, checks maps names to ok|fail,
// and no other fields are present.
func assertReadyBody(t *testing.T, raw, wantStatus string, wantChecks map[string]string) {
	t.Helper()
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("body %q does not match the schema: %v", raw, err)
	}
	if body.Status != wantStatus {
		t.Errorf("status = %q, want %q", body.Status, wantStatus)
	}
	if len(body.Checks) != len(wantChecks) {
		t.Errorf("checks = %v, want %v", body.Checks, wantChecks)
	}
	for name, want := range wantChecks {
		if got := body.Checks[name]; got != want {
			t.Errorf("checks[%q] = %q, want %q", name, got, want)
		}
	}
}

func TestReadyzDoesNotLeakErrors(t *testing.T) {
	p := newProbes(t, Options{Checks: []Check{failCheck(CheckDatabase)}})
	rec := get(t, p.Readyz(), http.MethodGet, "/readyz")
	for _, leak := range []string{"sqlite", "/data", "secret.key", "hunter2", "password", "open"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("body leaks %q: %s", leak, rec.Body.String())
		}
	}
}

func TestReadyzLogsFailures(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	p := newProbes(t, Options{Checks: []Check{failCheck(CheckDatabase)}, Logger: logger})
	get(t, p.Readyz(), http.MethodGet, "/readyz")
	out := logs.String()
	if !strings.Contains(out, "check=database") || !strings.Contains(out, "secret.key") {
		t.Errorf("failure not logged with check name and error: %s", out)
	}
}

func TestReadyzTimeoutAbandonsCheck(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	// The check ignores ctx and blocks until the test ends.
	stuck := Check{Name: CheckDatabase, Fn: func(context.Context) error {
		<-release
		return nil
	}}
	fake := clock.NewFake(time.Unix(0, 0))
	p := newProbes(t, Options{Checks: []Check{stuck}, Timeout: 5 * time.Second, Clock: fake})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- get(t, p.Readyz(), http.MethodGet, "/readyz") }()

	fake.BlockUntil(1)
	fake.Advance(5 * time.Second)
	rec := <-done
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	assertReadyBody(t, rec.Body.String(), "not_ready", map[string]string{CheckDatabase: "fail"})
}

func TestReadyzCheckIsNotTimedOutEarly(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	slow := Check{Name: CheckDatabase, Fn: func(context.Context) error { return nil }}
	p := newProbes(t, Options{Checks: []Check{slow}, Timeout: time.Second, Clock: fake})
	// The check returns at once, so the readiness request must not wait for
	// the timer, and the request must answer 200.
	rec := get(t, p.Readyz(), http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReadyzChecksRunInParallel(t *testing.T) {
	// Each check waits until the other has started. Sequential execution
	// would make both time out, so a 200 proves they run concurrently.
	aStarted, bStarted := make(chan struct{}), make(chan struct{})
	a := Check{Name: "a", Fn: func(context.Context) error {
		close(aStarted)
		<-bStarted
		return nil
	}}
	b := Check{Name: "b", Fn: func(context.Context) error {
		close(bStarted)
		<-aStarted
		return nil
	}}
	p := newProbes(t, Options{Checks: []Check{a, b}, Timeout: 10 * time.Second, Clock: clock.System()})

	rec := get(t, p.Readyz(), http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (checks ran sequentially?)", rec.Code)
	}
}

func TestReadyzPanicBecomesFailure(t *testing.T) {
	boom := Check{Name: CheckSecretKey, Fn: func(context.Context) error { panic("boom") }}
	p := newProbes(t, Options{Checks: []Check{boom, okCheck(CheckDatabase)}})
	rec := get(t, p.Readyz(), http.MethodGet, "/readyz")
	assertReadyBody(t, rec.Body.String(), "not_ready", map[string]string{CheckSecretKey: "fail", CheckDatabase: "ok"})
}

func TestReadyzCancelledRequest(t *testing.T) {
	stuck := Check{Name: CheckDatabase, Fn: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	p := newProbes(t, Options{Checks: []Check{stuck}, Timeout: time.Hour, Clock: clock.System()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	p.Readyz().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestRoutes(t *testing.T) {
	p := newProbes(t, Options{Checks: []Check{failCheck(CheckDatabase)}})
	h := p.Routes()
	tests := []struct {
		method, path string
		wantCode     int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodHead, "/healthz", http.StatusOK},
		{http.MethodGet, "/readyz", http.StatusServiceUnavailable},
		{http.MethodPost, "/readyz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/nope", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			if got := get(t, h, tt.method, tt.path).Code; got != tt.wantCode {
				t.Errorf("status = %d, want %d", got, tt.wantCode)
			}
		})
	}
}

func TestDispatcherCheck(t *testing.T) {
	tests := []struct {
		name    string
		stats   func() queue.Stats
		wantErr bool
	}{
		{name: "nil stats func", stats: nil, wantErr: true},
		{name: "not started", stats: func() queue.Stats { return queue.Stats{} }, wantErr: true},
		{name: "running", stats: func() queue.Stats { return queue.Stats{Started: true} }},
		{name: "closed", stats: func() queue.Stats { return queue.Stats{Started: true, Closed: true} }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DispatcherCheck(tt.stats)(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDispatcherCheckWiredToReadiness(t *testing.T) {
	stats := func() queue.Stats { return queue.Stats{} }
	p := newProbes(t, Options{Checks: []Check{{Name: CheckDispatcher, Fn: DispatcherCheck(stats)}}})
	rec := get(t, p.Readyz(), http.MethodGet, "/readyz")
	assertReadyBody(t, rec.Body.String(), "not_ready", map[string]string{CheckDispatcher: "fail"})
}

func assertHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}
