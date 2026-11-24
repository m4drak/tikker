// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sending

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sms"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

var t0 = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

// routerOS is what the RouterOS REST connector can do (T00): GSM-7, one send.
var routerOS = connector.Capabilities{Encodings: []domain.Encoding{domain.EncodingGSM7}, MaxSegments: 1}

// modem can send everything.
var modem = connector.Capabilities{
	Encodings:   []domain.Encoding{domain.EncodingGSM7, domain.EncodingUCS2},
	MaxSegments: 10, Concatenation: true,
}

type caps map[domain.ID]connector.Capabilities

func (c caps) ChannelCapabilities(id domain.ID) (connector.Capabilities, bool) {
	v, ok := c[id]
	return v, ok
}

type wake struct{ n int }

func (w *wake) Notify() { w.n++ }

type env struct {
	t    *testing.T
	ctx  context.Context
	st   *sqlite.Store
	svc  *Service
	caps caps
	wake *wake
	clk  *clock.Fake
	ids  domain.IDGenerator
}

func newEnv(t *testing.T, opts Options) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	st, err := sqlite.Open(context.Background(), sqlite.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &env{t: t, ctx: context.Background(), st: st, caps: caps{}, wake: &wake{}, clk: clk, ids: domain.NewULIDGenerator(clk)}
	e.svc, err = New(Deps{Store: st, Capabilities: e.caps, Notifier: e.wake, Clock: clk, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type chOpt func(*domain.Channel)

func withTags(tags ...string) chOpt { return func(c *domain.Channel) { c.Tags = tags } }
func withTranslit() chOpt           { return func(c *domain.Channel) { c.Transliterate = true } }
func withMax(n int) chOpt           { return func(c *domain.Channel) { c.MaxSegments = n } }
func disabled() chOpt               { return func(c *domain.Channel) { c.Enabled = false } }

// channel creates an enabled device with one channel that has capabilities cp.
func (e *env) channel(port string, cp connector.Capabilities, opts ...chOpt) domain.Channel {
	e.t.Helper()
	d := domain.Device{
		ID: e.ids.NewID(), Name: "dev-" + port, Connector: domain.ConnectorRouterOSREST, BaseURL: "https://10.0.0.1",
		Username: "api", PasswordEnc: []byte("x"), TLSMode: domain.TLSVerify, Enabled: true, CreatedAt: t0,
	}
	if err := e.st.Devices().Create(e.ctx, d); err != nil {
		e.t.Fatal(err)
	}
	c := domain.Channel{
		ID: e.ids.NewID(), DeviceID: d.ID, Port: port, Name: port, RatePerMin: 6, MaxSegments: 3,
		Enabled: true, State: domain.ChannelOK,
	}
	for _, o := range opts {
		o(&c)
	}
	if err := e.st.Channels().Create(e.ctx, c); err != nil {
		e.t.Fatal(err)
	}
	e.caps[c.ID] = cp
	return c
}

func sp(s string) *string { return &s }

var actor = domain.UserActor("01JZZZZZZZZZZZZZZZZZZZZZZZ")

func TestNew(t *testing.T) {
	if _, err := New(Deps{}); err == nil || !strings.Contains(err.Error(), "Store, Capabilities, Notifier") {
		t.Errorf("err = %v", err)
	}
	e := newEnv(t, Options{})
	if e.svc.opts.DefaultRegion != "PL" {
		t.Errorf("region = %q", e.svc.opts.DefaultRegion)
	}
	_, err := New(Deps{Store: e.st, Capabilities: e.caps, Notifier: e.wake, Options: Options{LongMessageStrategy: "bogus"}})
	if err == nil {
		t.Error("an unknown strategy is accepted")
	}
	if e.svc.clk == nil || e.svc.ids == nil {
		t.Error("defaults not set")
	}
	// Without a clock and ids, the system clock and ULIDs are used.
	s, err := New(Deps{Store: e.st, Capabilities: e.caps, Notifier: e.wake})
	if err != nil || s.clk == nil || s.ids == nil {
		t.Errorf("defaults: %v", err)
	}
}

func TestPlanValidation(t *testing.T) {
	e := newEnv(t, Options{})
	ch := e.channel("lte1", routerOS)
	e.channel("lte2", routerOS, disabled(), withTags("off"))
	tooLong := strings.Repeat("a", MaxBodyChars+1)
	past := "2026-03-04T09:00:00Z"
	future := "2026-03-05T09:00:00Z"
	tests := []struct {
		name string
		req  Request
		ptr  string
		want []string // "field:code"
	}{
		{"ok", Request{To: sp("600100200"), Body: sp("hello")}, "", nil},
		{"ok with future expiry", Request{To: sp("600100200"), Body: sp("hello"), ExpiresAt: &future, ClientRef: sp("r1")}, "", nil},
		{"missing to and body", Request{}, "", []string{"/to:required", "/body:required"}},
		{"empty to", Request{To: sp(""), Body: sp("x")}, "", []string{"/to:required"}},
		{"to too long", Request{To: sp(strings.Repeat("1", MaxToLen+1)), Body: sp("x")}, "", []string{"/to:out_of_range"}},
		{"invalid number", Request{To: sp("12"), Body: sp("x")}, "", []string{"/to:invalid_phone_number"}},
		{"body too long", Request{To: sp("600100200"), Body: &tooLong}, "", []string{"/body:out_of_range"}},
		{"channel id empty", Request{To: sp("600100200"), Body: sp("x"), ChannelID: sp("")}, "", []string{"/channel_id:invalid_format"}},
		{"channel id too long", Request{To: sp("600100200"), Body: sp("x"), ChannelID: sp(strings.Repeat("a", MaxIDLen+1))}, "", []string{"/channel_id:invalid_format"}},
		{"bad tags", Request{To: sp("600100200"), Body: sp("x"), Tags: []string{"A", "ok", "ok", ""}}, "", []string{"/tags/0:invalid_format", "/tags/2:invalid_format", "/tags/3:out_of_range"}},
		{"client ref too long", Request{To: sp("600100200"), Body: sp("x"), ClientRef: sp(strings.Repeat("r", MaxClientRef+1))}, "", []string{"/client_ref:out_of_range"}},
		{"expiry malformed", Request{To: sp("600100200"), Body: sp("x"), ExpiresAt: sp("tomorrow")}, "", []string{"/expires_at:invalid_format"}},
		{"expiry past", Request{To: sp("600100200"), Body: sp("x"), ExpiresAt: &past}, "", []string{"/expires_at:expires_at_in_past"}},
		{"unknown channel", Request{To: sp("600100200"), Body: sp("x"), ChannelID: sp("nope")}, "", []string{"/channel_id:unknown_channel"}},
		{"tags without match", Request{To: sp("600100200"), Body: sp("x"), Tags: []string{"vip"}}, "", []string{"/tags:no_matching_channel"}},
		{"batch pointer", Request{Body: sp("x")}, "/messages/3", []string{"/messages/3/to:required"}},
		{"polish text, GSM-7 only", Request{To: sp("600100200"), Body: sp("Zażółć")}, "", []string{"/body:unsupported_characters"}},
		{"too long for one send", Request{To: sp("600100200"), Body: sp(strings.Repeat("a", 161))}, "", []string{"/body:too_many_segments"}},
	}
	_ = ch
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, err := e.svc.LoadRouting(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			msgs, errs, err := e.svc.Plan(rt, tt.req, tt.ptr, actor)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, fe := range errs {
				got = append(got, fe.Field+":"+fe.Code)
				if fe.Detail == "" {
					t.Errorf("%s has no detail", fe.Field)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("errors = %v, want %v", got, tt.want)
			}
			if (len(tt.want) == 0) != (len(msgs) > 0) {
				t.Errorf("%d messages for errors %v", len(msgs), got)
			}
		})
	}
}

func TestPlanChannelSelection(t *testing.T) {
	e := newEnv(t, Options{})
	a := e.channel("lte1", routerOS, withTags("a"))
	off := e.channel("lte2", routerOS, disabled())
	noConn := e.channel("lte3", routerOS)
	delete(e.caps, noConn.ID)
	rt, err := e.svc.LoadRouting(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u := rt.Usable(); len(u) != 2 {
		t.Errorf("usable = %d channels", len(u))
	}
	plan := func(req Request) (string, string) {
		_, errs, err := e.svc.Plan(rt, req, "", actor)
		if err != nil {
			t.Fatal(err)
		}
		if len(errs) != 1 {
			return "", ""
		}
		return errs[0].Field, errs[0].Code
	}
	if f, c := plan(Request{To: sp("600100200"), Body: sp("x"), ChannelID: sp(string(off.ID))}); f != "/channel_id" || c != CodeChannelDisabled {
		t.Errorf("disabled channel: %s %s", f, c)
	}
	if f, c := plan(Request{To: sp("600100200"), Body: sp("x"), ChannelID: sp(string(noConn.ID))}); f != "/channel_id" || c != CodeChannelDisabled {
		t.Errorf("channel without connector: %s %s", f, c)
	}
	if f, c := plan(Request{To: sp("600100200"), Body: sp("x"), Tags: []string{"zzz"}}); f != "/tags" || c != CodeNoMatchingChannel {
		t.Errorf("tags: %s %s", f, c)
	}
	msgs, errs, err := e.svc.Plan(rt, Request{To: sp("600100200"), Body: sp("x"), ChannelID: sp(string(a.ID)), Tags: []string{"a"}, ClientRef: sp("ref")}, "", actor)
	if err != nil || len(errs) > 0 || len(msgs) != 1 {
		t.Fatalf("pinned: %v %v %d", err, errs, len(msgs))
	}
	m := msgs[0]
	if m.Route.ChannelID != a.ID || !reflect.DeepEqual(m.Route.Tags, []string{"a"}) || m.Peer != "+48600100200" ||
		m.Status != domain.StatusQueued || m.ClientRef != "ref" || m.CreatedBy != actor || m.Direction != domain.DirectionOut {
		t.Errorf("message = %+v", m)
	}

	// No usable channel at all.
	e2 := newEnv(t, Options{})
	rt2, _ := e2.svc.LoadRouting(e2.ctx)
	_, errs, _ = e2.svc.Plan(rt2, Request{To: sp("600100200"), Body: sp("x")}, "", actor)
	if len(errs) != 1 || errs[0].Code != CodeNoMatchingChannel || errs[0].Field != "" {
		t.Errorf("no channel: %+v", errs)
	}
}

func TestPlanTextForChannel(t *testing.T) {
	t.Run("transliteration makes Polish text sendable", func(t *testing.T) {
		e := newEnv(t, Options{})
		e.channel("lte1", routerOS, withTranslit())
		rt, _ := e.svc.LoadRouting(e.ctx)
		msgs, errs, err := e.svc.Plan(rt, Request{To: sp("600100200"), Body: sp("Zażółć gęślą")}, "", actor)
		if err != nil || len(errs) > 0 || len(msgs) != 1 {
			t.Fatalf("%v %v %d", err, errs, len(msgs))
		}
		if msgs[0].Body != "Zazolc gesla" || msgs[0].Encoding != domain.EncodingGSM7 {
			t.Errorf("message = %q %s", msgs[0].Body, msgs[0].Encoding)
		}
	})
	t.Run("UCS-2 channel keeps the text", func(t *testing.T) {
		e := newEnv(t, Options{})
		e.channel("lte1", modem)
		rt, _ := e.svc.LoadRouting(e.ctx)
		msgs, errs, _ := e.svc.Plan(rt, Request{To: sp("600100200"), Body: sp("Zażółć")}, "", actor)
		if len(errs) > 0 || msgs[0].Body != "Zażółć" || msgs[0].Encoding != domain.EncodingUCS2 {
			t.Errorf("%v %+v", errs, msgs)
		}
	})
	t.Run("split strategy gives several messages", func(t *testing.T) {
		e := newEnv(t, Options{LongMessageStrategy: sms.StrategySplit})
		e.channel("lte1", routerOS, withMax(5))
		rt, _ := e.svc.LoadRouting(e.ctx)
		body := strings.Repeat("word ", 50) // 250 characters
		msgs, errs, err := e.svc.Plan(rt, Request{To: sp("600100200"), Body: &body, ExpiresAt: sp("2026-03-05T09:00:00Z")}, "", actor)
		if err != nil || len(errs) > 0 || len(msgs) < 2 {
			t.Fatalf("%v %v %d", err, errs, len(msgs))
		}
		for i, m := range msgs {
			if !strings.HasPrefix(m.Body, "(") || m.Segments != 1 || m.ExpiresAt.IsZero() {
				t.Errorf("part %d = %+v", i, m)
			}
		}
	})
}

func TestStrictest(t *testing.T) {
	a := connector.Capabilities{Encodings: []domain.Encoding{domain.EncodingGSM7, domain.EncodingUCS2}, MaxSegments: 3, Concatenation: true}
	b := connector.Capabilities{Encodings: []domain.Encoding{domain.EncodingGSM7}, MaxSegments: 1}
	got := strictest(a, b)
	if got.MaxSegments != 1 || got.Concatenation || len(got.Encodings) != 1 || got.Encodings[0] != domain.EncodingGSM7 {
		t.Errorf("strictest = %+v", got)
	}
}

// Two channels with different capabilities: the plan must fit both.
func TestPlanStrictestChannels(t *testing.T) {
	e := newEnv(t, Options{})
	e.channel("lte1", modem, withTags("a"), withMax(2))
	e.channel("lte2", routerOS, withTags("a"), withMax(3), withTranslit())
	rt, _ := e.svc.LoadRouting(e.ctx)
	msgs, errs, err := e.svc.Plan(rt, Request{To: sp("600100200"), Body: sp("Zażółć"), Tags: []string{"a"}}, "", actor)
	if err != nil || len(errs) > 0 {
		t.Fatalf("%v %v", err, errs)
	}
	// Transliteration is on for the pair, because one channel needs it.
	if msgs[0].Body != "Zazolc" {
		t.Errorf("body = %q", msgs[0].Body)
	}
}

func TestDetails(t *testing.T) {
	if d := unsupportedDetail("abc"); !strings.Contains(d, "cannot send") {
		t.Errorf("detail without a non-GSM character = %q", d)
	}
	if d := unsupportedDetail("ażb"); !strings.Contains(d, "ż") || !strings.Contains(d, "transliterate") {
		t.Errorf("detail = %q", d)
	}
	if got := planDetail(errors.New("sms: too long")); got != "Too long." {
		t.Errorf("planDetail = %q", got)
	}
	if got := planDetail(errors.New("")); got != "." {
		t.Errorf("planDetail empty = %q", got)
	}
	if got := planDetail(errors.New("sms: ends well" + ".")); got != "Ends well." {
		t.Errorf("planDetail = %q", got)
	}
}

func TestEnqueueAndNotify(t *testing.T) {
	e := newEnv(t, Options{})
	e.channel("lte1", routerOS)
	rt, _ := e.svc.LoadRouting(e.ctx)
	msgs, _, _ := e.svc.Plan(rt, Request{To: sp("600100200"), Body: sp("hi")}, "", actor)
	idem := store.Idempotency{Key: "k1", RequestHash: "h", Since: t0.Add(-time.Hour)}
	res, err := e.svc.Enqueue(e.ctx, msgs, idem)
	if err != nil || res.Replayed || len(res.Messages) != 1 || e.wake.n != 1 {
		t.Fatalf("%v %+v woke %d", err, res, e.wake.n)
	}
	// A replay does not wake the queue; a different hash is a mismatch.
	res, err = e.svc.Enqueue(e.ctx, msgs, idem)
	if err != nil || !res.Replayed || e.wake.n != 1 {
		t.Errorf("replay: %v %+v woke %d", err, res, e.wake.n)
	}
	idem.RequestHash = "other"
	if _, err = e.svc.Enqueue(e.ctx, msgs, idem); !errors.Is(err, domain.ErrIdempotencyMismatch) {
		t.Errorf("mismatch: %v", err)
	}
}

func (e *env) audit() []domain.AuditLog {
	e.t.Helper()
	p, err := e.st.Audit().List(e.ctx, store.AuditFilter{}, store.PageRequest{Limit: 50})
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Items
}

func TestSend(t *testing.T) {
	e := newEnv(t, Options{})
	e.channel("lte1", routerOS)
	req := Request{To: sp("600 100 200"), Body: sp("hello")}

	res, errs, err := e.svc.Send(e.ctx, actor, req, SendOptions{IdempotencyKey: "ui:abc"})
	if err != nil || len(errs) > 0 || len(res.Messages) != 1 || e.wake.n != 1 {
		t.Fatalf("send: %v %v %+v", err, errs, res)
	}
	log := e.audit()
	if len(log) != 1 || log[0].Action != domain.AuditMessageSend || log[0].Actor != actor ||
		log[0].Target != "message:"+string(res.Messages[0].ID) || log[0].Meta["parts"] != "1" {
		t.Fatalf("audit = %+v", log)
	}
	for k, v := range log[0].Meta {
		if strings.Contains(k+v, "hello") || strings.Contains(k+v, "600") {
			t.Errorf("audit leaks content: %s=%s", k, v)
		}
	}

	// The same submission again: same message, no second audit entry or wake-up.
	res2, errs, err := e.svc.Send(e.ctx, actor, req, SendOptions{IdempotencyKey: "ui:abc"})
	if err != nil || len(errs) > 0 || res2.Messages[0].ID != res.Messages[0].ID || !res2.Replayed || len(e.audit()) != 1 || e.wake.n != 1 {
		t.Errorf("replay: %v %v %+v", err, errs, res2)
	}
	// The key reused with other content.
	other := Request{To: sp("600100200"), Body: sp("different")}
	if _, _, err = e.svc.Send(e.ctx, actor, other, SendOptions{IdempotencyKey: "ui:abc"}); !errors.Is(err, domain.ErrIdempotencyMismatch) {
		t.Errorf("mismatch: %v", err)
	}
	// Without a key every call sends.
	if _, _, err = e.svc.Send(e.ctx, actor, req, SendOptions{}); err != nil || len(e.audit()) != 2 {
		t.Errorf("no key: %v, %d audit entries", err, len(e.audit()))
	}
	// Validation problems are returned, nothing is stored.
	_, errs, err = e.svc.Send(e.ctx, actor, Request{Body: sp("x")}, SendOptions{})
	if err != nil || len(errs) != 1 || errs[0].Code != CodeRequired {
		t.Errorf("invalid: %v %v", err, errs)
	}
	if n, _ := e.st.Messages().Count(e.ctx, store.MessageFilter{}); n != 2 {
		t.Errorf("%d messages stored", n)
	}
}

func TestSendBatch(t *testing.T) {
	e := newEnv(t, Options{})
	e.channel("lte1", routerOS)
	reqs := []Request{
		{To: sp("600100200"), Body: sp("hello")},
		{To: sp("600100201"), Body: sp("hello")},
	}
	key := SendOptions{IdempotencyKey: "smtp:k1", Source: "smtp"}

	tests := []struct {
		name      string
		reqs      []Request
		so        SendOptions
		wantErr   error
		wantField string
		wantMsgs  int
		replayed  bool
	}{
		{name: "two recipients", reqs: reqs, so: key, wantMsgs: 2},
		{name: "replay before validation", reqs: reqs, so: key, wantMsgs: 2, replayed: true},
		{name: "same key, other request", reqs: reqs[:1], so: key, wantErr: domain.ErrIdempotencyMismatch},
		{name: "one invalid: nothing stored", reqs: []Request{reqs[0], {To: sp("12"), Body: sp("x")}}, so: SendOptions{Source: "smtp"}, wantField: "/messages/1/to"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, errs, err := e.svc.SendBatch(e.ctx, actor, tt.reqs, tt.so)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			case err != nil:
				t.Fatal(err)
			case tt.wantField != "":
				if len(errs) != 1 || errs[0].Field != tt.wantField {
					t.Fatalf("errs = %+v", errs)
				}
				return
			}
			if len(errs) > 0 || len(res.Messages) != tt.wantMsgs || res.Replayed != tt.replayed {
				t.Fatalf("res = %+v, errs = %v", res, errs)
			}
		})
	}
	if n, _ := e.st.Messages().Count(e.ctx, store.MessageFilter{}); n != 2 {
		t.Errorf("%d messages stored, want 2", n)
	}
	log := e.audit()
	if len(log) != 1 || log[0].Meta["source"] != "smtp" || log[0].Meta["recipients"] != "2" || e.wake.n != 1 {
		t.Errorf("audit = %+v, wake-ups %d", log, e.wake.n)
	}
	if _, _, err := e.svc.SendBatch(e.ctx, actor, nil, SendOptions{}); err == nil {
		t.Error("empty batch: no error")
	}
}

func TestSendSplitAudit(t *testing.T) {
	e := newEnv(t, Options{LongMessageStrategy: sms.StrategySplit})
	e.channel("lte1", routerOS, withMax(5))
	body := strings.Repeat("word ", 50)
	res, errs, err := e.svc.Send(e.ctx, actor, Request{To: sp("600100200"), Body: &body}, SendOptions{})
	if err != nil || len(errs) > 0 || len(res.Messages) < 2 {
		t.Fatalf("%v %v %d", err, errs, len(res.Messages))
	}
	if log := e.audit(); len(log) != 1 || log[0].Meta["parts"] == "1" {
		t.Errorf("audit = %+v", log)
	}
}

func TestSendRoutingError(t *testing.T) {
	e := newEnv(t, Options{})
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Send(e.ctx, actor, Request{To: sp("600100200"), Body: sp("x")}, SendOptions{}); err == nil {
		t.Error("closed store: no error")
	}
	if _, err := e.svc.Preview(e.ctx, PreviewRequest{Body: "x"}); err == nil {
		t.Error("closed store: preview without error")
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t, Options{})
	e.channel("lte1", routerOS)
	res, _, err := e.svc.Send(e.ctx, actor, Request{To: sp("600100200"), Body: sp("x")}, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Messages[0].ID
	m, err := e.svc.Cancel(e.ctx, actor, id)
	if err != nil || m.Status != domain.StatusCancelled {
		t.Fatalf("cancel: %v %+v", err, m)
	}
	log := e.audit()
	if len(log) != 2 || log[0].Action != domain.AuditMessageCancel || log[0].Target != "message:"+string(id) {
		t.Errorf("audit = %+v", log)
	}
	m, err = e.svc.Cancel(e.ctx, actor, id)
	if !errors.Is(err, domain.ErrInvalidTransition) || m.Status != domain.StatusCancelled || len(e.audit()) != 2 {
		t.Errorf("second cancel: %v %+v", err, m)
	}
	if _, err = e.svc.Cancel(e.ctx, actor, e.ids.NewID()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

func TestNormalizePeer(t *testing.T) {
	e := newEnv(t, Options{})
	for in, want := range map[string]string{"600 100 200": "+48600100200", "+48600100200": "+48600100200", "InfoSMS": "InfoSMS", "12": "12"} {
		if got := e.svc.NormalizePeer(in); got != want {
			t.Errorf("NormalizePeer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreview(t *testing.T) {
	e := newEnv(t, Options{LongMessageStrategy: sms.StrategySplit})
	plain := e.channel("plain", routerOS, withTags("plain"), withMax(5))
	tr := e.channel("translit", routerOS, withTags("tr"), withTranslit(), withMax(1))
	e.channel("ucs", modem, withTags("ucs"), withMax(5))
	off := e.channel("off", routerOS, disabled())

	field := func(p Preview) []string {
		var out []string
		for _, fe := range p.Errors {
			out = append(out, fe.Field+":"+fe.Code)
		}
		return out
	}
	prev := func(req PreviewRequest) Preview {
		t.Helper()
		p, err := e.svc.Preview(e.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("empty text", func(t *testing.T) {
		p := prev(PreviewRequest{Tags: []string{"plain"}})
		if p.Chars != 0 || p.Segments != 0 || p.Planned || len(p.Errors) != 0 || p.Channels != 1 || p.MaxSegments != 5 {
			t.Errorf("%+v", p)
		}
	})
	t.Run("plain GSM-7", func(t *testing.T) {
		p := prev(PreviewRequest{Body: "hello", ChannelID: string(plain.ID)})
		if p.Chars != 5 || p.Segments != 1 || p.Encoding != domain.EncodingGSM7 || !p.Planned || p.Parts != 1 || p.TotalSegments != 1 ||
			p.PerSegment != 160 || p.Remaining != 155 || p.Transliterate {
			t.Errorf("%+v", p)
		}
	})
	t.Run("Polish text without transliteration is refused", func(t *testing.T) {
		p := prev(PreviewRequest{Body: "Zażółć", ChannelID: string(plain.ID)})
		if !reflect.DeepEqual(field(p), []string{"/body:unsupported_characters"}) || p.Planned || p.Encoding != domain.EncodingUCS2 ||
			len(p.NonGSM) == 0 {
			t.Errorf("%+v", p)
		}
	})
	t.Run("Polish text with transliteration", func(t *testing.T) {
		p := prev(PreviewRequest{Body: "Zażółć gęślą", ChannelID: string(tr.ID)})
		if len(p.Errors) != 0 || !p.Transliterate || !p.Changed || p.Encoding != domain.EncodingGSM7 || !p.Planned || p.Chars != 12 || len(p.Substituted) != 0 {
			t.Errorf("%+v", p)
		}
		if len(p.NonGSM) == 0 {
			t.Error("the characters that need UCS-2 are not listed")
		}
	})
	t.Run("characters that cannot be converted", func(t *testing.T) {
		p := prev(PreviewRequest{Body: "ok 中", ChannelID: string(tr.ID)})
		if string(p.Substituted) != "中" {
			t.Errorf("substituted = %q", string(p.Substituted))
		}
	})
	t.Run("UCS-2 channel", func(t *testing.T) {
		p := prev(PreviewRequest{Body: "Zażółć", Tags: []string{"ucs"}})
		if len(p.Errors) != 0 || p.Encoding != domain.EncodingUCS2 || p.Transliterate || !p.Planned {
			t.Errorf("%+v", p)
		}
	})
	t.Run("split shows the parts", func(t *testing.T) {
		p := prev(PreviewRequest{Body: strings.Repeat("word ", 50), ChannelID: string(plain.ID)})
		if len(p.Errors) != 0 || p.Parts < 2 || p.TotalSegments != p.Parts || p.Segments < 2 {
			t.Errorf("%+v", p)
		}
	})
	t.Run("over the segment limit", func(t *testing.T) {
		p := prev(PreviewRequest{Body: strings.Repeat("word ", 300), ChannelID: string(tr.ID)})
		if !reflect.DeepEqual(field(p), []string{"/body:too_many_segments"}) || p.Planned {
			t.Errorf("%+v", p)
		}
	})
	t.Run("body over the character limit", func(t *testing.T) {
		p := prev(PreviewRequest{Body: strings.Repeat("a", MaxBodyChars+1), ChannelID: string(plain.ID)})
		if !reflect.DeepEqual(field(p), []string{"/body:out_of_range"}) {
			t.Errorf("%+v", p)
		}
	})
	t.Run("channel problems", func(t *testing.T) {
		for _, tt := range []struct {
			req  PreviewRequest
			want []string
		}{
			{PreviewRequest{Body: "x", ChannelID: "nope"}, []string{"/channel_id:unknown_channel"}},
			{PreviewRequest{Body: "x", ChannelID: string(off.ID)}, []string{"/channel_id:channel_disabled"}},
			{PreviewRequest{Body: "x", Tags: []string{"nothing"}}, []string{"/tags:no_matching_channel"}},
			{PreviewRequest{Body: "x", ChannelID: strings.Repeat("a", MaxIDLen+1)}, []string{"/channel_id:invalid_format"}},
			{PreviewRequest{Body: "x", Tags: []string{"Bad"}}, []string{"/tags/0:invalid_format"}},
		} {
			p := prev(tt.req)
			if !reflect.DeepEqual(field(p), tt.want) || p.Channels != 0 || p.Planned {
				t.Errorf("%+v: %+v", tt.req, p)
			}
			if p.Chars != 1 {
				t.Errorf("the text is still analysed: %+v", p)
			}
		}
	})
}
