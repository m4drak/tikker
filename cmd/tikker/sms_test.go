// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const smsToken = "tkr_test_supersecret"

// fakeAPI is a minimal tikker API: it records requests and answers from
// handler functions.
type fakeAPI struct {
	mu   sync.Mutex
	reqs []recorded
	srv  *httptest.Server
}

type recorded struct {
	method, path, query, auth, idemKey string
	body                               string
}

func msgJSON(id, status, body string) string {
	return `{"id":"` + id + `","direction":"out","channel_id":null,"peer":"+48600100200","body":"` + body +
		`","encoding":"gsm7","segments":1,"status":"` + status + `","attempts":0,"next_attempt_at":null,` +
		`"last_error":null,"possible_duplicate":false,"client_ref":null,"tags":[],"expires_at":null,` +
		`"created_by":"token:T1","created_at":"2026-11-24T10:00:00Z","sent_at":null,"received_at":null}`
}

// newFakeAPI starts a server; h receives the already-recorded request.
func newFakeAPI(t *testing.T, h func(w http.ResponseWriter, r *http.Request, rec recorded)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec := recorded{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), idemKey: r.Header.Get("Idempotency-Key"), body: string(b),
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		f.mu.Unlock()
		h(w, r, rec)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func writeBody(w http.ResponseWriter, status int, ct, body string) {
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// smsEnv configures the client environment and returns the temp dir.
func smsEnv(t *testing.T, url string) string {
	t.Helper()
	t.Setenv("TIKKER_URL", url)
	t.Setenv("TIKKER_TOKEN", smsToken)
	old := pollInterval
	oldMin, oldMax := retryBackoffMin, retryBackoffMax
	pollInterval, retryBackoffMin, retryBackoffMax = 5*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { pollInterval, retryBackoffMin, retryBackoffMax = old, oldMin, oldMax })
	return t.TempDir()
}

func runCLI(stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = runIO(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestSMSUsageErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		stdin   string
		wantErr string
	}{
		{name: "no subcommand", args: []string{"sms"}, wantErr: "usage: tikker sms"},
		{name: "unknown subcommand", args: []string{"sms", "burn"}, wantErr: `unknown command "burn"`},
		{name: "unknown flag", args: []string{"sms", "get", "--frob", "x"}, wantErr: "frob"},
		{name: "no token", args: []string{"sms", "get", "ID"}, env: map[string]string{"TIKKER_TOKEN": ""}, wantErr: "no API token"},
		{name: "token flag is not accepted", args: []string{"sms", "get", "ID", "--token", smsToken}, wantErr: "flag provided but not defined: -token"},
		{name: "missing token file", args: []string{"sms", "get", "ID", "--token-file", "/nonexistent/tok"}, wantErr: "--token-file"},
		{name: "bad output", args: []string{"sms", "get", "ID", "--output", "xml"}, wantErr: "invalid --output"},
		{name: "bad timeout", args: []string{"sms", "get", "ID", "--timeout", "0s"}, wantErr: "--timeout"},
		{name: "bad url", args: []string{"sms", "get", "ID", "--url", "localhost:8080"}, wantErr: "invalid URL"},
		{name: "get without id", args: []string{"sms", "get"}, wantErr: "exactly one message ID"},
		{name: "get two ids", args: []string{"sms", "get", "A", "B"}, wantErr: "exactly one message ID"},
		{name: "cancel without id", args: []string{"sms", "cancel"}, wantErr: "exactly one message ID"},
		{name: "send without to", args: []string{"sms", "send", "--body", "x"}, wantErr: "--to is required"},
		{name: "send body and body-file", args: []string{"sms", "send", "--to", "1", "--body", "x", "--body-file", "f"}, wantErr: "mutually exclusive"},
		{name: "send empty body from stdin", args: []string{"sms", "send", "--to", "1"}, stdin: "", wantErr: "body is empty"},
		{name: "send bad tag", args: []string{"sms", "send", "--to", "1", "--body", "x", "--tag", "Bad Tag"}, wantErr: "invalid --tag"},
		{name: "send bad expires", args: []string{"sms", "send", "--to", "1", "--body", "x", "--expires-at", "tomorrow"}, wantErr: "invalid --expires-at"},
		{name: "send negative wait", args: []string{"sms", "send", "--to", "1", "--body", "x", "--wait=-1s"}, wantErr: "--wait"},
		{name: "send stray argument", args: []string{"sms", "send", "--to", "1", "--body", "x", "extra"}, wantErr: `unexpected argument "extra"`},
		{name: "send missing body file", args: []string{"sms", "send", "--to", "1", "--body-file", "/nonexistent/body"}, wantErr: "--body-file"},
		{name: "list bad limit", args: []string{"sms", "list", "--limit", "500"}, wantErr: "between 1 and 200"},
		{name: "list bad status", args: []string{"sms", "list", "--status", "lost"}, wantErr: "invalid --status"},
		{name: "inbox has no status flag", args: []string{"sms", "inbox", "--status", "sent"}, wantErr: "status"},
		{name: "plain http to a remote host", args: []string{"sms", "list", "--url", "http://sms.example.org"}, wantErr: "refusing to send the API token over plain http://"},
		{name: "list stray argument", args: []string{"sms", "list", "x"}, wantErr: "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No test here may reach the network: point at a closed port.
			t.Setenv("TIKKER_URL", "http://127.0.0.1:1")
			t.Setenv("TIKKER_TOKEN", smsToken)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			code, out, errs := runCLI(tt.stdin, tt.args...)
			if code != exitConfig {
				t.Errorf("exit code = %d, want %d (stdout %q stderr %q)", code, exitConfig, out, errs)
			}
			if !strings.Contains(errs, tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", errs, tt.wantErr)
			}
			if strings.Contains(errs+out, smsToken) {
				t.Error("output leaks the token")
			}
		})
	}
}

func TestSMSHelp(t *testing.T) {
	for _, args := range [][]string{{"sms", "--help"}, {"sms", "send", "-h"}, {"sms", "help"}} {
		code, out, _ := runCLI("", args...)
		if code != exitOK || !strings.Contains(out, "usage: tikker sms") {
			t.Errorf("%v: code %d out %q", args, code, out)
		}
	}
}

func TestSMSSend(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		writeBody(w, 202, "application/json", `{"messages":[`+msgJSON("01A", "queued", "hi")+`]}`)
	})
	dir := smsEnv(t, api.srv.URL)
	bodyFile := filepath.Join(dir, "body.txt")
	if err := os.WriteFile(bodyFile, []byte("from file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte("  tkr_file_token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		args     []string
		stdin    string
		wantOut  string
		wantBody map[string]any
		wantIdem string
		wantAuth string
	}{
		{
			name:     "body flag, text output",
			args:     []string{"sms", "send", "--to", "+48600100200", "--body", "hi"},
			wantOut:  "01A\tqueued\t+48600100200\n",
			wantBody: map[string]any{"to": "+48600100200", "body": "hi"},
			wantAuth: "Bearer " + smsToken,
		},
		{
			name:     "stdin body keeps inner newlines, drops the last",
			args:     []string{"sms", "send", "--to", "600100200"},
			stdin:    "line1\nline2\n",
			wantOut:  "01A\tqueued\t+48600100200\n",
			wantBody: map[string]any{"to": "600100200", "body": "line1\nline2"},
			wantAuth: "Bearer " + smsToken,
		},
		{
			name:     "body-file dash reads stdin",
			args:     []string{"sms", "send", "--to", "1", "--body-file", "-"},
			stdin:    "piped",
			wantOut:  "01A\tqueued\t+48600100200\n",
			wantBody: map[string]any{"to": "1", "body": "piped"},
			wantAuth: "Bearer " + smsToken,
		},
		{
			name:     "body-file, all options, flags after values",
			args:     []string{"sms", "send", "--to", "1", "--body-file", bodyFile, "--channel", "CH1", "--tag", "a", "--tag", "b.c", "--client-ref", "r1", "--expires-at", "2026-11-25T09:55:00Z", "--idempotency-key", "k-1"},
			wantOut:  "01A\tqueued\t+48600100200\n",
			wantBody: map[string]any{"to": "1", "body": "from file", "channel_id": "CH1", "tags": []any{"a", "b.c"}, "client_ref": "r1", "expires_at": "2026-11-25T09:55:00Z"},
			wantIdem: "k-1",
			wantAuth: "Bearer " + smsToken,
		},
		{
			name:     "token file wins over env",
			args:     []string{"sms", "send", "--to", "1", "--body", "x", "--token-file", tokFile},
			wantOut:  "01A\tqueued\t+48600100200\n",
			wantBody: map[string]any{"to": "1", "body": "x"},
			wantAuth: "Bearer tkr_file_token",
		},
		{
			name:     "url flag wins over env",
			args:     []string{"sms", "send", "--to", "1", "--body", "x", "--url", api.srv.URL + "/"},
			wantOut:  "01A\tqueued\t+48600100200\n",
			wantBody: map[string]any{"to": "1", "body": "x"},
			wantAuth: "Bearer " + smsToken,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(api.requests())
			code, out, errs := runCLI(tt.stdin, tt.args...)
			if code != exitOK {
				t.Fatalf("exit %d, stderr %q", code, errs)
			}
			if out != tt.wantOut {
				t.Errorf("stdout = %q, want %q", out, tt.wantOut)
			}
			reqs := api.requests()
			if len(reqs) != before+1 {
				t.Fatalf("requests = %d, want %d", len(reqs), before+1)
			}
			r := reqs[before]
			if r.method != http.MethodPost || r.path != "/api/v1/messages" {
				t.Errorf("request = %s %s", r.method, r.path)
			}
			if tt.wantIdem == "" {
				if !autoKeyPattern.MatchString(r.idemKey) {
					t.Errorf("generated Idempotency-Key = %q", r.idemKey)
				}
			} else if r.idemKey != tt.wantIdem {
				t.Errorf("Idempotency-Key = %q, want %q", r.idemKey, tt.wantIdem)
			}
			if r.auth != tt.wantAuth {
				t.Errorf("idem %q auth %q", r.idemKey, r.auth)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(r.body), &got); err != nil {
				t.Fatal(err)
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(tt.wantBody)
			if string(gb) != string(wb) {
				t.Errorf("body = %s, want %s", gb, wb)
			}
			if strings.Contains(out+errs, smsToken) || strings.Contains(out+errs, "tkr_file_token") {
				t.Error("output leaks the token")
			}
		})
	}
}

func TestSMSSendJSONOutput(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		writeBody(w, 202, "application/json", `{"messages":[`+msgJSON("01A", "queued", "hi")+`]}`)
	})
	smsEnv(t, api.srv.URL)
	code, out, errs := runCLI("", "sms", "send", "--to", "1", "--body", "hi", "--output", "json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var resp struct {
		Messages []struct{ ID, Status string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || len(resp.Messages) != 1 || resp.Messages[0].ID != "01A" {
		t.Fatalf("json output = %q (%v)", out, err)
	}
	if !strings.Contains(out, "\n  ") {
		t.Errorf("output is not indented: %q", out)
	}
}

func TestSMSSendWait(t *testing.T) {
	tests := []struct {
		name       string
		statuses   []string // returned by successive GET requests, the last repeats
		wait       string
		output     string
		wantCode   int
		wantOut    string
		wantErrHas string
		wantGets   int
	}{
		{name: "sent after retries", statuses: []string{"queued", "sending", "sent"}, wait: "5s", wantCode: exitOK, wantOut: "01A\tsent\t+48600100200\n", wantGets: 3},
		{name: "failed", statuses: []string{"sending", "failed"}, wait: "5s", wantCode: exitFailure, wantOut: "01A\tfailed\t", wantErrHas: "ended in status failed", wantGets: 2},
		{name: "cancelled", statuses: []string{"cancelled"}, wait: "5s", wantCode: exitFailure, wantOut: "01A\tcancelled\t", wantErrHas: "ended in status cancelled", wantGets: 1},
		{name: "timeout", statuses: []string{"queued"}, wait: "60ms", wantCode: exitFailure, wantOut: "01A\tqueued\t", wantErrHas: "timed out after 60ms waiting for: 01A (queued)"},
		{name: "json output is the final message", statuses: []string{"sent"}, wait: "5s", output: "json", wantCode: exitOK, wantOut: `"status": "sent"`, wantGets: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			gets := 0
			api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
				if r.Method == http.MethodPost {
					writeBody(w, 202, "application/json", `{"messages":[`+msgJSON("01A", "queued", "hi")+`]}`)
					return
				}
				mu.Lock()
				i := min(gets, len(tt.statuses)-1)
				gets++
				mu.Unlock()
				writeBody(w, 200, "application/json", msgJSON("01A", tt.statuses[i], "hi"))
			})
			smsEnv(t, api.srv.URL)
			args := []string{"sms", "send", "--to", "1", "--body", "hi", "--wait", tt.wait}
			if tt.output != "" {
				args = append(args, "--output", tt.output)
			}
			code, out, errs := runCLI("", args...)
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.wantCode, errs)
			}
			if !strings.Contains(out, tt.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", out, tt.wantOut)
			}
			if !strings.Contains(errs, tt.wantErrHas) {
				t.Errorf("stderr = %q, want it to contain %q", errs, tt.wantErrHas)
			}
			mu.Lock()
			defer mu.Unlock()
			if tt.wantGets != 0 && gets != tt.wantGets {
				t.Errorf("GET requests = %d, want %d", gets, tt.wantGets)
			}
		})
	}
}

func TestSMSSendWaitSplitMessages(t *testing.T) {
	var mu sync.Mutex
	polls := map[string]int{}
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
		if r.Method == http.MethodPost {
			writeBody(w, 202, "application/json", `{"messages":[`+msgJSON("P1", "queued", "(1/2) a")+`,`+msgJSON("P2", "queued", "(2/2) b")+`]}`)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/messages/")
		mu.Lock()
		polls[id]++
		n := polls[id]
		mu.Unlock()
		status := "queued"
		if (id == "P1" && n >= 1) || (id == "P2" && n >= 2) {
			status = "sent"
		}
		writeBody(w, 200, "application/json", msgJSON(id, status, "x"))
	})
	smsEnv(t, api.srv.URL)
	code, out, errs := runCLI("", "sms", "send", "--to", "1", "--body", "long", "--wait", "5s")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if out != "P1\tsent\t+48600100200\nP2\tsent\t+48600100200\n" {
		t.Errorf("stdout = %q", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if polls["P1"] != 1 {
		t.Errorf("terminal message P1 polled %d times, want 1", polls["P1"])
	}
}

func TestSMSProblemMapping(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		status   int
		ct       string
		body     string
		wantErrs []string
	}{
		{
			name:   "validation with field errors",
			args:   []string{"sms", "send", "--to", "abc", "--body", "x"},
			status: 422, ct: "application/problem+json",
			body:     `{"type":"urn:tikker:problem:validation","title":"Request validation failed","status":422,"detail":"1 error","errors":[{"field":"/to","code":"invalid_phone_number","detail":"not a phone number"}]}`,
			wantErrs: []string{"API error 422 (validation): Request validation failed: 1 error", "/to: invalid_phone_number: not a phone number"},
		},
		{
			name:   "unauthorized",
			args:   []string{"sms", "get", "X"},
			status: 401, ct: "application/problem+json",
			body:     `{"type":"urn:tikker:problem:unauthorized","title":"Unauthorized","status":401}`,
			wantErrs: []string{"API error 401 (unauthorized): Unauthorized"},
		},
		{
			name:   "cancel conflict",
			args:   []string{"sms", "cancel", "X"},
			status: 409, ct: "application/problem+json",
			body:     `{"type":"urn:tikker:problem:conflict","title":"Conflict","status":409,"detail":"only queued messages can be cancelled"}`,
			wantErrs: []string{"(conflict)", "only queued messages can be cancelled"},
		},
		{
			name:   "non-problem response",
			args:   []string{"sms", "list"},
			status: 502, ct: "text/plain", body: "bad gateway",
			wantErrs: []string{"API error 502: bad gateway"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
				writeBody(w, tt.status, tt.ct, tt.body)
			})
			smsEnv(t, api.srv.URL)
			code, out, errs := runCLI("", tt.args...)
			if code != exitFailure {
				t.Errorf("exit = %d, want %d", code, exitFailure)
			}
			if out != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
			for _, s := range tt.wantErrs {
				if !strings.Contains(errs, s) {
					t.Errorf("stderr = %q, want it to contain %q", errs, s)
				}
			}
			if strings.Contains(errs, smsToken) {
				t.Error("stderr leaks the token")
			}
		})
	}
}

func TestSMSUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	smsEnv(t, url)
	code, _, errs := runCLI("", "sms", "get", "X")
	if code != exitFailure || !strings.Contains(errs, "request failed") || strings.Contains(errs, smsToken) {
		t.Errorf("code %d stderr %q", code, errs)
	}
}

func TestSMSGetAndCancel(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
		status := "sent"
		if r.Method == http.MethodDelete {
			status = "cancelled"
		}
		writeBody(w, 200, "application/json", msgJSON("01A", status, "hello\\nworld"))
	})
	smsEnv(t, api.srv.URL)

	code, out, errs := runCLI("", "sms", "get", "01A")
	if code != exitOK {
		t.Fatalf("get: %d %s", code, errs)
	}
	for _, s := range []string{"id:                 01A", "status:             sent", "body:               hello world", "channel:            -"} {
		if !strings.Contains(out, s) {
			t.Errorf("get output missing %q:\n%s", s, out)
		}
	}

	// Flags may follow the positional ID.
	code, out, _ = runCLI("", "sms", "get", "01A", "--output", "json")
	if code != exitOK || !strings.Contains(out, `"id": "01A"`) {
		t.Errorf("get json: %d %q", code, out)
	}

	code, out, errs = runCLI("", "sms", "cancel", "01A")
	if code != exitOK || !strings.Contains(out, "status:             cancelled") {
		t.Errorf("cancel: %d %q %q", code, out, errs)
	}
	reqs := api.requests()
	last := reqs[len(reqs)-1]
	if last.method != http.MethodDelete || last.path != "/api/v1/messages/01A" {
		t.Errorf("cancel request = %s %s", last.method, last.path)
	}
}

func listHandler(t *testing.T) func(w http.ResponseWriter, r *http.Request, rec recorded) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request, _ recorded) {
		switch r.URL.Query().Get("cursor") {
		case "":
			writeBody(w, 200, "application/json", `{"items":[`+msgJSON("03", "sent", "c")+`,`+msgJSON("02", "queued", "b")+`],"next_cursor":"C2"}`)
		case "C2":
			writeBody(w, 200, "application/json", `{"items":[`+msgJSON("01", "failed", "a")+`],"next_cursor":null}`)
		default:
			writeBody(w, 400, "application/problem+json", `{"type":"urn:tikker:problem:malformed-request","title":"Malformed request","status":400}`)
		}
	}
}

func TestSMSList(t *testing.T) {
	api := newFakeAPI(t, listHandler(t))
	smsEnv(t, api.srv.URL)

	t.Run("single page text with hint", func(t *testing.T) {
		code, out, errs := runCLI("", "sms", "list", "--limit", "2", "--status", "sent", "--status", "queued")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errs)
		}
		for _, s := range []string{"ID", "STATUS", "03", "02", "more messages available", "C2"} {
			if !strings.Contains(out, s) {
				t.Errorf("output missing %q:\n%s", s, out)
			}
		}
		if strings.Contains(out, "01  ") {
			t.Errorf("second page leaked into output:\n%s", out)
		}
		reqs := api.requests()
		if q := reqs[len(reqs)-1].query; q != "limit=2&status=sent&status=queued" {
			t.Errorf("query = %q", q)
		}
	})

	t.Run("all pages text", func(t *testing.T) {
		code, out, errs := runCLI("", "sms", "list", "--all")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errs)
		}
		for _, id := range []string{"03", "02", "01"} {
			if !strings.Contains(out, id+" ") {
				t.Errorf("output missing %s:\n%s", id, out)
			}
		}
		if strings.Contains(out, "more messages") {
			t.Errorf("unexpected hint:\n%s", out)
		}
	})

	t.Run("all pages json", func(t *testing.T) {
		code, out, errs := runCLI("", "sms", "list", "--all", "--output", "json")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errs)
		}
		var p struct {
			Items      []struct{ ID string } `json:"items"`
			NextCursor *string               `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Items) != 3 || p.Items[2].ID != "01" || p.NextCursor != nil {
			t.Errorf("merged page = %+v", p)
		}
	})

	t.Run("single page json is verbatim", func(t *testing.T) {
		code, out, _ := runCLI("", "sms", "list", "--output", "json")
		if code != exitOK || !strings.Contains(out, `"next_cursor": "C2"`) {
			t.Errorf("code %d out %q", code, out)
		}
	})

	t.Run("empty result", func(t *testing.T) {
		empty := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
			writeBody(w, 200, "application/json", `{"items":[],"next_cursor":null}`)
		})
		t.Setenv("TIKKER_URL", empty.srv.URL)
		code, out, _ := runCLI("", "sms", "list", "--all", "--output", "json")
		if code != exitOK || !strings.Contains(out, `"items": []`) {
			t.Errorf("code %d out %q", code, out)
		}
	})
}

func TestSMSInbox(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		writeBody(w, 200, "application/json", `{"items":[{"id":"IN1","direction":"in","channel_id":"C","peer":"+48511222333","body":"Odpowiedź\u001b[31m: tak","encoding":"ucs2","segments":1,"status":"received","attempts":0,"possible_duplicate":false,"tags":[],"created_at":"2026-11-24T10:00:00Z","received_at":"2026-11-24T09:59:50Z"}],"next_cursor":null}`)
	})
	smsEnv(t, api.srv.URL)
	code, out, errs := runCLI("", "sms", "inbox", "--limit", "5")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	reqs := api.requests()
	if q := reqs[0].query; q != "direction=in&limit=5" {
		t.Errorf("query = %q", q)
	}
	if !strings.Contains(out, "IN1") || !strings.Contains(out, "+48511222333") || !strings.Contains(out, "Odpowiedź [31m: tak") {
		t.Errorf("output:\n%s", out)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("escape character in output: %q", out)
	}
}

func TestOneLine(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"a\nb\tc", 10, "a b c"},
		{"  spaced   out ", 20, "spaced out"},
		{"abcdefghij", 8, "abcde..."},
		{"zażółć gęślą jaźń", 10, "zażółć ..."},
		{"x\x1b[2Jy", 10, "x [2Jy"},
	}
	for _, tt := range tests {
		if got := oneLine(tt.in, tt.max); got != tt.want {
			t.Errorf("oneLine(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

// TestSMSEndToEnd runs a real `tikker serve` and drives it with the client
// commands. No device is configured, so sending is rejected with a
// validation problem, which exercises the error mapping against the real API.
func TestSMSEndToEnd(t *testing.T) {
	isolateConfigEnv(t)
	addr := freeAddr(t)
	t.Setenv("TIKKER_LISTEN", addr)
	t.Setenv("TIKKER_ADMIN_PASSWORD", "admin-password-123")
	t.Setenv("TIKKER_LOG_FORMAT", "text")

	ctx, cancel := context.WithCancel(context.Background())
	var serveOut, serveErr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runServe(ctx, "", &serveOut, &serveErr) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("serve did not stop")
		}
	}()

	hc := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/readyz", nil)
		if res, err := hc.Do(req); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case c := <-done:
			t.Fatalf("serve exited with %d: %s %s", c, serveOut.String(), serveErr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	var tokOut, tokErr bytes.Buffer
	if code := run([]string{"token", "create", "--name", "cli", "--scope", "sms:send", "--scope", "sms:read"}, &tokOut, &tokErr); code != exitOK {
		t.Fatalf("token create = %d: %s", code, tokErr.String())
	}
	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, tokOut.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TIKKER_TOKEN", "")
	url := "http://" + addr

	code, out, errs := runCLI("", "sms", "list", "--url", url, "--token-file", tokFile, "--output", "json")
	if code != exitOK || !strings.Contains(out, `"items": []`) {
		t.Fatalf("list: %d %q %q", code, out, errs)
	}
	code, out, errs = runCLI("", "sms", "inbox", "--url", url, "--token-file", tokFile)
	if code != exitOK || !strings.HasPrefix(out, "ID") {
		t.Fatalf("inbox: %d %q %q", code, out, errs)
	}
	code, _, errs = runCLI("", "sms", "get", "--url", url, "--token-file", tokFile, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if code != exitFailure || !strings.Contains(errs, "API error 404 (not-found)") {
		t.Fatalf("get unknown: %d %q", code, errs)
	}
	code, _, errs = runCLI("", "sms", "send", "--url", url, "--token-file", tokFile, "--to", "+48600100200", "--body", "hello")
	if code != exitFailure || !strings.Contains(errs, "(validation)") {
		t.Fatalf("send without channels: %d %q", code, errs)
	}
	t.Setenv("TIKKER_TOKEN", "tkr_bad_token")
	code, _, errs = runCLI("", "sms", "list", "--url", url)
	if code != exitFailure || !strings.Contains(errs, "API error 401 (unauthorized)") || strings.Contains(errs, "tkr_bad_token") {
		t.Fatalf("bad token: %d %q", code, errs)
	}
}
