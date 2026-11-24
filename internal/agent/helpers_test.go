// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/m4drak/tikker/internal/agent/fakeagent"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var t0 = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

const (
	devID  = domain.ID("01J0000000000000000000DEV1")
	token  = "agent-token-0123456789abcdef-0123456789"
	waitUp = 10 * time.Second
)

var lte1 = connector.ChannelRef{ChannelID: "01J00000000000000000000CH1", Port: "lte1"}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// env is one hub served over HTTP, one connector and one fake agent.
type env struct {
	t     *testing.T
	clk   *clock.Fake
	hub   *Hub
	conn  *Conn
	srv   *httptest.Server
	agent *fakeagent.Agent
}

func newEnv(t *testing.T, opts Options, aopts fakeagent.Options) *env {
	t.Helper()
	e := &env{t: t, clk: clock.NewFake(t0)}
	if opts.Clock == nil {
		opts.Clock = e.clk
	}
	if opts.Logger == nil {
		opts.Logger = discard()
	}
	e.hub = NewHub(opts)
	conn, err := e.hub.NewConn(context.Background(), connector.DeviceConfig{DeviceID: devID, Kind: domain.ConnectorAgent, AgentToken: token})
	if err != nil {
		t.Fatal(err)
	}
	e.conn = conn
	e.srv = httptest.NewServer(e.hub.Handler())
	aopts.BaseURL, aopts.Token = e.srv.URL, token
	if aopts.Client == nil {
		aopts.Client = e.srv.Client()
	}
	e.agent = fakeagent.New(aopts)
	t.Cleanup(func() {
		_ = e.conn.Close()
		e.srv.CloseClientConnections()
		e.srv.Close()
	})
	return e
}

func msgOf(id string, body string) connector.OutboundSMS {
	return connector.OutboundSMS{MessageID: domain.ID(id), To: "+48600100200", Body: body, Encoding: domain.EncodingGSM7, Segments: 1}
}

type sendResult struct {
	res connector.SendResult
	err error
}

// sendAsync runs Send in a goroutine.
func (e *env) sendAsync(ctx context.Context, ch connector.ChannelRef, msg connector.OutboundSMS) <-chan sendResult {
	out := make(chan sendResult, 1)
	go func() {
		res, err := e.conn.Send(ctx, ch, msg)
		out <- sendResult{res, err}
	}()
	return out
}

// pollUntil polls as the agent until it receives n commands in one poll.
func (e *env) pollUntil(n int) []fakeagent.Command {
	e.t.Helper()
	deadline := time.Now().Add(waitUp)
	for time.Now().Before(deadline) {
		cmds, err := e.agent.Poll(context.Background(), 0)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(cmds) >= n {
			return cmds
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("agent did not receive %d commands", n)
	return nil
}

// queued waits until the session holds n unresolved commands.
func (e *env) queued(n int) {
	e.t.Helper()
	deadline := time.Now().Add(waitUp)
	for e.conn.s.snapshot().pendingCommands != n {
		if time.Now().After(deadline) {
			e.t.Fatalf("pending commands = %d, want %d", e.conn.s.snapshot().pendingCommands, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *env) result(ch <-chan sendResult) sendResult {
	e.t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitUp):
		e.t.Fatal("Send did not return")
	}
	return sendResult{}
}

// request performs a raw agent API request.
func (e *env) request(method, path, tok, body string) *http.Response {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func wantKind(t *testing.T, err error, kind domain.ErrorKind, ambiguous bool, cause error) {
	t.Helper()
	if domain.KindOf(err) != kind || domain.IsAmbiguous(err) != ambiguous {
		t.Fatalf("err = %v (kind %s, ambiguous %v), want %s ambiguous=%v", err, domain.KindOf(err), domain.IsAmbiguous(err), kind, ambiguous)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("err = %v, want cause %v", err, cause)
	}
}

// waitersOf returns the number of Sends waiting for message id.
func (s *session) waitersOf(id domain.ID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.byID[id]; c != nil {
		return c.waiters
	}
	return 0
}

// checkin reports ports as the agent; inbound SMS are accepted only for
// ports of the last check-in.
func (e *env) checkin(ports ...string) {
	e.t.Helper()
	var b strings.Builder
	for i, p := range ports {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"port":"` + p + `","registration":"registered"}`)
	}
	if res := e.request("POST", Prefix+"/checkin", token, `{"ports":[`+b.String()+`]}`); res.StatusCode != http.StatusOK {
		e.t.Fatalf("checkin = %d", res.StatusCode)
	}
}

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
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

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
