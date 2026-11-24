// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package fakeagent is an in-process stand-in for the RouterOS agent script
// (deploy/routeros/agent.rsc) for tests: it speaks the /agent/v1 protocol
// over real HTTP, keeps a simulated router inbox and records what it "sent".
// It follows the script's rules: commands are deduplicated by message ID,
// inbox entries are deleted only when tikker lists them in inbox_delete
// and phone and timestamp still match.
package fakeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/connector/fake"
)

// Command is a command received from tikker.
type Command struct {
	MessageID string `json:"message_id"`
	Port      string `json:"port"`
	To        string `json:"to"`
	Body      string `json:"body"`
	Encoding  string `json:"encoding"`
	Segments  int    `json:"segments"`
	PDUs      []struct {
		PDU    string `json:"pdu"`
		Length int    `json:"length"`
	} `json:"pdus"`
}

// Result is what the agent reports for a command.
type Result struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// InboxEntry is one SMS in the simulated router inbox.
type InboxEntry struct {
	Port      string `json:"port"`
	RemoteID  string `json:"remote_id"`
	Phone     string `json:"phone"`
	Timestamp string `json:"timestamp"`
	PDU       string `json:"pdu"`
	Body      string `json:"body"`
}

// ref names an inbox entry tikker asks to delete. Phone and Timestamp are
// what the agent pushed; the entry is deleted only if they still match.
type ref struct {
	Port      string `json:"port"`
	RemoteID  string `json:"remote_id"`
	Phone     string `json:"phone,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

func (r ref) matches(e InboxEntry) bool {
	if r.Port != e.Port || r.RemoteID != e.RemoteID {
		return false
	}
	if r.Phone == "" && r.Timestamp == "" {
		return true // older servers
	}
	return r.Phone == e.Phone && r.Timestamp == e.Timestamp
}

// Port is the modem state reported at check-in.
type Port struct {
	Port         string `json:"port"`
	Running      bool   `json:"running"`
	Registration string `json:"registration"`
	Operator     string `json:"operator"`
	Technology   string `json:"technology"`
	RSSI         int    `json:"rssi"`
}

// Options configure an Agent.
type Options struct {
	// BaseURL of tikker, without /agent/v1.
	BaseURL string
	Token   string
	Client  *http.Client // default: a client with a 30 s timeout
	Ports   []Port       // default: lte1, registered
	Version string       // default "7.16.1 (stable)"
	Board   string       // default "fake-board"
	// Decide, if set, chooses the result of a new command (default
	// accepted). Returning ok=false fetches the command without reporting
	// a result (the script crashed or lost the connection).
	Decide func(Command) (r Result, ok bool)
	// LoseResult, if set and true for a command, executes it but drops the
	// result report (the response was lost on the way to tikker).
	LoseResult func(Command) bool
}

// Agent is a fake router script. It is safe for concurrent use.
type Agent struct {
	opts Options

	mu      sync.Mutex
	sent    []Command         // commands executed, in order
	done    map[string]Result // executed message IDs (deduplication)
	inbox   []InboxEntry      // simulated router inbox
	nextID  int
	fetches int
}

// New returns an agent.
func New(opts Options) *Agent {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if len(opts.Ports) == 0 {
		opts.Ports = []Port{{Port: "lte1", Running: true, Registration: "registered", Operator: "Fake GSM", Technology: "LTE", RSSI: -67}}
	}
	if opts.Version == "" {
		opts.Version = "7.16.1 (stable)"
	}
	if opts.Board == "" {
		opts.Board = "fake-board"
	}
	return &Agent{opts: opts, done: map[string]Result{}}
}

// StatusError is a non-2xx answer of tikker.
type StatusError struct {
	Status      int
	ContentType string
	Body        string
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

func (a *Agent) post(ctx context.Context, path string, in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.opts.BaseURL, "/")+"/agent/v1"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.opts.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.opts.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		return &StatusError{Status: res.StatusCode, ContentType: res.Header.Get("Content-Type"), Body: string(body)}
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// Checkin reports version, board and ports.
func (a *Agent) Checkin(ctx context.Context) error {
	return a.post(ctx, "/checkin", map[string]any{
		"routeros_version": a.opts.Version, "board": a.opts.Board, "agent_version": "fakeagent", "ports": a.opts.Ports,
	}, nil)
}

// Receive puts an SMS into the simulated router inbox of port and returns
// its remote ID. The PDU is a real SMS-DELIVER (UCS-2 when needed).
func (a *Agent) Receive(port, from, text string, at time.Time) (string, error) {
	pdu, err := fake.BuildDeliverPDU(fake.PDUParams{Sender: from, Time: at}, text)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	id := fmt.Sprintf("*%X", a.nextID)
	a.inbox = append(a.inbox, InboxEntry{
		Port: port, RemoteID: id, Phone: from, Timestamp: at.Format("2006-01-02 15:04:05"), PDU: pdu, Body: text,
	})
	return id, nil
}

// PushInbox sends the whole simulated inbox to tikker and performs the
// deletions tikker asks for.
func (a *Agent) PushInbox(ctx context.Context) error {
	a.mu.Lock()
	msgs := append([]InboxEntry{}, a.inbox...)
	a.mu.Unlock()
	if len(msgs) == 0 {
		return nil
	}
	var resp struct {
		InboxDelete []ref `json:"inbox_delete"`
	}
	if err := a.post(ctx, "/inbox", map[string]any{"messages": msgs}, &resp); err != nil {
		return err
	}
	return a.deleteAndAck(ctx, resp.InboxDelete)
}

func (a *Agent) deleteAndAck(ctx context.Context, refs []ref) error {
	if len(refs) == 0 {
		return nil
	}
	a.mu.Lock()
	kept := a.inbox[:0]
	for _, e := range a.inbox {
		del := false
		for _, r := range refs {
			if r.matches(e) {
				del = true
				break
			}
		}
		if !del {
			kept = append(kept, e)
		}
	}
	a.inbox = kept
	a.mu.Unlock()
	// Every ref is acknowledged, also one whose entry is gone or holds
	// another message now: there is nothing left to delete for it.
	return a.post(ctx, "/inbox/ack", map[string]any{"deleted": refs}, nil)
}

// Poll fetches commands (long-polling up to wait seconds), executes the
// new ones, reports results and performs the requested inbox deletions. It
// returns the commands received.
func (a *Agent) Poll(ctx context.Context, wait int) ([]Command, error) {
	var resp struct {
		Commands    []Command `json:"commands"`
		InboxDelete []ref     `json:"inbox_delete"`
	}
	if err := a.post(ctx, "/commands", map[string]any{"wait": wait}, &resp); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.fetches += len(resp.Commands)
	a.mu.Unlock()
	var errs []error
	for _, c := range resp.Commands {
		a.mu.Lock()
		res, seen := a.done[c.MessageID]
		a.mu.Unlock()
		if !seen {
			ok := true
			res = Result{Status: "accepted"}
			if a.opts.Decide != nil {
				res, ok = a.opts.Decide(c)
			}
			if !ok {
				continue
			}
			a.mu.Lock()
			a.done[c.MessageID] = res
			if res.Status == "accepted" {
				a.sent = append(a.sent, c)
			}
			a.mu.Unlock()
		}
		if a.opts.LoseResult != nil && a.opts.LoseResult(c) {
			continue
		}
		if err := a.post(ctx, "/commands/"+c.MessageID+"/result", res, nil); err != nil {
			errs = append(errs, err)
		}
	}
	if err := a.deleteAndAck(ctx, resp.InboxDelete); err != nil {
		errs = append(errs, err)
	}
	return resp.Commands, errors.Join(errs...)
}

// Run loops like the script: check-in, push inbox, poll, until ctx ends.
// Errors are retried after interval.
func (a *Agent) Run(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		_ = a.Checkin(ctx)
		_ = a.PushInbox(ctx)
		_, _ = a.Poll(ctx, 1)
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// Sent returns the commands the agent executed successfully.
func (a *Agent) Sent() []Command {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Command(nil), a.sent...)
}

// Fetches counts the commands received (redeliveries included).
func (a *Agent) Fetches() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fetches
}

// Inbox returns the simulated router inbox.
func (a *Agent) Inbox() []InboxEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]InboxEntry(nil), a.inbox...)
}
