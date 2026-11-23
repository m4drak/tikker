// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/agent/fakeagent"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

type problemBody struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Instance string       `json:"instance"`
	Errors   []fieldError `json:"errors"`
}

func wantProblem(t *testing.T, res *http.Response, status int, typ string) problemBody {
	t.Helper()
	body := readAll(t, res)
	if res.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", res.StatusCode, status, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q", ct)
	}
	var p problemBody
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("problem: %v: %s", err, body)
	}
	if p.Type != typ || p.Status != status || p.Title == "" || p.Instance == "" {
		t.Fatalf("problem = %+v", p)
	}
	return p
}

func TestHandlerAuth(t *testing.T) {
	e := newEnv(t, Options{}, fakeagent.Options{})
	tests := []struct {
		name   string
		header string
		status int
	}{
		{"no header", "", 401},
		{"basic scheme", "Basic " + token, 401},
		{"wrong token", "Bearer wrong-token-0123456789abcdef-012345", 401},
		{"prefix of the token", "Bearer " + token[:len(token)-1], 401},
		{"token plus suffix", "Bearer " + token + "x", 401},
		{"empty bearer", "Bearer ", 401},
		{"valid", "Bearer " + token, 200},
		{"scheme case-insensitive", "bearer " + token, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.srv.URL+Prefix+"/status", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			res, err := e.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			if tt.status == 401 {
				wantProblem(t, res, 401, problemUnauthorized)
				if res.Header.Get("WWW-Authenticate") == "" {
					t.Fatal("no WWW-Authenticate")
				}
				return
			}
			var st statusResponse
			if err := json.NewDecoder(res.Body).Decode(&st); err != nil || res.StatusCode != 200 || st.DeviceID != devID.String() {
				t.Fatalf("status = %d %+v %v", res.StatusCode, st, err)
			}
		})
	}
}

// A token configured on two devices authenticates neither.
func TestHandlerSharedTokenRejected(t *testing.T) {
	e := newEnv(t, Options{}, fakeagent.Options{})
	other, err := e.hub.NewConn(context.Background(), connector.DeviceConfig{DeviceID: "01J0000000000000000000DEV2", AgentToken: token})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	wantProblem(t, e.request("GET", Prefix+"/status", token, ""), 401, problemUnauthorized)
}

func TestHandlerProblems(t *testing.T) {
	e := newEnv(t, Options{}, fakeagent.Options{})
	big := `{"messages":[{"port":"lte1","remote_id":"*1","body":"` + strings.Repeat("x", MaxBodyBytes) + `"}]}`
	tests := []struct {
		name   string
		method string
		path   string
		tok    string
		body   string
		status int
		typ    string
		field  string
	}{
		{"unknown path", "GET", Prefix + "/nope", token, "", 404, problemNotFound, ""},
		{"unknown path without token", "GET", Prefix + "/nope", "", "", 404, problemNotFound, ""},
		{"wrong method", "GET", Prefix + "/commands", token, "", 405, problemMethodNotAllow, ""},
		{"wrong method before auth", "DELETE", Prefix + "/checkin", "", "", 405, problemMethodNotAllow, ""},
		{"malformed json", "POST", Prefix + "/checkin", token, `{"ports":`, 400, problemMalformed, ""},
		{"wrong type", "POST", Prefix + "/checkin", token, `{"ports":"lte1"}`, 400, problemMalformed, ""},
		{"trailing data", "POST", Prefix + "/checkin", token, `{} {}`, 400, problemMalformed, ""},
		{"empty body required", "POST", Prefix + "/inbox", token, "", 400, problemMalformed, ""},
		{"too large", "POST", Prefix + "/inbox", token, big, 413, problemPayloadTooLarge, ""},
		{"bad port", "POST", Prefix + "/checkin", token, `{"ports":[{"port":"lte 1"}]}`, 422, problemValidation, "/ports/0/port"},
		{"duplicate port", "POST", Prefix + "/checkin", token, `{"ports":[{"port":"lte1"},{"port":"lte1"}]}`, 422, problemValidation, "/ports/1/port"},
		{"bad status", "POST", Prefix + "/commands/" + id1 + "/result", token, `{"status":"done"}`, 422, problemValidation, "/status"},
		{"bad message id", "POST", Prefix + "/commands/xyz/result", token, `{"status":"accepted"}`, 404, problemNotFound, ""},
		{"bad pdu", "POST", Prefix + "/inbox", token, `{"messages":[{"port":"lte1","remote_id":"*1","pdu":"zz"}]}`, 422, problemValidation, "/messages/0/pdu"},
		{"no content", "POST", Prefix + "/inbox", token, `{"messages":[{"port":"lte1","remote_id":"*1"}]}`, 422, problemValidation, "/messages/0"},
		{"no remote id", "POST", Prefix + "/inbox", token, `{"messages":[{"port":"lte1","body":"x"}]}`, 422, problemValidation, "/messages/0/remote_id"},
		{"control chars", "POST", Prefix + "/inbox", token, `{"messages":[{"port":"lte1","remote_id":"*1\n","body":"x"}]}`, 422, problemValidation, "/messages/0/remote_id"},
		{"bad ack", "POST", Prefix + "/inbox/ack", token, `{"deleted":[{"port":"","remote_id":"*1"}]}`, 422, problemValidation, "/deleted/0/port"},
		{"negative wait", "POST", Prefix + "/commands", token, `{"wait":-1}`, 422, problemValidation, ""},
		{"bad number", "POST", Prefix + "/commands", token, `{"max":"many"}`, 400, problemMalformed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := wantProblem(t, e.request(tt.method, tt.path, tt.tok, tt.body), tt.status, tt.typ)
			if tt.field != "" && (len(p.Errors) == 0 || p.Errors[0].Field != tt.field) {
				t.Fatalf("errors = %+v, want field %s", p.Errors, tt.field)
			}
		})
	}
}

// RouterOS scripts may quote numbers and booleans.
func TestCheckinLenientScalars(t *testing.T) {
	e := newEnv(t, Options{}, fakeagent.Options{})
	res := e.request("POST", Prefix+"/checkin", token, `{"routeros_version":"7.16.1","board":"hAP ax lite LTE6",
		"ports":[{"port":"lte1","running":"true","registration":"roaming","rssi":"-71dBm","rsrq":"-10.5dB","sinr":12},
		         {"port":"lte2","running":"no","registered":"yes","rsrp":""}]}`)
	if body := readAll(t, res); res.StatusCode != 200 || !strings.Contains(body, `"stale_after_s":90`) {
		t.Fatalf("checkin = %d %s", res.StatusCode, body)
	}
	h1, err := e.conn.Health(context.Background(), lte1)
	if err != nil || !h1.Running || !h1.Registered || *h1.Signal.RSSI != -71 || *h1.Signal.RSRQ != -11 || *h1.Signal.SINR != 12 || h1.Signal.RSRP != nil {
		t.Fatalf("lte1 = %+v %+v, %v", h1, h1.Signal, err)
	}
	h2, err := e.conn.Health(context.Background(), connector.ChannelRef{Port: "lte2"})
	if err != nil || h2.Running || !h2.Registered || h2.Signal != nil {
		t.Fatalf("lte2 = %+v, %v", h2, err)
	}
}

func TestCommandsPortFilterAndMax(t *testing.T) {
	e := newEnv(t, Options{MaxConcurrentSends: 3}, fakeagent.Options{})
	ids := []string{"01J00000000000000000000M11", "01J00000000000000000000M12", "01J00000000000000000000M13"}
	var dones []<-chan sendResult
	for i, id := range ids {
		port := "lte1"
		if i == 2 {
			port = "lte2"
		}
		dones = append(dones, e.sendAsync(context.Background(), connector.ChannelRef{Port: port}, msgOf(id, "x")))
	}
	e.queued(3)
	var resp commandsResponse
	res := e.request("POST", Prefix+"/commands", token, `{"ports":["lte2"]}`)
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil || len(resp.Commands) != 1 || resp.Commands[0].Port != "lte2" {
		t.Fatalf("lte2 commands = %+v, %v", resp, err)
	}
	res = e.request("POST", Prefix+"/commands", token, `{"max":"1"}`)
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil || len(resp.Commands) != 1 {
		t.Fatalf("max=1 commands = %+v, %v", resp, err)
	}
	for _, id := range ids {
		e.request("POST", Prefix+"/commands/"+id+"/result", token, `{"status":"accepted"}`)
	}
	for _, d := range dones {
		if r := e.result(d); r.err != nil {
			t.Fatal(r.err)
		}
	}
}

// A long-poll returns as soon as a command is queued.
func TestCommandsLongPoll(t *testing.T) {
	hub := NewHub(Options{Logger: discard()}) // real clock
	conn, err := hub.NewConn(context.Background(), connector.DeviceConfig{DeviceID: devID, AgentToken: token})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	srv := httptest.NewServer(hub.Handler())
	defer srv.Close()
	ag := fakeagent.New(fakeagent.Options{BaseURL: srv.URL, Token: token, Client: srv.Client()})

	type pollRes struct {
		cmds []fakeagent.Command
		err  error
		took time.Duration
	}
	polled := make(chan pollRes, 1)
	go func() {
		start := time.Now()
		cmds, err := ag.Poll(context.Background(), 20)
		polled <- pollRes{cmds, err, time.Since(start)}
	}()
	time.Sleep(50 * time.Millisecond) // let the poll start waiting
	res, err := conn.Send(context.Background(), lte1, msgOf(id1, "Hello"))
	if err != nil || res.AcceptedAt.IsZero() {
		t.Fatalf("Send = %+v, %v", res, err)
	}
	p := <-polled
	if p.err != nil || len(p.cmds) != 1 || p.took > 10*time.Second {
		t.Fatalf("poll = %+v", p)
	}

	// An empty long-poll ends after the requested wait; closing the
	// connector ends it at once.
	go func() {
		start := time.Now()
		cmds, err := ag.Poll(context.Background(), 20)
		polled <- pollRes{cmds, err, time.Since(start)}
	}()
	time.Sleep(50 * time.Millisecond)
	_ = conn.Close()
	p = <-polled
	if p.err != nil || len(p.cmds) != 0 || p.took > 10*time.Second {
		t.Fatalf("poll after close = %+v", p)
	}
}

// Several agents with several ports poll concurrently while their
// connectors send: every command reaches its own device and port and every
// Send completes (run with -race).
func TestManyAgentsConcurrently(t *testing.T) {
	const devices, ports, perPort = 4, 2, 5
	clk := clock.NewFake(t0)
	hub := NewHub(Options{Logger: discard(), Clock: clk, MaxConcurrentSends: perPort})
	srv := httptest.NewServer(hub.Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type dev struct {
		conn  *Conn
		agent *fakeagent.Agent
		id    domain.ID
	}
	var devs []dev
	var agentsWG sync.WaitGroup
	for d := range devices {
		id := domain.ID(fmt.Sprintf("01J0000000000000000000DE%02d", d))
		tok := fmt.Sprintf("%s-%02d", token, d)
		conn, err := hub.NewConn(ctx, connector.DeviceConfig{DeviceID: id, AgentToken: tok})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		var pp []fakeagent.Port
		for p := range ports {
			pp = append(pp, fakeagent.Port{Port: fmt.Sprintf("lte%d", p+1), Running: true, Registration: "home"})
		}
		ag := fakeagent.New(fakeagent.Options{BaseURL: srv.URL, Token: tok, Client: srv.Client(), Ports: pp})
		devs = append(devs, dev{conn, ag, id})
		agentsWG.Add(1)
		go func() {
			defer agentsWG.Done()
			for ctx.Err() == nil {
				_ = ag.Checkin(ctx)
				_, _ = ag.Poll(ctx, 0)
				time.Sleep(time.Millisecond)
			}
		}()
	}

	var sendsWG sync.WaitGroup
	errs := make(chan error, devices*ports*perPort)
	for d, dv := range devs {
		for p := range ports {
			for m := range perPort {
				sendsWG.Add(1)
				go func() {
					defer sendsWG.Done()
					id := domain.ID(fmt.Sprintf("01J000000000000000000%d%d%03d", d, p, m))
					ch := connector.ChannelRef{Port: fmt.Sprintf("lte%d", p+1)}
					if _, err := dv.conn.Send(ctx, ch, msgOf(string(id), fmt.Sprintf("dev%d", d))); err != nil {
						errs <- fmt.Errorf("device %d port %d msg %d: %w", d, p, m, err)
					}
					_, _ = dv.conn.Health(ctx, ch)
				}()
			}
		}
	}
	sendsWG.Wait()
	cancel()
	agentsWG.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for d, dv := range devs {
		sent := dv.agent.Sent()
		if len(sent) != ports*perPort {
			t.Errorf("device %d: agent sent %d, want %d", d, len(sent), ports*perPort)
		}
		for _, c := range sent {
			if c.Body != fmt.Sprintf("dev%d", d) {
				t.Errorf("device %d received a command of another device: %+v", d, c)
			}
		}
	}
}
