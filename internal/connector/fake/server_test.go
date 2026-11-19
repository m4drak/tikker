// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	tUser = "tikker"
	tPass = "tikker-test"
)

type rig struct {
	t   *testing.T
	s   *Server
	url string
	c   *http.Client
}

func newRig(t *testing.T, opts Options) *rig {
	t.Helper()
	s := New(opts)
	url := s.Start()
	t.Cleanup(s.Close)
	return &rig{t: t, s: s, url: url, c: &http.Client{Timeout: 5 * time.Second}}
}

type resp struct {
	status int
	body   string
	header http.Header
}

func (r *rig) do(method, path string, body any, mut ...func(*http.Request)) resp {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		var b []byte
		switch v := body.(type) {
		case string:
			b = []byte(v)
		default:
			b, _ = json.Marshal(v)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, r.url+path, rd)
	if err != nil {
		r.t.Fatal(err)
	}
	req.SetBasicAuth(tUser, tPass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, m := range mut {
		m(req)
	}
	res, err := r.c.Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, string(b), res.Header}
}

func (r *rig) send(phone, msg string) resp {
	return r.do("POST", "/rest/tool/sms/send", map[string]string{"port": "lte1", "phone-number": phone, "message": msg})
}

func wantStatus(t *testing.T, got resp, status int, detail string) {
	t.Helper()
	if got.status != status {
		t.Fatalf("status = %d (%s), want %d", got.status, got.body, status)
	}
	if detail != "" {
		var e errBody
		if err := json.Unmarshal([]byte(got.body), &e); err != nil {
			t.Fatalf("body %q is not an error document: %v", got.body, err)
		}
		if e.Error != status || e.Detail != detail || e.Message == "" {
			t.Fatalf("error body = %+v, want detail %q", e, detail)
		}
	}
}

func TestAuth(t *testing.T) {
	r := newRig(t, Options{})
	noAuth := func(q *http.Request) { q.Header.Del("Authorization") }
	wrong := func(q *http.Request) { q.SetBasicAuth(tUser, "nope") }
	for _, m := range []func(*http.Request){noAuth, wrong} {
		got := r.do("GET", "/rest/system/resource", nil, m)
		wantStatus(t, got, 401, "")
		if got.header.Get("WWW-Authenticate") == "" {
			t.Error("missing WWW-Authenticate")
		}
	}
	if r.s.AuthFailures() != 2 || r.s.Requests(EndpointAny) != 0 {
		t.Errorf("auth failures %d, requests %d", r.s.AuthFailures(), r.s.Requests(EndpointAny))
	}

	r.s.SetPolicies(Policies{RESTAPI: false, API: true})
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 401, "")
	r.s.SetPolicies(Policies{RESTAPI: true, API: false})
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 500, "not allowed (9)")
	r.s.SetPolicies(Policies{RESTAPI: true, API: true})
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 200, "")

	r.s.SetCredentials("other", "pw")
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 401, "")
}

func TestSendOK(t *testing.T) {
	r := newRig(t, Options{})
	got := r.send("+48600700800", "Kod: 1234")
	wantStatus(t, got, 200, "")
	if strings.TrimSpace(got.body) != "[]" {
		t.Errorf("body = %q", got.body)
	}
	sent := r.s.Sent()
	if len(sent) != 1 || sent[0].PhoneNumber != "+48600700800" || sent[0].Message != "Kod: 1234" ||
		sent[0].Port != "lte1" || sent[0].Seq != 1 || sent[0].Septets != 9 || sent[0].Ambiguous {
		t.Fatalf("sent = %+v", sent)
	}
	if !sent[0].CompletedAt.After(sent[0].ReceivedAt) && !sent[0].CompletedAt.Equal(sent[0].ReceivedAt) {
		t.Error("timestamps out of order")
	}
	if r.s.Requests(EndpointSend) != 1 {
		t.Error("send not counted")
	}
	// optional "type" is recorded; numeric JSON values are tolerated
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{"port":"lte1","phone-number":"123","message":"x","type":"class-0"}`), 200, "")
	if r.s.Sent()[1].Type != "class-0" {
		t.Error("type not recorded")
	}
	if !r.s.WaitSent(2, time.Second) || r.s.WaitSent(3, 10*time.Millisecond) {
		t.Error("WaitSent")
	}
}

func TestSendValidation(t *testing.T) {
	r := newRig(t, Options{Ports: []string{"lte1", "lte2"}})
	// 415 without Content-Type
	got := r.do("POST", "/rest/tool/sms/send", `{"port":"lte1","phone-number":"1","message":"x"}`,
		func(q *http.Request) { q.Header.Del("Content-Type") })
	wantStatus(t, got, 415, "")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{}`, func(q *http.Request) { q.Header.Set("Content-Type", "text/plain") }), 415, "")
	// GET (even with parameters) -> 500, nothing sent
	wantStatus(t, r.do("GET", "/rest/tool/sms/send?port=lte1&phone-number=1&message=x", nil), 500, "")
	wantStatus(t, r.do("PUT", "/rest/tool/sms/send", `{}`), 405, "")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{not json`), 400, "Failed to parse json")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{"phone-number":"1","message":"x"}`), 400, "missing value(s) of argument(s) port")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{"port":"lte1","message":"x"}`), 400, "missing value(s) of argument(s) phone-number")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{"port":"lte1","phone-number":"1"}`), 400, "missing value(s) of argument(s) message")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{"port":"lte9","phone-number":"1","message":"x"}`), 400, "input does not match any value of port")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{"port":"lte1","phone-number":"+48 600","message":"x"}`), 400, "invalid phone-number")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{"port":"lte1","phone-number":"","message":"x"}`), 400, "invalid phone-number")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{"port":"lte1","phone-number":"1","message":{"a":1}}`), 400, "missing value(s) of argument(s) message")
	wantStatus(t, r.do("POST", "/rest/tool/sms/send", `{"port":"lte1","phone-number":"*100#","message":"x"}`), 200, "")
	if len(r.s.Sent()) != 1 {
		t.Errorf("only the valid send should be recorded, got %d", len(r.s.Sent()))
	}
}

func TestSendEncodingLimits(t *testing.T) {
	r := newRig(t, Options{})
	wantStatus(t, r.send("1", strings.Repeat("a", 160)), 200, "")
	wantStatus(t, r.send("1", strings.Repeat("a", 161)), 400, "failure: Body too long")
	// extension characters cost two septets: 80 x '€' = 160 ok, 81 too long
	wantStatus(t, r.send("1", strings.Repeat("€", 80)), 200, "")
	wantStatus(t, r.send("1", strings.Repeat("€", 81)), 400, "failure: Body too long")
	wantStatus(t, r.send("1", "zażółć"), 400, "failure: message contains characters outside the GSM 7-bit alphabet")
	r.s.SetLenient(true)
	wantStatus(t, r.send("1", strings.Repeat("ż", 500)), 200, "")
	last := r.s.Sent()[len(r.s.Sent())-1]
	if last.Septets != 0 {
		t.Errorf("septets for non-GSM text = %d", last.Septets)
	}
}

func TestSendNotRegistered(t *testing.T) {
	r := newRig(t, Options{})
	r.s.SetLTE("lte1", func(l *LTEState) { l.Registration = "searching"; l.Signal = nil })
	wantStatus(t, r.send("1", "x"), 500, "failure: not registered to network")
	r.s.SetLTE("lte1", func(l *LTEState) { l.Registration = "roaming" })
	wantStatus(t, r.send("1", "x"), 200, "")
	if r.s.SetLTE("nope", func(*LTEState) {}) {
		t.Error("SetLTE accepted unknown port")
	}
	if _, ok := r.s.LTE("nope"); ok {
		t.Error("LTE returned unknown port")
	}
}

func TestSendSerializedPerPort(t *testing.T) {
	r := newRig(t, Options{Ports: []string{"lte1", "lte2"}, SendDelay: 40 * time.Millisecond})
	var wg sync.WaitGroup
	post := func(port string) {
		defer wg.Done()
		r.do("POST", "/rest/tool/sms/send", map[string]string{"port": port, "phone-number": "1", "message": "x"})
	}
	start := time.Now()
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go post("lte1")
	}
	wg.Add(1)
	go post("lte2")
	wg.Wait()
	if time.Since(start) < 110*time.Millisecond {
		t.Errorf("three sends on one port finished in %v, expected serialization", time.Since(start))
	}
	var lte1 []SentMessage
	for _, m := range r.s.Sent() {
		if m.Port == "lte1" {
			lte1 = append(lte1, m)
		}
	}
	if len(lte1) != 3 {
		t.Fatalf("lte1 sends = %d", len(lte1))
	}
	for i := 1; i < len(lte1); i++ {
		if lte1[i].StartedAt.Before(lte1[i-1].CompletedAt) {
			t.Errorf("send %d started %v before previous completed %v", i, lte1[i].StartedAt, lte1[i-1].CompletedAt)
		}
	}
	for _, m := range lte1 {
		if d := m.CompletedAt.Sub(m.StartedAt); d < 35*time.Millisecond {
			t.Errorf("send held the port only %v", d)
		}
	}
}

func TestSendDelayBeyondSessionTimeout(t *testing.T) {
	r := newRig(t, Options{SendDelay: time.Second, SessionTimeout: 50 * time.Millisecond})
	got := r.send("1", "x")
	wantStatus(t, got, 400, "Session closed")
	sent := r.s.Sent()
	if len(sent) != 1 || !sent[0].Ambiguous {
		t.Fatalf("expected an ambiguous recorded send, got %+v", sent)
	}
}

func TestFaults(t *testing.T) {
	r := newRig(t, Options{SessionTimeout: 80 * time.Millisecond})

	// n consecutive failures, then success.
	r.s.FailNext(EndpointSend, HTTPError(503, "busy"), 2)
	wantStatus(t, r.send("1", "x"), 503, "busy")
	wantStatus(t, r.send("1", "x"), 503, "busy")
	wantStatus(t, r.send("1", "x"), 200, "")
	if r.s.Requests(EndpointSend) != 3 || len(r.s.Sent()) != 1 {
		t.Errorf("requests %d sent %d", r.s.Requests(EndpointSend), len(r.s.Sent()))
	}

	// shorthand constructors
	r.s.FailNext(EndpointResource, Unauthorized(), 1)
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 401, "")
	r.s.FailNext(EndpointResource, NotAllowed(), 1)
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 500, "not allowed (9)")
	r.s.FailNext(EndpointResource, SessionClosed(), 1)
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 400, "Session closed")
	r.s.FailNext(EndpointResource, Fault{Kind: FaultHTTP}, 1) // defaults to 500
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 500, "")
	r.s.FailNext(EndpointResource, Fault{Kind: FaultHTTP, Status: 400, Message: "Custom", Detail: "d"}, 1)
	got := r.do("GET", "/rest/system/resource", nil)
	wantStatus(t, got, 400, "d")
	if !strings.Contains(got.body, `"Custom"`) {
		t.Errorf("message not custom: %s", got.body)
	}

	// EndpointAny applies to every endpoint; specific faults win; forever (-1) until cleared
	r.s.FailNext(EndpointAny, HTTPError(502, "gw"), -1)
	r.s.FailNext(EndpointLTE, HTTPError(500, "lte"), 1)
	wantStatus(t, r.do("GET", "/rest/interface/lte", nil), 500, "lte")
	wantStatus(t, r.do("GET", "/rest/interface/lte", nil), 502, "gw")
	wantStatus(t, r.do("GET", "/rest/tool/sms", nil), 502, "gw")
	r.s.ClearFaults()
	wantStatus(t, r.do("GET", "/rest/tool/sms", nil), 200, "")
	r.s.FailNext(EndpointSend, HTTPError(500, "x"), 0) // no-op
	wantStatus(t, r.send("1", "x"), 200, "")

	// drop: the client sees a transport error
	r.s.FailNext(EndpointResource, DropConnection(), 1)
	if _, err := r.c.Get(r.url + "/rest/system/resource"); err == nil {
		// Get without credentials would 401 before the fault; use do-like request
		t.Log("unauthenticated GET got an answer, expected")
	}
	req, _ := http.NewRequest("GET", r.url+"/rest/system/resource", nil)
	req.SetBasicAuth(tUser, tPass)
	if res, err := r.c.Do(req); err == nil {
		_ = res.Body.Close()
		t.Error("expected dropped connection")
	}
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 200, "")

	// hang: ends with Session closed after the session timeout
	r.s.FailNext(EndpointResource, Hang(), 1)
	start := time.Now()
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 400, "Session closed")
	if time.Since(start) < 70*time.Millisecond {
		t.Error("hang returned too early")
	}

	// delayed fault
	r.s.FailNext(EndpointResource, HTTPError(500, "slow").After(60*time.Millisecond), 1)
	start = time.Now()
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 500, "slow")
	if time.Since(start) < 55*time.Millisecond {
		t.Error("fault not delayed")
	}
}

func TestAmbiguousSend(t *testing.T) {
	r := newRig(t, Options{SessionTimeout: 60 * time.Millisecond})

	// accepted, then the connection is cut
	r.s.FailNext(EndpointSend, DropConnection().AfterAccept(), 1)
	req, _ := http.NewRequest("POST", r.url+"/rest/tool/sms/send", strings.NewReader(`{"port":"lte1","phone-number":"1","message":"drop"}`))
	req.SetBasicAuth(tUser, tPass)
	req.Header.Set("Content-Type", "application/json")
	if res, err := r.c.Do(req); err == nil {
		_ = res.Body.Close()
		t.Error("expected transport error")
	}
	// accepted, then 500
	r.s.FailNext(EndpointSend, HTTPError(500, "oops").AfterAccept(), 1)
	wantStatus(t, r.send("1", "five-hundred"), 500, "oops")
	// accepted, then the client times out while the router "hangs"
	r.s.FailNext(EndpointSend, Hang().AfterAccept(), 1)
	short := &http.Client{Timeout: 20 * time.Millisecond}
	req2, _ := http.NewRequest("POST", r.url+"/rest/tool/sms/send", strings.NewReader(`{"port":"lte1","phone-number":"1","message":"hang"}`))
	req2.SetBasicAuth(tUser, tPass)
	req2.Header.Set("Content-Type", "application/json")
	if res, err := short.Do(req2); err == nil {
		_ = res.Body.Close()
		t.Error("expected client timeout")
	}
	if !r.s.WaitSent(3, time.Second) {
		t.Fatalf("sent = %+v", r.s.Sent())
	}
	for _, m := range r.s.Sent() {
		if !m.Ambiguous {
			t.Errorf("%q should be ambiguous", m.Message)
		}
	}
	// normal send after the faults is not ambiguous
	wantStatus(t, r.send("1", "fine"), 200, "")
	if s := r.s.Sent(); s[len(s)-1].Ambiguous {
		t.Error("clean send marked ambiguous")
	}
}

func TestInboxLifecycle(t *testing.T) {
	r := newRig(t, Options{Ports: []string{"lte1", "lte2"}})
	wantStatus(t, r.do("GET", "/rest/tool/sms/inbox", nil), 200, "")
	if got := r.do("GET", "/rest/tool/sms/inbox", nil); strings.TrimSpace(got.body) != "[]" {
		t.Errorf("empty inbox = %q", got.body)
	}
	when := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ids, err := r.s.InjectSMS(IncomingSMS{From: "+48511222333", Text: "Cześć", Time: when})
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	if _, err := r.s.InjectSMS(IncomingSMS{Port: "lte2", From: "BANK", Text: "Kod 1", Time: when, Class0: true}); err != nil {
		t.Fatal(err)
	}
	got := r.do("GET", "/rest/tool/sms/inbox", nil)
	var list []map[string]string
	if err := json.Unmarshal([]byte(got.body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("inbox = %v", list)
	}
	first := list[0]
	if first[".id"] != "*1" || first["phone"] != "+48511222333" || first["timestamp"] != "2026-11-19 12:00:00" || first["type"] != "" {
		t.Errorf("first = %v", first)
	}
	if _, hasPort := first["port"]; hasPort {
		t.Error("port field present without InboxPortField")
	}
	// UCS-2 text shows up mangled in "message" but intact in the PDU.
	if first["message"] == "Cześć" || first["message"] == "" {
		t.Errorf("UCS-2 message not mangled: %q", first["message"])
	}
	if d := decodeDeliver(t, first["pdu"]); d.text != "Cześć" || d.sender != "+48511222333" {
		t.Errorf("pdu decodes to %+v", d)
	}
	if list[1]["type"] != "class-0" || list[1]["message"] != "Kod 1" || list[1][".id"] != "*2" {
		t.Errorf("second = %v", list[1])
	}

	// DELETE one; second delete -> 404; unknown id -> 404
	if got := r.do("DELETE", "/rest/tool/sms/inbox/*1", nil); got.status != 204 {
		t.Errorf("delete = %d", got.status)
	}
	wantStatus(t, r.do("DELETE", "/rest/tool/sms/inbox/*1", nil), 404, "no such item (4)")
	wantStatus(t, r.do("GET", "/rest/tool/sms/inbox/*2", nil), 405, "")

	// POST remove: unknown id fails atomically, then comma list works
	wantStatus(t, r.do("POST", "/rest/tool/sms/inbox/remove", map[string]string{".id": "*2,*99"}), 400, "no such item (4)")
	if len(r.s.Inbox()) != 1 {
		t.Fatal("failed remove must not delete anything")
	}
	wantStatus(t, r.do("POST", "/rest/tool/sms/inbox/remove", `{}`), 400, "missing value(s) of argument(s) numbers")
	wantStatus(t, r.do("POST", "/rest/tool/sms/inbox/remove", `{`), 400, "Failed to parse json")
	wantStatus(t, r.do("POST", "/rest/tool/sms/inbox/remove", `{".id":"*2"}`, func(q *http.Request) { q.Header.Del("Content-Type") }), 415, "")
	wantStatus(t, r.do("GET", "/rest/tool/sms/inbox/remove", nil), 405, "")
	wantStatus(t, r.do("POST", "/rest/tool/sms/inbox/remove", map[string]string{".id": "*2"}), 200, "")
	if len(r.s.Inbox()) != 0 {
		t.Error("inbox should be empty")
	}
	if r.s.Requests(EndpointInbox) != 3 || r.s.Requests(EndpointInboxDelete) != 3 || r.s.Requests(EndpointInboxRemove) != 6 {
		t.Errorf("counters = %v", r.s.AllRequests())
	}
}

func TestInboxTimestampFormatsAndPort(t *testing.T) {
	r := newRig(t, Options{FormatTimestamp: TimestampLegacy, InboxPortField: true, Ports: []string{"lte1", "lte2"}})
	when := time.Date(2026, 11, 9, 7, 6, 5, 0, time.UTC)
	if _, err := r.s.InjectSMS(IncomingSMS{Port: "lte2", From: "+48511222333", Text: "a", Time: when}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.InjectRaw(RawInbox{Phone: "+1", TimestampText: "garbage", Message: "m", PDU: "zz"}); err != nil {
		t.Fatal(err)
	}
	var list []map[string]string
	_ = json.Unmarshal([]byte(r.do("GET", "/rest/tool/sms/inbox", nil).body), &list)
	if list[0]["timestamp"] != "nov/09/2026 07:06:05" || list[0]["port"] != "lte2" {
		t.Errorf("entry = %v", list[0])
	}
	if list[1]["timestamp"] != "garbage" || list[1]["pdu"] != "zz" || list[1]["port"] != "lte1" {
		t.Errorf("raw entry = %v", list[1])
	}
	if _, err := r.s.InjectRaw(RawInbox{Port: "nope"}); err == nil {
		t.Error("unknown port accepted")
	}
	if _, err := r.s.InjectSMS(IncomingSMS{Port: "nope", From: "1", Text: "x"}); err == nil {
		t.Error("unknown port accepted")
	}
	if _, err := r.s.InjectSMS(IncomingSMS{From: "", Text: "x"}); err == nil {
		t.Error("empty sender accepted")
	}
	if _, err := r.s.InjectSMS(IncomingSMS{From: "+1", Text: "x", Concat: &Concat{Ref: 1, Total: 1, Seq: 2}}); err == nil {
		t.Error("bad concat accepted")
	}
	if _, err := r.s.InjectSMS(IncomingSMS{From: "+1", Text: "zaż", Encoding: EncodingGSM7}); err == nil {
		t.Error("non-GSM text forced to GSM-7 accepted")
	}
}

func TestInjectConcatenated(t *testing.T) {
	r := newRig(t, Options{})
	long := strings.Repeat("Zażółć gęślą jaźń. ", 12) // UCS-2, > 70 units
	ids, err := r.s.InjectSMS(IncomingSMS{From: "+48511222333", Text: long})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) < 2 {
		t.Fatalf("ids = %v", ids)
	}
	var text strings.Builder
	var refs = map[uint16]bool{}
	for i, e := range r.s.Inbox() {
		d := decodeDeliver(t, e.PDU)
		if d.concat == nil || d.concat.Seq != i+1 || d.concat.Total != len(ids) {
			t.Fatalf("part %d concat = %+v", i, d.concat)
		}
		refs[d.concat.Ref] = true
		text.WriteString(d.text)
	}
	if text.String() != long || len(refs) != 1 {
		t.Errorf("reassembly failed, refs %v", refs)
	}
	// a second long message gets a different reference number
	if _, err := r.s.InjectSMS(IncomingSMS{From: "+48511222333", Text: long}); err != nil {
		t.Fatal(err)
	}
	last := r.s.Inbox()[len(r.s.Inbox())-1]
	if d := decodeDeliver(t, last.PDU); refs[d.concat.Ref] {
		t.Error("concat reference reused")
	}
	// inject exactly one chosen part (e.g. to simulate a missing part)
	before := len(r.s.Inbox())
	if _, err := r.s.InjectSMS(IncomingSMS{From: "+48511222333", Text: "part two", Concat: &Concat{Ref: 9, Total: 3, Seq: 2}}); err != nil {
		t.Fatal(err)
	}
	if len(r.s.Inbox()) != before+1 {
		t.Error("explicit part should add exactly one entry")
	}
}

func TestInboxFull(t *testing.T) {
	r := newRig(t, Options{InboxLimit: 2})
	for i := 0; i < 2; i++ {
		if _, err := r.s.InjectSMS(IncomingSMS{From: "+48511222333", Text: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.s.InjectSMS(IncomingSMS{From: "+48511222333", Text: "lost"}); err != ErrInboxFull {
		t.Fatalf("err = %v, want ErrInboxFull", err)
	}
	if len(r.s.Inbox()) != 2 {
		t.Error("full inbox must drop the new message")
	}
	// auto-erase drops the oldest instead
	r.s.SetSettings(func(s *Settings) { s.AutoErase = true })
	if _, err := r.s.InjectSMS(IncomingSMS{From: "+48511222333", Text: "newest"}); err != nil {
		t.Fatal(err)
	}
	in := r.s.Inbox()
	if len(in) != 2 || in[0].ID != "*2" || in[1].Message != "newest" {
		t.Errorf("inbox = %+v", in)
	}
	// a long message larger than the whole inbox keeps only what fits
	if _, err := r.s.InjectSMS(IncomingSMS{From: "+1", Text: strings.Repeat("ż", 300)}); err != nil {
		t.Fatal(err)
	}
	if len(r.s.Inbox()) > 2 {
		t.Errorf("inbox over limit: %d", len(r.s.Inbox()))
	}
	r.s.SetInboxLimit(0)
	for i := 0; i < 5; i++ {
		_, _ = r.s.InjectSMS(IncomingSMS{From: "+1", Text: "x"})
	}
	if len(r.s.Inbox()) < 5 {
		t.Error("limit 0 means unlimited")
	}
}

func TestSettingsLTEResource(t *testing.T) {
	r := newRig(t, Options{Ports: []string{"lte1", "lte2"}, Version: "7.18 (stable)", Model: "Chateau"})
	var set map[string]string
	got := r.do("GET", "/rest/tool/sms", nil)
	wantStatus(t, got, 200, "")
	_ = json.Unmarshal([]byte(got.body), &set)
	if set["port"] != "lte1" || set["receive-enabled"] != "true" || set["auto-erase"] != "false" {
		t.Errorf("settings = %v", set)
	}
	r.s.SetSettings(func(s *Settings) { s.AutoErase = true; s.Port = "lte2" })
	_ = json.Unmarshal([]byte(r.do("GET", "/rest/tool/sms", nil).body), &set)
	if set["auto-erase"] != "true" || set["port"] != "lte2" || !r.s.Settings().AutoErase {
		t.Errorf("settings = %v", set)
	}

	var lte []map[string]string
	_ = json.Unmarshal([]byte(r.do("GET", "/rest/interface/lte", nil).body), &lte)
	if len(lte) != 2 || lte[0]["name"] != "lte1" || lte[1][".id"] != "*2" || lte[0]["running"] != "true" {
		t.Errorf("lte = %v", lte)
	}
	r.s.SetLTE("lte2", func(l *LTEState) { l.Running = false })
	_ = json.Unmarshal([]byte(r.do("GET", "/rest/interface/lte", nil).body), &lte)
	if lte[1]["running"] != "false" {
		t.Errorf("lte2 = %v", lte[1])
	}

	var res map[string]string
	_ = json.Unmarshal([]byte(r.do("GET", "/rest/system/resource", nil).body), &res)
	if res["version"] != "7.18 (stable)" || res["board-name"] != "Chateau" {
		t.Errorf("resource = %v", res)
	}
	var rb map[string]string
	_ = json.Unmarshal([]byte(r.do("GET", "/rest/system/routerboard", nil).body), &rb)
	if rb["model"] != "Chateau" || rb["routerboard"] != "true" {
		t.Errorf("routerboard = %v", rb)
	}
	for _, p := range []string{"/rest/tool/sms", "/rest/interface/lte", "/rest/system/resource", "/rest/system/routerboard", "/rest/tool/sms/inbox"} {
		wantStatus(t, r.do("POST", p, `{}`), 405, "")
	}
	wantStatus(t, r.do("GET", "/rest/interface/lte/monitor", nil), 405, "")
}

func TestLTEMonitor(t *testing.T) {
	r := newRig(t, Options{Ports: []string{"lte1", "lte2"}, SessionTimeout: 60 * time.Millisecond})
	mon := func(body any) resp { return r.do("POST", "/rest/interface/lte/monitor", body) }
	var out []map[string]string
	got := mon(map[string]any{"numbers": "lte1", "once": true})
	wantStatus(t, got, 200, "")
	if err := json.Unmarshal([]byte(got.body), &out); err != nil || len(out) != 1 {
		t.Fatalf("body %s: %v", got.body, err)
	}
	m := out[0]
	if m["registration-status"] != "home" || m["current-operator"] != "Fake Mobile PL" || m["rssi"] != "-67" ||
		m["rsrp"] != "-95" || m["rsrq"] != "-10" || m["sinr"] != "12" || m["status"] != "connected" || m["access-technology"] != "LTE" {
		t.Errorf("monitor = %v", m)
	}
	// numbers by .id, once as string
	wantStatus(t, mon(map[string]any{"numbers": "*2", "once": "true"}), 200, "")
	// not registered: no signal figures
	r.s.SetLTE("lte1", func(l *LTEState) { l.Registration = "searching"; l.Signal = nil; l.Operator = "" })
	out = nil
	_ = json.Unmarshal([]byte(mon(map[string]any{"numbers": "lte1", "once": true}).body), &out)
	if _, has := out[0]["rssi"]; has || out[0]["registration-status"] != "searching" || out[0]["status"] != "searching" {
		t.Errorf("unregistered monitor = %v", out[0])
	}
	wantStatus(t, mon(map[string]any{"numbers": "lte7", "once": true}), 400, "no such item (4)")
	wantStatus(t, mon(map[string]any{"once": true}), 400, "missing value(s) of argument(s) numbers")
	wantStatus(t, mon(`{`), 400, "Failed to parse json")
	// without "once" the call streams until the session limit
	wantStatus(t, mon(map[string]any{"numbers": "lte1"}), 400, "Session closed")
	st, _ := r.s.LTE("lte1")
	if st.Signal != nil || st.Registered() {
		t.Errorf("LTE state = %+v", st)
	}
}

func TestUnknownPath(t *testing.T) {
	r := newRig(t, Options{})
	wantStatus(t, r.do("GET", "/rest/ip/address", nil), 400, "no such command or directory (ip/address)")
	wantStatus(t, r.do("GET", "/other", nil), 400, "")
	if r.s.Requests(EndpointUnknown) != 2 {
		t.Errorf("unknown = %d", r.s.Requests(EndpointUnknown))
	}
}

func TestReset(t *testing.T) {
	r := newRig(t, Options{})
	r.send("1", "x")
	_, _ = r.s.InjectSMS(IncomingSMS{From: "+1", Text: "x"})
	r.s.FailNext(EndpointSend, HTTPError(500, ""), -1)
	r.s.SetLTE("lte1", func(l *LTEState) { l.Registration = "denied" })
	r.s.SetCredentials("a", "b")
	r.s.SetSendDelay(time.Hour)
	r.s.Reset()
	if len(r.s.Sent()) != 0 || len(r.s.Inbox()) != 0 || r.s.Requests(EndpointAny) != 0 {
		t.Error("state survived Reset")
	}
	wantStatus(t, r.send("1", "x"), 200, "")
	ids, _ := r.s.InjectSMS(IncomingSMS{From: "+1", Text: "x"})
	if ids[0] != "*1" {
		t.Errorf("ids not reset: %v", ids)
	}
}

func TestTLS(t *testing.T) {
	s := New(Options{})
	url := s.StartTLS()
	defer s.Close()
	if !strings.HasPrefix(url, "https://") {
		t.Fatal(url)
	}
	fp := s.Fingerprint()
	if len(fp) != 32*3-1 || fp != strings.ToUpper(fp) {
		t.Errorf("fingerprint = %q", fp)
	}
	req, _ := http.NewRequest("GET", url+"/rest/system/resource", nil)
	req.SetBasicAuth(tUser, tPass)
	res, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Error(res.StatusCode)
	}
}

func TestSelfSignedCert(t *testing.T) {
	cert, fp, err := SelfSignedCert("fake.example", "10.1.2.3", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) != 1 || len(fp) != 95 {
		t.Errorf("cert/fp: %d %q", len(cert.Certificate), fp)
	}
	if FormatFingerprint([]byte{0xAB, 0x01}) != "AB:01" {
		t.Error("FormatFingerprint")
	}
}

func TestCloseReleasesWaiters(t *testing.T) {
	s := New(Options{SendDelay: time.Hour})
	url := s.Start()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(context.Background(), "POST", url+"/rest/tool/sms/send",
			strings.NewReader(`{"port":"lte1","phone-number":"1","message":"x"}`))
		req.SetBasicAuth(tUser, tPass)
		req.Header.Set("Content-Type", "application/json")
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	s.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not release the waiting send")
	}
	s.Close() // idempotent
}

// TestConcurrentUse exercises every control method while requests are in
// flight; it is meaningful under -race.
func TestConcurrentUse(t *testing.T) {
	r := newRig(t, Options{Ports: []string{"lte1", "lte2"}, SendDelay: time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			port := []string{"lte1", "lte2"}[i%2]
			for j := 0; j < 10; j++ {
				r.do("POST", "/rest/tool/sms/send", map[string]string{"port": port, "phone-number": "1", "message": "x"})
				r.do("GET", "/rest/tool/sms/inbox", nil)
				r.do("GET", "/rest/interface/lte", nil)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 30; j++ {
			_, _ = r.s.InjectSMS(IncomingSMS{From: "+48511222333", Text: "x"})
			r.s.FailNext(EndpointInbox, HTTPError(500, ""), 1)
			r.s.SetLTE("lte1", func(l *LTEState) { l.Operator = "op" })
			_ = r.s.Sent()
			_ = r.s.Inbox()
			_ = r.s.AllRequests()
			r.s.SetSendDelay(time.Millisecond)
			r.s.SetLenient(j%2 == 0)
			if in := r.s.Inbox(); len(in) > 0 {
				r.do("DELETE", "/rest/tool/sms/inbox/"+in[0].ID, nil)
			}
		}
	}()
	wg.Wait()
	if r.s.Requests(EndpointSend) != 60 {
		t.Errorf("send requests = %d", r.s.Requests(EndpointSend))
	}
	if n := len(r.s.Sent()); n != 60 {
		t.Errorf("sent = %d, want 60", n)
	}
	// per-port serialization held under load
	byPort := map[string][]SentMessage{}
	for _, m := range r.s.Sent() {
		byPort[m.Port] = append(byPort[m.Port], m)
	}
	for port, ms := range byPort {
		for i := 1; i < len(ms); i++ {
			if ms[i].StartedAt.Before(ms[i-1].CompletedAt) {
				t.Errorf("%s: overlapping sends", port)
			}
		}
	}
}
