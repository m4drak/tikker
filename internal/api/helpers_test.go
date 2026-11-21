// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/connector/routeros"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/inbox"
	"github.com/m4drak/tikker/internal/queue"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

// t0 is the start of the fake clock.
var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

const (
	routerUser = "tikker"
	routerPass = "r0uter-Secret-Pa55"
	userPass   = "correct horse battery"
)

// logBuf is a goroutine-safe log sink.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// envConfig tunes newEnv.
type envConfig struct {
	realClock bool // system clock instead of the fake one (queue tests)
	run       bool // start the dispatcher
	opts      Options
	noHTTP    bool // do not allow http:// devices
}

// testEnv is a complete tikker: SQLite in memory, auth, fake RouterOS, the
// RouterOS connector, the dispatcher and the API under test.
type testEnv struct {
	t    *testing.T
	ctx  context.Context
	clk  clock.Clock
	fake *clock.Fake // nil with a real clock
	st   *sqlite.Store
	auth *auth.Service
	srv  *fake.Server
	url  string // base URL of the fake router
	reg  *connector.Registry
	sync *ChannelSync
	disp *queue.Dispatcher
	h    http.Handler
	api  *API
	logs *logBuf

	reloader     *countingReloader
	adminTok     string // scope admin, role admin
	sendTok      string // sms:send + sms:read, role operator
	readTok      string // sms:read, role viewer
	admin        domain.User
	operator     domain.User
	viewer       domain.User
	val          *contract
	rateLimitOff bool // the API runs without a request budget: no RateLimit-* headers
}

// countingReloader wraps ChannelSync to count reloads.
type countingReloader struct {
	mu   sync.Mutex
	n    int
	next ChannelReloader
}

func (c *countingReloader) Reload(ctx context.Context) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.next.Reload(ctx)
}

func (c *countingReloader) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func newEnv(t *testing.T, mod ...func(*envConfig)) *testEnv {
	t.Helper()
	var cfg envConfig
	for _, m := range mod {
		m(&cfg)
	}
	e := &testEnv{t: t, ctx: context.Background(), logs: &logBuf{}, val: loadContract(t)}
	if cfg.realClock {
		e.clk = clock.System()
	} else {
		e.fake = clock.NewFake(t0)
		e.clk = e.fake
	}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	st, err := sqlite.Open(e.ctx, sqlite.Options{Clock: e.clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st

	e.auth, err = auth.New(auth.Config{
		Store: st, Clock: e.clk, Logger: log,
		Params: auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
	})
	if err != nil {
		t.Fatal(err)
	}

	e.srv = fake.New(fake.Options{User: routerUser, Password: routerPass, Ports: []string{"lte1", "lte2"}, Now: e.clk.Now})
	e.url = e.srv.Start()
	t.Cleanup(e.srv.Close)

	e.reg = connector.NewRegistry()
	if err := routeros.Register(e.reg, routeros.Options{Logger: log, Clock: e.clk}); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	creds := secrets.NewDeviceCredentials(ring)

	// The dispatcher and the sync reference each other: connectors released
	// by the dispatcher are closed by the sync.
	var syncRef *ChannelSync
	e.disp, err = queue.New(queue.Options{
		Messages: st.Messages(), Clock: e.clk, Logger: log,
		OnConnectorReleased: func(c connector.Connector) { syncRef.Release(c) },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.sync, err = NewChannelSync(SyncOptions{
		Store: st, Credentials: creds, Connectors: e.reg, Logger: log,
		Sinks: []SpecSink{DispatcherSink(e.disp)},
	})
	if err != nil {
		t.Fatal(err)
	}
	syncRef = e.sync
	e.reloader = &countingReloader{next: e.sync}
	if cfg.run {
		if err := e.disp.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.disp.Shutdown(ctx)
		_ = e.sync.Close()
	})

	opts := cfg.opts
	e.rateLimitOff = opts.RateLimit.Requests < 0
	if !cfg.noHTTP {
		opts.AllowedHTTPHosts = append(opts.AllowedHTTPHosts, "127.0.0.1")
	}
	e.api, err = newAPI(Deps{
		Store: st, Auth: e.auth, Dispatcher: e.disp, Reloader: e.reloader, Capabilities: e.sync,
		Credentials: creds, Connectors: e.reg, Clock: e.clk, Logger: log, Options: opts,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.h = e.api.recoverer(e.api.mux)

	e.admin = e.mkUser("admin1", domain.RoleAdmin)
	e.operator = e.mkUser("operator1", domain.RoleOperator)
	e.viewer = e.mkUser("viewer1", domain.RoleViewer)
	e.adminTok = e.mkToken(e.admin, domain.ScopeAdmin)
	e.sendTok = e.mkToken(e.operator, domain.ScopeSMSSend, domain.ScopeSMSRead)
	e.readTok = e.mkToken(e.viewer, domain.ScopeSMSRead)
	return e
}

func (e *testEnv) mkUser(name string, role domain.Role) domain.User {
	e.t.Helper()
	u, err := e.auth.CreateUser(e.ctx, domain.SystemActor, auth.NewUser{Username: name, Password: userPass, Role: role})
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *testEnv) mkToken(owner domain.User, scopes ...domain.Scope) string {
	e.t.Helper()
	_, full, err := e.auth.CreateToken(e.ctx, domain.SystemActor, auth.NewToken{UserID: owner.ID, Name: "t", Scopes: scopes})
	if err != nil {
		e.t.Fatal(err)
	}
	return full
}

// result is one recorded HTTP exchange.
type result struct {
	t      *testing.T
	Status int
	Header http.Header
	Body   []byte
}

func (r *result) decode(v any) {
	r.t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		r.t.Fatalf("response is not JSON: %v\n%s", err, r.Body)
	}
}

// obj decodes the body into a generic map.
func (r *result) obj() map[string]any {
	r.t.Helper()
	var m map[string]any
	r.decode(&m)
	return m
}

func (r *result) String() string { return fmt.Sprintf("%d %s", r.Status, r.Body) }

// msgs returns the messages of a SendMessageResponse (POST /messages).
func (r *result) msgs() []map[string]any {
	r.t.Helper()
	var resp struct {
		Messages []map[string]any `json:"messages"`
	}
	r.decode(&resp)
	if len(resp.Messages) == 0 {
		r.t.Fatalf("no messages in the response: %s", r)
	}
	return resp.Messages
}

// msg returns the only message of a SendMessageResponse.
func (r *result) msg() map[string]any {
	r.t.Helper()
	ms := r.msgs()
	if len(ms) != 1 {
		r.t.Fatalf("%d messages in the response, want 1: %s", len(ms), r)
	}
	return ms[0]
}

// batchItems returns the messages of each item of a SendBatchResponse.
func (r *result) batchItems() [][]map[string]any {
	r.t.Helper()
	var resp struct {
		Items []struct {
			Messages []map[string]any `json:"messages"`
		} `json:"items"`
	}
	r.decode(&resp)
	out := make([][]map[string]any, len(resp.Items))
	for i, it := range resp.Items {
		out[i] = it.Messages
	}
	return out
}

// problem asserts a problem+json response and returns it.
func (r *result) problem(status int, typ string) map[string]any {
	r.t.Helper()
	if r.Status != status {
		r.t.Fatalf("status = %d, want %d: %s", r.Status, status, r.Body)
	}
	if ct := r.Header.Get("Content-Type"); ct != contentTypeProblemJSON {
		r.t.Fatalf("Content-Type = %q, want problem+json", ct)
	}
	m := r.obj()
	if m["type"] != typ {
		r.t.Fatalf("problem type = %v, want %s: %s", m["type"], typ, r.Body)
	}
	return m
}

// codes returns the errors[].code values of a problem.
func problemCodes(m map[string]any) []string {
	var out []string
	errs, _ := m["errors"].([]any)
	for _, e := range errs {
		em := e.(map[string]any)
		out = append(out, fmt.Sprintf("%s:%s", em["field"], em["code"]))
	}
	return out
}

// req sends a request through the handler and checks the response against the
// contract. body may be a string (sent as is), []byte, or any value (JSON).
func (e *testEnv) req(method, path, token string, body any, hdr ...string) *result {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "192.0.2.1:1234"
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	res := &result{t: e.t, Status: w.Code, Header: w.Header(), Body: w.Body.Bytes()}
	e.val.check(e.t, method, path, res, !e.rateLimitOff)
	return res
}

func (e *testEnv) get(path, token string) *result { return e.req(http.MethodGet, path, token, nil) }

// startDevice adds a device for the fake router through the API.
func (e *testEnv) addDevice(name string) string {
	e.t.Helper()
	res := e.req(http.MethodPost, "/api/v1/devices", e.adminTok, map[string]any{
		"name": name, "base_url": e.url, "username": routerUser, "password": routerPass,
	})
	if res.Status != http.StatusCreated {
		e.t.Fatalf("create device: %s", res)
	}
	return res.obj()["id"].(string)
}

// addChannel creates a channel through the API and marks it healthy so that
// the dispatcher may use it.
func (e *testEnv) addChannel(deviceID, port string, extra map[string]any) string {
	e.t.Helper()
	body := map[string]any{"device_id": deviceID, "port": port}
	for k, v := range extra {
		body[k] = v
	}
	res := e.req(http.MethodPost, "/api/v1/channels", e.adminTok, body)
	if res.Status != http.StatusCreated {
		e.t.Fatalf("create channel: %s", res)
	}
	id := res.obj()["id"].(string)
	e.markHealthy(id)
	return id
}

// markHealthy stores state "ok" for the channel and reloads the dispatcher.
func (e *testEnv) markHealthy(channelID string) {
	e.t.Helper()
	err := e.st.Channels().UpdateHealth(e.ctx, domain.ID(channelID), healthOK(e.clk.Now()))
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.sync.Reload(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// pollInbox runs one inbox poll of the channel against the fake router.
func (e *testEnv) pollInbox(channelID string) {
	e.t.Helper()
	ch, err := e.st.Channels().Get(e.ctx, domain.ID(channelID))
	if err != nil {
		e.t.Fatal(err)
	}
	conn, ok := e.sync.Connector(ch.DeviceID)
	if !ok {
		e.t.Fatal("no connector for the device")
	}
	p, err := inbox.New(inbox.Config{Channel: ch, Conn: conn, Store: e.st, Clock: e.clk})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := p.PollOnce(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// eventually polls cond for up to 10 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitStatus waits until the message has the status, via the API.
func (e *testEnv) waitStatus(id, status string) map[string]any {
	e.t.Helper()
	var last map[string]any
	eventually(e.t, "message "+id+" to be "+status, func() bool {
		res := e.get("/api/v1/messages/"+id, e.readTok)
		last = res.obj()
		return last["status"] == status
	})
	return last
}

// healthWithSignal is a health update with a signal and an operator.
func healthWithSignal(at time.Time, rssi *int) store.ChannelHealthUpdate {
	return store.ChannelHealthUpdate{
		State: domain.ChannelOK, At: at, Operator: "Orange PL", Signal: &domain.Signal{RSSI: rssi},
	}
}

// healthOK is a health update that marks a channel "ok".
func healthOK(at time.Time) store.ChannelHealthUpdate {
	return store.ChannelHealthUpdate{State: domain.ChannelOK, At: at}
}

func fakeSMS(from, text string) fake.IncomingSMS { return fake.IncomingSMS{From: from, Text: text} }

func auditFilter() store.AuditFilter { return store.AuditFilter{} }

func pageRequest(n int) store.PageRequest { return store.PageRequest{Limit: n} }

// selfSignedPEM returns a PEM encoded self-signed CA certificate.
func selfSignedPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, IsCA: true,
		NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour), BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// fakePolicies builds router policies; rest=false makes every call a 401.
func fakePolicies(rest bool) fake.Policies { return fake.Policies{RESTAPI: rest, API: true} }

// rebuildAdmin recreates the admin service from the (possibly replaced)
// collaborators of the API under test.
func (e *testEnv) rebuildAdmin(reloader admin.ChannelReloader) {
	e.t.Helper()
	if reloader == nil {
		reloader = e.reloader
	}
	var err error
	e.api.admin, err = admin.New(admin.Deps{
		Store: e.st, Credentials: e.api.creds, Connectors: e.api.conns, Reloader: reloader, Health: e.api.hv,
		Clock: e.clk, Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Options: admin.Options{AllowedHTTPHosts: e.api.opts.AllowedHTTPHosts, ProbeTimeout: e.api.opts.ProbeTimeout},
	})
	if err != nil {
		e.t.Fatal(err)
	}
}
