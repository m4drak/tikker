// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

func TestSingleMessages(t *testing.T) {
	tests := []struct {
		name string
		text string
		enc  domain.Encoding
	}{
		{"gsm7", "Kod: 1234. Nie podawaj go nikomu!", domain.EncodingGSM7},
		{"gsm7 extension characters", "Cena: 5 € [promo] {x}", domain.EncodingGSM7},
		{"ucs2 polish", "Zażółć gęślą jaźń", domain.EncodingUCS2},
		{"ucs2 emoji", "Hej 😀 do zobaczenia 🎉", domain.EncodingUCS2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.inject(tt.text)
			res := e.poll()
			if res.Fetched != 1 || res.Stored != 1 || res.Deleted != 1 || res.Duplicates != 0 {
				t.Fatalf("result = %+v", res)
			}
			got := e.inbound()
			if len(got) != 1 {
				t.Fatalf("stored %d messages", len(got))
			}
			m := got[0]
			if m.Body != tt.text || m.Encoding != tt.enc || m.Peer != sender || m.Segments != 1 ||
				m.Direction != domain.DirectionIn || m.Status != domain.StatusReceived ||
				m.ChannelID != e.ch.ID || m.LastError != "" || m.DedupHash == "" {
				t.Fatalf("message = %+v", m)
			}
			if !m.ReceivedAt.Equal(t0) {
				t.Errorf("ReceivedAt = %v, want %v", m.ReceivedAt, t0)
			}
			if n := e.routerInbox(); n != 0 {
				t.Errorf("router inbox still holds %d entries", n)
			}
			if h := e.hookedMessages(); len(h) != 1 || h[0].ID != m.ID {
				t.Errorf("hook calls = %+v", h)
			}
		})
	}
}

// The dedup hash is computed from the raw router data: the PDU when there is
// one (ADR 0002 §4).
func TestDedupHashFromRawPDU(t *testing.T) {
	e := newEnv(t, nil)
	e.inject("Zażółć gęślą jaźń")
	entry := e.srv.Inbox()[0]
	e.poll()
	want := domain.InboundDedupHash(e.dev.ID, "lte1", sender, fake.TimestampISO(entry.Timestamp), entry.PDU)
	if got := e.inbound()[0].DedupHash; got != want {
		t.Fatalf("DedupHash = %s, want %s", got, want)
	}
}

func TestMultipart(t *testing.T) {
	polish := strings.Repeat("Zażółć gęślą jaźń. ", 9) // UCS-2, 3 parts
	ascii := strings.Repeat("Hello, this is a long message. ", 8)
	emoji := strings.Repeat("Hej 😀 ", 25)
	tests := []struct {
		name  string
		text  string
		parts int
		enc   domain.Encoding
		order []int
	}{
		{"gsm7 in order", ascii, 2, domain.EncodingGSM7, nil},
		{"gsm7 reversed", ascii, 2, domain.EncodingGSM7, []int{2, 1}},
		{"ucs2 in order", polish, 3, domain.EncodingUCS2, nil},
		{"ucs2 reversed", polish, 3, domain.EncodingUCS2, []int{3, 2, 1}},
		{"ucs2 shuffled", polish, 3, domain.EncodingUCS2, []int{2, 3, 1}},
		{"emoji across parts", emoji, 3, domain.EncodingUCS2, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.injectParts(tt.text, 42, tt.order...)
			res := e.poll()
			if res.Fetched != tt.parts || res.Stored != 1 || res.Pending != 0 || res.Deleted != tt.parts {
				t.Fatalf("result = %+v", res)
			}
			got := e.inbound()
			if len(got) != 1 {
				t.Fatalf("stored %d messages", len(got))
			}
			m := got[0]
			if m.Body != tt.text || m.Encoding != tt.enc || m.Segments != tt.parts || m.LastError != "" || m.Peer != sender {
				t.Fatalf("message = %+v (body match %v)", m, m.Body == tt.text)
			}
			if e.pending() != 0 || e.routerInbox() != 0 {
				t.Errorf("pending parts %d, router entries %d", e.pending(), e.routerInbox())
			}
		})
	}
}

func TestMultipartAutoSplitByRouter(t *testing.T) {
	e := newEnv(t, nil)
	text := strings.Repeat("Zażółć gęślą jaźń. ", 9)
	e.inject(text) // the fake splits it into parts with UDH
	if e.routerInbox() < 2 {
		t.Fatalf("router inbox has %d entries", e.routerInbox())
	}
	e.poll()
	got := e.inbound()
	if len(got) != 1 || got[0].Body != text {
		t.Fatalf("messages = %+v", got)
	}
}

func TestPartsInDifferentCycles(t *testing.T) {
	e := newEnv(t, nil)
	text := strings.Repeat("Zażółć gęślą jaźń. ", 9)

	e.injectParts(text, 7, 2)
	res := e.poll()
	if res.Stored != 0 || res.Pending != 1 || res.Deleted != 1 {
		t.Fatalf("cycle 1 = %+v", res)
	}
	if e.routerInbox() != 0 || e.pending() != 1 || len(e.inbound()) != 0 {
		t.Fatalf("after cycle 1: router %d pending %d messages %d", e.routerInbox(), e.pending(), len(e.inbound()))
	}

	e.injectParts(text, 7, 1)
	res = e.poll()
	if res.Stored != 0 || res.Pending != 1 {
		t.Fatalf("cycle 2 = %+v", res)
	}

	e.injectParts(text, 7, 3)
	res = e.poll()
	if res.Stored != 1 || res.Pending != 0 || res.Fetched != 1 {
		t.Fatalf("cycle 3 = %+v", res)
	}
	got := e.inbound()
	if len(got) != 1 || got[0].Body != text || got[0].Segments != 3 || e.pending() != 0 {
		t.Fatalf("messages = %+v, pending %d", got, e.pending())
	}
}

// Two messages with the same reference number from different senders, and
// two messages from one sender, are kept apart.
func TestInterleavedMultipart(t *testing.T) {
	e := newEnv(t, nil)
	a := strings.Repeat("Zażółć gęślą jaźń. ", 9)
	b := strings.Repeat("Pójdźże, kiń tę chmurność w głąb flaszy. ", 4)
	e.injectParts(a, 9, 1, 3)
	e.injectParts(b, 10, 2)
	e.poll()
	if len(e.inbound()) != 0 || e.pending() != 3 {
		t.Fatalf("messages %d pending %d", len(e.inbound()), e.pending())
	}
	e.injectParts(b, 10, 1, 3)
	e.injectParts(a, 9, 2)
	res := e.poll()
	if res.Stored != 2 {
		t.Fatalf("result = %+v", res)
	}
	var bodies []string
	for _, m := range e.inbound() {
		bodies = append(bodies, m.Body)
	}
	if len(bodies) != 2 || (bodies[0] != a || bodies[1] != b) && (bodies[0] != b || bodies[1] != a) {
		t.Fatalf("bodies = %q", bodies)
	}
}

// A part the network delivered twice (same sequence number, different
// timestamp, so a different hash) is consumed with its message.
func TestRepeatedPartInOneMessage(t *testing.T) {
	e := newEnv(t, nil)
	text := strings.Repeat("Zażółć gęślą jaźń. ", 9)
	e.injectParts(text, 5, 1, 1)
	e.clk.Advance(time.Second)
	e.injectParts(text, 5, 2, 3)
	e.poll()
	got := e.inbound()
	if len(got) != 1 || got[0].Body != text || e.pending() != 0 || e.routerInbox() != 0 {
		t.Fatalf("messages = %+v pending %d router %d", got, e.pending(), e.routerInbox())
	}
}

func TestIncompleteMessageExpires(t *testing.T) {
	e := newEnv(t, nil)
	text := strings.Repeat("Zażółć gęślą jaźń. ", 9)
	e.injectParts(text, 11, 1, 3)
	e.poll()

	e.clk.Advance(23 * time.Hour)
	if res := e.poll(); res.Expired != 0 || len(e.inbound()) != 0 {
		t.Fatalf("expired too early: %+v", res)
	}
	e.clk.Advance(time.Hour + time.Second)
	res := e.poll()
	if res.Expired != 1 || res.Stored != 1 || res.Fetched != 0 {
		t.Fatalf("result = %+v", res)
	}
	got := e.inbound()
	if len(got) != 1 {
		t.Fatalf("messages = %+v", got)
	}
	m := got[0]
	if m.LastError != "incomplete: 2/3 parts" || m.Segments != 3 || m.Status != domain.StatusReceived ||
		m.Encoding != domain.EncodingUCS2 || m.Peer != sender || m.Body == "" || !strings.HasPrefix(m.Body, "Zażółć") {
		t.Fatalf("message = %+v", m)
	}
	if e.pending() != 0 {
		t.Fatalf("pending = %d after expiry", e.pending())
	}
	if h := e.hookedMessages(); len(h) != 1 || h[0].ID != m.ID {
		t.Errorf("hook calls = %+v", h)
	}
	// Nothing more happens afterwards.
	if res := e.poll(); res != (CycleResult{}) {
		t.Fatalf("later cycle = %+v", res)
	}
}

func TestIncompleteExpiryConfigurable(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.IncompleteAfter = time.Minute })
	e.injectParts(strings.Repeat("Zażółć gęślą jaźń. ", 9), 12, 2)
	e.poll()
	e.clk.Advance(2 * time.Minute)
	e.poll()
	got := e.inbound()
	if len(got) != 1 || got[0].LastError != "incomplete: 1/3 parts" {
		t.Fatalf("messages = %+v", got)
	}
}

// ADR 0002 §4: a crash between commit and DeleteInbox leaves the SMS on the
// router; the next cycle must not create a second row and must delete again.
func TestCrashBetweenStoreAndDelete(t *testing.T) {
	e := newEnv(t, nil)
	e.inject("Kod: 1234")
	e.srv.FailNext(fake.EndpointInboxDelete, fake.HTTPError(500, "boom"), 1)

	_, err := e.p.PollOnce(e.ctx)
	if err == nil || !errors.Is(err, domain.ErrTransient) {
		t.Fatalf("PollOnce err = %v, want transient", err)
	}
	if n := len(e.inbound()); n != 1 || e.routerInbox() != 1 {
		t.Fatalf("after failed delete: %d stored, %d on router", n, e.routerInbox())
	}

	res := e.poll()
	if res.Fetched != 1 || res.Stored != 0 || res.Duplicates != 1 || res.Deleted != 1 {
		t.Fatalf("retry = %+v", res)
	}
	if n := len(e.inbound()); n != 1 || e.routerInbox() != 0 {
		t.Fatalf("after retry: %d stored, %d on router", n, e.routerInbox())
	}
	if h := e.hookedMessages(); len(h) != 1 {
		t.Fatalf("hook called %d times, want 1", len(h))
	}
}

// The same for the parts of a message that completed in the failed cycle:
// redelivered parts must neither duplicate the message nor start a new
// pending message that would expire as "incomplete" a day later.
func TestCrashBetweenStoreAndDeleteMultipart(t *testing.T) {
	e := newEnv(t, nil)
	text := strings.Repeat("Zażółć gęślą jaźń. ", 9)
	e.injectParts(text, 3)
	e.srv.FailNext(fake.EndpointInboxDelete, fake.HTTPError(500, "boom"), 1)
	if _, err := e.p.PollOnce(e.ctx); err == nil {
		t.Fatal("PollOnce succeeded despite failing delete")
	}
	if len(e.inbound()) != 1 {
		t.Fatalf("stored %d messages", len(e.inbound()))
	}
	// The failed delete removed the first entry only if the fake handled it
	// before failing; either way the rest is redelivered.
	left := e.routerInbox()
	if left == 0 {
		t.Fatal("nothing left on the router to redeliver")
	}
	res := e.poll()
	if res.Stored != 0 || res.Duplicates != left || res.Pending != 0 || e.routerInbox() != 0 {
		t.Fatalf("retry = %+v (router entries before: %d)", res, left)
	}
	e.clk.Advance(48 * time.Hour)
	e.poll()
	if got := e.inbound(); len(got) != 1 || got[0].LastError != "" {
		t.Fatalf("messages after expiry window = %+v", got)
	}
	if e.pending() != 0 {
		t.Fatalf("pending = %d", e.pending())
	}
}

func TestTombstonesPurged(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PartRetention = 24 * time.Hour })
	text := strings.Repeat("Zażółć gęślą jaźń. ", 9)
	e.injectParts(text, 3)
	e.srv.FailNext(fake.EndpointInboxDelete, fake.HTTPError(500, "boom"), 1)
	if _, err := e.p.PollOnce(e.ctx); err == nil {
		t.Fatal("PollOnce succeeded despite failing delete")
	}
	// Run (not PollOnce) purges tombstones, at most hourly.
	e.clk.Advance(48 * time.Hour)
	e.p.step(e.ctx) // dedups the redelivered parts, purges only afterwards
	e.clk.Advance(2 * time.Hour)
	e.p.step(e.ctx)
	n, err := e.st.InboundParts().PurgeConsumed(e.ctx, e.clk.Now().Add(24*time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("tombstones left after purge: %d, %v", n, err)
	}
	if len(e.inbound()) != 1 {
		t.Fatalf("messages = %d", len(e.inbound()))
	}
}

// A full router inbox drops new SMS; the poller must keep it drained.
func TestFullInbox(t *testing.T) {
	e := newEnv(t, nil)
	const limit = 3
	e.srv.SetInboxLimit(limit)
	for i := range limit {
		e.inject("msg " + string(rune('A'+i)))
	}
	if _, err := e.srv.InjectSMS(fake.IncomingSMS{From: sender, Text: "lost"}); !errors.Is(err, fake.ErrInboxFull) {
		t.Fatalf("inject into full inbox: %v", err)
	}

	total := 0
	for round := range 5 {
		res := e.poll()
		total += res.Stored
		if res.Fetched != limit {
			t.Fatalf("round %d fetched %d, want %d", round, res.Fetched, limit)
		}
		for i := range limit {
			e.inject("round " + string(rune('0'+round)) + string(rune('A'+i)))
		}
	}
	total += e.poll().Stored
	if total != 6*limit || len(e.inbound()) != 6*limit {
		t.Fatalf("stored %d (list %d), want %d", total, len(e.inbound()), 6*limit)
	}
	if e.routerInbox() != 0 {
		t.Fatalf("router inbox holds %d entries", e.routerInbox())
	}
}

func TestNoPDUUsesDeviceText(t *testing.T) {
	t.Run("raw entry without pdu", func(t *testing.T) {
		e := newEnv(t, nil)
		if _, err := e.srv.InjectRaw(fake.RawInbox{Phone: sender, Message: "plain text"}); err != nil {
			t.Fatal(err)
		}
		e.poll()
		got := e.inbound()
		if len(got) != 1 || got[0].Body != "plain text" || got[0].Encoding != domain.EncodingGSM7 || got[0].LastError != "" {
			t.Fatalf("messages = %+v", got)
		}
		want := domain.InboundDedupHash(e.dev.ID, "lte1", sender, fake.TimestampISO(t0), "plain text")
		if got[0].DedupHash != want {
			t.Errorf("hash = %s, want %s", got[0].DedupHash, want)
		}
	})
	t.Run("connector without inbound pdu", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.Conn = noPDU{c.Conn} })
		e.inject("Zażółć")
		entry := e.srv.Inbox()[0]
		e.poll()
		got := e.inbound()
		if len(got) != 1 || got[0].Body != entry.Message || got[0].Encoding != domain.EncodingGSM7 {
			t.Fatalf("messages = %+v, device text %q", got, entry.Message)
		}
	})
}

func TestBadPDUDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.srv.InjectRaw(fake.RawInbox{Phone: sender, Message: "device text", PDU: "ZZ-not-hex"}); err != nil {
		t.Fatal(err)
	}
	e.inject("good one")
	if _, err := e.srv.InjectRaw(fake.RawInbox{Phone: sender, Message: "short", PDU: "0011"}); err != nil {
		t.Fatal(err)
	}
	res := e.poll()
	if res.Stored != 3 || res.Deleted != 3 || e.routerInbox() != 0 {
		t.Fatalf("result = %+v, router %d", res, e.routerInbox())
	}
	var bad, good int
	for _, m := range e.inbound() {
		switch m.Body {
		case "device text", "short":
			bad++
			if m.LastError != warnPDUDecode || m.Encoding != domain.EncodingGSM7 {
				t.Errorf("bad pdu message = %+v", m)
			}
		case "good one":
			good++
			if m.LastError != "" {
				t.Errorf("good message = %+v", m)
			}
		default:
			t.Errorf("unexpected body %q", m.Body)
		}
	}
	if bad != 2 || good != 1 {
		t.Fatalf("bad %d good %d", bad, good)
	}
	if !strings.Contains(e.logs.String(), "pdu not decodable") {
		t.Error("no warning logged for the bad PDU")
	}
}

// A sender missing from the device entry is taken from the PDU, and a PDU
// that is not a part of a concatenated message is a message of its own.
func TestPeerFromPDU(t *testing.T) {
	e := newEnv(t, nil)
	pduHex, err := fake.BuildDeliverPDU(fake.PDUParams{Sender: "Bank", Time: t0, Encoding: fake.EncodingGSM7}, "Saldo 10 zl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.InjectRaw(fake.RawInbox{Phone: "", PDU: pduHex, Message: "Saldo 10 zl"}); err != nil {
		t.Fatal(err)
	}
	e.poll()
	got := e.inbound()
	if len(got) != 1 || got[0].Peer != "Bank" || got[0].Body != "Saldo 10 zl" {
		t.Fatalf("messages = %+v", got)
	}
}

// ReceivedAt falls back to the PDU time stamp and then to the fetch time.
func TestReceivedAtFallbacks(t *testing.T) {
	e := newEnv(t, nil)
	pduTime := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	withPDU, err := fake.BuildDeliverPDU(fake.PDUParams{Sender: sender, Time: pduTime, Encoding: fake.EncodingGSM7}, "from pdu")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.InjectRaw(fake.RawInbox{Phone: sender, TimestampText: "???", PDU: withPDU, Message: "from pdu"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.InjectRaw(fake.RawInbox{Phone: sender, TimestampText: "???", Message: "no time"}); err != nil {
		t.Fatal(err)
	}
	e.poll()
	got := e.inbound()
	if len(got) != 2 {
		t.Fatalf("messages = %+v", got)
	}
	for _, m := range got {
		want := t0
		if m.Body == "from pdu" {
			want = pduTime
		}
		if !m.ReceivedAt.Equal(want) {
			t.Errorf("%q: ReceivedAt = %v, want %v", m.Body, m.ReceivedAt, want)
		}
	}
}

// A pending part whose stored PDU is no longer a part (damaged data) is
// kept as a message of its own instead of being lost or blocking the rest.
func TestDamagedPendingPartIsKept(t *testing.T) {
	e := newEnv(t, nil)
	bad := store.InboundPart{
		ID: "01JPART000000000000000000A", ChannelID: e.ch.ID, DedupHash: "h-bad", Phone: sender,
		ReceivedAt: t0, Body: "kept text", PDU: "not hex", CreatedAt: t0,
	}
	if _, err := e.st.InboundParts().Add(e.ctx, bad); err != nil {
		t.Fatal(err)
	}
	plain, err := fake.BuildDeliverPDU(fake.PDUParams{Sender: sender, Time: t0, Encoding: fake.EncodingGSM7}, "plain pdu text")
	if err != nil {
		t.Fatal(err)
	}
	good := bad
	good.ID, good.DedupHash, good.PDU, good.Body = "01JPART000000000000000000B", "h-plain", plain, "x"
	if _, err := e.st.InboundParts().Add(e.ctx, good); err != nil {
		t.Fatal(err)
	}
	e.inject("trigger")
	e.poll()
	bodies := map[string]string{}
	for _, m := range e.inbound() {
		bodies[m.Body] = m.LastError
	}
	if len(bodies) != 3 || bodies["kept text"] != warnPDUDecode || bodies["plain pdu text"] != "" || e.pending() != 0 {
		t.Fatalf("bodies = %v pending %d", bodies, e.pending())
	}
}

func TestLogsHaveNoBodiesOrFullNumbers(t *testing.T) {
	e := newEnv(t, nil)
	secret := "Twój kod to 987654, tajne"
	e.inject(secret)
	e.injectParts(strings.Repeat("Zażółć gęślą jaźń. ", 9), 4, 1) // pending part, then expiry
	if _, err := e.srv.InjectRaw(fake.RawInbox{Phone: sender, Message: "bad pdu body", PDU: "zz"}); err != nil {
		t.Fatal(err)
	}
	e.p.step(e.ctx)
	e.clk.Advance(25 * time.Hour)
	e.p.step(e.ctx)
	logs := e.logs.String()
	for _, leak := range []string{"987654", "tajne", "bad pdu body", "Zażółć", "511222333", routerPass} {
		if strings.Contains(logs, leak) {
			t.Errorf("log leaks %q:\n%s", leak, logs)
		}
	}
	if !strings.Contains(logs, "incomplete multi-part") || !strings.Contains(logs, "+48") {
		t.Errorf("expected log lines missing:\n%s", logs)
	}
}

func TestNewValidation(t *testing.T) {
	e := newEnv(t, nil)
	ok := Config{Channel: e.ch, Conn: e.conn, Store: e.st}
	tests := []struct {
		name string
		mod  func(*Config)
	}{
		{"no connector", func(c *Config) { c.Conn = nil }},
		{"no store", func(c *Config) { c.Store = nil }},
		{"no channel id", func(c *Config) { c.Channel.ID = "" }},
		{"no device id", func(c *Config) { c.Channel.DeviceID = "" }},
		{"no port", func(c *Config) { c.Channel.Port = "" }},
		{"negative interval", func(c *Config) { c.Interval = -1 }},
		{"negative backoff", func(c *Config) { c.MaxBackoff = -1 }},
		{"negative incomplete", func(c *Config) { c.IncompleteAfter = -1 }},
		{"negative retention", func(c *Config) { c.PartRetention = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ok
			tt.mod(&c)
			if _, err := New(c); err == nil {
				t.Fatal("New accepted the config")
			}
		})
	}
	p, err := New(ok)
	if err != nil {
		t.Fatalf("New with defaults: %v", err)
	}
	if p.interval != DefaultInterval || p.maxBackoff != DefaultMaxBackoff ||
		p.incompleteAfter != DefaultIncompleteAfter || p.partRetention != DefaultPartRetention {
		t.Fatalf("defaults not applied: %+v", p)
	}
	if low, _ := New(Config{Channel: e.ch, Conn: e.conn, Store: e.st, Interval: time.Hour, MaxBackoff: time.Minute}); low.maxBackoff != time.Hour {
		t.Errorf("MaxBackoff below Interval = %v", low.maxBackoff)
	}
}

func TestStoreFailureKeepsEntriesOnRouter(t *testing.T) {
	e := newEnv(t, nil)
	e.inject("will not be stored")
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.PollOnce(e.ctx); err == nil {
		t.Fatal("PollOnce succeeded with a closed store")
	}
	if e.routerInbox() != 1 {
		t.Fatalf("entry was deleted although nothing was stored")
	}
	if got := e.hookedMessages(); len(got) != 0 {
		t.Fatalf("hook called: %+v", got)
	}
}

func TestContextCancelledIsNotAChannelFailure(t *testing.T) {
	e := newEnv(t, nil)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	if d := e.p.step(ctx); d != e.p.interval {
		t.Errorf("delay after cancel = %v", d)
	}
	if len(e.rep.get()) != 0 || e.p.failures != 0 {
		t.Errorf("cancel counted as failure: %v, %d", e.rep.get(), e.p.failures)
	}
}
