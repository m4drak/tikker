// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/agent/fakeagent"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

const (
	id1 = "01J00000000000000000000M01"
	id2 = "01J00000000000000000000M02"
)

func TestSendAcknowledgedByAgent(t *testing.T) {
	tests := []struct {
		name      string
		result    fakeagent.Result
		kind      domain.ErrorKind
		ambiguous bool
	}{
		{"accepted", fakeagent.Result{Status: "accepted"}, "", false},
		{"permanent", fakeagent.Result{Status: "permanent", Reason: "invalid number +48600100200"}, domain.KindPermanent, false},
		{"transient", fakeagent.Result{Status: "transient", Reason: "modem busy"}, domain.KindTransient, false},
		{"ambiguous", fakeagent.Result{Status: "ambiguous", Reason: "at-chat timeout"}, domain.KindTransient, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Options{}, fakeagent.Options{
				Decide: func(fakeagent.Command) (fakeagent.Result, bool) { return tt.result, true },
			})
			done := e.sendAsync(context.Background(), lte1, msgOf(id1, "Hello"))
			cmds := e.pollUntil(1)
			c := cmds[0]
			if c.MessageID != id1 || c.Port != "lte1" || c.To != "+48600100200" || c.Body != "Hello" ||
				c.Encoding != "gsm7" || c.Segments != 1 || len(c.PDUs) != 1 {
				t.Fatalf("command = %+v", c)
			}
			r := e.result(done)
			if tt.kind == "" {
				if r.err != nil || !r.res.AcceptedAt.Equal(t0) {
					t.Fatalf("Send = %+v, %v", r.res, r.err)
				}
			} else {
				wantKind(t, r.err, tt.kind, tt.ambiguous, nil)
				if strings.Contains(r.err.Error(), "600100200") {
					t.Fatalf("error leaks the number: %v", r.err)
				}
			}
			e.queued(0)
		})
	}
}

// A command the agent never fetched fails without ambiguity and is not
// delivered afterwards; one it fetched without reporting is ambiguous.
func TestSendWithoutResult(t *testing.T) {
	tests := []struct {
		name      string
		fetch     bool
		ambiguous bool
		cause     error
	}{
		{"not fetched", false, false, ErrNotFetched},
		{"fetched, no result", true, true, ErrNoResult},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Options{}, fakeagent.Options{
				Decide: func(fakeagent.Command) (fakeagent.Result, bool) { return fakeagent.Result{}, false },
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := e.sendAsync(ctx, lte1, msgOf(id1, "Hello"))
			e.queued(1)
			if tt.fetch {
				e.pollUntil(1)
			}
			cancel()
			r := e.result(done)
			wantKind(t, r.err, domain.KindTransient, tt.ambiguous, tt.cause)
			if !errors.Is(r.err, context.Canceled) {
				t.Fatalf("cause lost: %v", r.err)
			}
			e.queued(0)
			if cmds, err := e.agent.Poll(context.Background(), 0); err != nil || len(cmds) != 0 {
				t.Fatalf("abandoned command delivered again: %v %v", cmds, err)
			}
		})
	}
}

// SendTimeout bounds a Send on the injected clock.
func TestSendTimeout(t *testing.T) {
	e := newEnv(t, Options{SendTimeout: 30 * time.Second}, fakeagent.Options{})
	done := e.sendAsync(context.Background(), lte1, msgOf(id1, "Hello"))
	e.clk.BlockUntil(1)
	e.clk.Advance(30 * time.Second)
	r := e.result(done)
	wantKind(t, r.err, domain.KindTransient, false, ErrNotFetched)
	if domain.CodeOf(r.err) != domain.CodeTimeout || !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", r.err)
	}
}

// The result was lost after the agent sent the SMS: the Send is ambiguous,
// the retried Send with the same message ID is redelivered and the agent
// answers from its own deduplication without sending a second SMS.
func TestRedeliveryDeduplicatedByAgent(t *testing.T) {
	var mu sync.Mutex
	lose := true
	e := newEnv(t, Options{}, fakeagent.Options{LoseResult: func(fakeagent.Command) bool {
		mu.Lock()
		defer mu.Unlock()
		return lose
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := e.sendAsync(ctx, lte1, msgOf(id1, "Hello"))
	e.pollUntil(1)
	cancel()
	wantKind(t, e.result(done).err, domain.KindTransient, true, ErrNoResult)

	mu.Lock()
	lose = false
	mu.Unlock()
	done = e.sendAsync(context.Background(), lte1, msgOf(id1, "Hello"))
	e.pollUntil(1)
	if r := e.result(done); r.err != nil {
		t.Fatalf("retried Send = %v", r.err)
	}
	if n := len(e.agent.Sent()); n != 1 {
		t.Fatalf("agent sent %d SMS, want 1", n)
	}
	if n := e.agent.Fetches(); n != 2 {
		t.Fatalf("fetches = %d, want 2", n)
	}
}

// A final result that arrives after Send gave up (or for a message from
// before a restart) is remembered: the retried Send completes at once and
// the command never reaches the agent again.
func TestRedeliveryDeduplicatedByTikker(t *testing.T) {
	tests := []struct {
		status string
		kind   domain.ErrorKind
	}{
		{"accepted", ""},
		{"permanent", domain.KindPermanent},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			e := newEnv(t, Options{}, fakeagent.Options{})
			res := e.request("POST", Prefix+"/commands/"+id1+"/result", token, `{"status":"`+tt.status+`","remote_ref":"r1"}`)
			if res.StatusCode != 200 {
				t.Fatalf("result = %d", res.StatusCode)
			}
			r, err := e.conn.Send(context.Background(), lte1, msgOf(id1, "Hello"))
			if domain.KindOf(err) != tt.kind {
				t.Fatalf("Send = %+v, %v", r, err)
			}
			if tt.kind == "" && r.RemoteRef != "r1" {
				t.Fatalf("RemoteRef = %q", r.RemoteRef)
			}
			if e.conn.s.snapshot().pendingCommands != 0 {
				t.Fatal("command queued despite a remembered result")
			}
		})
	}
	// Transient results are not remembered: the next Send is queued.
	e := newEnv(t, Options{}, fakeagent.Options{})
	e.request("POST", Prefix+"/commands/"+id2+"/result", token, `{"status":"transient"}`)
	done := e.sendAsync(context.Background(), lte1, msgOf(id2, "Hello"))
	e.pollUntil(1)
	if r := e.result(done); r.err != nil {
		t.Fatal(r.err)
	}
}

// pollIDs polls once as the raw agent and returns the message IDs received.
func (e *env) pollIDs() []string {
	e.t.Helper()
	res := e.request("POST", Prefix+"/commands", token, "")
	var resp commandsResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		e.t.Fatal(err)
	}
	ids := make([]string, len(resp.Commands))
	for i, c := range resp.Commands {
		ids[i] = c.MessageID
	}
	return ids
}

// A command handed to the agent is hidden from overlapping polls for
// RedeliverAfter; once handed out again, its outcome is a possible
// duplicate whatever the agent reports (ADR 0002).
func TestRedeliveryLease(t *testing.T) {
	tests := []struct {
		status string
		kind   domain.ErrorKind
		// retryOK: a retried Send completes from the remembered result.
		retryOK bool
	}{
		{"accepted", domain.KindTransient, true},
		{"transient", domain.KindTransient, false},
		{"permanent", domain.KindPermanent, false},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			e := newEnv(t, Options{SendTimeout: time.Minute, RedeliverAfter: 10 * time.Second}, fakeagent.Options{})
			done := e.sendAsync(context.Background(), lte1, msgOf(id1, "Hello"))
			e.queued(1)
			if got := e.pollIDs(); !slices.Equal(got, []string{id1}) {
				t.Fatalf("first poll = %v", got)
			}
			if got := e.pollIDs(); len(got) != 0 {
				t.Fatalf("overlapping poll got %v, want nothing", got)
			}
			e.clk.Advance(9 * time.Second)
			if got := e.pollIDs(); len(got) != 0 {
				t.Fatalf("poll within the lease got %v", got)
			}
			e.clk.Advance(time.Second)
			if got := e.pollIDs(); !slices.Equal(got, []string{id1}) {
				t.Fatalf("poll after the lease = %v", got)
			}
			e.request("POST", Prefix+"/commands/"+id1+"/result", token, `{"status":"`+tt.status+`"}`)
			r := e.result(done)
			wantKind(t, r.err, tt.kind, true, ErrRedelivered)
			if tt.retryOK {
				if _, err := e.conn.Send(context.Background(), lte1, msgOf(id1, "Hello")); err != nil {
					t.Fatalf("retried Send = %v", err)
				}
			}
		})
	}
}

// A command delivered once keeps the agent's outcome unchanged.
func TestSingleDeliveryNotAmbiguous(t *testing.T) {
	e := newEnv(t, Options{RedeliverAfter: 10 * time.Second}, fakeagent.Options{})
	done := e.sendAsync(context.Background(), lte1, msgOf(id1, "Hello"))
	e.queued(1)
	if got := e.pollIDs(); len(got) != 1 {
		t.Fatalf("poll = %v", got)
	}
	e.request("POST", Prefix+"/commands/"+id1+"/result", token, `{"status":"accepted"}`)
	if r := e.result(done); r.err != nil {
		t.Fatalf("Send = %v", r.err)
	}
}

// Remembered results expire after ResultTTL.
func TestResultTTL(t *testing.T) {
	e := newEnv(t, Options{ResultTTL: time.Hour}, fakeagent.Options{})
	e.request("POST", Prefix+"/commands/"+id1+"/result", token, `{"status":"accepted"}`)
	e.clk.Advance(time.Hour + time.Second)
	done := e.sendAsync(context.Background(), lte1, msgOf(id1, "Hello"))
	e.queued(1)
	e.pollUntil(1)
	if r := e.result(done); r.err != nil {
		t.Fatal(r.err)
	}
}

// Two Sends of the same message ID share one command.
func TestConcurrentSendsShareCommand(t *testing.T) {
	e := newEnv(t, Options{MaxConcurrentSends: 2}, fakeagent.Options{})
	a := e.sendAsync(context.Background(), lte1, msgOf(id1, "Hello"))
	b := e.sendAsync(context.Background(), lte1, msgOf(id1, "Hello"))
	e.queued(1)
	deadline := time.Now().Add(waitUp)
	for e.conn.s.waitersOf(id1) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second Send did not join")
		}
		time.Sleep(time.Millisecond)
	}
	e.pollUntil(1)
	if ra, rb := e.result(a), e.result(b); ra.err != nil || rb.err != nil {
		t.Fatalf("Send = %v / %v", ra.err, rb.err)
	}
	if n := len(e.agent.Sent()); n != 1 {
		t.Fatalf("agent sent %d", n)
	}
}

func TestSendValidation(t *testing.T) {
	long := strings.Repeat("x", 161)
	tests := []struct {
		name string
		opts Options
		ch   connector.ChannelRef
		msg  connector.OutboundSMS
	}{
		{"no port", Options{}, connector.ChannelRef{}, msgOf(id1, "x")},
		{"no message id", Options{}, lte1, connector.OutboundSMS{To: "+48600100200", Body: "x"}},
		{"no recipient", Options{}, lte1, connector.OutboundSMS{MessageID: id1, Body: "x"}},
		{"alphanumeric recipient", Options{}, lte1, connector.OutboundSMS{MessageID: id1, To: "Bank", Body: "x"}},
		{"ucs2 not enabled", Options{Encodings: []domain.Encoding{domain.EncodingGSM7}}, lte1,
			connector.OutboundSMS{MessageID: id1, To: "+48600100200", Body: "ż", Encoding: domain.EncodingUCS2}},
		{"non gsm7 text", Options{}, lte1, connector.OutboundSMS{MessageID: id1, To: "+48600100200", Body: "ż", Encoding: domain.EncodingGSM7}},
		{"too many segments", Options{MaxSegments: 1}, lte1, connector.OutboundSMS{MessageID: id1, To: "+48600100200", Body: long}},
		{"declared segments", Options{MaxSegments: 2}, lte1, connector.OutboundSMS{MessageID: id1, To: "+48600100200", Body: "x", Segments: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.opts, fakeagent.Options{})
			_, err := e.conn.Send(context.Background(), tt.ch, tt.msg)
			wantKind(t, err, domain.KindPermanent, false, ErrInvalidMessage)
			if e.conn.s.snapshot().pendingCommands != 0 {
				t.Fatal("invalid message queued")
			}
		})
	}
}

// UCS-2 and concatenated messages carry one PDU per segment.
func TestSendMultipartCarriesPDUs(t *testing.T) {
	e := newEnv(t, Options{}, fakeagent.Options{})
	body := strings.Repeat("Zażółć gęślą jaźń. ", 6) // 114 UCS-2 chars: 2 segments
	done := e.sendAsync(context.Background(), lte1, connector.OutboundSMS{
		MessageID: id1, To: "+48600100200", Body: body, Encoding: domain.EncodingUCS2, Segments: 2,
	})
	c := e.pollUntil(1)[0]
	if c.Encoding != "ucs2" || c.Segments != 2 || len(c.PDUs) != 2 || c.PDUs[0].Length == 0 {
		t.Fatalf("command = %+v", c)
	}
	if r := e.result(done); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestCloseFailsWaitingSend(t *testing.T) {
	e := newEnv(t, Options{}, fakeagent.Options{})
	done := e.sendAsync(context.Background(), lte1, msgOf(id1, "Hello"))
	e.queued(1)
	if err := e.conn.Close(); err != nil {
		t.Fatal(err)
	}
	wantKind(t, e.result(done).err, domain.KindTransient, false, ErrClosed)
	_, err := e.conn.Send(context.Background(), lte1, msgOf(id2, "x"))
	wantKind(t, err, domain.KindTransient, false, ErrClosed)
	if _, err := e.conn.FetchInbox(context.Background(), lte1); !errors.Is(err, ErrClosed) {
		t.Fatalf("FetchInbox after Close = %v", err)
	}
	if err := e.conn.DeleteInbox(context.Background(), lte1, []string{"*1"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("DeleteInbox after Close = %v", err)
	}
	if _, err := e.conn.Health(context.Background(), lte1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Health after Close = %v", err)
	}
	if _, err := e.conn.Probe(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Probe after Close = %v", err)
	}
	if err := e.conn.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	// The mailbox is gone: the token no longer authenticates.
	if res := e.request("GET", Prefix+"/status", token, ""); res.StatusCode != 401 {
		t.Fatalf("status after close = %d", res.StatusCode)
	}
}

// Connectors of one device share the mailbox; it lives until the last one
// is closed (the device test builds a second connector next to the
// channel sync's).
func TestConnectorsShareMailbox(t *testing.T) {
	e := newEnv(t, Options{}, fakeagent.Options{})
	if err := e.agent.Checkin(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := e.hub.NewConn(context.Background(), connector.DeviceConfig{DeviceID: devID, AgentToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := second.Probe(context.Background()); err != nil || p.BoardName != "fake-board" {
		t.Fatalf("second connector Probe = %+v, %v", p, err)
	}
	_ = second.Close()
	if _, err := e.conn.Health(context.Background(), lte1); err != nil {
		t.Fatalf("Health after closing the other connector = %v", err)
	}

	// A connector built with a new token replaces the old one.
	third, err := e.hub.NewConn(context.Background(), connector.DeviceConfig{DeviceID: devID, AgentToken: token + "-new"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Close() }()
	if res := e.request("GET", Prefix+"/status", token, ""); res.StatusCode != 401 {
		t.Fatalf("old token = %d", res.StatusCode)
	}
	if res := e.request("GET", Prefix+"/status", token+"-new", ""); res.StatusCode != 200 {
		t.Fatalf("new token = %d", res.StatusCode)
	}
}

// isHandleOf reports whether handle is a connector RemoteID of the router
// entry remoteID.
func isHandleOf(handle, remoteID string) bool {
	return strings.HasPrefix(handle, remoteID+"@") && len(handle) == len(remoteID)+13
}

func TestInboxStoreThenDelete(t *testing.T) {
	e := newEnv(t, Options{}, fakeagent.Options{})
	ctx := context.Background()
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	r1, _ := e.agent.Receive("lte1", "+48511222333", "Odpowiedź: tak", at)
	r2, _ := e.agent.Receive("lte1", "+48511222444", "second", at.Add(time.Minute))
	r3, _ := e.agent.Receive("lte2", "+48511222555", "other port", at)
	if err := e.agent.PushInbox(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := e.conn.FetchInbox(ctx, lte1)
	if err != nil || len(got) != 2 {
		t.Fatalf("FetchInbox = %+v, %v", got, err)
	}
	if !isHandleOf(got[0].RemoteID, r1) || got[0].Phone != "+48511222333" || got[0].PDU == "" ||
		got[0].RawTimestamp != "2026-11-23 10:00:00" || !got[0].Timestamp.Equal(at) || !isHandleOf(got[1].RemoteID, r2) {
		t.Fatalf("entries = %+v", got)
	}
	if !e.conn.Capabilities().InboundPDU {
		t.Fatal("InboundPDU not set")
	}

	// Pushing again changes nothing.
	if err := e.agent.PushInbox(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.conn.FetchInbox(ctx, lte1); len(got) != 2 {
		t.Fatalf("after re-push: %d entries", len(got))
	}

	// Stored -> deleted: gone from FetchInbox, listed to the agent.
	if err := e.conn.DeleteInbox(ctx, lte1, []string{got[0].RemoteID, r1, "*missing"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.conn.FetchInbox(ctx, lte1); len(got) != 1 || !isHandleOf(got[0].RemoteID, r2) {
		t.Fatalf("after delete: %+v", got)
	}
	// A push before the agent has deleted it does not bring it back.
	if err := e.agent.PushInbox(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.conn.FetchInbox(ctx, lte1); len(got) != 1 {
		t.Fatalf("deleted entry came back: %+v", got)
	}
	if n := len(e.agent.Inbox()); n != 2 {
		t.Fatalf("router inbox has %d entries, want 2 (r1 deleted via the push response)", n)
	}
	if st := e.conn.s.snapshot(); st.pendingDeletes != 0 {
		t.Fatalf("pending deletes after ack = %d", st.pendingDeletes)
	}
	// Other ports are separate.
	if got, _ := e.conn.FetchInbox(ctx, connector.ChannelRef{Port: "lte2"}); len(got) != 1 || !isHandleOf(got[0].RemoteID, r3) {
		t.Fatalf("lte2 = %+v", got)
	}
}

// RouterOS reuses an ID after deletion: a different message under an ID
// tikker asked to delete is new.
func TestInboxRemoteIDReuse(t *testing.T) {
	e := newEnv(t, Options{}, fakeagent.Options{})
	ctx := context.Background()
	push := func(body string) {
		t.Helper()
		res := e.request("POST", Prefix+"/inbox", token,
			`{"messages":[{"port":"lte1","remote_id":"*1","phone":"+48511222333","timestamp":"2026-11-23 10:00:00","body":"`+body+`"}]}`)
		if res.StatusCode != 200 {
			t.Fatalf("push = %d", res.StatusCode)
		}
	}
	push("first")
	got, _ := e.conn.FetchInbox(ctx, lte1)
	if len(got) != 1 {
		t.Fatalf("FetchInbox = %+v", got)
	}
	_ = e.conn.DeleteInbox(ctx, lte1, []string{got[0].RemoteID})
	push("first") // same content: already stored
	if got, _ := e.conn.FetchInbox(ctx, lte1); len(got) != 0 {
		t.Fatalf("same entry re-added: %+v", got)
	}
	push("second") // same ID, new content
	if got, _ := e.conn.FetchInbox(ctx, lte1); len(got) != 1 || got[0].Body != "second" {
		t.Fatalf("reused ID: %+v", got)
	}
	if st := e.conn.s.snapshot(); st.pendingDeletes != 0 {
		t.Fatalf("stale deletion kept: %d", st.pendingDeletes)
	}
}

// The router reuses an ID while tikker still buffers the old entry under
// it: FetchInbox returned A, the agent then pushes B under the same ID.
// DeleteInbox of what was fetched must remove and mark A only; B stays
// buffered, and the agent is told to delete only an entry matching A.
func TestInboxRemoteIDReusedBeforeDelete(t *testing.T) {
	const (
		tsA = "2026-11-23 10:00:00"
		tsB = "2026-11-23 10:05:00"
	)
	tests := []struct {
		name   string
		phoneB string
		tsB    string
	}{
		{"other sender", "+48511222444", tsB},
		{"same sender, later", "+48511222333", tsB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Options{}, fakeagent.Options{})
			ctx := context.Background()
			push := func(phone, ts, body string) {
				t.Helper()
				res := e.request("POST", Prefix+"/inbox", token,
					`{"messages":[{"port":"lte1","remote_id":"*1","phone":"`+phone+`","timestamp":"`+ts+`","body":"`+body+`"}]}`)
				if res.StatusCode != 200 {
					t.Fatalf("push = %d", res.StatusCode)
				}
			}
			push("+48511222333", tsA, "A")
			fetched, _ := e.conn.FetchInbox(ctx, lte1)
			if len(fetched) != 1 || fetched[0].Body != "A" {
				t.Fatalf("FetchInbox = %+v", fetched)
			}
			push(tt.phoneB, tt.tsB, "B") // the router reused *1
			if err := e.conn.DeleteInbox(ctx, lte1, []string{fetched[0].RemoteID}); err != nil {
				t.Fatal(err)
			}
			got, _ := e.conn.FetchInbox(ctx, lte1)
			if len(got) != 1 || got[0].Body != "B" {
				t.Fatalf("after deleting A: %+v, want B still buffered", got)
			}
			res := e.request("POST", Prefix+"/commands", token, "")
			var resp commandsResponse
			if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
				t.Fatal(err)
			}
			want := []inboxRef{{Port: "lte1", RemoteID: "*1", Phone: "+48511222333", Timestamp: tsA}}
			if !slices.Equal(resp.InboxDelete, want) {
				t.Fatalf("inbox_delete = %+v, want %+v", resp.InboxDelete, want)
			}
		})
	}
}

// An ack with phone and timestamp forgets only the matching deletion; one
// without (older scripts) forgets every deletion under the ID.
func TestInboxAckMatchesContent(t *testing.T) {
	tests := []struct {
		name    string
		ack     string
		removed int
	}{
		{"matching", `{"port":"lte1","remote_id":"*1","phone":"+48511222333","timestamp":"2026-11-23 10:00:00"}`, 1},
		{"other content", `{"port":"lte1","remote_id":"*1","phone":"+48511222999","timestamp":"2026-11-23 10:00:00"}`, 0},
		{"legacy", `{"port":"lte1","remote_id":"*1"}`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Options{}, fakeagent.Options{})
			ctx := context.Background()
			e.request("POST", Prefix+"/inbox", token,
				`{"messages":[{"port":"lte1","remote_id":"*1","phone":"+48511222333","timestamp":"2026-11-23 10:00:00","body":"A"}]}`)
			got, _ := e.conn.FetchInbox(ctx, lte1)
			if err := e.conn.DeleteInbox(ctx, lte1, []string{got[0].RemoteID}); err != nil {
				t.Fatal(err)
			}
			body := readAll(t, e.request("POST", Prefix+"/inbox/ack", token, `{"deleted":[`+tt.ack+`]}`))
			if !strings.Contains(body, fmt.Sprintf(`"removed":%d`, tt.removed)) {
				t.Fatalf("ack = %s", body)
			}
			if n := e.conn.s.snapshot().pendingDeletes; n != 1-tt.removed {
				t.Fatalf("pending deletes = %d", n)
			}
		})
	}
}

// When the pending deletions are at their cap, DeleteInbox keeps the
// entries it cannot mark (and fails transiently) instead of dropping them
// unmarked, which would make the next push add them again.
func TestInboxPendingDeletesCap(t *testing.T) {
	e := newEnv(t, Options{maxPendingDeletes: 1}, fakeagent.Options{})
	ctx := context.Background()
	e.request("POST", Prefix+"/inbox", token, `{"messages":[
		{"port":"lte1","remote_id":"*1","phone":"+48511222333","body":"a"},
		{"port":"lte1","remote_id":"*2","phone":"+48511222333","body":"b"}]}`)
	got, _ := e.conn.FetchInbox(ctx, lte1)
	if len(got) != 2 {
		t.Fatalf("FetchInbox = %+v", got)
	}
	err := e.conn.DeleteInbox(ctx, lte1, []string{got[0].RemoteID, got[1].RemoteID})
	wantKind(t, err, domain.KindTransient, false, ErrTooManyDeletes)
	left, _ := e.conn.FetchInbox(ctx, lte1)
	if len(left) != 1 || left[0].Body != "b" {
		t.Fatalf("after capped delete: %+v, want b kept", left)
	}
	if n := e.conn.s.snapshot().pendingDeletes; n != 1 {
		t.Fatalf("pending deletes = %d", n)
	}
	// Re-pushing does not duplicate b.
	e.request("POST", Prefix+"/inbox", token, `{"messages":[{"port":"lte1","remote_id":"*2","phone":"+48511222333","body":"b"}]}`)
	if left, _ := e.conn.FetchInbox(ctx, lte1); len(left) != 1 {
		t.Fatalf("after re-push: %+v", left)
	}
}

func TestInboxLimit(t *testing.T) {
	e := newEnv(t, Options{MaxInbox: 2}, fakeagent.Options{})
	for i := range 3 {
		if _, err := e.agent.Receive("lte1", "+48511222333", "m", t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	res := e.request("POST", Prefix+"/inbox", token, `{"messages":[
		{"port":"lte1","remote_id":"*1","body":"a"},{"port":"lte1","remote_id":"*2","body":"b"},{"port":"lte1","remote_id":"*3","body":"c"}]}`)
	body := readAll(t, res)
	if res.StatusCode != 200 || !strings.Contains(body, `"accepted":2`) || !strings.Contains(body, `"refused":1`) {
		t.Fatalf("push = %d %s", res.StatusCode, body)
	}
}

func TestHealth(t *testing.T) {
	e := newEnv(t, Options{StaleAfter: time.Minute}, fakeagent.Options{})
	ctx := context.Background()

	_, err := e.conn.Health(ctx, lte1)
	wantKind(t, err, domain.KindTransient, false, ErrNoCheckin)
	if domain.CodeOf(err) != domain.CodeUnreachable {
		t.Fatalf("code = %q", domain.CodeOf(err))
	}

	if err := e.agent.Checkin(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := e.conn.Health(ctx, lte1)
	if err != nil || !h.Running || !h.Registered || h.Operator != "Fake GSM" || h.Technology != "LTE" ||
		h.Signal == nil || *h.Signal.RSSI != -67 || !h.CheckedAt.Equal(t0) {
		t.Fatalf("Health = %+v, %v", h, err)
	}
	_, err = e.conn.Health(ctx, connector.ChannelRef{Port: "lte9"})
	wantKind(t, err, domain.KindPermanent, false, ErrUnknownPort)

	e.clk.Advance(time.Minute + time.Second)
	_, err = e.conn.Health(ctx, lte1)
	wantKind(t, err, domain.KindTransient, false, ErrStale)

	// Polling alone does not refresh the modem state.
	if _, err := e.agent.Poll(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.conn.Health(ctx, lte1); !errors.Is(err, ErrStale) {
		t.Fatalf("Health after poll = %v", err)
	}
	if err := e.agent.Checkin(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.conn.Health(ctx, lte1); err != nil {
		t.Fatalf("Health after new check-in = %v", err)
	}
}

func TestProbe(t *testing.T) {
	e := newEnv(t, Options{StaleAfter: time.Minute}, fakeagent.Options{
		Ports: []fakeagent.Port{{Port: "lte1", Running: true, Registration: "home"}, {Port: "lte2", Registration: "searching"}},
	})
	ctx := context.Background()
	_, err := e.conn.Probe(ctx)
	wantKind(t, err, domain.KindTransient, false, ErrNoCheckin)
	if domain.CodeOf(err) != domain.CodeUnreachable {
		t.Fatalf("code = %q", domain.CodeOf(err))
	}
	if err := e.agent.Checkin(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := e.conn.Probe(ctx)
	if err != nil || p.RouterOSVersion != "7.16.1 (stable)" || p.BoardName != "fake-board" || p.TLS != nil || len(p.Ports) != 2 {
		t.Fatalf("Probe = %+v, %v", p, err)
	}
	if !p.Ports[0].Health.Registered || p.Ports[1].Health.Registered || p.Ports[1].Health.Running {
		t.Fatalf("ports = %+v", p.Ports)
	}
	if got := e.conn.LastCheckin(); !got.Equal(t0) {
		t.Fatalf("LastCheckin = %v", got)
	}
	e.clk.Advance(2 * time.Minute)
	p, err = e.conn.Probe(ctx)
	wantKind(t, err, domain.KindTransient, false, ErrStale)
	if p.BoardName != "fake-board" {
		t.Fatalf("stale probe lost the data: %+v", p)
	}
}

func TestCapabilities(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want connector.Capabilities
	}{
		{"defaults", Options{}, connector.Capabilities{
			Encodings: []domain.Encoding{domain.EncodingGSM7, domain.EncodingUCS2}, MaxSegments: DefaultMaxSegments,
			Concatenation: true, MaxConcurrentSends: 1, SendTimeout: DefaultSendTimeout, InboundPDU: true,
		}},
		{"single gsm7", Options{Encodings: []domain.Encoding{domain.EncodingGSM7}, MaxSegments: 1, SendTimeout: time.Minute, MaxConcurrentSends: 2},
			connector.Capabilities{
				Encodings: []domain.Encoding{domain.EncodingGSM7}, MaxSegments: 1,
				Concatenation: false, MaxConcurrentSends: 2, SendTimeout: time.Minute, InboundPDU: true,
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.opts, fakeagent.Options{})
			got := e.conn.Capabilities()
			if !slices.Equal(got.Encodings, tt.want.Encodings) || got.MaxSegments != tt.want.MaxSegments ||
				got.Concatenation != tt.want.Concatenation || got.MaxConcurrentSends != tt.want.MaxConcurrentSends ||
				got.SendTimeout != tt.want.SendTimeout || got.InboundPDU != tt.want.InboundPDU {
				t.Fatalf("Capabilities = %+v, want %+v", got, tt.want)
			}
			got.Encodings[0] = "x" // a copy
			if e.conn.Capabilities().Encodings[0] == "x" {
				t.Fatal("Capabilities shares its slice")
			}
		})
	}
}

func TestFactory(t *testing.T) {
	hub := NewHub(Options{Logger: discard()})
	reg := connector.NewRegistry()
	if err := hub.Register(reg); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		cfg  connector.DeviceConfig
		ok   bool
	}{
		{"ok", connector.DeviceConfig{DeviceID: devID, Kind: domain.ConnectorAgent, AgentToken: token}, true},
		{"no token", connector.DeviceConfig{DeviceID: devID, Kind: domain.ConnectorAgent}, false},
		{"no id", connector.DeviceConfig{Kind: domain.ConnectorAgent, AgentToken: token}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := reg.New(context.Background(), tt.cfg)
			if (err == nil) != tt.ok {
				t.Fatalf("New = %v", err)
			}
			if c != nil {
				_ = c.Close()
			}
		})
	}
	if _, err := hub.NewConn(context.Background(), connector.DeviceConfig{DeviceID: devID, Kind: domain.ConnectorRouterOSREST, AgentToken: token}); err == nil {
		t.Fatal("wrong kind accepted")
	}
}
