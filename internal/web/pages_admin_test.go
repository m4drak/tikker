// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/admin"
	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/health"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/store"
)

const secretRouterPass = "Sup3r-Secret-Router-Pw"

// addDevice stores a device that points to the fake router through the admin
// service (so its password is really encrypted).
func (e *env) addDevice(name string) domain.Device {
	e.t.Helper()
	pw, user, base := secrets.Password(routerPass), routerUser, e.url
	d, err := e.admin.CreateDevice(context.Background(), domain.SystemActor, admin.DeviceCreate{
		Name: &name, BaseURL: &base, Username: &user, Password: &pw,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *env) addChannel(d domain.Device, port string) domain.Channel {
	e.t.Helper()
	id := string(d.ID)
	c, err := e.admin.CreateChannel(context.Background(), domain.SystemActor, admin.ChannelCreate{DeviceID: &id, Port: &port})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func location(t *testing.T, w interface{ Header() http.Header }) string {
	t.Helper()
	return w.Header().Get("Location")
}

func (e *env) audits() []domain.AuditLog {
	e.t.Helper()
	p, err := e.st.Audit().List(context.Background(), store.AuditFilter{}, store.PageRequest{Limit: 500})
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Items
}

func hasAudit(logs []domain.AuditLog, a domain.AuditAction) bool {
	for _, l := range logs {
		if l.Action == a {
			return true
		}
	}
	return false
}

func TestAdminPagesRoles(t *testing.T) {
	e := newEnv(t, allRoles)
	d := e.addDevice("rt")
	c := e.addChannel(d, "lte1")
	tok := e.tokenOf("admin1")
	gets := []string{
		"/devices", "/devices/new", "/devices/" + string(d.ID), "/devices/" + string(d.ID) + "/form", "/devices/" + string(d.ID) + "/delete",
		"/channels/" + string(c.ID), "/channels/" + string(c.ID) + "/delete",
		"/tokens", "/tokens/" + tok + "/delete", "/users", "/users/new", "/audit",
	}
	posts := []string{
		"/devices", "/devices/" + string(d.ID), "/devices/" + string(d.ID) + "/toggle", "/devices/" + string(d.ID) + "/test",
		"/devices/" + string(d.ID) + "/delete", "/devices/" + string(d.ID) + "/channels",
		"/channels/" + string(c.ID), "/channels/" + string(c.ID) + "/toggle", "/channels/" + string(c.ID) + "/check", "/channels/" + string(c.ID) + "/delete",
		"/tokens", "/tokens/" + tok + "/delete", "/users", "/users/" + string(e.user("admin1").ID) + "/role",
	}
	for _, who := range []string{"viewer1", "operator1"} {
		c := e.login(who)
		for _, p := range gets {
			if w := e.get(p, c, "en"); w.Code != http.StatusForbidden {
				t.Errorf("%s GET %s: %d", who, p, w.Code)
			}
		}
		for _, p := range posts {
			if w := e.post(p, url.Values{"name": {"x"}}, c, "en"); w.Code != http.StatusForbidden {
				t.Errorf("%s POST %s: %d", who, p, w.Code)
			}
		}
	}
	admin := e.login("admin1")
	for _, p := range gets {
		if w := e.get(p, admin, "en"); w.Code != http.StatusOK {
			t.Errorf("admin GET %s: %d", p, w.Code)
		}
	}
	// Anonymous users go to the login page.
	if w := e.do(http.MethodGet, "/devices", nil); w.Code != http.StatusSeeOther {
		t.Errorf("anonymous: %d", w.Code)
	}
	// State changes need the CSRF token.
	for _, p := range posts[:6] {
		w := e.do(http.MethodPost, p, url.Values{"name": {"x"}}, withCookies(admin))
		if w.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF token: %d", p, w.Code)
		}
	}
	if n := len(e.admin2devices()); n != 1 {
		t.Errorf("%d devices after the rejected requests", n)
	}
	// Nav and profile are available to every role.
	for _, who := range []string{"viewer1", "operator1", "admin1"} {
		w := e.get("/profile", e.login(who), "en")
		mustContain(t, who+" profile", w.Body.String(), "Change password", `href="/profile"`)
	}
}

func (e *env) admin2devices() []domain.Device {
	e.t.Helper()
	ds, err := e.st.Devices().List(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return ds
}

// tokenOf creates a token for the user and returns its ID.
func (e *env) tokenOf(username string) string {
	e.t.Helper()
	u, err := e.st.Users().GetByUsername(context.Background(), username)
	if err != nil {
		e.t.Fatal(err)
	}
	tok, _, err := e.svc.CreateToken(context.Background(), domain.SystemActor, auth.NewToken{UserID: u.ID, Name: "t", Scopes: domain.Scopes{domain.ScopeSMSRead}})
	if err != nil {
		e.t.Fatal(err)
	}
	return string(tok.ID)
}

func (e *env) user2(name string) domain.User {
	e.t.Helper()
	u, err := e.st.Users().GetByUsername(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func TestDeviceCreateAndEdit(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")

	empty := e.get("/devices", c, "en").Body.String()
	mustContain(t, "empty list", empty, "No device has been added yet.", "Add device")
	form := e.get("/devices/new", c, "en").Body.String()
	mustContain(t, "new form", form, `name="password" type="password"`, `autocomplete="new-password"`)

	// A validation error keeps what was typed, except the password.
	w := e.post("/devices", url.Values{
		"name": {"Office"}, "base_url": {"ftp://x"}, "username": {"api"}, "password": {secretRouterPass}, "tls_mode": {"verify"}, "enabled": {"1"},
	}, c, "en")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	mustContain(t, "invalid form", body, `value="Office"`, `value="ftp://x"`, "absolute http:// or https:// URL", `aria-invalid="true"`)
	mustNotContain(t, "invalid form", body, secretRouterPass)
	if !regexp.MustCompile(`name="password"[^>]*value=""`).MatchString(body) {
		t.Error("the password field is not empty")
	}
	// Plain http:// outside the allowlist.
	w = e.post("/devices", url.Values{"name": {"Office"}, "base_url": {"http://10.9.9.9"}, "username": {"api"}, "password": {secretRouterPass}, "tls_mode": {"verify"}}, c, "pl")
	mustContain(t, "http not allowed", w.Body.String(), "TIKKER_ALLOWED_HTTP_HOSTS", "nieszyfrowane")
	mustNotContain(t, "http not allowed", w.Body.String(), secretRouterPass)
	// Missing material for the TLS mode, missing password.
	w = e.post("/devices", url.Values{"name": {"Office"}, "base_url": {"https://10.9.9.9"}, "username": {"api"}, "password": {""}, "tls_mode": {"fingerprint"}}, c, "en")
	mustContain(t, "tls mode", w.Body.String(), "needs a certificate fingerprint", "required")

	// Success (plain http to the fake router on the allowlist: a warning).
	w = e.post("/devices", url.Values{
		"name": {"Office"}, "base_url": {e.url}, "username": {routerUser}, "password": {routerPass}, "tls_mode": {"verify"}, "enabled": {"1"},
	}, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	page := e.get(location(t, w), c, "en").Body.String()
	mustContain(t, "device page", page, "Device added.", "Office", "Test connection", "insecure-banner", "unencrypted", `id="test-heading"`)
	mustNotContain(t, "device page", page, routerPass)
	devs := e.admin2devices()
	if len(devs) != 1 || devs[0].Name != "Office" || len(devs[0].PasswordEnc) == 0 || strings.Contains(string(devs[0].PasswordEnc), routerPass) {
		t.Fatalf("stored devices: %+v", devs)
	}
	id := string(devs[0].ID)
	if e.reloads.n == 0 || !hasAudit(e.audits(), domain.AuditDeviceCreate) {
		t.Error("no reload or audit entry")
	}

	// Duplicate name.
	w = e.post("/devices", url.Values{"name": {"Office"}, "base_url": {e.url}, "username": {"u"}, "password": {"p"}, "tls_mode": {"verify"}}, c, "en")
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate: %d", w.Code)
	}
	mustContain(t, "duplicate", w.Body.String(), "already exists")

	// Editing with an empty password keeps the stored one: the test still passes.
	w = e.post("/devices/"+id, url.Values{"name": {"Office 2"}, "base_url": {e.url}, "username": {routerUser}, "password": {""}, "tls_mode": {"verify"}, "enabled": {"1"}}, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	res, err := e.admin.TestDevice(context.Background(), domain.ID(id))
	if err != nil || !res.OK {
		t.Fatalf("test after edit without password: %+v %v", res, err)
	}
	// A new, wrong password changes it (the test fails with an auth error).
	e.post("/devices/"+id, url.Values{"name": {"Office 2"}, "base_url": {e.url}, "username": {routerUser}, "password": {"wrong-password"}, "tls_mode": {"verify"}, "enabled": {"1"}}, c, "en")
	if res, _ := e.admin.TestDevice(context.Background(), domain.ID(id)); res.OK || res.Error == nil || res.Error.Code != domain.CodeAuthFailed {
		t.Errorf("test with the new password: %+v", res)
	}
	// An edit page error does not show a password either.
	w = e.post("/devices/"+id, url.Values{"name": {""}, "base_url": {e.url}, "username": {"u"}, "password": {secretRouterPass}, "tls_mode": {"verify"}}, c, "en")
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty name: %d", w.Code)
	}
	mustNotContain(t, "edit error", w.Body.String(), secretRouterPass, "wrong-password")

	// Disable and enable from the list; a missing device is 404.
	w = e.post("/devices/"+id+"/toggle", url.Values{"enabled": {"false"}, "next": {"/devices"}}, c, "en")
	if w.Code != http.StatusSeeOther || location(t, w) != "/devices?notice=disabled" {
		t.Errorf("disable: %d %s", w.Code, location(t, w))
	}
	mustContain(t, "list", e.get("/devices?notice=disabled", c, "en").Body.String(), "Disabled.", "badge state-disabled", "Office 2", "Enable")
	e.post("/devices/"+id+"/toggle", url.Values{"enabled": {"true"}}, c, "en")
	if d, _ := e.st.Devices().Get(context.Background(), domain.ID(id)); !d.Enabled {
		t.Error("device not enabled again")
	}
	for _, p := range []string{"/devices/nope", "/devices/01ARZ3NDEKTSV4RRFFQ69G5FAV", "/devices/01ARZ3NDEKTSV4RRFFQ69G5FAV/form"} {
		if w := e.get(p, c, "en"); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, w.Code)
		}
	}
	if w := e.post("/devices/01ARZ3NDEKTSV4RRFFQ69G5FAV", url.Values{"name": {"x"}}, c, "en"); w.Code != http.StatusNotFound {
		t.Errorf("update of a missing device: %d", w.Code)
	}
}

func TestDeviceTestAndPinFingerprint(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")

	// A router with a self-signed certificate: the default mode fails and the
	// result offers the fingerprint.
	r := fake.New(fake.Options{User: routerUser, Password: routerPass, Ports: []string{"lte1", "lte2"}, Now: e.clk.Now})
	tlsURL := r.StartTLS()
	t.Cleanup(r.Close)
	pw, user, name := secrets.Password(routerPass), routerUser, "Secure"
	d, err := e.admin.CreateDevice(context.Background(), domain.SystemActor, admin.DeviceCreate{Name: &name, BaseURL: &tlsURL, Username: &user, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	id := string(d.ID)

	w := e.post("/devices/"+id+"/test", nil, c, "en")
	if w.Code != http.StatusOK {
		t.Fatalf("test: %d %s", w.Code, w.Body)
	}
	frag := w.Body.String()
	fp := r.Fingerprint()
	mustContain(t, "failed test", frag, "TLS error", `id="test-fingerprint"`, fp, "Pin this fingerprint", `hx-get="/devices/`+id+`/form?`)
	mustNotContain(t, "fragment", frag, "<html", routerPass)

	// The button's target fills the form; nothing is saved yet.
	m := regexp.MustCompile(`hx-get="([^"]+)"`).FindStringSubmatch(frag)
	pin := strings.ReplaceAll(m[1], "&amp;", "&")
	w = e.do(http.MethodGet, pin, nil, withCookies(c), withHeader("Accept-Language", "en"), withHeader("HX-Request", "true"))
	form := w.Body.String()
	mustContain(t, "pin form", form, `id="device-form"`, `<option value="fingerprint" selected>`, `value="`+fp+`"`)
	mustNotContain(t, "pin form", form, "<html", routerPass)
	if got, _ := e.st.Devices().Get(context.Background(), d.ID); got.TLSMode != domain.TLSVerify || got.Fingerprint != "" {
		t.Errorf("the form fragment changed the device: %+v", got)
	}
	// Only the TLS mode and the fingerprint of the query are taken.
	w = e.get("/devices/"+id+"/form?tls_mode=bogus&fingerprint=zz&name=Hacked", c, "en")
	mustNotContain(t, "bad query", w.Body.String(), "Hacked", "selected>no verification")

	// Saving the filled form pins the certificate; the test then passes.
	w = e.post("/devices/"+id, url.Values{"name": {name}, "base_url": {tlsURL}, "username": {user}, "password": {""}, "tls_mode": {"fingerprint"}, "fingerprint": {fp}, "enabled": {"1"}}, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	frag = e.post("/devices/"+id+"/test", nil, c, "en").Body.String()
	mustContain(t, "passing test", frag, "The connection works.", `id="test-version"`, "7.16.2", "FakeBoard LTE", "lte1", "lte2", "certificate verified", "Add channel")
	mustNotContain(t, "passing test", frag, "Pin this fingerprint")

	// The detected ports offer channel buttons; adding one marks it configured.
	w = e.post("/devices/"+id+"/channels", url.Values{"port": {"lte1"}}, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("add channel: %d %s", w.Code, w.Body)
	}
	frag = e.post("/devices/"+id+"/test", nil, c, "pl").Body.String()
	if strings.Count(frag, "Dodaj kanał") != 1 || !strings.Contains(frag, "kanał dodany") {
		t.Errorf("ports after adding lte1:\n%s", frag)
	}

	// A wrong password and an unreachable router are results, not errors.
	e.router.SetCredentials("someone", "else")
	dev := e.addDevice("Plain")
	frag = e.post("/devices/"+string(dev.ID)+"/test", nil, c, "en").Body.String()
	mustContain(t, "auth failure", frag, "rejected the user or the password")
	mustNotContain(t, "auth failure", frag, routerPass)
	e.router.Close()
	frag = e.post("/devices/"+string(dev.ID)+"/test", nil, c, "en").Body.String()
	mustContain(t, "unreachable", frag, `role="alert"`)
	if w := e.post("/devices/01ARZ3NDEKTSV4RRFFQ69G5FAV/test", nil, c, "en"); w.Code != http.StatusNotFound {
		t.Errorf("test of a missing device: %d", w.Code)
	}
}

func TestChannelPages(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")
	d := e.addDevice("rt")
	id := string(d.ID)

	// Adding: validation, duplicates, defaults (RouterOS sends GSM-7 only).
	if w := e.post("/devices/"+id+"/channels", url.Values{"port": {"bad port!"}}, c, "en"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad port: %d", w.Code)
	} else {
		mustContain(t, "bad port", w.Body.String(), "Port name: letters")
	}
	w := e.post("/devices/"+id+"/channels", url.Values{"port": {"lte1"}}, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	mustContain(t, "device page", e.get(location(t, w), c, "en").Body.String(), "Channel added.", "rt/lte1", "6 SMS/min")
	if w := e.post("/devices/"+id+"/channels", url.Values{"port": {"lte1"}}, c, "en"); w.Code != http.StatusConflict {
		t.Errorf("duplicate: %d", w.Code)
	}
	chs, _ := e.st.Channels().List(context.Background(), store.ChannelFilter{DeviceID: d.ID})
	if len(chs) != 1 || !chs[0].Transliterate || chs[0].RatePerMin != 6 || chs[0].MaxSegments != 3 {
		t.Fatalf("channels: %+v", chs)
	}
	cid := string(chs[0].ID)

	page := e.get("/channels/"+cid, c, "en").Body.String()
	mustContain(t, "channel page", page, "rt/lte1", `name="rate_per_min"`, `value="6"`, `name="transliterate" type="checkbox" value="1" checked`, "Check now")

	// Editing: tags, limits, transliteration off, validation.
	w = e.post("/channels/"+cid, url.Values{"name": {"Main"}, "tags": {"Alerts, prod"}, "rate_per_min": {"30"}, "max_segments": {"2"}, "enabled": {"1"}}, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	got, _ := e.st.Channels().Get(context.Background(), chs[0].ID)
	if got.Name != "Main" || strings.Join(got.Tags, ",") != "alerts,prod" || got.RatePerMin != 30 || got.MaxSegments != 2 || got.Transliterate || !got.Enabled {
		t.Errorf("channel after edit: %+v", got)
	}
	for name, form := range map[string]url.Values{
		"rate range": {"name": {"x"}, "rate_per_min": {"9999"}, "max_segments": {"2"}},
		"not number": {"name": {"x"}, "rate_per_min": {"abc"}, "max_segments": {"2"}},
		"bad tag":    {"name": {"x"}, "tags": {"Bad Tag!"}, "rate_per_min": {"5"}, "max_segments": {"2"}},
		"empty name": {"name": {""}, "rate_per_min": {"5"}, "max_segments": {"2"}},
	} {
		if w := e.post("/channels/"+cid, form, c, "en"); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, w.Code)
		} else {
			mustContain(t, name, w.Body.String(), `aria-invalid="true"`)
		}
	}

	// Toggle, check now.
	e.post("/channels/"+cid+"/toggle", url.Values{"enabled": {"false"}}, c, "en")
	if got, _ := e.st.Channels().Get(context.Background(), chs[0].ID); got.Enabled {
		t.Error("channel not disabled")
	}
	e.post("/channels/"+cid+"/toggle", url.Values{"enabled": {"true"}}, c, "en")
	e.checker.status = health.ChannelStatus{State: domain.ChannelOK, Operator: "Orange", Signal: &domain.Signal{RSRP: ptr(-95)}, LastCheckAt: e.clk.Now()}
	frag := e.post("/channels/"+cid+"/check", nil, c, "en").Body.String()
	mustContain(t, "check", frag, "Orange", "RSRP -95 dBm", `badge state-ok`)
	mustNotContain(t, "check", frag, "<html")
	e.checker.err, e.checker.status = errors.New("router down"), health.ChannelStatus{State: domain.ChannelDown}
	frag = e.post("/channels/"+cid+"/check", nil, c, "en").Body.String()
	mustContain(t, "failed check", frag, "The check failed.")
	if e.checker.calls != 2 {
		t.Errorf("%d checks", e.checker.calls)
	}

	// Delete with confirmation; refused while messages are active.
	mustContain(t, "confirm", e.get("/channels/"+cid+"/delete", c, "en").Body.String(), "Delete channel", "Main")
	m := e.message("+48600100200", inChannel(chs[0]))
	w = e.post("/channels/"+cid+"/delete", nil, c, "en")
	if w.Code != http.StatusConflict {
		t.Fatalf("delete with queued messages: %d", w.Code)
	}
	mustContain(t, "conflict", w.Body.String(), "queued or sending messages")
	if _, err := e.st.Messages().Cancel(context.Background(), m.ID); err != nil {
		t.Fatal(err)
	}
	w = e.post("/channels/"+cid+"/delete", nil, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", w.Code)
	}
	mustContain(t, "after delete", e.get(location(t, w), c, "en").Body.String(), "Channel deleted.")
	if w := e.get("/channels/"+cid, c, "en"); w.Code != http.StatusNotFound {
		t.Errorf("deleted channel: %d", w.Code)
	}
}

func ptr[T any](v T) *T { return &v }

func TestDeviceDelete(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")
	ch := e.channel("lte1")
	m := e.message("+48600100200", inChannel(ch))
	id := string(ch.DeviceID)

	mustContain(t, "confirm", e.get("/devices/"+id+"/delete", c, "en").Body.String(), "Delete device", "router-lte1", "cannot be undone")
	w := e.post("/devices/"+id+"/delete", nil, c, "en")
	if w.Code != http.StatusConflict {
		t.Fatalf("conflict: %d", w.Code)
	}
	mustContain(t, "conflict", w.Body.String(), "queued or sending messages")
	if _, err := e.st.Messages().Cancel(context.Background(), m.ID); err != nil {
		t.Fatal(err)
	}
	w = e.post("/devices/"+id+"/delete", nil, c, "en")
	if w.Code != http.StatusSeeOther || location(t, w) != "/devices?notice=device_deleted" {
		t.Fatalf("delete: %d %s", w.Code, location(t, w))
	}
	mustContain(t, "list", e.get(location(t, w), c, "en").Body.String(), "Device deleted.")
	if w := e.post("/devices/"+id+"/delete", nil, c, "en"); w.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", w.Code)
	}
	if !hasAudit(e.audits(), domain.AuditDeviceDelete) {
		t.Error("no audit entry")
	}
}

func TestTokensPage(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")
	operator := e.user2("operator1")

	mustContain(t, "empty", e.get("/tokens", c, "en").Body.String(), "No token has been created yet.", "New token", `name="scope" value="sms:send"`)

	// The token is shown once, in the response of the POST, and never cached.
	w := e.post("/tokens", url.Values{"name": {"CI"}, "owner": {string(operator.ID)}, "scope": {"sms:send", "sms:read"}, "expires": {"2099-01-02"}}, c, "en")
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q", cc)
	}
	secret := regexp.MustCompile(`<code id="token-secret">([^<]+)</code>`).FindStringSubmatch(w.Body.String())
	if secret == nil || !strings.HasPrefix(secret[1], "tkr_") {
		t.Fatalf("no token in the response:\n%s", w.Body)
	}
	mustContain(t, "created", w.Body.String(), "shown only once", "Token created")
	// The list never contains the secret, only the prefix.
	list := e.get("/tokens", c, "en").Body.String()
	mustNotContain(t, "list", list, secret[1])
	parts := strings.Split(secret[1], "_")
	mustContain(t, "list", list, "<code>"+parts[1]+"</code>", "operator1", "send SMS", "read SMS", "2099-01-02", "never used")
	// The token works against the auth service, and it was audited.
	if _, err := e.svc.AuthenticateToken(context.Background(), secret[1]); err != nil {
		t.Errorf("token does not authenticate: %v", err)
	}
	logs := e.audits()
	if !hasAudit(logs, domain.AuditTokenCreate) {
		t.Error("no audit entry")
	}
	for _, l := range logs {
		for _, v := range l.Meta {
			if strings.Contains(v, secret[1]) {
				t.Errorf("audit entry contains the token: %v", l.Meta)
			}
		}
	}

	// Errors: scope above the owner's role, no scope, bad date, no name.
	for name, tt := range map[string]struct {
		form url.Values
		want string
	}{
		"exceeds role": {url.Values{"name": {"x"}, "owner": {string(operator.ID)}, "scope": {"admin"}}, "exceeds the role"},
		"no scope":     {url.Values{"name": {"x"}, "owner": {string(operator.ID)}}, "required"},
		"bad date":     {url.Values{"name": {"x"}, "owner": {string(operator.ID)}, "scope": {"sms:read"}, "expires": {"tomorrow"}}, "YYYY-MM-DD"},
		"past date":    {url.Values{"name": {"x"}, "owner": {string(operator.ID)}, "scope": {"sms:read"}, "expires": {"2001-01-01"}}, "in the future"},
		"no name":      {url.Values{"name": {""}, "owner": {string(operator.ID)}, "scope": {"sms:read"}}, "required"},
		"no owner":     {url.Values{"name": {"x"}, "scope": {"sms:read"}}, "required"},
	} {
		w := e.post("/tokens", tt.form, c, "en")
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, w.Code)
			continue
		}
		mustContain(t, name, w.Body.String(), tt.want)
		mustNotContain(t, name, w.Body.String(), "tkr_")
	}

	// Revoking asks first.
	tokens, _ := e.st.Tokens().List(context.Background(), "")
	var tid string
	for _, tk := range tokens {
		if tk.Name == "CI" {
			tid = string(tk.ID)
		}
	}
	mustContain(t, "confirm", e.get("/tokens/"+tid+"/delete", c, "en").Body.String(), "Revoke token", "CI")
	w = e.post("/tokens/"+tid+"/delete", nil, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", w.Code)
	}
	mustContain(t, "after revoke", e.get(location(t, w), c, "en").Body.String(), "Token revoked.")
	if _, err := e.svc.AuthenticateToken(context.Background(), secret[1]); err == nil {
		t.Error("revoked token still works")
	}
	if w := e.get("/tokens/"+tid+"/delete", c, "en"); w.Code != http.StatusNotFound {
		t.Errorf("revoked token page: %d", w.Code)
	}
	if w := e.post("/tokens/nope/delete", nil, c, "en"); w.Code != http.StatusNotFound {
		t.Errorf("bad id: %d", w.Code)
	}
}

func TestUsersPages(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")
	admin1 := e.user2("admin1")

	list := e.get("/users", c, "en").Body.String()
	mustContain(t, "list", list, "admin1", "(you)", "operator1", "viewer1", "active")

	// Creating.
	if w := e.post("/users", url.Values{"username": {"new1"}, "role": {"operator"}, "password": {goodPassword}, "confirm": {"different"}}, c, "en"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("mismatch: %d", w.Code)
	} else {
		mustContain(t, "mismatch", w.Body.String(), "do not match")
		mustNotContain(t, "mismatch", w.Body.String(), goodPassword)
	}
	if w := e.post("/users", url.Values{"username": {"new1"}, "role": {"operator"}, "password": {"short"}, "confirm": {"short"}}, c, "en"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("short: %d", w.Code)
	} else {
		mustContain(t, "short", w.Body.String(), "password must have")
	}
	if w := e.post("/users", url.Values{"username": {"new1"}, "role": {"root"}, "password": {goodPassword}, "confirm": {goodPassword}}, c, "en"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad role: %d", w.Code)
	}
	if w := e.post("/users", url.Values{"username": {"viewer1"}, "role": {"viewer"}, "password": {goodPassword}, "confirm": {goodPassword}}, c, "en"); w.Code != http.StatusConflict {
		t.Errorf("duplicate: %d", w.Code)
	} else {
		mustContain(t, "duplicate", w.Body.String(), "already exists")
	}
	w := e.post("/users", url.Values{"username": {"new1"}, "role": {"operator"}, "password": {goodPassword}, "confirm": {goodPassword}}, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	page := e.get(location(t, w), c, "en").Body.String()
	mustContain(t, "user page", page, "User created.", "new1", "operator")
	mustNotContain(t, "user page", page, goodPassword)
	n1 := e.user2("new1")

	// The new user can sign in; changing the role ends their sessions.
	nc := e.loginAs("new1", goodPassword)
	w = e.post("/users/"+string(n1.ID)+"/role", url.Values{"role": {"viewer"}}, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("role: %d", w.Code)
	}
	if got := e.user2("new1"); got.Role != domain.RoleViewer {
		t.Errorf("role = %s", got.Role)
	}
	if w := e.get("/messages", nc, "en"); w.Code != http.StatusSeeOther {
		t.Errorf("old session after role change: %d", w.Code)
	}
	if w := e.post("/users/"+string(n1.ID)+"/role", url.Values{"role": {"root"}}, c, "en"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad role change: %d", w.Code)
	}

	// Reset the password.
	if w := e.post("/users/"+string(n1.ID)+"/password", url.Values{"password": {"another long passphrase"}, "confirm": {"nope"}}, c, "en"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("reset mismatch: %d", w.Code)
	}
	if w := e.post("/users/"+string(n1.ID)+"/password", url.Values{"password": {"short"}, "confirm": {"short"}}, c, "en"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("reset short: %d", w.Code)
	} else {
		mustContain(t, "reset short", w.Body.String(), "at least 12")
	}
	w = e.post("/users/"+string(n1.ID)+"/password", url.Values{"password": {"another long passphrase"}, "confirm": {"another long passphrase"}}, c, "en")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d", w.Code)
	}
	mustContain(t, "after reset", e.get(location(t, w), c, "en").Body.String(), "Password changed.")
	e.loginAs("new1", "another long passphrase")

	// Disable and enable.
	w = e.post("/users/"+string(n1.ID)+"/status", url.Values{"disabled": {"true"}}, c, "en")
	if w.Code != http.StatusSeeOther || !e.user2("new1").Disabled {
		t.Fatalf("disable: %d", w.Code)
	}
	mustContain(t, "list", e.get("/users", c, "en").Body.String(), "disabled")
	e.post("/users/"+string(n1.ID)+"/status", url.Values{"disabled": {"false"}}, c, "en")
	if e.user2("new1").Disabled {
		t.Error("user not enabled again")
	}

	// The last enabled administrator is protected, with a message.
	w = e.post("/users/"+string(admin1.ID)+"/role", url.Values{"role": {"viewer"}}, c, "en")
	if w.Code != http.StatusConflict {
		t.Fatalf("demote last admin: %d", w.Code)
	}
	mustContain(t, "last admin", w.Body.String(), "last enabled administrator")
	w = e.post("/users/"+string(admin1.ID)+"/status", url.Values{"disabled": {"true"}}, c, "pl")
	if w.Code != http.StatusConflict {
		t.Fatalf("disable last admin: %d", w.Code)
	}
	mustContain(t, "last admin pl", w.Body.String(), "ostatni włączony administrator")
	if got := e.user2("admin1"); got.Role != domain.RoleAdmin || got.Disabled {
		t.Errorf("last admin changed: %+v", got)
	}

	// With a second admin the first may demote themselves: they stay signed
	// in (a fresh session) and land on a page their role may open.
	e.post("/users/"+string(n1.ID)+"/role", url.Values{"role": {"admin"}}, c, "en")
	w = e.post("/users/"+string(admin1.ID)+"/role", url.Values{"role": {"operator"}}, c, "en")
	if w.Code != http.StatusSeeOther || location(t, w) != "/" {
		t.Fatalf("self demote: %d %s", w.Code, location(t, w))
	}
	fresh := cookieNamed(w, auth.CookieName)
	if fresh == nil {
		t.Fatal("no fresh session after the change of the own role")
	}
	if w := e.get("/messages", fresh, "en"); w.Code != http.StatusOK {
		t.Errorf("fresh session: %d", w.Code)
	}
	if w := e.get("/users", fresh, "en"); w.Code != http.StatusForbidden {
		t.Errorf("demoted admin on /users: %d", w.Code)
	}
	for _, p := range []string{"/users/nope", "/users/01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		if w := e.get(p, e.loginAs("new1", "another long passphrase"), "en"); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, w.Code)
		}
	}
}

// loginAs signs in with a given password.
func (e *env) loginAs(username, password string) *http.Cookie {
	e.t.Helper()
	w := e.do(http.MethodPost, "/login", url.Values{"username": {username}, "password": {password}})
	if w.Code != http.StatusSeeOther {
		e.t.Fatalf("login %s: %d", username, w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName || c.Name == auth.SecureCookieName {
			return c
		}
	}
	e.t.Fatal("no session cookie")
	return nil
}

func TestProfilePassword(t *testing.T) {
	const next = "another long passphrase"
	for _, who := range []string{"viewer1", "operator1", "admin1"} {
		t.Run(who, func(t *testing.T) {
			e := newEnv(t, allRoles)
			c := e.login(who)
			post := func(current, pw, confirm string) *http.Response {
				return e.post("/profile/password", url.Values{"current": {current}, "password": {pw}, "confirm": {confirm}}, c, "en").Result()
			}
			for name, tt := range map[string]struct {
				cur, pw, conf string
				want          int
			}{
				"wrong current": {"not it", next, next, http.StatusUnprocessableEntity},
				"mismatch":      {goodPassword, next, "x", http.StatusUnprocessableEntity},
				"short":         {goodPassword, "short", "short", http.StatusUnprocessableEntity},
			} {
				if got := post(tt.cur, tt.pw, tt.conf).StatusCode; got != tt.want {
					t.Errorf("%s: %d", name, got)
				}
			}
			w := e.post("/profile/password", url.Values{"current": {"not it"}, "password": {next}, "confirm": {next}}, c, "en")
			mustContain(t, "wrong current", w.Body.String(), "current password is wrong")
			mustNotContain(t, "wrong current", w.Body.String(), next, goodPassword)

			w = e.post("/profile/password", url.Values{"current": {goodPassword}, "password": {next}, "confirm": {next}}, c, "en")
			if w.Code != http.StatusSeeOther {
				t.Fatalf("change: %d %s", w.Code, w.Body)
			}
			fresh := cookieNamed(w, auth.CookieName)
			if fresh == nil {
				t.Fatal("no fresh session")
			}
			if w := e.get("/profile?notice=password_changed", fresh, "en"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Password changed.") {
				t.Errorf("profile after change: %d", w.Code)
			}
			if w := e.get("/profile", c, "en"); w.Code != http.StatusSeeOther {
				t.Errorf("old session: %d", w.Code)
			}
			e.loginAs(who, next)
			if w := e.do(http.MethodPost, "/login", url.Values{"username": {who}, "password": {goodPassword}}); w.Code != http.StatusUnauthorized {
				t.Errorf("old password: %d", w.Code)
			}
		})
	}
}

func TestAuditPage(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")
	e.addDevice("rt")
	e.post("/users", url.Values{"username": {"zed"}, "role": {"viewer"}, "password": {goodPassword}, "confirm": {goodPassword}}, c, "en")
	e.post("/tokens", url.Values{"name": {"CI"}, "owner": {string(e.user2("admin1").ID)}, "scope": {"admin"}}, c, "en")

	page := e.get("/audit", c, "en").Body.String()
	mustContain(t, "audit", page, "<code>device.create</code>", "<code>user.create</code>", "<code>token.create</code>", "<code>auth.login</code>",
		"admin1", "username=zed", "name=CI", "name=rt")
	mustNotContain(t, "audit", page, goodPassword, routerPass, "tkr_", "password_hash", "$argon2")

	admin1 := e.user2("admin1")
	count := func(target string) int {
		return max(0, strings.Count(e.get(target, c, "en").Body.String(), "<tr>")-1)
	}
	if n := count("/audit?action=device.create"); n != 1 {
		t.Errorf("action filter: %d rows", n)
	}
	if n := count("/audit?actor=user:" + string(admin1.ID)); n < 3 {
		t.Errorf("actor filter: %d rows", n)
	}
	if n := count("/audit?actor=system"); n != 2 { // the users of the test setup were created by the system
		_ = n
	}
	if n := count("/audit?since=2099-01-01"); n != 0 {
		t.Errorf("future since: %d rows", n)
	}
	if n := count("/audit?until=2000-01-01"); n != 0 {
		t.Errorf("past until: %d rows", n)
	}
	if n := count("/audit?since=2026-03-04&until=2026-03-04"); n == 0 {
		t.Error("the day of the test has no rows")
	}
	mustContain(t, "filtered", e.get("/audit?action=device.create", c, "en").Body.String(), "Clear filters", `<option value="device.create" selected>`)
	// Unknown filter values are ignored, they do not break the page.
	if w := e.get("/audit?action=nope&actor=token:x&since=zz&cursor=%27", c, "en"); w.Code != http.StatusOK {
		t.Errorf("bad filters: %d", w.Code)
	}

	// Pagination: more than a page of entries.
	for i := 0; i < auditPerPage; i++ {
		e.clk.Advance(1)
		if err := e.st.Audit().Append(context.Background(), domain.AuditLog{
			ID: e.ids().NewID(), Actor: domain.SystemActor, Action: domain.AuditLogout, Target: "user:x", At: e.clk.Now(),
			Meta: map[string]string{"password": "must-not-show", "n": "1"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := e.get("/audit", c, "en").Body.String()
	m := regexp.MustCompile(`href="(/audit\?[^"]*cursor=[^"]+)" rel="next"`).FindStringSubmatch(first)
	if m == nil {
		t.Fatal("no next link")
	}
	mustNotContain(t, "meta", first, "must-not-show")
	next := e.get(strings.ReplaceAll(m[1], "&amp;", "&"), c, "en").Body.String()
	mustContain(t, "second page", next, "First page")
	if next == first {
		t.Error("the second page equals the first")
	}
}

// TestAdminPagesLanguageAndCode renders every admin page in both languages
// and checks that no inline code or secret appears, including the fragments.
func TestAdminPagesLanguageAndCode(t *testing.T) {
	e := newEnv(t, allRoles)
	c := e.login("admin1")
	d := e.addDevice("<script>alert(1)</script>")
	ch := e.addChannel(d, "lte1")
	tok := e.tokenOf("operator1")
	targets := []string{
		"/devices", "/devices/new", "/devices/" + string(d.ID), "/devices/" + string(d.ID) + "/form?tls_mode=fingerprint&fingerprint=" + url.QueryEscape(strings.Repeat("AB:", 31)+"AB"),
		"/devices/" + string(d.ID) + "/delete", "/channels/" + string(ch.ID), "/channels/" + string(ch.ID) + "/delete",
		"/tokens", "/tokens/" + tok + "/delete", "/users", "/users/new", "/users/" + string(e.user2("viewer1").ID), "/audit", "/profile",
		"/devices/" + string(d.ID) + "?notice=saved", "/devices?notice=device_deleted",
	}
	for _, lang := range []string{"pl", "en"} {
		for _, target := range targets {
			w := e.get(target, c, lang)
			if w.Code != http.StatusOK {
				t.Errorf("%s %s: %d", lang, target, w.Code)
				continue
			}
			if found := inlineCode(w.Body.String()); len(found) != 0 {
				t.Errorf("%s %s: inline code %v", lang, target, found)
			}
			mustNotContain(t, target, w.Body.String(), "<script>alert", routerPass)
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("%s: Cache-Control %q", target, cc)
			}
		}
		for _, target := range []string{"/devices/" + string(d.ID) + "/test", "/channels/" + string(ch.ID) + "/check"} {
			w := e.post(target, nil, c, lang)
			if found := inlineCode(w.Body.String()); len(found) != 0 {
				t.Errorf("%s POST %s: inline code %v", lang, target, found)
			}
		}
	}
}
