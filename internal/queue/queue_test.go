// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

const (
	chA domain.ID = "01JCHANNELAAAAAAAAAAAAAAAA"
	chB domain.ID = "01JCHANNELBBBBBBBBBBBBBBBB"
)

func ids(ms []domain.Message) []domain.ID {
	out := make([]domain.ID, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func callIDs(cs []stubCall) []domain.ID {
	out := make([]domain.ID, len(cs))
	for i, c := range cs {
		out[i] = c.MessageID
	}
	return out
}

func TestHappyPathFIFO(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(e.clk, nil)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()

	ms := e.enqueue(e.msg("+48600000001"), e.msg("+48600000002"), e.msg("+48600000003"))
	e.waitStatus(chA, domain.StatusSent, ids(ms)...)

	if got := callIDs(conn.Calls()); !slices.Equal(got, ids(ms)) {
		t.Fatalf("send order = %v, want %v", got, ids(ms))
	}
	for i, c := range conn.Calls() {
		if c.ChannelID != chA || c.Port != "lte1" || c.To != ms[i].Peer {
			t.Errorf("call %d = %+v", i, c)
		}
	}
	for _, m := range ms {
		got := e.get(m.ID)
		if got.ChannelID != chA || got.Attempts != 1 || !got.SentAt.Equal(t0) || got.PossibleDuplicate || !got.LeaseUntil.IsZero() {
			t.Errorf("message %s = %+v", m.ID, got)
		}
	}
	// Idle: sleeping MaxIdle, because nothing is due.
	e.tr.wait(t, "idle", func(v view) bool { return v.sleepsFor(chA, DefaultMaxIdle) })

	st := e.d.Stats()
	if st.Totals.Sent != 3 || len(st.Channels) != 1 || st.Channels[0].Sent != 3 || !st.Channels[0].Active ||
		st.Channels[0].Sending || st.Channels[0].LastMessageID != ms[2].ID || !st.Started {
		t.Fatalf("stats = %+v", st)
	}
	if got := e.events.kinds(); len(got) != 3 || got[0] != EventSent {
		t.Fatalf("events = %v", got)
	}
	logs := e.logs.String()
	if strings.Contains(logs, "secret body") || strings.Contains(logs, "+48600000001") {
		t.Fatalf("logs leak body or number:\n%s", logs)
	}
	if !strings.Contains(logs, "message_id="+string(ms[0].ID)) || !strings.Contains(logs, "attempt=1") ||
		!strings.Contains(logs, "channel_id="+string(chA)) {
		t.Fatalf("logs miss attributes:\n%s", logs)
	}
}

func TestRouting(t *testing.T) {
	e := newEnv(t, envConfig{})
	connA, connB := newStub(e.clk, nil), newStub(e.clk, nil)
	e.d.SetChannels([]ChannelSpec{spec(chA, connA, "a"), spec(chB, connB, "b", "x")})
	e.start()

	ms := e.enqueue(
		e.msg("+48600000001", pinned(chB)),               // pinned to B
		e.msg("+48600000002", tagged("a")),               // only A has "a"
		e.msg("+48600000003", tagged("b", "x")),          // only B
		e.msg("+48600000004", tagged("a", "b")),          // nobody
		e.msg("+48600000005"),                            // anyone
		e.msg("+48600000006", pinned(chA), tagged("zz")), // pin wins over tags
	)
	sent := []domain.ID{ms[0].ID, ms[1].ID, ms[2].ID, ms[4].ID, ms[5].ID}
	e.tr.wait(t, "routed", func(v view) bool {
		for _, id := range sent {
			if e.status(id) != domain.StatusSent {
				return false
			}
		}
		return v.asleep(chA) && v.asleep(chB)
	})
	want := map[int]domain.ID{0: chB, 1: chA, 2: chB, 5: chA}
	for i, ch := range want {
		if got := e.get(ms[i].ID).ChannelID; got != ch {
			t.Errorf("message %d sent on %s, want %s", i, got, ch)
		}
	}
	if got := e.get(ms[4].ID).ChannelID; got != chA && got != chB {
		t.Errorf("untagged message on %q", got)
	}
	if got := e.get(ms[3].ID); got.Status != domain.StatusQueued || got.Attempts != 0 {
		t.Errorf("unroutable message = %+v", got)
	}
	if connA.N()+connB.N() != 5 {
		t.Fatalf("sends: A=%d B=%d", connA.N(), connB.N())
	}
}

func TestTwoChannelsShareQueue(t *testing.T) {
	e := newEnv(t, envConfig{})
	g := newGate()
	connA, connB := newStub(e.clk, g.send), newStub(e.clk, g.send)
	e.d.SetChannels([]ChannelSpec{spec(chA, connA), spec(chB, connB)})
	e.start()

	ms := e.enqueue(e.msg("+48600000001"), e.msg("+48600000002"), e.msg("+48600000003"), e.msg("+48600000004"))
	c1, c2 := g.waitEntered(t), g.waitEntered(t)
	if c1.ChannelID == c2.ChannelID {
		t.Fatalf("both in-flight sends on %s", c1.ChannelID)
	}
	if got := []domain.ID{c1.MessageID, c2.MessageID}; !slices.Contains(got, ms[0].ID) || !slices.Contains(got, ms[1].ID) {
		t.Fatalf("first sends = %v, want the two oldest", got)
	}
	for range ms {
		g.release <- nil
	}
	e.tr.wait(t, "all sent", func(v view) bool {
		for _, m := range ms {
			if e.status(m.ID) != domain.StatusSent {
				return false
			}
		}
		return v.asleep(chA) && v.asleep(chB)
	})
	all := append(callIDs(connA.Calls()), callIDs(connB.Calls())...)
	slices.Sort(all)
	if !slices.Equal(all, ids(ms)) {
		t.Fatalf("sent %v, want each of %v once", all, ids(ms))
	}
	if connA.N() == 0 || connB.N() == 0 {
		t.Fatalf("one channel idle: A=%d B=%d", connA.N(), connB.N())
	}
}

func TestRateLimit(t *testing.T) {
	tests := []struct {
		name    string
		rate    int
		burst   int
		n       int
		wantGap []time.Duration // offsets from t0 of each send
	}{
		{"6 per minute", 6, 1, 4, []time.Duration{0, 10 * time.Second, 20 * time.Second, 30 * time.Second}},
		{"burst 3", 6, 3, 5, []time.Duration{0, 0, 0, 10 * time.Second, 20 * time.Second}},
		{"7 per minute rounds up", 7, 1, 2, []time.Duration{0, 8571428572}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envConfig{opts: func(o *Options) { o.RateBurst = tt.burst; o.MaxIdle = time.Hour }})
			conn := newStub(e.clk, nil)
			s := spec(chA, conn)
			s.Channel.RatePerMin = tt.rate
			e.d.SetChannels([]ChannelSpec{s})
			var msgs []domain.Message
			for range tt.n {
				msgs = append(msgs, e.msg("+48600000001"))
			}
			// Enqueue before Start without Notify: the only wake-ups are the
			// rate-limit timers, so each Advance below hits a registered one.
			if _, err := e.st.Messages().Enqueue(e.ctx, msgs, store.Idempotency{}); err != nil {
				t.Fatal(err)
			}
			e.start()

			last := 0
			for {
				e.tr.wait(t, "rate wait", func(v view) bool {
					d, ok := v.sleeping[chA]
					return ok && d < time.Hour && conn.N() > last
				})
				last = conn.N()
				if last == tt.n {
					break
				}
				e.tr.mu.Lock()
				d := e.tr.sleeping[chA]
				e.tr.mu.Unlock()
				e.clk.Advance(d)
			}
			calls := conn.Calls()
			for i, c := range calls {
				if got := c.At.Sub(t0); got != tt.wantGap[i] {
					t.Errorf("send %d at +%v, want +%v", i, got, tt.wantGap[i])
				}
			}
			// Never more than burst + rate sends within any minute.
			for i := range calls {
				n := 0
				for _, c := range calls[i:] {
					if c.At.Sub(calls[i].At) < time.Minute {
						n++
					}
				}
				if n > tt.rate+tt.burst-1 {
					t.Errorf("%d sends within a minute from send %d", n, i)
				}
			}
		})
	}
}

func TestTokenBucket(t *testing.T) {
	tests := []struct {
		name  string
		rate  int
		burst int
		takes []time.Duration // offsets at which take is called
		at    time.Duration
		want  time.Duration
	}{
		{"unlimited", 0, 1, []time.Duration{0, 0, 0}, 0, 0},
		{"fresh bucket", 6, 1, nil, 0, 0},
		{"after one take", 6, 1, []time.Duration{0}, 0, 10 * time.Second},
		{"partially waited", 6, 1, []time.Duration{0}, 4 * time.Second, 6 * time.Second},
		{"idle long", 6, 1, []time.Duration{0}, time.Hour, 0},
		{"burst left", 6, 2, []time.Duration{0}, 0, 0},
		{"burst used", 6, 2, []time.Duration{0, 0}, 0, 10 * time.Second},
		{"idle does not bank more than burst", 6, 2, []time.Duration{0, time.Hour, time.Hour}, time.Hour, 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b tokenBucket
			for _, off := range tt.takes {
				b.take(t0.Add(off), tt.rate)
			}
			if got := b.wait(t0.Add(tt.at), tt.rate, tt.burst); got != tt.want {
				t.Fatalf("wait = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSendFailureClasses(t *testing.T) {
	cause := errors.New("boom")
	tests := []struct {
		name       string
		err        error
		maxAtt     int
		wantStatus domain.MessageStatus
		wantDup    bool
		wantEvent  EventKind
		wantErrStr string
	}{
		{"transient", domain.Transient("send", cause), 5, domain.StatusQueued, false, EventSendFailed, "transient"},
		{"ambiguous", domain.TransientAmbiguous("send", cause), 5, domain.StatusQueued, true, EventSendFailed, "ambiguous"},
		{"unclassified", cause, 5, domain.StatusQueued, false, EventSendFailed, "boom"},
		{"bare deadline", context.DeadlineExceeded, 5, domain.StatusQueued, true, EventSendFailed, "deadline"},
		{"permanent", domain.Permanent("send", cause), 5, domain.StatusFailed, false, EventSendFailed, "permanent"},
		{"transient at limit", domain.Transient("send", cause), 1, domain.StatusFailed, false, EventSendFailed, "transient"},
		{"ambiguous at limit", domain.TransientAmbiguous("send", cause), 1, domain.StatusFailed, true, EventSendFailed, "ambiguous"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envConfig{opts: func(o *Options) {
				o.Retry = domain.DefaultRetryPolicy()
				o.Retry.MaxAttempts = tt.maxAtt
			}})
			conn := newStub(e.clk, func(context.Context, int, stubCall) (connector.SendResult, error) {
				return connector.SendResult{}, tt.err
			})
			e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
			e.start()
			m := e.enqueue(e.msg("+48600000001"))[0]
			e.waitStatus(chA, tt.wantStatus, m.ID)
			e.tr.wait(t, "settled", func(v view) bool { return conn.N() == 1 && v.asleep(chA) })

			got := e.get(m.ID)
			if got.Attempts != 1 || got.PossibleDuplicate != tt.wantDup || !strings.Contains(got.LastError, tt.wantErrStr) {
				t.Fatalf("message = %+v", got)
			}
			if tt.wantStatus == domain.StatusQueued {
				if want := t0.Add(30 * time.Second); !got.NextAttemptAt.Equal(want) {
					t.Fatalf("NextAttemptAt = %v, want %v", got.NextAttemptAt, want)
				}
				if got.ChannelID != "" {
					t.Fatalf("unpinned requeued message keeps channel %s", got.ChannelID)
				}
			}
			st := e.d.Stats().Totals
			if (st.Retried == 1) != (tt.wantStatus == domain.StatusQueued) || (st.Failed == 1) != (tt.wantStatus == domain.StatusFailed) ||
				(st.PossibleDuplicates == 1) != tt.wantDup {
				t.Fatalf("stats = %+v", st)
			}
			if evs := e.events.all(); len(evs) != 1 || evs[0].Kind != tt.wantEvent || evs[0].MessageID != m.ID || evs[0].Err == nil {
				t.Fatalf("events = %+v", evs)
			}
		})
	}
}

func TestRetryBackoffUntilFailed(t *testing.T) {
	e := newEnv(t, envConfig{opts: func(o *Options) {
		o.Retry = domain.RetryPolicy{MaxAttempts: 3, Initial: 30 * time.Second, Multiplier: 4, Max: 30 * time.Minute, Jitter: 0.2}
		o.MaxIdle = time.Hour
	}})
	conn := newStub(e.clk, func(context.Context, int, stubCall) (connector.SendResult, error) {
		return connector.SendResult{}, domain.Transient("send", errors.New("modem busy"))
	})
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	m := e.enqueue(e.msg("+48600000001"))[0]

	for i, backoff := range []time.Duration{30 * time.Second, 2 * time.Minute} {
		e.tr.wait(t, "backoff sleep", func(v view) bool { return conn.N() == i+1 && v.sleepsFor(chA, backoff) })
		got := e.get(m.ID)
		if got.Status != domain.StatusQueued || got.Attempts != i+1 || !got.NextAttemptAt.Equal(e.clk.Now().Add(backoff)) {
			t.Fatalf("after attempt %d: %+v", i+1, got)
		}
		e.clk.Advance(backoff)
	}
	e.waitStatus(chA, domain.StatusFailed, m.ID)
	calls := conn.Calls()
	if len(calls) != 3 || calls[1].At.Sub(calls[0].At) != 30*time.Second || calls[2].At.Sub(calls[1].At) != 2*time.Minute {
		t.Fatalf("calls = %+v", calls)
	}
	if got := e.get(m.ID); got.Attempts != 3 || !strings.Contains(got.LastError, "modem busy") {
		t.Fatalf("message = %+v", got)
	}
	if st := e.d.Stats().Totals; st.Retried != 2 || st.Failed != 1 || st.Sent != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestAmbiguousThenSent(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(e.clk, func(_ context.Context, n int, _ stubCall) (connector.SendResult, error) {
		if n == 1 {
			return connector.SendResult{}, domain.TransientAmbiguous("send", errors.New("timeout after write"))
		}
		return connector.SendResult{AcceptedAt: t0.Add(time.Hour)}, nil
	})
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	m := e.enqueue(e.msg("+48600000001"))[0]
	e.tr.wait(t, "requeued", func(v view) bool { return conn.N() == 1 && v.sleepsFor(chA, 30*time.Second) })
	e.clk.Advance(30 * time.Second)
	e.waitStatus(chA, domain.StatusSent, m.ID)
	got := e.get(m.ID)
	if !got.PossibleDuplicate || got.Attempts != 2 || !got.SentAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("message = %+v", got)
	}
	if !strings.Contains(e.logs.String(), "possible duplicate") {
		t.Fatal("possible duplicate not logged")
	}
}

func TestAuthDegradesChannel(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(e.clk, func(_ context.Context, n int, _ stubCall) (connector.SendResult, error) {
		if n == 1 {
			return connector.SendResult{}, domain.AuthError("send", errors.New("HTTP 401"))
		}
		return connector.SendResult{}, nil
	})
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	m1 := e.enqueue(e.msg("+48600000001"))[0]

	e.tr.wait(t, "paused", func(v view) bool { return conn.N() == 1 && v.sleepsFor(chA, 0) })
	got := e.get(m1.ID)
	if got.Status != domain.StatusQueued || got.Attempts != 1 || !got.NextAttemptAt.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("message after auth error = %+v", got)
	}
	st := e.d.Stats()
	if c := st.Channels[0]; c.State != domain.ChannelDegraded || c.Active || !strings.Contains(c.StateReason, "authentication") ||
		c.AuthErrors != 1 || c.LastErrorKind != domain.KindAuth {
		t.Fatalf("channel stats = %+v", c)
	}
	if evs := e.events.all(); len(evs) != 1 || evs[0].Kind != EventAuthFailed || evs[0].ChannelID != chA || evs[0].ErrorKind != domain.KindAuth {
		t.Fatalf("events = %+v", evs)
	}

	// Time passes and new messages arrive: the paused worker takes nothing.
	e.clk.Advance(time.Hour)
	before := e.tr.sleeps(chA)
	m2 := e.enqueue(e.msg("+48600000002"))[0]
	e.tr.wait(t, "woken and paused again", func(v view) bool { return v.count[chA] > before && v.sleepsFor(chA, 0) })
	if conn.N() != 1 {
		t.Fatalf("paused channel sent %d messages", conn.N())
	}

	// Health checker restores the channel.
	if !e.d.SetChannelState(chA, domain.ChannelOK, "") {
		t.Fatal("SetChannelState: unknown channel")
	}
	e.waitStatus(chA, domain.StatusSent, m1.ID, m2.ID)
	if conn.N() != 3 || e.d.Stats().Channels[0].State != domain.ChannelOK {
		t.Fatalf("after restore: calls=%d stats=%+v", conn.N(), e.d.Stats())
	}
	if e.d.SetChannelState("01JNOSUCHCHANNELXXXXXXXXXX", domain.ChannelOK, "") {
		t.Fatal("SetChannelState accepted an unknown channel")
	}
}

func TestDoesNotFitCapabilities(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(e.clk, nil)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	m := e.enqueue(e.msg("+48600000001", func(m *domain.Message) { m.Encoding = domain.EncodingUCS2 }))[0]
	e.waitStatus(chA, domain.StatusFailed, m.ID)
	if conn.N() != 0 {
		t.Fatal("connector called for a message it cannot send")
	}
	if got := e.get(m.ID); !strings.Contains(got.LastError, "does not fit") {
		t.Fatalf("LastError = %q", got.LastError)
	}
}

func TestNotifyWakesIdleWorker(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(e.clk, nil)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	e.tr.wait(t, "idle", func(v view) bool { return v.sleepsFor(chA, DefaultMaxIdle) })
	// No clock movement: only Notify (inside enqueue) can wake the worker.
	m := e.enqueue(e.msg("+48600000001"))[0]
	e.waitStatus(chA, domain.StatusSent, m.ID)
	if !e.get(m.ID).SentAt.Equal(t0) {
		t.Fatal("sent at a later time; Notify did not wake the worker")
	}
}

func TestSleepsUntilNextDue(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(e.clk, nil)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	m := e.enqueue(e.msg("+48600000001", func(m *domain.Message) { m.NextAttemptAt = t0.Add(12 * time.Second) }))[0]
	e.tr.wait(t, "sleep until due", func(v view) bool { return v.sleepsFor(chA, 12*time.Second) })
	e.clk.Advance(12 * time.Second)
	e.waitStatus(chA, domain.StatusSent, m.ID)
	// Far in the future: capped at MaxIdle.
	e.enqueue(e.msg("+48600000002", func(m *domain.Message) { m.NextAttemptAt = t0.Add(time.Hour) }))
	e.tr.wait(t, "capped sleep", func(v view) bool { return v.sleepsFor(chA, DefaultMaxIdle) })
}

func TestStartErrors(t *testing.T) {
	e := newEnv(t, envConfig{})
	e.start()
	if err := e.d.Start(e.ctx); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v", err)
	}
	e.shutdown()
	if err := e.d.Start(e.ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Shutdown = %v", err)
	}
	if !e.d.Stats().Closed {
		t.Fatal("Stats.Closed = false")
	}
	e.shutdown() // idempotent
	e.d.SetChannels([]ChannelSpec{spec(chA, newStub(e.clk, nil))})
	e.d.Notify()
	if len(e.d.Stats().Channels) != 0 {
		t.Fatal("SetChannels after Shutdown took effect")
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"no store", Options{}},
		{"bad retry", Options{Messages: fakeRepo{}, Retry: domain.RetryPolicy{MaxAttempts: 0, Initial: time.Second}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil {
				t.Fatal("New accepted invalid options")
			}
		})
	}
	d, err := New(Options{Messages: fakeRepo{}})
	if err != nil {
		t.Fatal(err)
	}
	if d.opts.MaxIdle != DefaultMaxIdle || d.opts.Retry != domain.DefaultRetryPolicy() || d.opts.RateBurst != 1 || d.clk == nil || d.log == nil {
		t.Fatalf("defaults = %+v", d.opts)
	}
	if err := d.Shutdown(context.Background()); err != nil { // never started
		t.Fatal(err)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"abcdef", 3, "abc"},
		{"ąąą", 3, "ą"}, // 2-byte runes: never split one
		{"ąąą", 4, "ąą"},
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.n); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestSendReporterTransitions(t *testing.T) {
	var mu sync.Mutex
	var reports []domain.ErrorKind
	e := newEnv(t, envConfig{opts: func(o *Options) {
		o.MaxIdle = time.Hour
		o.Reporter = SendReporterFunc(func(ch domain.ID, err error) {
			if ch != chA {
				t.Errorf("report for %s", ch)
			}
			mu.Lock()
			reports = append(reports, domain.KindOf(err))
			mu.Unlock()
		})
	}})
	script := []error{
		domain.Transient("send", errors.New("busy")), // first failure: reported
		domain.Transient("send", errors.New("busy")), // same class: not reported
		nil, // recovered: reported (nil)
		domain.Permanent("send", errors.New("bad")), // message problem: not reported
		nil, // no change
	}
	conn := newStub(e.clk, func(_ context.Context, n int, _ stubCall) (connector.SendResult, error) {
		return connector.SendResult{}, script[n-1]
	})
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	m1 := e.enqueue(e.msg("+48600000001"))[0]
	for i, backoff := range []time.Duration{30 * time.Second, 2 * time.Minute} {
		e.tr.wait(t, "backoff", func(v view) bool { return conn.N() == i+1 && v.sleepsFor(chA, backoff) })
		e.clk.Advance(backoff)
	}
	e.waitStatus(chA, domain.StatusSent, m1.ID)
	ms := e.enqueue(e.msg("+48600000002"), e.msg("+48600000003"))
	e.waitStatus(chA, domain.StatusFailed, ms[0].ID)
	e.waitStatus(chA, domain.StatusSent, ms[1].ID)
	mu.Lock()
	defer mu.Unlock()
	if want := []domain.ErrorKind{domain.KindTransient, ""}; !slices.Equal(reports, want) {
		t.Fatalf("reports = %v, want %v", reports, want)
	}
}
