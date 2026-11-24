// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package mailgw

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const testDomain = "sms.example"

var t0 = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

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

type caps map[domain.ID]connector.Capabilities

func (c caps) ChannelCapabilities(id domain.ID) (connector.Capabilities, bool) {
	v, ok := c[id]
	return v, ok
}

type nopNotifier struct{}

func (nopNotifier) Notify() {}

// env is a running gateway backed by a real store, auth and sending service.
type env struct {
	t     *testing.T
	ctx   context.Context
	st    *sqlite.Store
	clk   *clock.Fake
	auth  *auth.Service
	srv   *Server
	addr  string
	logs  *syncBuffer
	caps  caps
	tls   *tls.Config // client config trusting the test certificate (nil: insecure server)
	admin domain.User
	done  chan error

	// Tokens: sms:send, sms:read only, an operator's sms:send.
	send, read, operator string
	operatorUser         domain.User
}

type envOpt func(*Options)

func withTLS(implicit bool) envOpt {
	return func(o *Options) {
		o.Insecure = false
		o.TLSConfig = serverTLS()
		o.ImplicitTLS = implicit
	}
}

var (
	certOnce   sync.Once
	certServer *tls.Config
	certPool   *x509.CertPool
)

// serverTLS returns a self-signed certificate for 127.0.0.1, generated once.
func serverTLS() *tls.Config {
	certOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tikker test"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			panic(err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			panic(err)
		}
		certPool = x509.NewCertPool()
		certPool.AddCert(leaf)
		certServer = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		}
	})
	return certServer
}

func clientTLS() *tls.Config {
	serverTLS()
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: certPool, ServerName: "127.0.0.1"}
}

// newEnv starts an insecure (loopback) gateway unless an option adds TLS.
func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(t0)
	st, err := sqlite.Open(ctx, sqlite.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	as, err := auth.New(auth.Config{Store: st, Clock: clk, Logger: log,
		Params: auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.Bootstrap(ctx, "admin-password-123"); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: ctx, st: st, clk: clk, auth: as, logs: logs, caps: caps{}, done: make(chan error, 1)}
	users, err := st.Users().List(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("users: %v %v", users, err)
	}
	e.admin = users[0]
	e.send = e.token(e.admin.ID, domain.Scopes{domain.ScopeSMSSend}, time.Time{})
	e.read = e.token(e.admin.ID, domain.Scopes{domain.ScopeSMSRead}, time.Time{})
	e.operatorUser, err = as.CreateUser(ctx, domain.SystemActor, auth.NewUser{Username: "operator", Password: "operator-password-1", Role: domain.RoleOperator})
	if err != nil {
		t.Fatal(err)
	}
	e.operator = e.token(e.operatorUser.ID, domain.Scopes{domain.ScopeSMSSend}, time.Time{})
	e.channel()

	snd, err := sending.New(sending.Deps{Store: st, Capabilities: e.caps, Notifier: nopNotifier{}, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		Domain: testDomain, Insecure: true, Auth: as, Sender: snd, Clock: clk, Logger: log,
		CommandTimeout: 5 * time.Second, DataTimeout: 5 * time.Second,
	}
	for _, f := range opts {
		f(&o)
	}
	e.start(o)
	if o.TLSConfig != nil {
		e.tls = clientTLS()
	}
	return e
}

// start runs a server with o and stops it at the end of the test.
func (e *env) start(o Options) {
	e.t.Helper()
	srv, err := New(o)
	if err != nil {
		e.t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv, e.addr = srv, ln.Addr().String()
	go func() { e.done <- srv.Serve(ln) }()
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			e.t.Errorf("Shutdown() = %v", err)
		}
		if err := <-e.done; !errors.Is(err, ErrServerClosed) {
			e.t.Errorf("Serve() = %v", err)
		}
	})
}

func (e *env) token(user domain.ID, scopes domain.Scopes, expires time.Time) string {
	e.t.Helper()
	_, raw, err := e.auth.CreateToken(e.ctx, domain.SystemActor, auth.NewToken{UserID: user, Name: "t" + string(scopes[0]) + expires.String(), Scopes: scopes, ExpiresAt: expires})
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

// channel creates an enabled GSM-7 channel with transliteration.
func (e *env) channel() domain.Channel {
	e.t.Helper()
	ids := domain.NewULIDGenerator(e.clk)
	d := domain.Device{
		ID: ids.NewID(), Name: "router", Connector: domain.ConnectorRouterOSREST, BaseURL: "https://10.0.0.1",
		Username: "api", PasswordEnc: []byte("x"), TLSMode: domain.TLSVerify, Enabled: true, CreatedAt: t0,
	}
	if err := e.st.Devices().Create(e.ctx, d); err != nil {
		e.t.Fatal(err)
	}
	c := domain.Channel{
		ID: ids.NewID(), DeviceID: d.ID, Port: "lte1", Name: "lte1", RatePerMin: 6, MaxSegments: 3,
		Enabled: true, State: domain.ChannelOK, Transliterate: false,
	}
	if err := e.st.Channels().Create(e.ctx, c); err != nil {
		e.t.Fatal(err)
	}
	e.caps[c.ID] = connector.Capabilities{
		Encodings: []domain.Encoding{domain.EncodingGSM7, domain.EncodingUCS2}, MaxSegments: 3, Concatenation: true,
	}
	return c
}

// messages returns the stored outbound messages, oldest first.
func (e *env) messages() []domain.Message {
	e.t.Helper()
	p, err := e.st.Messages().List(e.ctx, store.MessageFilter{}, store.PageRequest{Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	out := p.Items
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// conn is a raw SMTP client for protocol tests.
type conn struct {
	t  *testing.T
	nc net.Conn
	tp *textproto.Conn
}

func (e *env) dial() *conn {
	e.t.Helper()
	nc, err := net.DialTimeout("tcp", e.addr, 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	_ = nc.SetDeadline(time.Now().Add(10 * time.Second))
	c := &conn{t: e.t, nc: nc, tp: textproto.NewConn(nc)}
	e.t.Cleanup(func() { _ = c.tp.Close() })
	return c
}

// expect reads one (possibly multi-line) reply and checks its code.
func (c *conn) expect(code int) string {
	c.t.Helper()
	got, msg, err := c.tp.ReadResponse(0)
	if err != nil && got == 0 {
		c.t.Fatalf("read reply (want %d): %v", code, err)
	}
	if got != code {
		c.t.Fatalf("reply %d %q, want %d", got, msg, code)
	}
	return msg
}

// cmd sends a command and checks the reply code.
func (c *conn) cmd(code int, format string, args ...any) string {
	c.t.Helper()
	if err := c.tp.PrintfLine(format, args...); err != nil {
		c.t.Fatalf("send %q: %v", format, err)
	}
	return c.expect(code)
}

// raw writes bytes as they are.
func (c *conn) raw(s string) {
	c.t.Helper()
	if _, err := c.nc.Write([]byte(s)); err != nil {
		c.t.Fatal(err)
	}
}

// startTLS upgrades the raw connection after a 220 to STARTTLS.
func (c *conn) startTLS() {
	c.t.Helper()
	c.cmd(220, "STARTTLS")
	tc := tls.Client(c.nc, clientTLS())
	if err := tc.Handshake(); err != nil {
		c.t.Fatal(err)
	}
	c.nc = tc
	c.tp = textproto.NewConn(tc)
}

// login greets and authenticates with AUTH PLAIN.
func (c *conn) login(token string) {
	c.t.Helper()
	c.cmd(250, "EHLO client.test")
	c.cmd(235, "AUTH PLAIN %s", plain("token", token))
}

func plain(user, pass string) string {
	return b64("\x00" + user + "\x00" + pass)
}

// mailText builds a simple text/plain e-mail.
func mailText(msgID, body string) string {
	var b strings.Builder
	b.WriteString("From: monitor@example.net\r\nTo: 600100200@" + testDomain + "\r\nSubject: alert\r\n")
	if msgID != "" {
		b.WriteString("Message-ID: <" + msgID + ">\r\n")
	}
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return b.String()
}
