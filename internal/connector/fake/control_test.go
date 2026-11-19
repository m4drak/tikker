// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ctl(t *testing.T, r *rig, method, name string, body any) resp {
	t.Helper()
	// The control API is served by the same listener in cmd/fake-routeros; here
	// call the handler through a mux on the same server for the same effect.
	return r.do(method, ControlPrefix+name, body)
}

func newControlRig(t *testing.T, opts Options) *rig {
	t.Helper()
	s := New(opts)
	mux := http.NewServeMux()
	mux.Handle("/", s)
	mux.Handle(ControlPrefix, s.ControlHandler())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close)
	return &rig{t: t, s: s, url: ts.URL, c: &http.Client{Timeout: 5 * time.Second}}
}

func TestControlAPI(t *testing.T) {
	r := newControlRig(t, Options{Ports: []string{"lte1", "lte2"}})

	// inject: auto encoding, explicit port, class0, time
	got := ctl(t, r, "POST", "inject", map[string]any{
		"port": "lte2", "from": "+48511222333", "text": "Zażółć", "time": "2026-11-19T12:00:00+01:00", "class0": true,
	})
	wantStatus(t, got, 200, "")
	var ids struct{ IDs []string }
	_ = json.Unmarshal([]byte(got.body), &ids)
	if len(ids.IDs) != 1 {
		t.Fatalf("ids = %s", got.body)
	}
	in := r.s.Inbox()
	if in[0].Port != "lte2" || in[0].Type != "class-0" || decodeDeliver(t, in[0].PDU).text != "Zażółć" {
		t.Errorf("inbox = %+v", in)
	}
	wantStatus(t, ctl(t, r, "POST", "inject", map[string]any{"from": "+1", "text": "x", "encoding": "ucs2"}), 200, "")
	wantStatus(t, ctl(t, r, "POST", "inject", map[string]any{"from": "+1", "text": "x", "encoding": "gsm7"}), 200, "")
	wantStatus(t, ctl(t, r, "POST", "inject", map[string]any{"from": "+1", "text": "x", "encoding": "klingon"}), 400, "")
	wantStatus(t, ctl(t, r, "POST", "inject", map[string]any{"from": "", "text": "x"}), 400, "")
	wantStatus(t, ctl(t, r, "POST", "inject", `{`), 400, "")
	wantStatus(t, ctl(t, r, "POST", "inject", map[string]any{"raw": map[string]any{"phone": "+1", "message": "m", "pdu": "00", "timestamp_text": "t", "type": "x"}}), 200, "")
	wantStatus(t, ctl(t, r, "POST", "inject", map[string]any{"raw": map[string]any{"port": "zz"}}), 400, "")

	// inbox visible through both APIs
	var list []inboxCtlJSON
	_ = json.Unmarshal([]byte(ctl(t, r, "GET", "inbox", nil).body), &list)
	if len(list) != 4 || list[3].Message != "m" {
		t.Errorf("control inbox = %+v", list)
	}

	// fail: http with count, then clear
	wantStatus(t, ctl(t, r, "POST", "fail", map[string]any{"endpoint": "send", "status": 503, "detail": "busy", "count": 2}), 204, "")
	wantStatus(t, r.send("1", "x"), 503, "busy")
	wantStatus(t, r.send("1", "x"), 503, "busy")
	wantStatus(t, r.send("1", "x"), 200, "")
	wantStatus(t, ctl(t, r, "POST", "fail", map[string]any{"status": 502, "count": -1}), 204, "") // any endpoint, forever
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 502, "")
	wantStatus(t, ctl(t, r, "DELETE", "fail", nil), 204, "")
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 200, "")
	wantStatus(t, ctl(t, r, "POST", "fail", map[string]any{"endpoint": "send", "kind": "drop", "accepted": true, "delay_ms": 1}), 204, "")
	wantStatus(t, ctl(t, r, "POST", "fail", map[string]any{"endpoint": "resource", "kind": "hang"}), 204, "")
	wantStatus(t, ctl(t, r, "POST", "fail", map[string]any{"kind": "meteor"}), 400, "")
	wantStatus(t, ctl(t, r, "POST", "fail", `{`), 400, "")
	r.s.ClearFaults()

	// config
	wantStatus(t, ctl(t, r, "POST", "config", map[string]any{
		"send_delay_ms": 30, "session_timeout_ms": 5000, "lenient": true, "inbox_limit": 50, "auto_erase": true,
	}), 204, "")
	if !r.s.Settings().AutoErase {
		t.Error("auto_erase not applied")
	}
	start := time.Now()
	wantStatus(t, r.send("1", strings.Repeat("ż", 200)), 200, "") // lenient + delayed
	if time.Since(start) < 25*time.Millisecond {
		t.Error("send_delay not applied")
	}
	wantStatus(t, ctl(t, r, "POST", "config", map[string]any{"rest_api": false}), 204, "")
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 401, "")
	r.s.SetPolicies(Policies{RESTAPI: true})
	wantStatus(t, r.do("GET", "/rest/system/resource", nil), 500, "not allowed (9)")
	wantStatus(t, ctl(t, r, "POST", "config", map[string]any{"rest_api": true, "api": true}), 204, "")
	wantStatus(t, ctl(t, r, "POST", "config", `{`), 400, "")

	// lte
	wantStatus(t, ctl(t, r, "POST", "lte", map[string]any{
		"port": "lte2", "registration": "roaming", "operator": "Roam", "access_technology": "5G", "running": false,
		"signal": map[string]int{"RSSI": -80, "RSRP": -100, "RSRQ": -12, "SINR": 3},
	}), 204, "")
	st, _ := r.s.LTE("lte2")
	if st.Registration != "roaming" || st.Operator != "Roam" || st.AccessTechnology != "5G" || st.Running || st.Signal.RSRP != -100 {
		t.Errorf("lte2 = %+v %+v", st, st.Signal)
	}
	wantStatus(t, ctl(t, r, "POST", "lte", map[string]any{"no_signal": true, "registration": "searching"}), 204, "")
	if st, _ := r.s.LTE("lte1"); st.Signal != nil || st.Registered() {
		t.Errorf("lte1 = %+v", st)
	}
	wantStatus(t, ctl(t, r, "POST", "lte", map[string]any{"port": "nope"}), 400, "")
	wantStatus(t, ctl(t, r, "POST", "lte", `{`), 400, "")

	// sent + requests
	var sent []sentJSON
	_ = json.Unmarshal([]byte(ctl(t, r, "GET", "sent", nil).body), &sent)
	if len(sent) != 2 || sent[0].PhoneNumber != "1" {
		t.Errorf("sent = %+v", sent)
	}
	var reqs struct {
		Endpoints    map[string]int `json:"endpoints"`
		AuthFailures int            `json:"auth_failures"`
	}
	_ = json.Unmarshal([]byte(ctl(t, r, "GET", "requests", nil).body), &reqs)
	if reqs.Endpoints["send"] != 4 || reqs.AuthFailures != 2 {
		t.Errorf("requests = %+v", reqs)
	}

	// full inbox -> 507
	r.s.SetSettings(func(s *Settings) { s.AutoErase = false })
	r.s.SetInboxLimit(1)
	wantStatus(t, ctl(t, r, "POST", "inject", map[string]any{"from": "+1", "text": "x"}), 507, "")

	// reset + unknown
	wantStatus(t, ctl(t, r, "POST", "reset", nil), 204, "")
	if len(r.s.Sent()) != 0 || len(r.s.Inbox()) != 0 {
		t.Error("reset did not clear state")
	}
	wantStatus(t, ctl(t, r, "GET", "nothing", nil), 404, "")
	wantStatus(t, ctl(t, r, "PUT", "inject", nil), 404, "")
}
