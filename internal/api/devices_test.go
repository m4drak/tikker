// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
)

const (
	devicesPath  = "/api/v1/devices"
	channelsPath = "/api/v1/channels"
	fpA          = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"
	fpLower      = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func TestDeviceCRUD(t *testing.T) {
	e := newEnv(t)
	res := e.req(http.MethodPost, devicesPath, e.adminTok, map[string]any{
		"name": "Kraków", "base_url": "https://10.20.0.1", "username": "tikker", "password": "S3cret-router-pass",
		"tls_mode": "fingerprint", "fingerprint": fpLower,
	})
	if res.Status != http.StatusCreated {
		t.Fatal(res)
	}
	d := res.obj()
	id := d["id"].(string)
	want := map[string]any{
		"name": "Kraków", "base_url": "https://10.20.0.1", "username": "tikker", "tls_mode": "fingerprint",
		"ca_pem": nil, "fingerprint": fpA, "enabled": true, "insecure_transport": false,
		"created_at": "2026-11-19T12:00:00Z",
	}
	for k, v := range want {
		if !reflect.DeepEqual(d[k], v) {
			t.Errorf("%s = %#v, want %#v", k, d[k], v)
		}
	}
	if _, has := d["password"]; has || strings.Contains(string(res.Body), "S3cret") {
		t.Errorf("response contains the password: %s", res.Body)
	}
	if res.Header.Get("Location") != "/api/v1/devices/"+id {
		t.Errorf("Location = %q", res.Header.Get("Location"))
	}

	// The password is stored encrypted, never in clear.
	stored, err := e.st.Devices().Get(e.ctx, domain.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.PasswordEnc) == 0 || strings.Contains(string(stored.PasswordEnc), "S3cret") {
		t.Errorf("stored password = %q", stored.PasswordEnc)
	}

	// Reads.
	if r := e.get(devicesPath+"/"+id, e.adminTok); r.Status != 200 || strings.Contains(string(r.Body), "S3cret") {
		t.Errorf("get: %s", r)
	}
	list := e.get(devicesPath, e.adminTok).obj()
	if items := list["items"].([]any); len(items) != 1 || list["next_cursor"] != nil {
		t.Errorf("list = %v", list)
	}
	e.get(devicesPath+"/nope", e.adminTok).problem(404, problemNotFound)

	// Duplicate name.
	e.req(http.MethodPost, devicesPath, e.adminTok, map[string]any{
		"name": "Kraków", "base_url": "https://10.20.0.2", "username": "u", "password": "p",
	}).problem(409, problemConflict)

	// Patch: merge semantics, password rotation, null removes.
	res = e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, map[string]any{
		"name": "Kraków 2", "password": "N3w-pass", "enabled": false,
	})
	if res.Status != 200 {
		t.Fatal(res)
	}
	d = res.obj()
	if d["name"] != "Kraków 2" || d["enabled"] != false || d["tls_mode"] != "fingerprint" || d["fingerprint"] != fpA || d["username"] != "tikker" {
		t.Errorf("patched = %v", d)
	}
	if strings.Contains(string(res.Body), "N3w-pass") {
		t.Error("password in the patch response")
	}
	after, _ := e.st.Devices().Get(e.ctx, domain.ID(id))
	if string(after.PasswordEnc) == string(stored.PasswordEnc) {
		t.Error("password was not re-encrypted")
	}
	// Omitting the password keeps it.
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, map[string]any{"username": "other"})
	kept, _ := e.st.Devices().Get(e.ctx, domain.ID(id))
	if string(kept.PasswordEnc) != string(after.PasswordEnc) || kept.Username != "other" {
		t.Error("password changed by a patch without password")
	}
	// Switching to verify and removing the fingerprint is consistent.
	res = e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, map[string]any{"tls_mode": "verify", "fingerprint": nil})
	if res.Status != 200 || res.obj()["fingerprint"] != nil {
		t.Fatal(res)
	}
	// Removing material the mode needs is a 422 and changes nothing.
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, map[string]any{"tls_mode": "ca"}).problem(422, problemValidation)
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, map[string]any{"tls_mode": "fingerprint"}).problem(422, problemValidation)
	if cur, _ := e.st.Devices().Get(e.ctx, domain.ID(id)); cur.TLSMode != domain.TLSVerify {
		t.Errorf("failed patch changed tls_mode to %s", cur.TLSMode)
	}
	// Patch errors.
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, `{}`).problem(422, problemValidation)
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, `{"bogus":1}`).problem(400, problemMalformed)
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, `[1]`).problem(400, problemMalformed)
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, `{"name": 5}`).problem(400, problemMalformed)
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, `{"name": null}`).problem(422, problemValidation)
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, `{"name": ""}`).problem(422, problemValidation)
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, ``).problem(400, problemMalformed)
	e.req(http.MethodPatch, devicesPath+"/nope", e.adminTok, `{"name":"x"}`).problem(404, problemNotFound)

	// Duplicate name on patch.
	e.req(http.MethodPost, devicesPath, e.adminTok, map[string]any{"name": "Second", "base_url": "https://10.20.0.3", "username": "u", "password": "p"})
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, map[string]any{"name": "Second"}).problem(409, problemConflict)

	// Delete.
	if r := e.req(http.MethodDelete, devicesPath+"/"+id, e.adminTok, nil); r.Status != 204 || len(r.Body) != 0 {
		t.Fatal(r)
	}
	e.req(http.MethodDelete, devicesPath+"/"+id, e.adminTok, nil).problem(404, problemNotFound)
	e.get(devicesPath+"/"+id, e.adminTok).problem(404, problemNotFound)

	// Every change was audited, without secrets.
	page, _ := e.st.Audit().List(e.ctx, auditFilter(), pageRequest(100))
	var actions []domain.AuditAction
	for _, a := range page.Items {
		for k, v := range a.Meta {
			if strings.Contains(v, "S3cret") || strings.Contains(v, "N3w-pass") {
				t.Errorf("audit meta %s leaks a password: %s", k, v)
			}
		}
		actions = append(actions, a.Action)
	}
	for _, want := range []domain.AuditAction{domain.AuditDeviceCreate, domain.AuditDeviceUpdate, domain.AuditDeviceDelete} {
		found := false
		for _, a := range actions {
			found = found || a == want
		}
		if !found {
			t.Errorf("no audit entry %s", want)
		}
	}
	if e.reloader.count() == 0 {
		t.Error("the reloader was never called")
	}
}

func TestDeviceValidation(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.opts.AllowedHTTPHosts = []string{"172.17.0.0/16", "router.lan", "fe80::1"} })
	ok := func() map[string]any {
		return map[string]any{"name": "d", "base_url": "https://10.0.0.1", "username": "u", "password": "p"}
	}
	with := func(kv ...any) map[string]any {
		m := ok()
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	without := func(keys ...string) map[string]any {
		m := ok()
		for _, k := range keys {
			delete(m, k)
		}
		return m
	}
	caPEM := selfSignedPEM(t)
	tests := []struct {
		name  string
		body  any
		codes []string
	}{
		{"missing all", map[string]any{}, []string{"/name:required", "/base_url:required", "/username:required", "/password:required"}},
		{"missing password", without("password"), []string{"/password:required"}},
		{"empty name", with("name", ""), []string{"/name:out_of_range"}},
		{"long name", with("name", strings.Repeat("n", 65)), []string{"/name:out_of_range"}},
		{"long username", with("username", strings.Repeat("u", 65)), []string{"/username:out_of_range"}},
		{"empty password", with("password", ""), []string{"/password:out_of_range"}},
		{"long password", with("password", strings.Repeat("p", 257)), []string{"/password:out_of_range"}},
		{"ftp", with("base_url", "ftp://10.0.0.1"), []string{"/base_url:invalid_format"}},
		{"no host", with("base_url", "https://"), []string{"/base_url:invalid_format"}},
		{"not a url", with("base_url", "10.0.0.1"), []string{"/base_url:invalid_format"}},
		{"credentials in url", with("base_url", "https://u:p@10.0.0.1"), []string{"/base_url:invalid_format"}},
		{"query in url", with("base_url", "https://10.0.0.1/?a=b"), []string{"/base_url:invalid_format"}},
		{"long url", with("base_url", "https://"+strings.Repeat("a", 260)+".example"), []string{"/base_url:out_of_range"}},
		{"http to public host", with("base_url", "http://10.0.0.1"), []string{"/base_url:insecure_transport_not_allowed"}},
		{"http to other prefix", with("base_url", "http://172.18.0.1"), []string{"/base_url:insecure_transport_not_allowed"}},
		{"bad tls mode", with("tls_mode", "maybe"), []string{"/tls_mode:invalid_format"}},
		{"ca without pem", with("tls_mode", "ca"), []string{"/ca_pem:required"}},
		{"fingerprint without value", with("tls_mode", "fingerprint"), []string{"/fingerprint:required"}},
		{"bad fingerprint", with("fingerprint", "AB:CD"), []string{"/fingerprint:invalid_format"}},
		{"bad pem", with("tls_mode", "ca", "ca_pem", "not a pem"), []string{"/ca_pem:invalid_format"}},
		{"pem too long", with("tls_mode", "ca", "ca_pem", strings.Repeat("x", 16385)), []string{"/ca_pem:out_of_range"}},
		{"several", with("name", "", "base_url", "http://8.8.8.8"), []string{"/name:out_of_range", "/base_url:insecure_transport_not_allowed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := e.req(http.MethodPost, devicesPath, e.adminTok, tc.body).problem(422, problemValidation)
			if got := problemCodes(p); !reflect.DeepEqual(got, tc.codes) {
				t.Errorf("errors = %v, want %v", got, tc.codes)
			}
		})
	}
	e.req(http.MethodPost, devicesPath, e.adminTok, with("extra", 1)).problem(400, problemMalformed)
	e.req(http.MethodPost, devicesPath, e.adminTok, `{"password": 5}`).problem(400, problemMalformed)

	// Accepted forms.
	for name, body := range map[string]map[string]any{
		"http veth":      with("name", "a", "base_url", "http://172.17.0.1"),
		"http by name":   with("name", "b", "base_url", "http://Router.LAN:8080"),
		"http ipv6":      with("name", "c", "base_url", "http://[fe80::1]"),
		"https insecure": with("name", "d", "tls_mode", "insecure"),
		"ca":             with("name", "e", "tls_mode", "ca", "ca_pem", caPEM),
		"disabled":       with("name", "f", "enabled", false),
	} {
		t.Run(name, func(t *testing.T) {
			res := e.req(http.MethodPost, devicesPath, e.adminTok, body)
			if res.Status != http.StatusCreated {
				t.Fatal(res)
			}
			wantInsecure := name == "http veth" || name == "http by name" || name == "http ipv6" || name == "https insecure"
			if got := res.obj()["insecure_transport"]; got != wantInsecure {
				t.Errorf("insecure_transport = %v", got)
			}
		})
	}
	// Patching the URL is checked like creating it, but only when it is patched.
	id := e.get(devicesPath, e.adminTok).obj()["items"].([]any)[0].(map[string]any)["id"].(string)
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, map[string]any{"base_url": "http://8.8.8.8"}).problem(422, problemValidation)
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, map[string]any{"base_url": "http://172.17.0.9"})
	e.req(http.MethodPatch, devicesPath+"/"+id, e.adminTok, map[string]any{"fingerprint": "zz"}).problem(422, problemValidation)

	// No http:// at all without an allow list.
	strict := newEnv(t, func(c *envConfig) { c.noHTTP = true })
	strict.req(http.MethodPost, devicesPath, strict.adminTok, with("base_url", "http://127.0.0.1")).problem(422, problemValidation)
}

func TestDevicePagination(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		ids = append(ids, e.addDevice(n))
	}
	var seen []string
	cursor := ""
	for i := 0; i < 5; i++ {
		q := devicesPath + "?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		m := e.get(q, e.adminTok).obj()
		for _, it := range m["items"].([]any) {
			seen = append(seen, it.(map[string]any)["id"].(string))
		}
		if m["next_cursor"] == nil {
			break
		}
		cursor = m["next_cursor"].(string)
	}
	if !reflect.DeepEqual(seen, ids) {
		t.Errorf("pages = %v, want %v", seen, ids)
	}
	e.get(devicesPath+"?cursor=bogus", e.adminTok).problem(400, problemMalformed)
	e.get(devicesPath+"?x=1", e.adminTok).problem(400, problemMalformed)
}

func TestDeviceTest(t *testing.T) {
	e := newEnv(t)
	dev := e.addDevice("router")
	ch := e.addChannel(dev, "lte1", nil)

	res := e.req(http.MethodPost, devicesPath+"/"+dev+"/test", e.adminTok, nil)
	if res.Status != 200 {
		t.Fatal(res)
	}
	m := res.obj()
	if m["ok"] != true || m["error"] != nil || m["tls"] != nil || m["routeros_version"] == nil {
		t.Fatalf("result = %v", m)
	}
	ports := m["lte_ports"].([]any)
	if len(ports) != 2 {
		t.Fatalf("ports = %v", ports)
	}
	p1, p2 := ports[0].(map[string]any), ports[1].(map[string]any)
	if p1["port"] != "lte1" || p1["channel_id"] != ch || p2["port"] != "lte2" || p2["channel_id"] != nil {
		t.Errorf("ports = %v", ports)
	}
	// The test changed nothing.
	if c := e.get(channelsPath+"/"+ch, e.readTok).obj(); c["state"] != "ok" {
		t.Errorf("channel state after the test = %v", c["state"])
	}

	// Wrong credentials are a result, not an HTTP error.
	e.srv.SetCredentials("tikker", "changed-on-router")
	res = e.req(http.MethodPost, devicesPath+"/"+dev+"/test", e.adminTok, nil)
	m = res.obj()
	if res.Status != 200 || m["ok"] != false || m["routeros_version"] != nil {
		t.Fatalf("auth failure: %s", res)
	}
	if code := m["error"].(map[string]any)["code"]; code != "auth_failed" {
		t.Errorf("code = %v", code)
	}
	if strings.Contains(string(res.Body), routerPass) {
		t.Error("password in the test result")
	}

	// A router that is gone.
	e.srv.Close()
	res = e.req(http.MethodPost, devicesPath+"/"+dev+"/test", e.adminTok, nil)
	m = res.obj()
	if m["ok"] != false || m["latency_ms"] != nil {
		t.Fatalf("unreachable: %s", res)
	}
	if code := m["error"].(map[string]any)["code"]; code != "unreachable" && code != "timeout" {
		t.Errorf("code = %v", code)
	}
	e.req(http.MethodPost, devicesPath+"/nope/test", e.adminTok, nil).problem(404, problemNotFound)
}

func TestDeviceTestTLS(t *testing.T) {
	e := newEnv(t)
	tlsSrv := fake.New(fake.Options{User: routerUser, Password: routerPass})
	url := tlsSrv.StartTLS()
	t.Cleanup(tlsSrv.Close)

	// Pinned to a wrong fingerprint: the result carries the real one.
	res := e.req(http.MethodPost, devicesPath, e.adminTok, map[string]any{
		"name": "tls", "base_url": url, "username": routerUser, "password": routerPass,
		"tls_mode": "fingerprint", "fingerprint": fpA,
	})
	dev := res.obj()["id"].(string)
	m := e.req(http.MethodPost, devicesPath+"/"+dev+"/test", e.adminTok, nil).obj()
	if m["ok"] != false || m["error"].(map[string]any)["code"] != "tls_error" {
		t.Fatalf("mismatch: %v", m)
	}
	tlsInfo := m["tls"].(map[string]any)
	got := tlsInfo["peer_fingerprint"].(string)
	if tlsInfo["mode"] != "fingerprint" || tlsInfo["verified"] != false || got == "" {
		t.Fatalf("tls = %v", tlsInfo)
	}
	// Pin it, and the test passes.
	if r := e.req(http.MethodPatch, devicesPath+"/"+dev, e.adminTok, map[string]any{"fingerprint": got}); r.Status != 200 {
		t.Fatal(r)
	}
	m = e.req(http.MethodPost, devicesPath+"/"+dev+"/test", e.adminTok, nil).obj()
	if m["ok"] != true || m["tls"].(map[string]any)["verified"] != true {
		t.Fatalf("pinned: %v", m)
	}
}

func TestDeviceDeleteWithActiveMessages(t *testing.T) {
	e := newEnv(t)
	dev := e.addDevice("router")
	e.addChannel(dev, "lte1", nil)
	e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "pending"})
	// The message is not pinned, but the device is in use through its channel
	// only when the queue holds messages for it; pin one to be explicit.
	ch := e.get(channelsPath, e.readTok).obj()["items"].([]any)[0].(map[string]any)["id"].(string)
	e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "pinned", "channel_id": ch})
	p := e.req(http.MethodDelete, devicesPath+"/"+dev, e.adminTok, nil).problem(409, problemConflict)
	if !strings.Contains(p["detail"].(string), "queued") {
		t.Errorf("detail = %v", p["detail"])
	}
	p = e.req(http.MethodDelete, channelsPath+"/"+ch, e.adminTok, nil).problem(409, problemConflict)
	if !strings.Contains(p["detail"].(string), `has 1 messages in status "queued" or "sending"`) {
		t.Errorf("detail = %v", p["detail"])
	}
	// Cancel the message and the channel can go.
	id := e.get(messagesPath+"?channel_id="+ch, e.readTok).obj()["items"].([]any)[0].(map[string]any)["id"].(string)
	e.req(http.MethodDelete, messagesPath+"/"+id, e.sendTok, nil)
	if r := e.req(http.MethodDelete, channelsPath+"/"+ch, e.adminTok, nil); r.Status != 204 {
		t.Fatal(r)
	}
	e.req(http.MethodDelete, channelsPath+"/"+ch, e.adminTok, nil).problem(404, problemNotFound)
	if r := e.req(http.MethodDelete, devicesPath+"/"+dev, e.adminTok, nil); r.Status != 204 {
		t.Fatal(r)
	}
}
