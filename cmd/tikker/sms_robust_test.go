// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// autoKeyPattern is the shape of a generated key: 32 hex characters, which is
// printable ASCII (0x21-0x7E) and within the 255 character limit.
var autoKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestSMSInsecureHTTP(t *testing.T) {
	t.Setenv("TIKKER_TOKEN", smsToken)
	// Refused without the flag: exit 2 and no request is made.
	code, _, errs := runCLI("", "sms", "list", "--url", "http://203.0.113.7:1")
	if code != exitConfig || !strings.Contains(errs, "--insecure-http") {
		t.Fatalf("refusal: %d %q", code, errs)
	}
	// Allowed with the flag, with a warning (the connection itself fails: the
	// address is a documentation address and the timeout is short).
	code, _, errs = runCLI("", "sms", "list", "--url", "http://203.0.113.7:1", "--insecure-http", "--timeout", "50ms")
	if code != exitFailure || !strings.Contains(errs, "warning: --insecure-http") || strings.Contains(errs, smsToken) {
		t.Fatalf("override: %d %q", code, errs)
	}
	// No warning for loopback.
	api := newFakeAPI(t, listHandler(t))
	code, _, errs = runCLI("", "sms", "list", "--url", api.srv.URL, "--insecure-http")
	if code != exitOK || strings.Contains(errs, "warning") {
		t.Fatalf("loopback: %d %q", code, errs)
	}
}

func TestSMSSendRedirectFailsLoudly(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.Header().Set("Location", "https://sms.example.org/api/v1/messages")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	smsEnv(t, api.srv.URL)
	code, out, errs := runCLI("", "sms", "send", "--to", "1", "--body", "x")
	if code != exitFailure || out != "" {
		t.Fatalf("code %d out %q", code, out)
	}
	for _, s := range []string{"API error 301", "redirect to https://sms.example.org/api/v1/messages"} {
		if !strings.Contains(errs, s) {
			t.Errorf("stderr = %q, want %q", errs, s)
		}
	}
	if strings.Contains(errs, "no messages") {
		t.Errorf("stderr = %q", errs)
	}
	if n := len(api.requests()); n != 1 {
		t.Errorf("requests = %d, the redirect must not be followed", n)
	}
}

func TestSMSSendUnknownOutcomeHint(t *testing.T) {
	tests := []struct {
		name     string
		handler  func(w http.ResponseWriter, r *http.Request)
		extra    []string
		wantHint bool
	}{
		{name: "timeout", handler: func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, extra: []string{"--timeout", "50ms"}, wantHint: true},
		{name: "server error", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, wantHint: true},
		{name: "validation error", handler: func(w http.ResponseWriter, _ *http.Request) {
			writeBody(w, 422, "application/problem+json", `{"type":"urn:tikker:problem:validation","title":"Request validation failed","status":422}`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) { tt.handler(w, r) })
			smsEnv(t, api.srv.URL)
			args := append([]string{"sms", "send", "--to", "1", "--body", "x"}, tt.extra...)
			code, _, errs := runCLI("", args...)
			if code != exitFailure {
				t.Fatalf("exit = %d", code)
			}
			key := api.requests()[0].idemKey
			if !autoKeyPattern.MatchString(key) {
				t.Fatalf("key = %q", key)
			}
			want := "retry safely with --idempotency-key " + key
			if got := strings.Contains(errs, want); got != tt.wantHint {
				t.Errorf("hint present = %v, want %v; stderr %q", got, tt.wantHint, errs)
			}
		})
	}

	t.Run("explicit key is echoed", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) { w.WriteHeader(502) })
		smsEnv(t, api.srv.URL)
		_, _, errs := runCLI("", "sms", "send", "--to", "1", "--body", "x", "--idempotency-key", "mine-1")
		if !strings.Contains(errs, "retry safely with --idempotency-key mine-1") {
			t.Errorf("stderr = %q", errs)
		}
	})
}

func TestSMSGeneratedKeysAreUnique(t *testing.T) {
	a, err1 := newIdempotencyKey()
	b, err2 := newIdempotencyKey()
	if err1 != nil || err2 != nil || a == b || !autoKeyPattern.MatchString(a) {
		t.Errorf("keys %q %q (%v %v)", a, b, err1, err2)
	}
}

func TestSMSWaitRetriesTransientErrors(t *testing.T) {
	tests := []struct {
		name     string
		failures []func(w http.ResponseWriter, r *http.Request)
		wantCode int
		wantErr  string
	}{
		{name: "429 then 503 then ok", failures: []func(http.ResponseWriter, *http.Request){
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "0")
				writeBody(w, 429, "application/problem+json", `{"type":"urn:tikker:problem:rate-limited","title":"Too many requests","status":429}`)
			},
			func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) },
		}, wantCode: exitOK},
		{name: "dropped connection then ok", failures: []func(http.ResponseWriter, *http.Request){
			func(w http.ResponseWriter, _ *http.Request) {
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			},
		}, wantCode: exitOK},
		{name: "401 is not retried", failures: []func(http.ResponseWriter, *http.Request){
			func(w http.ResponseWriter, _ *http.Request) {
				writeBody(w, 401, "application/problem+json", `{"type":"urn:tikker:problem:unauthorized","title":"Unauthorized","status":401}`)
			},
		}, wantCode: exitFailure, wantErr: "API error 401"},
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
				n := gets
				gets++
				mu.Unlock()
				if n < len(tt.failures) {
					tt.failures[n](w, r)
					return
				}
				writeBody(w, 200, "application/json", msgJSON("01A", "sent", "hi"))
			})
			smsEnv(t, api.srv.URL)
			code, out, errs := runCLI("", "sms", "send", "--to", "1", "--body", "hi", "--wait", "5s")
			if code != tt.wantCode || !strings.Contains(errs, tt.wantErr) {
				t.Fatalf("exit %d stderr %q", code, errs)
			}
			if tt.wantCode == exitOK && out != "01A\tsent\t+48600100200\n" {
				t.Errorf("stdout = %q", out)
			}
			mu.Lock()
			defer mu.Unlock()
			if want := len(tt.failures) + 1; tt.wantCode == exitOK && gets != want {
				t.Errorf("GETs = %d, want %d", gets, want)
			}
			if tt.wantCode == exitFailure && gets != 1 {
				t.Errorf("GETs = %d, want 1 (no retry)", gets)
			}
		})
	}
}

func TestSMSWaitHonoursRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
		if r.Method == http.MethodPost {
			writeBody(w, 202, "application/json", `{"messages":[`+msgJSON("01A", "queued", "hi")+`]}`)
			return
		}
		mu.Lock()
		times = append(times, time.Now())
		n := len(times)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		writeBody(w, 200, "application/json", msgJSON("01A", "sent", "hi"))
	})
	smsEnv(t, api.srv.URL)
	if code, _, errs := runCLI("", "sms", "send", "--to", "1", "--body", "hi", "--wait", "10s"); code != exitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 2 || times[1].Sub(times[0]) < 900*time.Millisecond {
		t.Errorf("poll times %v: Retry-After: 1 was not honoured", times)
	}
}

func TestSMSWaitTransientErrorsUntilTimeout(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
		if r.Method == http.MethodPost {
			writeBody(w, 202, "application/json", `{"messages":[`+msgJSON("01A", "queued", "hi")+`]}`)
			return
		}
		w.WriteHeader(503)
	})
	smsEnv(t, api.srv.URL)
	code, _, errs := runCLI("", "sms", "send", "--to", "1", "--body", "hi", "--wait", "150ms")
	if code != exitFailure || !strings.Contains(errs, "timed out after 150ms waiting for: 01A (queued)") {
		t.Errorf("exit %d stderr %q", code, errs)
	}
}

func TestSMSWaitInterrupted(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
		if r.Method == http.MethodPost {
			writeBody(w, 202, "application/json", `{"messages":[`+msgJSON("01A", "queued", "hi")+`]}`)
			return
		}
		writeBody(w, 200, "application/json", msgJSON("01A", "queued", "hi"))
	})
	smsEnv(t, api.srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	var out, errb bytes.Buffer
	code := runSMS(ctx, []string{"send", "--to", "1", "--body", "hi", "--wait", "30s"}, strings.NewReader(""), &out, &errb)
	if code != exitFailure || !strings.Contains(errb.String(), "interrupted while waiting for: 01A (queued)") || strings.Contains(errb.String(), "timed out") {
		t.Errorf("exit %d stderr %q", code, errb.String())
	}
}

func TestSMSListCursorFlag(t *testing.T) {
	api := newFakeAPI(t, listHandler(t))
	smsEnv(t, api.srv.URL)
	code, out, errs := runCLI("", "sms", "list", "--cursor", "C2")
	if code != exitOK || !strings.Contains(out, "01 ") || strings.Contains(out, "more messages") {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
	if q := api.requests()[0].query; q != "cursor=C2" {
		t.Errorf("query = %q", q)
	}
	if code, _, _ = runCLI("", "sms", "inbox", "--cursor", "C2"); code != exitOK {
		t.Errorf("inbox --cursor: exit %d", code)
	}
}

func TestSMSListAllGuards(t *testing.T) {
	t.Run("repeating cursor", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
			next := "A"
			switch r.URL.Query().Get("cursor") {
			case "A":
				next = "B"
			case "B":
				next = "A"
			}
			writeBody(w, 200, "application/json", `{"items":[`+msgJSON("01", "sent", "x")+`],"next_cursor":"`+next+`"}`)
		})
		smsEnv(t, api.srv.URL)
		code, _, errs := runCLI("", "sms", "list", "--all")
		if code != exitFailure || !strings.Contains(errs, "already used") {
			t.Errorf("exit %d stderr %q", code, errs)
		}
		if n := len(api.requests()); n != 3 {
			t.Errorf("requests = %d, want 3", n)
		}
	})
	t.Run("same cursor as --cursor", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
			writeBody(w, 200, "application/json", `{"items":[],"next_cursor":"S"}`)
		})
		smsEnv(t, api.srv.URL)
		code, _, errs := runCLI("", "sms", "list", "--all", "--cursor", "S")
		if code != exitFailure || !strings.Contains(errs, "already used") {
			t.Errorf("exit %d stderr %q", code, errs)
		}
	})
	t.Run("page cap", func(t *testing.T) {
		n := 0
		api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
			n++
			writeBody(w, 200, "application/json", `{"items":[],"next_cursor":"c`+strconv.Itoa(n)+`"}`)
		})
		smsEnv(t, api.srv.URL)
		code, _, errs := runCLI("", "sms", "list", "--all")
		if code != exitFailure || !strings.Contains(errs, "stopped after 1000 pages; continue with --cursor c1000") {
			t.Errorf("exit %d stderr %q", code, errs)
		}
		if got := len(api.requests()); got != maxListPages {
			t.Errorf("requests = %d, want %d", got, maxListPages)
		}
	})
}

func TestSMSSanitizesServerStrings(t *testing.T) {
	const esc = `\u001b[31m`
	evil := `{"id":"I` + esc + `D","direction":"in` + esc + `","channel_id":null,"peer":"+1` + esc + `","body":"b` + esc +
		`","encoding":"u` + esc + `","segments":1,"status":"st` + esc + `","attempts":0,"last_error":"er` + esc +
		`r\nforged","possible_duplicate":false,"tags":["t` + esc + `"],"created_at":"c` + esc + `","sent_at":null}`
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
		switch {
		case r.Method == http.MethodPost:
			writeBody(w, 202, "application/json", `{"messages":[`+evil+`]}`)
		case r.URL.Path == "/api/v1/messages":
			writeBody(w, 200, "application/json", `{"items":[`+evil+`],"next_cursor":"cur`+esc+`"}`)
		default:
			writeBody(w, 200, "application/json", evil)
		}
	})
	smsEnv(t, api.srv.URL)
	for _, args := range [][]string{
		{"sms", "send", "--to", "1", "--body", "x"},
		{"sms", "get", "X"},
		{"sms", "cancel", "X"},
		{"sms", "list"},
		{"sms", "inbox"},
	} {
		code, out, errs := runCLI("", args...)
		if code != exitOK {
			t.Fatalf("%v: exit %d %s", args, code, errs)
		}
		if strings.ContainsRune(out+errs, 0x1b) {
			t.Errorf("%v: escape character in output %q", args, out)
		}
		if strings.Contains(out, "\nforged") {
			t.Errorf("%v: injected line in output %q", args, out)
		}
	}

	// A failed message with a hostile last_error, and a hostile non-problem error body.
	fail := strings.Replace(evil, `"st`+esc+`"`, `"failed"`, 1)
	api2 := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
		if r.Method == http.MethodPost {
			writeBody(w, 202, "application/json", `{"messages":[`+fail+`]}`)
			return
		}
		w.WriteHeader(500)
	})
	t.Setenv("TIKKER_URL", api2.srv.URL)
	_, _, errs := runCLI("", "sms", "send", "--to", "1", "--body", "x", "--wait", "1s")
	if strings.ContainsRune(errs, 0x1b) || strings.Contains(errs, "\nforged") {
		t.Errorf("stderr = %q", errs)
	}
	api3 := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		writeBody(w, 500, "text/plain", "boom\x1b[31m\nforged")
	})
	t.Setenv("TIKKER_URL", api3.srv.URL)
	_, _, errs = runCLI("", "sms", "list")
	if strings.ContainsRune(errs, 0x1b) || strings.Contains(errs, "\nforged") {
		t.Errorf("stderr = %q", errs)
	}
}
