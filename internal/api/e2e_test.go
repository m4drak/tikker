// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/sms"
)

// End to end: API -> store -> dispatcher -> RouterOS connector -> fake router,
// and fake router -> inbox poller -> store -> API. The clock is the real one.

func TestE2ESendAndReceive(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.realClock = true; c.run = true })
	ch := e.basic()

	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "Hello from tikker"})
	if res.Status != 202 {
		t.Fatal(res)
	}
	id := res.msg()["id"].(string)
	m := e.waitStatus(id, "sent")
	if m["channel_id"] != ch || m["sent_at"] == nil || m["attempts"] != float64(1) {
		t.Errorf("sent message = %v", m)
	}
	sent := e.srv.Sent()
	if len(sent) != 1 || sent[0].Message != "Hello from tikker" || !strings.HasSuffix(sent[0].PhoneNumber, "600100200") {
		t.Errorf("router got %+v", sent)
	}
	// A sent message cannot be cancelled any more.
	e.req(http.MethodDelete, messagesPath+"/"+id, e.sendTok, nil).problem(409, problemConflict)

	// Incoming SMS become visible.
	if _, err := e.srv.InjectSMS(fakeSMS("+48511222333", "Odpowiedź: tak")); err != nil {
		t.Fatal(err)
	}
	e.pollInbox(ch)
	in := e.get(messagesPath+"?direction=in", e.readTok).obj()["items"].([]any)
	if len(in) != 1 {
		t.Fatalf("inbound = %v", in)
	}
	im := in[0].(map[string]any)
	if im["body"] != "Odpowiedź: tak" || im["peer"] != "+48511222333" || im["status"] != "received" || im["encoding"] != "ucs2" {
		t.Errorf("inbound = %v", im)
	}
	if got := e.get(channelsPath+"/"+ch, e.readTok).obj()["queue_depth"]; got != float64(0) {
		t.Errorf("queue_depth = %v", got)
	}
}

func TestE2ESplitIsSentAsSeparateMessages(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.realClock = true; c.run = true; c.opts.LongMessageStrategy = sms.StrategySplit })
	e.addChannel(e.addDevice("router"), "lte1", map[string]any{"rate_per_min": 600, "max_segments": 3})
	body := strings.Repeat("abcdefghi ", 20) // 200 characters
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": body})
	if res.Status != 202 || len(res.msgs()) != 2 {
		t.Fatalf("split: %s", res)
	}
	for _, m := range res.msgs() {
		e.waitStatus(m["id"].(string), "sent")
	}
	sent := e.srv.Sent()
	if len(sent) != 2 || !strings.HasPrefix(sent[0].Message, "(1/2) ") || !strings.HasPrefix(sent[1].Message, "(2/2) ") {
		t.Errorf("router got %+v", sent)
	}
}

func TestE2EQueuedUntilChannelIsHealthyAndCancel(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.realClock = true; c.run = true })
	dev := e.addDevice("router")
	// A channel that was never checked is "down": messages wait.
	r := e.req(http.MethodPost, channelsPath, e.adminTok, map[string]any{"device_id": dev, "port": "lte1"})
	ch := r.obj()["id"].(string)
	id := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "wait"}).msg()["id"].(string)
	time.Sleep(200 * time.Millisecond)
	if m := e.get(messagesPath+"/"+id, e.readTok).obj(); m["status"] != "queued" {
		t.Fatalf("status = %v", m["status"])
	}
	if d := e.get(channelsPath+"/"+ch, e.readTok).obj()["queue_depth"]; d != float64(0) {
		// not pinned: not counted for the channel
		t.Errorf("queue_depth = %v", d)
	}
	// Cancel it before the channel comes up.
	if r := e.req(http.MethodDelete, messagesPath+"/"+id, e.sendTok, nil); r.Status != 200 {
		t.Fatal(r)
	}
	e.markHealthy(ch)
	// A fresh message is sent now; the cancelled one never is.
	id2 := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "go"}).msg()["id"].(string)
	e.waitStatus(id2, "sent")
	if n := len(e.srv.Sent()); n != 1 {
		t.Errorf("%d messages sent", n)
	}
	if m := e.get(messagesPath+"/"+id, e.readTok).obj(); m["status"] != "cancelled" {
		t.Errorf("status = %v", m["status"])
	}
}

func TestE2EPermanentFailure(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.realClock = true; c.run = true })
	e.basic()
	e.srv.SetPolicies(fakePolicies(false))
	id := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "x"}).msg()["id"].(string)
	eventually(t, "a recorded attempt", func() bool {
		m := e.get(messagesPath+"/"+id, e.readTok).obj()
		return m["attempts"].(float64) >= 1
	})
	m := e.get(messagesPath+"/"+id, e.readTok).obj()
	if m["status"] == "sent" {
		t.Errorf("message was sent despite the router refusing: %v", m)
	}
}

func TestSecretsNeverLogged(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.realClock = true; c.run = true })
	dev := e.addDevice("router")
	e.addChannel(dev, "lte1", nil)
	e.srv.SetCredentials(routerUser, "Rotated-Secret-99")
	e.req(http.MethodPatch, devicesPath+"/"+dev, e.adminTok, map[string]any{"password": "Rotated-Secret-99"})
	e.req(http.MethodPost, devicesPath+"/"+dev+"/test", e.adminTok, nil)
	e.req(http.MethodPost, usersPath, e.adminTok, map[string]any{"username": "pw.user", "password": "Another-Secret-Pw-1", "role": "viewer"})
	id := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "very private text"}).msg()["id"].(string)
	e.waitStatus(id, "sent")
	logs := e.logs.String()
	for _, secret := range []string{routerPass, "Rotated-Secret-99", "Another-Secret-Pw-1", e.adminTok, e.sendTok, e.readTok, "very private text", "600100200"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
}

// ---------------------------------------------------------------------------
// ChannelSync

func TestChannelSync(t *testing.T) {
	e := newEnv(t)
	dev := e.addDevice("router")
	ch := e.addChannel(dev, "lte1", nil)
	caps, ok := e.sync.ChannelCapabilities(domain.ID(ch))
	if !ok || caps.MaxSegments != 1 || !caps.SupportsEncoding(domain.EncodingGSM7) {
		t.Fatalf("caps = %+v ok=%v", caps, ok)
	}
	first, ok := e.sync.Connector(domain.ID(dev))
	if !ok {
		t.Fatal("no connector")
	}
	// Reloading without a change keeps the connector.
	if err := e.sync.Reload(e.ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.sync.Connector(domain.ID(dev)); again != first {
		t.Error("connector rebuilt without a change")
	}
	// A changed device gets a new connector; the old one is closed on Release.
	e.req(http.MethodPatch, devicesPath+"/"+dev, e.adminTok, map[string]any{"username": "other"})
	second, _ := e.sync.Connector(domain.ID(dev))
	if second == first {
		t.Fatal("connector not rebuilt")
	}
	e.sync.Release(second) // current: untouched
	e.sync.Release(first)  // retired: closed (idempotent)
	e.sync.Release(first)
	// A disabled device has no connector and its channels no capabilities.
	e.req(http.MethodPatch, devicesPath+"/"+dev, e.adminTok, map[string]any{"enabled": false})
	if _, ok := e.sync.Connector(domain.ID(dev)); ok {
		t.Error("disabled device kept its connector")
	}
	if _, ok := e.sync.ChannelCapabilities(domain.ID(ch)); ok {
		t.Error("disabled device kept capabilities")
	}
	if err := e.sync.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

type failingFactory struct{}

func (failingFactory) New(context.Context, connector.DeviceConfig) (connector.Connector, error) {
	return nil, errors.New("boom")
}

func TestChannelSyncFailures(t *testing.T) {
	e := newEnv(t)
	dev := e.addDevice("router")
	ch := e.addChannel(dev, "lte1", nil)
	if _, err := NewChannelSync(SyncOptions{}); err == nil {
		t.Error("empty options accepted")
	}
	s, err := NewChannelSync(SyncOptions{Store: e.st, Credentials: e.api.creds, Connectors: failingFactory{}})
	if err != nil {
		t.Fatal(err)
	}
	var got []ChannelSpec
	s.opts.Sinks = []SpecSink{SpecSinkFunc(func(sp []ChannelSpec) { got = sp })}
	if err := s.Reload(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Connector != nil || got[0].Channel.ID != domain.ID(ch) {
		t.Errorf("specs = %+v", got)
	}
	if _, ok := s.ChannelCapabilities(domain.ID(ch)); ok {
		t.Error("capabilities without a connector")
	}
	// Without a connector the API refuses to plan for the channel.
	svc, err := sending.New(sending.Deps{
		Store: e.st, Capabilities: s, Notifier: e.disp, Clock: e.api.clk, IDs: e.api.ids,
		Options: sending.Options{DefaultRegion: e.api.opts.DefaultRegion},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.api.send = svc
	e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "x", "channel_id": ch}).problem(422, problemValidation)
	e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "x"}).problem(422, problemValidation)
}

func TestNewValidatesDeps(t *testing.T) {
	if _, err := New(Deps{}); err == nil || !strings.Contains(err.Error(), "Store") {
		t.Errorf("err = %v", err)
	}
	e := newEnv(t)
	_, err := New(Deps{Store: e.st, Auth: e.auth, Dispatcher: e.disp, Reloader: e.sync, Capabilities: e.sync,
		Credentials: e.api.creds, Connectors: e.reg, Options: Options{LongMessageStrategy: "nope"}})
	if err == nil {
		t.Error("bad strategy accepted")
	}
	_, err = New(Deps{Store: e.st, Auth: e.auth, Dispatcher: e.disp, Reloader: e.sync, Capabilities: e.sync,
		Credentials: e.api.creds, Connectors: e.reg, Options: Options{AllowedHTTPHosts: []string{"1.2.3.4/99"}}})
	if err == nil {
		t.Error("bad allowlist accepted")
	}
	h, err := New(Deps{Store: e.st, Auth: e.auth, Dispatcher: e.disp, Reloader: e.sync, Capabilities: e.sync,
		Credentials: e.api.creds, Connectors: e.reg})
	if err != nil || h == nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Units

type panicNotifier struct{}

func (panicNotifier) Notify() { panic("boom") }

func TestPanicBecomesInternalError(t *testing.T) {
	e := newEnv(t)
	e.basic()
	svc, err := sending.New(sending.Deps{
		Store: e.st, Capabilities: e.sync, Notifier: panicNotifier{}, Clock: e.api.clk, IDs: e.api.ids,
		Options: sending.Options{DefaultRegion: e.api.opts.DefaultRegion},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.api.send = svc
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "x"})
	p := res.problem(500, problemInternal)
	if !strings.HasPrefix(p["instance"].(string), requestInstancePrefix) || p["detail"] != nil {
		t.Errorf("problem = %v", p)
	}
	if strings.Contains(string(res.Body), "boom") {
		t.Error("panic value leaked")
	}
}

type failReloader struct{}

func (failReloader) Reload(context.Context) error { return errors.New("reload failed") }

func TestReloadFailureDoesNotFailRequest(t *testing.T) {
	e := newEnv(t)
	e.api.reld = failReloader{}
	dev := e.req(http.MethodPost, devicesPath, e.adminTok, map[string]any{
		"name": "d", "base_url": e.url, "username": "u", "password": "p",
	})
	if dev.Status != 201 || !strings.Contains(e.logs.String(), "reloading channels") {
		t.Errorf("status %d, logs %q", dev.Status, e.logs.String())
	}
}

func TestPagingHelpers(t *testing.T) {
	ids := []domain.ID{"01A", "01B", "01C", "01D"}
	id := func(s domain.ID) domain.ID { return s }
	page, next := pageOf(append([]domain.ID(nil), ids...), id, "", 2, false)
	if len(page) != 2 || page[1] != "01B" || next == "" {
		t.Fatalf("page = %v %q", page, next)
	}
	after, err := decodeCursor(next)
	if err == nil {
		t.Errorf("non-ULID cursor accepted: %q", after)
	}
	page, next = pageOf(append([]domain.ID(nil), ids...), id, "01B", 5, true)
	if len(page) != 1 || page[0] != "01A" || next != "" {
		t.Errorf("desc page = %v %q", page, next)
	}
	if got := newPage[int](nil, ""); got.Items == nil || got.NextCursor != nil {
		t.Errorf("empty page = %+v", got)
	}
}

func TestLimiterSweep(t *testing.T) {
	e := newEnv(t)
	l := newLimiter(RateLimit{Requests: 2, Window: time.Second}, e.clk)
	for i := 0; i < limiterSweepSize; i++ {
		l.take(domain.ID(strings.Repeat("a", 3) + string(rune('A'+i%26)) + time.Duration(i).String()))
	}
	e.fake.Advance(time.Minute)
	l.take("fresh")
	if n := len(l.buckets); n > 2 {
		t.Errorf("%d buckets kept after the sweep", n)
	}
}

func TestProbeErrorCode(t *testing.T) {
	ctx := context.Background()
	if c := probeErrorCode(ctx, domain.AuthError("x", errors.New("a"))); c != domain.CodeAuthFailed {
		t.Errorf("auth -> %s", c)
	}
	if c := probeErrorCode(ctx, domain.WithCode(domain.Transient("x", errors.New("a")), domain.CodeTLSError)); c != domain.CodeTLSError {
		t.Errorf("code kept -> %s", c)
	}
	if c := probeErrorCode(ctx, domain.WithCode(domain.Transient("x", errors.New("a")), "send_failed")); c != domain.CodeUnexpectedResponse {
		t.Errorf("unknown code -> %s", c)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if c := probeErrorCode(done, errors.New("x")); c != domain.CodeTimeout {
		t.Errorf("expired ctx -> %s", c)
	}
	if got := scrub("password hunter2 here", "hunter2"); strings.Contains(got, "hunter2") {
		t.Errorf("scrub = %q", got)
	}
	if got := scrub("ab", "ab"); got != "ab" {
		t.Errorf("short secrets are left alone: %q", got)
	}
	if truncateRunes("abcdef", 3) != "abc..." {
		t.Error("string helpers")
	}
}
