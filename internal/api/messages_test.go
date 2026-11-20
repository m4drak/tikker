// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sms"
)

const messagesPath = "/api/v1/messages"

// basic adds one device and one channel (lte1) and returns the channel ID.
func (e *testEnv) basic() string {
	e.t.Helper()
	return e.addChannel(e.addDevice("router"), "lte1", nil)
}

func TestSendMessage(t *testing.T) {
	e := newEnv(t)
	ch := e.addChannel(e.addDevice("router"), "lte1", map[string]any{"tags": []string{"bulk"}})

	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{
		"to": "600 100 200", "body": "Hello there", "client_ref": "ref-1", "tags": []string{"bulk"},
	})
	if res.Status != http.StatusAccepted {
		t.Fatalf("want 202: %s", res)
	}
	m := res.obj()
	id := m["id"].(string)
	if loc := res.Header.Get("Location"); loc != "/api/v1/messages/"+id {
		t.Errorf("Location = %q", loc)
	}
	want := map[string]any{
		"direction": "out", "channel_id": nil, "peer": "+48600100200", "body": "Hello there", "encoding": "gsm7",
		"segments": float64(1), "status": "queued", "attempts": float64(0), "next_attempt_at": nil, "last_error": nil,
		"client_ref": "ref-1", "tags": []any{"bulk"}, "expires_at": nil, "created_by": "token:" + tokenID(t, e, e.sendTok),
		"created_at": "2026-11-19T12:00:00Z", "sent_at": nil, "received_at": nil,
	}
	for k, v := range want {
		if !reflect.DeepEqual(m[k], v) {
			t.Errorf("%s = %#v, want %#v", k, m[k], v)
		}
	}
	if res.Header.Get(replayedHeader) != "" || res.Header.Get(partIDsHeader) != "" {
		t.Errorf("unexpected headers: %v", res.Header)
	}

	// Explicit channel: pinned, so the channel is reported at once.
	res = e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "+48600100200", "body": "pinned", "channel_id": ch})
	if res.Status != http.StatusAccepted || res.obj()["channel_id"] != ch {
		t.Fatalf("pinned: %s", res)
	}
	// RouterOS sends GSM-7 only: Polish text without transliteration is refused.
	res = e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "+48600100200", "body": "Zażółć gęślą jaźń"})
	res.problem(http.StatusUnprocessableEntity, problemValidation)
}

func tokenID(t *testing.T, e *testEnv, raw string) string {
	t.Helper()
	prefix := strings.Split(raw, "_")[1]
	tok, err := e.st.Tokens().GetByPrefix(e.ctx, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return string(tok.ID)
}

func TestSendMessageTransliterate(t *testing.T) {
	e := newEnv(t)
	e.addChannel(e.addDevice("router"), "lte1", map[string]any{"transliterate": true})
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "+48600100200", "body": "Zażółć gęślą jaźń"})
	if res.Status != http.StatusAccepted {
		t.Fatal(res)
	}
	m := res.obj()
	if m["body"] != "Zazolc gesla jazn" || m["encoding"] != "gsm7" {
		t.Errorf("body/encoding = %v / %v", m["body"], m["encoding"])
	}
}

func TestSendMessageValidation(t *testing.T) {
	e := newEnv(t)
	e.basic()
	disabled := e.addChannel(e.addDevice("second"), "lte1", map[string]any{"enabled": false, "name": "off"})
	long := strings.Repeat("a", 161)

	tests := []struct {
		name   string
		body   any
		status int
		typ    string
		codes  []string
	}{
		{"missing to", map[string]any{"body": "x"}, 422, problemValidation, []string{"/to:required"}},
		{"empty to", map[string]any{"to": "", "body": "x"}, 422, problemValidation, []string{"/to:required"}},
		{"long to", map[string]any{"to": strings.Repeat("1", 33), "body": "x"}, 422, problemValidation, []string{"/to:out_of_range"}},
		{"bad number", map[string]any{"to": "12", "body": "x"}, 422, problemValidation, []string{"/to:invalid_phone_number"}},
		{"letters", map[string]any{"to": "abc", "body": "x"}, 422, problemValidation, []string{"/to:invalid_phone_number"}},
		{"missing body", map[string]any{"to": "600100200"}, 422, problemValidation, []string{"/body:required"}},
		{"huge body", map[string]any{"to": "600100200", "body": strings.Repeat("a", 1601)}, 422, problemValidation, []string{"/body:out_of_range"}},
		{"too many segments", map[string]any{"to": "600100200", "body": long}, 422, problemValidation, []string{"/body:too_many_segments"}},
		{"ucs2 needs transliteration", map[string]any{"to": "600100200", "body": "zażółć"}, 422, problemValidation, []string{"/body:invalid_format"}},
		{"unknown channel", map[string]any{"to": "600100200", "body": "x", "channel_id": "nope"}, 422, problemValidation, []string{"/channel_id:unknown_channel"}},
		{"disabled channel", map[string]any{"to": "600100200", "body": "x", "channel_id": disabled}, 422, problemValidation, []string{"/channel_id:channel_disabled"}},
		{"bad channel id", map[string]any{"to": "600100200", "body": "x", "channel_id": strings.Repeat("x", 65)}, 422, problemValidation, []string{"/channel_id:invalid_format"}},
		{"no matching tags", map[string]any{"to": "600100200", "body": "x", "tags": []string{"nothing"}}, 422, problemValidation, []string{"/tags:no_matching_channel"}},
		{"bad tags", map[string]any{"to": "600100200", "body": "x", "tags": []string{"Bad Tag", "a", "a"}}, 422, problemValidation, []string{"/tags/0:invalid_format", "/tags/2:invalid_format"}},
		{"long client_ref", map[string]any{"to": "600100200", "body": "x", "client_ref": strings.Repeat("r", 129)}, 422, problemValidation, []string{"/client_ref:out_of_range"}},
		{"expiry in the past", map[string]any{"to": "600100200", "body": "x", "expires_at": "2026-11-19T11:59:59Z"}, 422, problemValidation, []string{"/expires_at:expires_at_in_past"}},
		{"expiry now", map[string]any{"to": "600100200", "body": "x", "expires_at": "2026-11-19T12:00:00Z"}, 422, problemValidation, []string{"/expires_at:expires_at_in_past"}},
		{"expiry malformed", map[string]any{"to": "600100200", "body": "x", "expires_at": "tomorrow"}, 422, problemValidation, []string{"/expires_at:invalid_format"}},
		{"several errors", map[string]any{"to": "1", "body": ""}, 422, problemValidation, []string{"/to:invalid_phone_number", "/body:required"}},
		{"unknown member", map[string]any{"to": "600100200", "body": "x", "priority": 1}, 400, problemMalformed, []string{"/priority:invalid_format"}},
		{"wrong type", `{"to": 600100200, "body": "x"}`, 400, problemMalformed, []string{"/to:invalid_format"}},
		{"not json", `{"to": `, 400, problemMalformed, nil},
		{"syntax error", `{"to" "x"}`, 400, problemMalformed, nil},
		{"array", `[1]`, 400, problemMalformed, nil},
		{"trailing data", `{"to":"600100200","body":"x"} {}`, 400, problemMalformed, nil},
		{"empty body", ``, 400, problemMalformed, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := e.req(http.MethodPost, messagesPath, e.sendTok, tc.body)
			p := res.problem(tc.status, tc.typ)
			if tc.codes != nil {
				got := problemCodes(p)
				if !reflect.DeepEqual(got, tc.codes) {
					t.Errorf("errors = %v, want %v", got, tc.codes)
				}
			}
		})
	}
	// Nothing of the above was queued.
	page := e.get(messagesPath, e.readTok).obj()
	if items := page["items"].([]any); len(items) != 0 {
		t.Errorf("%d messages queued by invalid requests", len(items))
	}
}

func TestSendMessageNoChannels(t *testing.T) {
	e := newEnv(t)
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "x"})
	p := res.problem(422, problemValidation)
	if got := problemCodes(p); !reflect.DeepEqual(got, []string{":no_matching_channel"}) {
		t.Errorf("errors = %v", got)
	}
}

func TestSendMessageDisabledDeviceExcludesChannels(t *testing.T) {
	e := newEnv(t)
	dev := e.addDevice("router")
	ch := e.addChannel(dev, "lte1", nil)
	if r := e.req(http.MethodPatch, "/api/v1/devices/"+dev, e.adminTok, map[string]any{"enabled": false}); r.Status != 200 {
		t.Fatal(r)
	}
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "x", "channel_id": ch})
	res.problem(422, problemValidation)
	res = e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "x"})
	res.problem(422, problemValidation)
	// The channel is reported as disabled while its device is.
	c := e.get("/api/v1/channels/"+ch, e.readTok).obj()
	if c["state"] != "disabled" || c["state_reason"] != "device disabled" {
		t.Errorf("state = %v (%v)", c["state"], c["state_reason"])
	}
}

func TestSendMessageSplit(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.opts.LongMessageStrategy = sms.StrategySplit })
	e.addChannel(e.addDevice("router"), "lte1", map[string]any{"max_segments": 3})
	body := strings.Repeat("0123456789 ", 20) // 220 characters: two parts of at most 160 septets
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": body, "client_ref": "doc-7"},
		"Idempotency-Key", "split-1")
	if res.Status != http.StatusAccepted {
		t.Fatal(res)
	}
	ids := strings.Split(res.Header.Get(partIDsHeader), ",")
	if len(ids) < 2 {
		t.Fatalf("expected several parts, header %q", res.Header.Get(partIDsHeader))
	}
	first := res.obj()
	if first["id"] != ids[0] || !strings.HasPrefix(first["body"].(string), fmt.Sprintf("(1/%d) ", len(ids))) {
		t.Errorf("first part = %v", first)
	}
	var joined string
	for i, id := range ids {
		m := e.get(messagesPath+"/"+id, e.readTok).obj()
		if m["client_ref"] != "doc-7" || m["status"] != "queued" || m["segments"] != float64(1) {
			t.Errorf("part %d = %v", i+1, m)
		}
		prefix := fmt.Sprintf("(%d/%d) ", i+1, len(ids))
		if !strings.HasPrefix(m["body"].(string), prefix) {
			t.Errorf("part %d body %q lacks prefix %q", i+1, m["body"], prefix)
		}
		joined += strings.TrimPrefix(m["body"].(string), prefix)
	}
	if strings.Join(strings.Fields(joined), " ") != strings.Join(strings.Fields(body), " ") {
		t.Errorf("parts do not add up to the text: %q", joined)
	}
	// A replay returns the same parts and creates no new ones.
	again := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": body, "client_ref": "doc-7"},
		"Idempotency-Key", "split-1")
	if again.Header.Get(partIDsHeader) != res.Header.Get(partIDsHeader) || again.Header.Get(replayedHeader) != "true" {
		t.Errorf("replay headers = %v", again.Header)
	}
	page := e.get(messagesPath, e.readTok).obj()
	if n := len(page["items"].([]any)); n != len(ids) {
		t.Errorf("%d messages stored, want %d", n, len(ids))
	}
}

func TestSendMessageSplitTooLong(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.opts.LongMessageStrategy = sms.StrategySplit })
	e.addChannel(e.addDevice("router"), "lte1", map[string]any{"max_segments": 2})
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": strings.Repeat("a", 500)})
	p := res.problem(422, problemValidation)
	if got := problemCodes(p); !reflect.DeepEqual(got, []string{"/body:too_many_segments"}) {
		t.Errorf("errors = %v", got)
	}
}

func TestSendMessageTruncate(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.opts.LongMessageStrategy = sms.StrategyTruncate })
	e.basic()
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": strings.Repeat("a", 200)})
	if res.Status != 202 || len(res.obj()["body"].(string)) != 160 {
		t.Fatal(res)
	}
}

func TestSendMessageShortNumbers(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.opts.AllowShortNumbers = true })
	e.basic()
	for _, to := range []string{"8012", "MyBank"} {
		res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": to, "body": "x"})
		if res.Status != 202 || res.obj()["peer"] != to {
			t.Errorf("%s: %s", to, res)
		}
	}
	e2 := newEnv(t)
	e2.basic()
	e2.req(http.MethodPost, messagesPath, e2.sendTok, map[string]any{"to": "8012", "body": "x"}).problem(422, problemValidation)
}

func TestSendMessageExpiry(t *testing.T) {
	e := newEnv(t)
	e.basic()
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{
		"to": "600100200", "body": "x", "expires_at": "2026-11-19T12:05:00+01:00",
	})
	// 12:05 at +01:00 is 11:05 UTC, before the fake clock's 12:00: in the past.
	res.problem(422, problemValidation)
	res = e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{
		"to": "600100200", "body": "x", "expires_at": "2026-11-19T13:05:00+01:00",
	})
	if res.Status != 202 || res.obj()["expires_at"] != "2026-11-19T12:05:00Z" {
		t.Fatal(res)
	}
}

func TestIdempotency(t *testing.T) {
	e := newEnv(t)
	e.basic()
	body := `{"to":"600100200","body":"once","client_ref":"a"}`
	reordered := `{ "client_ref": "a", "body": "once", "to": "600100200" }`

	first := e.req(http.MethodPost, messagesPath, e.sendTok, body, "Idempotency-Key", "k-1")
	if first.Status != 202 || first.Header.Get(replayedHeader) != "" {
		t.Fatal(first)
	}
	for _, b := range []string{body, reordered} {
		again := e.req(http.MethodPost, messagesPath, e.sendTok, b, "Idempotency-Key", "k-1")
		if again.Status != 202 || again.Header.Get(replayedHeader) != "true" {
			t.Fatalf("replay: %s %v", again, again.Header)
		}
		if again.obj()["id"] != first.obj()["id"] {
			t.Errorf("replay returned another message")
		}
		if again.Header.Get("Location") != first.Header.Get("Location") {
			t.Errorf("Location differs")
		}
	}
	// Another body with the same key.
	res := e.req(http.MethodPost, messagesPath, e.sendTok, `{"to":"600100200","body":"twice"}`, "Idempotency-Key", "k-1")
	res.problem(422, problemIdemMismatch)
	// The key is not shared with the batch endpoint.
	res = e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, `{"messages":[{"to":"600100200","body":"once","client_ref":"a"}]}`, "Idempotency-Key", "k-1")
	res.problem(422, problemIdemMismatch)

	// Keys are per token.
	other := e.mkToken(e.operator, domain.ScopeSMSSend)
	res = e.req(http.MethodPost, messagesPath, other, body, "Idempotency-Key", "k-1")
	if res.Status != 202 || res.Header.Get(replayedHeader) != "" {
		t.Errorf("another token must not replay: %s", res)
	}
	// A failed request does not reserve the key.
	res = e.req(http.MethodPost, messagesPath, e.sendTok, `{"to":"1","body":"x"}`, "Idempotency-Key", "k-2")
	res.problem(422, problemValidation)
	res = e.req(http.MethodPost, messagesPath, e.sendTok, `{"to":"600100200","body":"fine"}`, "Idempotency-Key", "k-2")
	if res.Status != 202 {
		t.Errorf("key was reserved by a failed request: %s", res)
	}
	// Without a key every request creates a message.
	a := e.req(http.MethodPost, messagesPath, e.sendTok, body).obj()["id"]
	b := e.req(http.MethodPost, messagesPath, e.sendTok, body).obj()["id"]
	if a == b {
		t.Error("requests without a key must not be merged")
	}
	// Keys expire after 24 h.
	e.fake.Advance(25 * time.Hour)
	res = e.req(http.MethodPost, messagesPath, e.sendTok, `{"to":"600100200","body":"later"}`, "Idempotency-Key", "k-1")
	if res.Status != 202 || res.Header.Get(replayedHeader) != "" {
		t.Errorf("expired key must be reusable: %s", res)
	}
}

func TestIdempotencyKeyFormat(t *testing.T) {
	e := newEnv(t)
	e.basic()
	for name, key := range map[string]string{"space": "a b", "control": "a\x01b", "too long": strings.Repeat("k", 256), "non ascii": "kłucz"} {
		t.Run(name, func(t *testing.T) {
			res := e.req(http.MethodPost, messagesPath, e.sendTok, `{"to":"600100200","body":"x"}`, "Idempotency-Key", key)
			p := res.problem(400, problemMalformed)
			if got := problemCodes(p); !reflect.DeepEqual(got, []string{"Idempotency-Key:invalid_format"}) {
				t.Errorf("errors = %v", got)
			}
		})
	}
}

func TestIdempotencyInFlight(t *testing.T) {
	e := newEnv(t)
	e.basic()
	// Reserve the key as a concurrent request would.
	api := e.api
	actor := domain.TokenActor(domain.ID(tokenID(t, e, e.sendTok)))
	if !api.inflight.acquire(actor, "busy") {
		t.Fatal("acquire")
	}
	res := e.req(http.MethodPost, messagesPath, e.sendTok, `{"to":"600100200","body":"x"}`, "Idempotency-Key", "busy")
	res.problem(409, problemIdemInUse)
	api.inflight.release(actor, "busy")
	res = e.req(http.MethodPost, messagesPath, e.sendTok, `{"to":"600100200","body":"x"}`, "Idempotency-Key", "busy")
	if res.Status != 202 {
		t.Fatal(res)
	}
}

func TestBatch(t *testing.T) {
	e := newEnv(t)
	e.basic()
	res := e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, map[string]any{"messages": []map[string]any{
		{"to": "600100200", "body": "one", "client_ref": "1"},
		{"to": "+48600100201", "body": "two", "tags": []string{}},
	}}, "Idempotency-Key", "b-1")
	if res.Status != 202 {
		t.Fatal(res)
	}
	items := res.obj()["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["peer"] != "+48600100200" || items[1].(map[string]any)["body"] != "two" {
		t.Fatalf("items = %v", items)
	}
	again := e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, map[string]any{"messages": []map[string]any{
		{"to": "600100200", "body": "one", "client_ref": "1"},
		{"to": "+48600100201", "body": "two", "tags": []string{}},
	}}, "Idempotency-Key", "b-1")
	if again.Header.Get(replayedHeader) != "true" || !reflect.DeepEqual(again.obj()["items"].([]any)[0].(map[string]any)["id"], items[0].(map[string]any)["id"]) {
		t.Errorf("replay: %s", again)
	}
}

func TestBatchAtomic(t *testing.T) {
	e := newEnv(t)
	e.basic()
	res := e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, map[string]any{"messages": []map[string]any{
		{"to": "600100200", "body": "ok"},
		{"to": "12", "body": "bad number"},
		{"to": "600100200", "body": strings.Repeat("a", 161)},
	}})
	p := res.problem(422, problemValidation)
	want := []string{"/messages/1/to:invalid_phone_number", "/messages/2/body:too_many_segments"}
	if got := problemCodes(p); !reflect.DeepEqual(got, want) {
		t.Errorf("errors = %v, want %v", got, want)
	}
	if d := p["detail"].(string); d != "Batch rejected: 2 of 3 messages are invalid." {
		t.Errorf("detail = %q", d)
	}
	page := e.get(messagesPath, e.readTok).obj()
	if n := len(page["items"].([]any)); n != 0 {
		t.Errorf("%d messages of a rejected batch were queued", n)
	}
}

func TestBatchLimits(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.opts.MaxBodyBytes = 64 << 10 })
	e.basic()
	msgs := make([]map[string]any, 101)
	for i := range msgs {
		msgs[i] = map[string]any{"to": "600100200", "body": "x"}
	}
	res := e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, map[string]any{"messages": msgs})
	p := res.problem(413, problemBatchTooLarge)
	if p["title"] != "Batch too large" || !strings.Contains(p["detail"].(string), "101 messages") {
		t.Errorf("problem = %v", p)
	}
	// 100 are fine.
	res = e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, map[string]any{"messages": msgs[:100]})
	if res.Status != 202 || len(res.obj()["items"].([]any)) != 100 {
		t.Fatalf("100 messages: %d", res.Status)
	}
	// Empty batch.
	e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, `{"messages":[]}`).problem(422, problemValidation)
	e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, `{}`).problem(422, problemValidation)
	e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, `{"messages":[{"to":"1","body":"x","bogus":1}]}`).problem(400, problemMalformed)
	// A body above the size limit.
	big := map[string]any{"messages": []map[string]any{{"to": "600100200", "body": strings.Repeat("a", 70<<10)}}}
	e.req(http.MethodPost, messagesPath+"/batch", e.sendTok, big).problem(413, problemBatchTooLarge)
	// POST /messages answers 413 for an oversized body too, which the
	// contract only declares for the batch endpoint.
	e.undocumented(func() {
		e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": strings.Repeat("a", 70<<10)}).problem(413, problemBatchTooLarge)
	})
}

func TestListMessages(t *testing.T) {
	e := newEnv(t)
	ch := e.basic()
	var ids []string
	for i := 0; i < 5; i++ {
		e.fake.Advance(time.Minute)
		to := "+48600100200"
		if i%2 == 1 {
			to = "+48600100201"
		}
		res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": to, "body": fmt.Sprintf("m%d", i)})
		ids = append(ids, res.obj()["id"].(string))
	}
	// Cancel one, so statuses differ.
	e.req(http.MethodDelete, messagesPath+"/"+ids[0], e.sendTok, nil)
	// An inbound message.
	if _, err := e.srv.InjectSMS(fakeSMS("+48511222333", "incoming")); err != nil {
		t.Fatal(err)
	}
	e.pollInbox(ch)

	list := func(q string) (items []map[string]any, next any) {
		t.Helper()
		res := e.get(messagesPath+q, e.readTok)
		if res.Status != 200 {
			t.Fatalf("%s: %s", q, res)
		}
		m := res.obj()
		for _, it := range m["items"].([]any) {
			items = append(items, it.(map[string]any))
		}
		return items, m["next_cursor"]
	}
	all, next := list("")
	if len(all) != 6 || next != nil {
		t.Fatalf("all = %d next = %v", len(all), next)
	}
	// Newest first.
	for i := 1; i < len(all); i++ {
		if all[i-1]["id"].(string) <= all[i]["id"].(string) {
			t.Errorf("not sorted by descending id at %d", i)
		}
	}
	tests := []struct {
		q    string
		want int
	}{
		{"?direction=in", 1},
		{"?direction=out", 5},
		{"?status=queued", 4},
		{"?status=queued&status=cancelled", 5},
		{"?status=queued&status=queued", 4},
		{"?status=received", 1},
		{"?peer=%2B48600100201", 2},
		{"?peer=600100201", 2},
		{"?peer=%2B48511222333", 1},
		{"?peer=%2B48000000000", 0},
		{"?channel_id=" + ch, 1},
		{"?channel_id=other", 0},
		{"?since=2026-11-19T12:03:00Z", 3 + 1 /* the inbound poll time is 12:05 */},
		{"?until=2026-11-19T12:03:00Z", 2},
		{"?since=2026-11-19T12:02:00Z&until=2026-11-19T12:04:00Z", 2},
		{"?direction=out&status=queued&peer=%2B48600100200", 2},
	}
	for _, tc := range tests {
		got, _ := list(tc.q)
		if len(got) != tc.want {
			t.Errorf("%s: %d messages, want %d", tc.q, len(got), tc.want)
		}
	}

	// Inbound message fields.
	in, _ := list("?direction=in")
	if in[0]["peer"] != "+48511222333" || in[0]["status"] != "received" || in[0]["created_by"] != nil || in[0]["body"] != "incoming" ||
		in[0]["channel_id"] != ch || in[0]["received_at"] == nil || in[0]["attempts"] != float64(0) {
		t.Errorf("inbound = %v", in[0])
	}

	// Pagination walks everything exactly once, same filters on every page.
	var seen []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		q := "?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		items, next := list(q)
		for _, it := range items {
			seen = append(seen, it["id"].(string))
		}
		if next == nil {
			break
		}
		cursor = next.(string)
		if len(items) != 2 {
			t.Errorf("page of %d", len(items))
		}
	}
	if len(seen) != 6 {
		t.Errorf("pagination returned %d messages", len(seen))
	}
}

func TestListMessagesBadParameters(t *testing.T) {
	e := newEnv(t)
	tests := []struct{ q, field string }{
		{"?direction=sideways", "direction"},
		{"?status=lost", "status"},
		{"?limit=0", "limit"},
		{"?limit=201", "limit"},
		{"?limit=ten", "limit"},
		{"?cursor=garbage", "cursor"},
		{"?cursor=" + strings.Repeat("a", 600), "cursor"},
		{"?since=yesterday", "since"},
		{"?until=2026-13-01", "until"},
		{"?bogus=1", "bogus"},
		{"?direction=in&direction=out", "direction"},
		{"?channel_id=" + strings.Repeat("c", 65), "channel_id"},
		{"?peer=" + strings.Repeat("1", 33), "peer"},
	}
	for _, tc := range tests {
		t.Run(tc.q, func(t *testing.T) {
			p := e.get(messagesPath+tc.q, e.readTok).problem(400, problemMalformed)
			codes := problemCodes(p)
			if len(codes) == 0 || !strings.HasPrefix(codes[0], tc.field+":") {
				t.Errorf("errors = %v, want field %s", codes, tc.field)
			}
		})
	}
	// A well-formed cursor of an unknown shape is rejected as well.
	e.get(messagesPath+"?cursor="+encodeCursor("not-an-id"), e.readTok).problem(400, problemMalformed)
	// A cursor from the same server works, even for an ID that is gone.
	if r := e.get(messagesPath+"?cursor="+encodeCursor(domain.ID("01ARZ3NDEKTSV4RRFFQ69G5FAV")), e.readTok); r.Status != 200 {
		t.Errorf("valid cursor: %s", r)
	}
}

func TestGetMessage(t *testing.T) {
	e := newEnv(t)
	e.basic()
	id := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "hi"}).obj()["id"].(string)
	res := e.get(messagesPath+"/"+id, e.readTok)
	if res.Status != 200 || res.obj()["id"] != id {
		t.Fatal(res)
	}
	e.get(messagesPath+"/01ARZ3NDEKTSV4RRFFQ69G5FAV", e.readTok).problem(404, problemNotFound)
	e.get(messagesPath+"/not-a-ulid", e.readTok).problem(400, problemMalformed)
	e.get(messagesPath+"/"+strings.ToLower(id), e.readTok).problem(400, problemMalformed)
}

func TestCancelMessage(t *testing.T) {
	e := newEnv(t)
	ch := e.basic()
	id := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "hi"}).obj()["id"].(string)

	res := e.req(http.MethodDelete, messagesPath+"/"+id, e.sendTok, nil)
	if res.Status != 200 || res.obj()["status"] != "cancelled" {
		t.Fatal(res)
	}
	// Cancelling again, or a message that is not queued, is a conflict.
	p := e.req(http.MethodDelete, messagesPath+"/"+id, e.sendTok, nil).problem(409, problemConflict)
	if !strings.Contains(p["detail"].(string), `status "cancelled"`) {
		t.Errorf("detail = %v", p["detail"])
	}
	e.req(http.MethodDelete, messagesPath+"/01ARZ3NDEKTSV4RRFFQ69G5FAV", e.sendTok, nil).problem(404, problemNotFound)
	e.req(http.MethodDelete, messagesPath+"/bad", e.sendTok, nil).problem(400, problemMalformed)

	// Inbound messages cannot be cancelled.
	if _, err := e.srv.InjectSMS(fakeSMS("+48511222333", "in")); err != nil {
		t.Fatal(err)
	}
	e.pollInbox(ch)
	in := e.get(messagesPath+"?direction=in", e.readTok).obj()["items"].([]any)[0].(map[string]any)["id"].(string)
	e.req(http.MethodDelete, messagesPath+"/"+in, e.sendTok, nil).problem(409, problemConflict)

	// The cancellation is audited.
	found := false
	page, err := e.st.Audit().List(e.ctx, auditFilter(), pageRequest(100))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range page.Items {
		if a.Action == domain.AuditMessageCancel && a.Target == "message:"+id {
			found = true
		}
	}
	if !found {
		t.Error("no audit entry for the cancellation")
	}
}

func TestStrictest(t *testing.T) {
	// Several candidate channels: the plan must fit all of them.
	e := newEnv(t, func(c *envConfig) { c.opts.LongMessageStrategy = sms.StrategySplit })
	dev := e.addDevice("router")
	e.addChannel(dev, "lte1", map[string]any{"tags": []string{"a"}, "max_segments": 3})
	e.addChannel(dev, "lte2", map[string]any{"tags": []string{"a", "b"}, "max_segments": 2, "transliterate": true})

	// Tag "a": both channels. The strictest max_segments is 2 and transliteration
	// is on if any channel has it, so one stored text can go through both.
	res := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "Zażółć gęślą jaźń", "tags": []string{"a"}})
	if res.Status != 202 || res.obj()["body"] != "Zazolc gesla jazn" {
		t.Fatalf("tag a: %s", res)
	}
	// Tag "b" only matches lte2; a text needing 3 segments is too long there.
	res = e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": strings.Repeat("a", 400), "tags": []string{"b"}})
	res.problem(422, problemValidation)
	// Without tags both channels are candidates: 2 segments at most.
	res = e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": strings.Repeat("a", 400)})
	res.problem(422, problemValidation)
}
