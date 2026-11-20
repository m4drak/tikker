// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

const (
	tokensPath = "/api/v1/tokens"
	usersPath  = "/api/v1/users"
)

func TestTokens(t *testing.T) {
	e := newEnv(t)
	res := e.req(http.MethodPost, tokensPath, e.adminTok, map[string]any{
		"name": "CRM", "scopes": []string{"sms:send", "sms:read"}, "user_id": string(e.operator.ID),
		"expires_at": "2027-11-25T00:00:00Z",
	})
	if res.Status != http.StatusCreated {
		t.Fatal(res)
	}
	created := res.obj()
	secret := created["token"].(string)
	if !strings.HasPrefix(secret, "tkr_") || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("token = %q, Cache-Control = %q", secret, res.Header.Get("Cache-Control"))
	}
	id := created["id"].(string)
	if created["user_id"] != string(e.operator.ID) || created["expires_at"] != "2027-11-25T00:00:00Z" || created["last_used_at"] != nil ||
		created["prefix"] != strings.Split(secret, "_")[1] || !reflect.DeepEqual(created["scopes"], []any{"sms:send", "sms:read"}) {
		t.Errorf("created = %v", created)
	}
	if res.Header.Get("Location") != "/api/v1/tokens/"+id {
		t.Errorf("Location = %q", res.Header.Get("Location"))
	}

	// The new token works.
	if r := e.req(http.MethodPost, messagesPath, secret, `{}`); r.Status == http.StatusUnauthorized {
		t.Fatal("new token rejected")
	}

	// The secret is shown once: reads never contain it.
	for _, p := range []string{tokensPath, tokensPath + "/" + id, tokensPath + "?user_id=" + string(e.operator.ID)} {
		r := e.get(p, e.adminTok)
		if r.Status != 200 || strings.Contains(string(r.Body), secret) || strings.Contains(string(r.Body), `"token"`) {
			t.Errorf("GET %s leaks the secret or fails: %s", p, r)
		}
	}
	if items := e.get(tokensPath+"?user_id="+string(e.admin.ID), e.adminTok).obj()["items"].([]any); len(items) != 1 {
		t.Errorf("filter by user: %v", items)
	}
	e.get(tokensPath+"/nope", e.adminTok).problem(404, problemNotFound)

	// Owner defaults to the caller.
	res = e.req(http.MethodPost, tokensPath, e.adminTok, map[string]any{"name": "mine", "scopes": []string{"admin"}})
	if res.Status != 201 || res.obj()["user_id"] != string(e.admin.ID) {
		t.Errorf("default owner: %s", res)
	}

	// Revocation is immediate; a token may revoke itself.
	if r := e.req(http.MethodDelete, tokensPath+"/"+id, e.adminTok, nil); r.Status != 204 {
		t.Fatal(r)
	}
	e.req(http.MethodPost, messagesPath, secret, `{}`).problem(401, problemUnauthorized)
	e.req(http.MethodDelete, tokensPath+"/"+id, e.adminTok, nil).problem(404, problemNotFound)
	self := tokenID(t, e, e.adminTok)
	if r := e.req(http.MethodDelete, tokensPath+"/"+self, e.adminTok, nil); r.Status != 204 {
		t.Fatal(r)
	}
	e.get(tokensPath, e.adminTok).problem(401, problemUnauthorized)
}

func TestTokenValidation(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name   string
		body   any
		status int
		codes  []string
	}{
		{"nothing", map[string]any{}, 422, []string{"/name:required", "/scopes:required"}},
		{"empty scopes", map[string]any{"name": "n", "scopes": []string{}}, 422, []string{"/scopes:required"}},
		{"unknown scope", map[string]any{"name": "n", "scopes": []string{"root"}}, 422, []string{"/scopes/0:invalid_format"}},
		{"duplicate scope", map[string]any{"name": "n", "scopes": []string{"admin", "admin"}}, 422, []string{"/scopes/1:invalid_format"}},
		{"exceeds role", map[string]any{"name": "n", "scopes": []string{"admin"}, "user_id": string(e.viewer.ID)}, 422, []string{"/scopes/0:scope_exceeds_role"}},
		{"send exceeds viewer", map[string]any{"name": "n", "scopes": []string{"sms:send"}, "user_id": string(e.viewer.ID)}, 422, []string{"/scopes/0:scope_exceeds_role"}},
		{"unknown user", map[string]any{"name": "n", "scopes": []string{"sms:read"}, "user_id": "nope"}, 422, []string{"/user_id:unknown_user"}},
		{"long name", map[string]any{"name": strings.Repeat("n", 65), "scopes": []string{"sms:read"}}, 422, []string{"/name:out_of_range"}},
		{"past expiry", map[string]any{"name": "n", "scopes": []string{"sms:read"}, "expires_at": "2026-01-01T00:00:00Z"}, 422, []string{"/expires_at:expires_at_in_past"}},
		{"bad expiry", map[string]any{"name": "n", "scopes": []string{"sms:read"}, "expires_at": "soon"}, 422, []string{"/expires_at:invalid_format"}},
		{"bad user id", map[string]any{"name": "n", "scopes": []string{"sms:read"}, "user_id": strings.Repeat("u", 65)}, 422, []string{"/user_id:invalid_format"}},
		{"unknown member", map[string]any{"name": "n", "scopes": []string{"sms:read"}, "secret": "x"}, 400, nil},
		{"scopes type", `{"name": "n", "scopes": "admin"}`, 400, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			typ := problemValidation
			if tc.status == 400 {
				typ = problemMalformed
			}
			p := e.req(http.MethodPost, tokensPath, e.adminTok, tc.body).problem(tc.status, typ)
			if tc.codes != nil {
				if got := problemCodes(p); !reflect.DeepEqual(got, tc.codes) {
					t.Errorf("errors = %v, want %v", got, tc.codes)
				}
			}
		})
	}
}

func TestTokenListPaging(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 4; i++ {
		e.mkToken(e.viewer, domain.ScopeSMSRead)
	}
	var ids []string
	cursor := ""
	for i := 0; i < 10; i++ {
		q := tokensPath + "?limit=3"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		m := e.get(q, e.adminTok).obj()
		for _, it := range m["items"].([]any) {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		if m["next_cursor"] == nil {
			break
		}
		cursor = m["next_cursor"].(string)
	}
	if len(ids) != 7 {
		t.Fatalf("%d tokens", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] <= ids[i] {
			t.Errorf("tokens are not newest first: %v", ids)
		}
	}
}

func TestUsers(t *testing.T) {
	e := newEnv(t)
	res := e.req(http.MethodPost, usersPath, e.adminTok, map[string]any{"username": "jkowalska", "password": "correct-horse-battery-staple", "role": "operator"})
	if res.Status != http.StatusCreated {
		t.Fatal(res)
	}
	u := res.obj()
	id := u["id"].(string)
	if u["username"] != "jkowalska" || u["role"] != "operator" || u["disabled"] != false || strings.Contains(string(res.Body), "staple") ||
		strings.Contains(string(res.Body), "argon") {
		t.Errorf("created = %s", res.Body)
	}
	if res.Header.Get("Location") != "/api/v1/users/"+id {
		t.Errorf("Location = %q", res.Header.Get("Location"))
	}
	e.req(http.MethodPost, usersPath, e.adminTok, map[string]any{"username": "JKowalska", "password": "correct-horse-battery-staple", "role": "viewer"}).problem(409, problemConflict)

	list := e.get(usersPath, e.adminTok)
	if items := list.obj()["items"].([]any); len(items) != 4 || strings.Contains(string(list.Body), "argon") {
		t.Errorf("list = %s", list)
	}
	if r := e.get(usersPath+"/"+id, e.adminTok); r.Status != 200 {
		t.Fatal(r)
	}
	e.get(usersPath+"/nope", e.adminTok).problem(404, problemNotFound)

	// Demote, reset the password, disable.
	if r := e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, map[string]any{"role": "viewer"}); r.Status != 200 || r.obj()["role"] != "viewer" {
		t.Fatal(r)
	}
	res = e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, map[string]any{"password": "another-long-passphrase"})
	if res.Status != 200 || strings.Contains(string(res.Body), "passphrase") {
		t.Fatal(res)
	}
	tok := e.mkToken(mustUser(t, e, id), domain.ScopeSMSRead)
	if r := e.get(messagesPath, tok); r.Status != 200 {
		t.Fatal(r)
	}
	if r := e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, map[string]any{"disabled": true}); r.Status != 200 || r.obj()["disabled"] != true {
		t.Fatal(r)
	}
	// A disabled user's tokens stop working at once.
	e.get(messagesPath, tok).problem(401, problemUnauthorized)

	// Validation.
	for name, tc := range map[string]struct {
		body   any
		status int
	}{
		"nothing":       {map[string]any{}, 422},
		"weak password": {map[string]any{"username": "valid.name", "password": "short", "role": "viewer"}, 422},
		"bad username":  {map[string]any{"username": "no spaces", "password": "correct-horse-battery-staple", "role": "viewer"}, 422},
		"bad role":      {map[string]any{"username": "valid.name", "password": "correct-horse-battery-staple", "role": "root"}, 422},
		"unknown":       {map[string]any{"username": "valid.name", "password": "correct-horse-battery-staple", "role": "viewer", "x": 1}, 400},
		"password type": {`{"username":"valid.name","password":1,"role":"viewer"}`, 400},
	} {
		t.Run(name, func(_ *testing.T) {
			typ := problemValidation
			if tc.status == 400 {
				typ = problemMalformed
			}
			e.req(http.MethodPost, usersPath, e.adminTok, tc.body).problem(tc.status, typ)
		})
	}
	e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, `{}`).problem(422, problemValidation)
	e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, `{"role": "root"}`).problem(422, problemValidation)
	e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, `{"role": null}`).problem(422, problemValidation)
	e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, `{"username": "x"}`).problem(400, problemMalformed)
	e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, `{"disabled": "yes"}`).problem(400, problemMalformed)
	e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, `{"password": "short"}`).problem(422, problemValidation)
	e.req(http.MethodPatch, usersPath+"/nope", e.adminTok, `{"disabled": true}`).problem(404, problemNotFound)
}

func mustUser(t *testing.T, e *testEnv, id string) domain.User {
	t.Helper()
	u, err := e.st.Users().Get(e.ctx, domain.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLastAdminIsProtected(t *testing.T) {
	e := newEnv(t)
	id := string(e.admin.ID)
	e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, map[string]any{"role": "viewer"}).problem(409, problemConflict)
	e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, map[string]any{"disabled": true}).problem(409, problemConflict)
	// With a second admin the first can be demoted.
	res := e.req(http.MethodPost, usersPath, e.adminTok, map[string]any{"username": "second", "password": "correct-horse-battery-staple", "role": "admin"})
	if res.Status != 201 {
		t.Fatal(res)
	}
	if r := e.req(http.MethodPatch, usersPath+"/"+id, e.adminTok, map[string]any{"role": "viewer"}); r.Status != 200 {
		t.Fatal(r)
	}
	// The demoted admin's token lost its admin rights (role intersection).
	e.get(usersPath, e.adminTok).problem(403, problemForbidden)
}

// ---------------------------------------------------------------------------
// Authentication, scopes, roles, rate limit

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	e.basic()
	paths := []struct{ method, path string }{
		{"GET", messagesPath}, {"POST", messagesPath}, {"GET", channelsPath}, {"GET", devicesPath}, {"GET", tokensPath}, {"GET", usersPath},
		{"DELETE", messagesPath + "/01ARZ3NDEKTSV4RRFFQ69G5FAV"}, {"POST", devicesPath + "/x/test"},
	}
	for _, p := range paths {
		for name, tok := range map[string]string{"none": "", "garbage": "nope", "unknown": "tkr_abcdefgh_" + strings.Repeat("A", 32)} {
			t.Run(p.method+p.path+"/"+name, func(t *testing.T) {
				res := e.req(p.method, p.path, tok, nil)
				res.problem(401, problemUnauthorized)
				if !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Bearer") {
					t.Errorf("WWW-Authenticate = %q", res.Header.Get("WWW-Authenticate"))
				}
			})
		}
	}
	// A wrong scheme is not accepted.
	r := e.req("GET", messagesPath, "", nil, "Authorization", "Basic "+e.readTok)
	r.problem(401, problemUnauthorized)
}

func TestScopes(t *testing.T) {
	e := newEnv(t)
	e.basic()
	msg := e.req(http.MethodPost, messagesPath, e.sendTok, map[string]any{"to": "600100200", "body": "x"}).obj()["id"].(string)
	body := map[string]any{"to": "600100200", "body": "x"}

	type op struct {
		method, path string
		body         any
	}
	send := []op{{"POST", messagesPath, body}, {"POST", messagesPath + "/batch", map[string]any{"messages": []any{body}}}, {"DELETE", messagesPath + "/" + msg, nil}}
	read := []op{{"GET", messagesPath, nil}, {"GET", messagesPath + "/" + msg, nil}, {"GET", channelsPath, nil}}
	admin := []op{
		{"POST", channelsPath, map[string]any{}}, {"PATCH", channelsPath + "/x", `{"name":"x"}`}, {"DELETE", channelsPath + "/x", nil},
		{"GET", devicesPath, nil}, {"POST", devicesPath, map[string]any{}}, {"GET", devicesPath + "/x", nil},
		{"PATCH", devicesPath + "/x", `{"name":"x"}`}, {"DELETE", devicesPath + "/x", nil}, {"POST", devicesPath + "/x/test", nil},
		{"GET", tokensPath, nil}, {"POST", tokensPath, map[string]any{}}, {"GET", tokensPath + "/x", nil}, {"DELETE", tokensPath + "/x", nil},
		{"GET", usersPath, nil}, {"POST", usersPath, map[string]any{}}, {"GET", usersPath + "/x", nil}, {"PATCH", usersPath + "/x", `{"disabled":true}`},
	}
	other := e.mkToken(e.operator, domain.ScopeSMSRead) // operator role, but only the read scope
	tests := []struct {
		who     string
		tok     string
		allowed [][]op
	}{
		{"read-only viewer", e.readTok, [][]op{read}},
		{"read-only token of an operator", other, [][]op{read}},
		{"operator", e.sendTok, [][]op{read, send}},
		{"admin", e.adminTok, [][]op{read, send, admin}},
	}
	for _, tc := range tests {
		allowed := map[string]bool{}
		for _, group := range tc.allowed {
			for _, o := range group {
				allowed[o.method+" "+o.path] = true
			}
		}
		for _, group := range [][]op{send, read, admin} {
			for _, o := range group {
				t.Run(tc.who+"/"+o.method+" "+o.path, func(t *testing.T) {
					res := e.req(o.method, o.path, tc.tok, o.body)
					if allowed[o.method+" "+o.path] {
						if res.Status == http.StatusForbidden || res.Status == http.StatusUnauthorized {
							t.Errorf("must be allowed: %s", res)
						}
						return
					}
					p := res.problem(403, problemForbidden)
					if !strings.Contains(p["detail"].(string), "lacks required scope") {
						t.Errorf("detail = %v", p["detail"])
					}
				})
			}
		}
	}
	// Demoting the owner narrows the token at once.
	e.req(http.MethodPatch, usersPath+"/"+string(e.operator.ID), e.adminTok, map[string]any{"role": "viewer"})
	e.req(http.MethodPost, messagesPath, e.sendTok, body).problem(403, problemForbidden)
	if r := e.get(messagesPath, e.sendTok); r.Status != 200 {
		t.Errorf("read must stay allowed: %s", r)
	}
}

func TestExpiredToken(t *testing.T) {
	e := newEnv(t)
	res := e.req(http.MethodPost, tokensPath, e.adminTok, map[string]any{"name": "short", "scopes": []string{"sms:read"}, "expires_at": "2026-11-19T13:00:00Z", "user_id": string(e.viewer.ID)})
	tok := res.obj()["token"].(string)
	if r := e.get(messagesPath, tok); r.Status != 200 {
		t.Fatal(r)
	}
	e.fake.Advance(2 * time.Hour)
	e.get(messagesPath, tok).problem(401, problemUnauthorized)
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.opts.RateLimit = RateLimit{Requests: 3, Window: time.Minute} })
	for i, want := range []string{"2", "1", "0"} {
		r := e.get(messagesPath, e.readTok)
		if r.Status != 200 || r.Header.Get("RateLimit-Limit") != "3" || r.Header.Get("RateLimit-Remaining") != want {
			t.Fatalf("request %d: %d limit=%s remaining=%s", i+1, r.Status, r.Header.Get("RateLimit-Limit"), r.Header.Get("RateLimit-Remaining"))
		}
	}
	r := e.get(messagesPath, e.readTok)
	p := r.problem(429, problemRateLimited)
	if r.Header.Get("Retry-After") != "20" || r.Header.Get("RateLimit-Remaining") != "0" || r.Header.Get("RateLimit-Reset") != "60" {
		t.Errorf("headers = %v", r.Header)
	}
	if !strings.Contains(p["detail"].(string), "retry in 20 s") {
		t.Errorf("detail = %v", p["detail"])
	}
	// Another token has its own budget.
	if r := e.get(messagesPath, e.sendTok); r.Status != 200 {
		t.Errorf("other token: %s", r)
	}
	// A token without the scope is charged too.
	e.fake.Advance(20 * time.Second)
	e.req(http.MethodPost, messagesPath, e.readTok, `{}`).problem(403, problemForbidden)
	e.get(messagesPath, e.readTok).problem(429, problemRateLimited)
	// The bucket refills.
	e.fake.Advance(time.Minute)
	if r := e.get(messagesPath, e.readTok); r.Status != 200 || r.Header.Get("RateLimit-Remaining") != "2" {
		t.Errorf("after refill: %s remaining=%s", r, r.Header.Get("RateLimit-Remaining"))
	}
}

func TestRateLimitDisabled(t *testing.T) {
	e := newEnv(t, func(c *envConfig) { c.opts.RateLimit = RateLimit{Requests: -1} })
	// Without a limit there is nothing to report in RateLimit-* headers.
	e.undocumented(func() {
		for i := 0; i < 300; i++ {
			if r := e.get(messagesPath, e.readTok); r.Status != 200 || r.Header.Get("RateLimit-Limit") != "" {
				t.Fatalf("request %d: %s", i, r)
			}
		}
	})
}

func TestRoutingErrors(t *testing.T) {
	e := newEnv(t)
	// Unknown path and wrong method: problems, outside the per-operation contract.
	p := e.req("GET", "/api/v1/nothing", e.readTok, nil)
	p.problem(404, problemNotFound)
	p = e.req("PUT", messagesPath, e.readTok, nil)
	p.problem(405, problemMalformed)
	if allow := p.Header.Get("Allow"); allow != "GET, POST" {
		t.Errorf("Allow = %q", allow)
	}
	e.req("POST", messagesPath+"/01ARZ3NDEKTSV4RRFFQ69G5FAV", e.readTok, nil).problem(405, problemMalformed)
	e.req("GET", "/other", "", nil).problem(404, problemNotFound)
}
