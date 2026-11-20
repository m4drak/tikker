// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/config"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const (
	adminPassword = "admin-password-123"
	routerUser    = "tikker"
	routerPass    = "router-secret-42"
	waitTimeout   = 20 * time.Second
)

// fastParams keeps argon2id cheap in tests.
var fastParams = auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Listen = "127.0.0.1:1" // unused: tests pass a listener
	cfg.AdminPassword = adminPassword
	cfg.AllowedHTTPHosts = []string{"127.0.0.1"}
	cfg.InboxPollInterval = time.Second
	cfg.HealthInterval = time.Second
	cfg.ShutdownTimeout = 10 * time.Second
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// client is a small JSON client for the running server.
type client struct {
	t     *testing.T
	base  string
	token string
	http  *http.Client
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	var m map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			c.t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, data)
		}
	}
	return res.StatusCode, m
}

// waitFor polls cond until it holds or waitTimeout passes. End-to-end
// tests run on the real clock, so polling is the only way to observe them.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		<-tick.C
	}
}

// TestServeEndToEnd runs the assembled server against the fake router:
// bootstrap admin, CLI token, device and channel through the API, an outbound
// SMS reaching the router, an inbound SMS reaching the API, readiness and
// a clean shutdown without leaked goroutines (TestMain).
func TestServeEndToEnd(t *testing.T) {
	router := fake.New(fake.Options{User: routerUser, Password: routerPass, Ports: []string{"lte1"}})
	routerURL := router.Start()
	defer router.Close()

	cfg := testConfig(t)
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := New(ctx, Options{Config: cfg, Logger: log, Listener: ln, PasswordParams: fastParams})
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	// The first token comes from the command line path, against the live
	// database.
	_, token, err := CreateToken(context.Background(), cfg, log, TokenRequest{
		Username: auth.BootstrapUsername, Name: "e2e",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeSMSSend, domain.ScopeSMSRead},
	})
	if err != nil {
		t.Fatal(err)
	}

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	c := &client{t: t, base: "http://" + a.Addr().String(), token: token, http: &http.Client{Transport: tr, Timeout: 10 * time.Second}}

	if code, _ := c.do(http.MethodGet, "/healthz", nil); code != http.StatusOK {
		t.Errorf("/healthz = %d", code)
	}
	if code, body := c.do(http.MethodGet, "/readyz", nil); code != http.StatusOK || body["status"] != "ready" {
		t.Errorf("/readyz = %d %v", code, body)
	}
	if code, _ := (&client{t: t, base: c.base, http: c.http}).do(http.MethodGet, "/api/v1/messages", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous API request = %d, want 401", code)
	}

	code, dev := c.do(http.MethodPost, "/api/v1/devices", map[string]any{
		"name": "router", "base_url": routerURL, "username": routerUser, "password": routerPass,
	})
	if code != http.StatusCreated {
		t.Fatalf("create device: %d %v", code, dev)
	}
	code, ch := c.do(http.MethodPost, "/api/v1/channels", map[string]any{"device_id": dev["id"], "port": "lte1"})
	if code != http.StatusCreated {
		t.Fatalf("create channel: %d %v", code, ch)
	}
	chPath := fmt.Sprintf("/api/v1/channels/%s", ch["id"])
	waitFor(t, "channel ok", func() bool {
		_, got := c.do(http.MethodGet, chPath, nil)
		return got["state"] == "ok"
	})
	waitFor(t, "inbox poller", func() bool { return a.pollers.count() == 1 })

	code, msg := c.do(http.MethodPost, "/api/v1/messages", map[string]any{"to": "600100200", "body": "Hello from tikker"})
	if code != http.StatusAccepted {
		t.Fatalf("send: %d %v", code, msg)
	}
	msgPath := fmt.Sprintf("/api/v1/messages/%s", msg["id"])
	waitFor(t, "message sent", func() bool {
		_, got := c.do(http.MethodGet, msgPath, nil)
		return got["status"] == "sent"
	})
	sent := router.Sent()
	if len(sent) != 1 || sent[0].Message != "Hello from tikker" || !strings.HasSuffix(sent[0].PhoneNumber, "600100200") {
		t.Errorf("router got %+v", sent)
	}

	if _, err := router.InjectSMS(fake.IncomingSMS{From: "+48511222333", Text: "Odpowiedź: tak"}); err != nil {
		t.Fatal(err)
	}
	var inbound []any
	waitFor(t, "inbound message", func() bool {
		_, got := c.do(http.MethodGet, "/api/v1/messages?direction=in", nil)
		inbound, _ = got["items"].([]any)
		return len(inbound) > 0
	})
	in := inbound[0].(map[string]any)
	if in["body"] != "Odpowiedź: tak" || in["peer"] != "+48511222333" || in["status"] != "received" {
		t.Errorf("inbound = %v", in)
	}
	waitFor(t, "router inbox emptied", func() bool { return len(router.Inbox()) == 0 })

	// Disabling the channel stops its poller.
	if code, body := c.do(http.MethodPatch, chPath, map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("disable channel: %d %v", code, body)
	}
	waitFor(t, "poller stopped", func() bool { return a.pollers.count() == 0 })

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return after cancellation")
	}
	tr.CloseIdleConnections()

	out := logs.String()
	for _, want := range []string{"tikker listening", "shutting down", "inbox pollers stopped", "connectors closed", "database closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs miss %q", want)
		}
	}
	for _, secret := range []string{adminPassword, routerPass, token, "Hello from tikker", "600100200"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}

	// The database was closed and is consistent: it reopens with the data.
	st, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.DataDir + "/" + DatabaseFile})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	m, err := st.Messages().Get(context.Background(), domain.ID(msg["id"].(string)))
	if err != nil || m.Status != domain.StatusSent {
		t.Errorf("stored message = %+v, %v", m, err)
	}
}

// TestNewRejectsWrongSecretKey: a key that does not open the stored device
// passwords stops the start-up with ErrKeyMismatch.
func TestNewRejectsWrongSecretKey(t *testing.T) {
	cfg := testConfig(t)
	keyA := bytes.Repeat([]byte{1}, 32)
	keyB := bytes.Repeat([]byte{2}, 32)

	st, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.DataDir + "/" + DatabaseFile})
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(keyA)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.ID("01M7VZ3WHKECDDAP0Z1XWWG3A0")
	enc, err := secrets.NewDeviceCredentials(ring).Encrypt(id, "pw")
	if err != nil {
		t.Fatal(err)
	}
	err = st.Devices().Create(context.Background(), domain.Device{
		ID: id, Name: "r", Connector: domain.ConnectorRouterOSREST, BaseURL: "https://192.0.2.1",
		Username: "u", PasswordEnc: enc, TLSMode: domain.TLSVerify, Enabled: true, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		key     []byte
		wantErr error
	}{
		{name: "wrong key", key: keyB, wantErr: ErrKeyMismatch},
		{name: "right key", key: keyA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cfg
			c.SecretKey = config.Secret(base64.StdEncoding.EncodeToString(tt.key))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a, err := New(ctx, Options{Config: c, Logger: slog.New(slog.DiscardHandler), Listener: ln, PasswordParams: fastParams})
			if tt.wantErr != nil {
				_ = ln.Close()
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("New() = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			if err := a.Run(ctx); err != nil {
				t.Fatalf("Run() = %v", err)
			}
		})
	}
}

// TestBootstrapWithoutPassword: without TIKKER_ADMIN_PASSWORD and no users
// the server starts and says how to create the admin.
func TestBootstrapWithoutPassword(t *testing.T) {
	cfg := testConfig(t)
	cfg.AdminPassword = ""
	logs := &syncBuffer{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := New(ctx, Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(logs, nil)), Listener: ln, PasswordParams: fastParams})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "TIKKER_ADMIN_PASSWORD") {
		t.Errorf("logs do not explain how to create the admin:\n%s", logs)
	}
	_, _, err = CreateToken(context.Background(), cfg, slog.New(slog.DiscardHandler), TokenRequest{Username: "admin", Name: "x", Scopes: domain.Scopes{domain.ScopeAdmin}})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("CreateToken() = %v, want unknown user", err)
	}
}

func TestNewRejectsBadTLSCertificate(t *testing.T) {
	cfg := testConfig(t)
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.TLS = config.TLSConfig{CertFile: bad, KeyFile: bad}
	_, err := New(context.Background(), Options{Config: cfg, Logger: slog.New(slog.DiscardHandler), PasswordParams: fastParams})
	if err == nil || !strings.Contains(err.Error(), "tls certificate") {
		t.Fatalf("New() = %v, want a TLS error", err)
	}
}

func TestCreateTokenWithoutDatabase(t *testing.T) {
	cfg := testConfig(t)
	_, _, err := CreateToken(context.Background(), cfg, nil, TokenRequest{Username: "admin", Name: "x", Scopes: domain.Scopes{domain.ScopeAdmin}})
	if err == nil || !strings.Contains(err.Error(), "tikker serve") {
		t.Errorf("CreateToken() = %v", err)
	}
}
