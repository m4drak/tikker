// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package inbox

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/connector/routeros"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

const (
	routerUser = "tikker"
	routerPass = "s3cr3t-Pa55"
	sender     = "+48511222333"
)

// logBuf is a goroutine-safe buffer behind a debug-level slog handler.
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

// reporter records ReportInbox calls.
type reporter struct {
	mu    sync.Mutex
	calls []error
}

func (r *reporter) ReportInbox(_ domain.ID, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, err)
}

func (r *reporter) get() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// noPDU is a connector that does not report PDUs (Capabilities.InboundPDU
// false), like a connector for another device family.
type noPDU struct{ connector.Connector }

func (n noPDU) Capabilities() connector.Capabilities {
	c := n.Connector.Capabilities()
	c.InboundPDU = false
	return c
}

type env struct {
	t    *testing.T
	ctx  context.Context
	clk  *clock.Fake
	st   *sqlite.Store
	path string
	srv  *fake.Server
	conn connector.Connector
	dev  domain.Device
	ch   domain.Channel
	logs *logBuf
	rep  *reporter
	p    *Poller

	mu     sync.Mutex
	hooked []domain.Message
}

// newEnv wires a file-backed store, a fake RouterOS, the RouterOS connector and
// a poller. mod may adjust the poller config and wrap the connector.
func newEnv(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	ctx := context.Background()
	e := &env{t: t, ctx: ctx, clk: clock.NewFake(t0), logs: &logBuf{}, rep: &reporter{}}

	e.path = filepath.Join(t.TempDir(), "tikker.db")
	st, err := sqlite.Open(ctx, sqlite.Options{Path: e.path, Clock: e.clk})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st

	e.srv = fake.New(fake.Options{User: routerUser, Password: routerPass, Ports: []string{"lte1", "lte2"}, Now: e.clk.Now})
	url := e.srv.Start()
	t.Cleanup(e.srv.Close)

	e.dev = domain.Device{
		ID: "01JDEVICE0000000000000000A", Name: "r1", Connector: domain.ConnectorRouterOSREST, BaseURL: url,
		Username: routerUser, PasswordEnc: []byte("x"), TLSMode: domain.TLSVerify, Enabled: true, CreatedAt: t0,
	}
	e.ch = domain.Channel{
		ID: "01JCHANNEL000000000000000A", DeviceID: e.dev.ID, Port: "lte1", Name: "r1/lte1",
		RatePerMin: 6, MaxSegments: 3, Enabled: true, State: domain.ChannelOK,
	}
	for _, err := range []error{st.Devices().Create(ctx, e.dev), st.Channels().Create(ctx, e.ch)} {
		if err != nil {
			t.Fatal(err)
		}
	}

	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rc, err := routeros.NewConn(ctx, connector.DeviceConfig{
		DeviceID: e.dev.ID, Kind: domain.ConnectorRouterOSREST, BaseURL: url,
		Username: routerUser, Password: routerPass, TLSMode: domain.TLSVerify,
	}, routeros.Options{Logger: log, Clock: e.clk})
	if err != nil {
		t.Fatalf("routeros.NewConn: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	e.conn = rc

	cfg := Config{
		Channel: e.ch, Conn: rc, Store: st, Clock: e.clk, Logger: log,
		Interval: 15 * time.Second, Reporter: e.rep,
		OnMessage: func(m domain.Message) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.hooked = append(e.hooked, m)
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	e.p, err = New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func (e *env) hookedMessages() []domain.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.hooked)
}

// inject adds an SMS to the fake inbox.
func (e *env) inject(text string) {
	e.t.Helper()
	if _, err := e.srv.InjectSMS(fake.IncomingSMS{From: sender, Text: text}); err != nil {
		e.t.Fatalf("InjectSMS: %v", err)
	}
}

// injectParts splits text into concatenated parts and injects them in the
// given order (1-based part numbers).
func (e *env) injectParts(text string, ref uint16, order ...int) {
	e.t.Helper()
	parts, err := fake.BuildDeliverPDUs(fake.PDUParams{SMSC: "+48000000001", Sender: sender, Time: e.clk.Now()}, text, ref, false)
	if err != nil {
		e.t.Fatalf("BuildDeliverPDUs: %v", err)
	}
	if order == nil {
		for i := range parts {
			order = append(order, i+1)
		}
	}
	for _, n := range order {
		if _, err := e.srv.InjectRaw(fake.RawInbox{Phone: sender, Timestamp: e.clk.Now(), PDU: parts[n-1].PDU, Message: parts[n-1].Text}); err != nil {
			e.t.Fatalf("InjectRaw: %v", err)
		}
	}
}

func (e *env) poll() CycleResult {
	e.t.Helper()
	res, err := e.p.PollOnce(e.ctx)
	if err != nil {
		e.t.Fatalf("PollOnce: %v", err)
	}
	return res
}

// inbound returns the stored inbound messages, oldest first.
func (e *env) inbound() []domain.Message {
	e.t.Helper()
	var out []domain.Message
	cursor := ""
	for {
		page, err := e.st.Messages().List(e.ctx, store.MessageFilter{Direction: domain.DirectionIn}, store.PageRequest{Limit: 100, Cursor: cursor})
		if err != nil {
			e.t.Fatalf("List: %v", err)
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	slices.Reverse(out)
	return out
}

func (e *env) routerInbox() int { return len(e.srv.Inbox()) }

func (e *env) pending() int {
	e.t.Helper()
	p, err := e.st.InboundParts().ListPending(e.ctx, e.ch.ID)
	if err != nil {
		e.t.Fatalf("ListPending: %v", err)
	}
	return len(p)
}
