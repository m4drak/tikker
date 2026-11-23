// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/webhook"
)

const webhooksPath = "/api/v1/webhooks"

func TestWebhookCRUD(t *testing.T) {
	e := newEnv(t)
	res := e.req(http.MethodPost, webhooksPath, e.adminTok, map[string]any{
		"name": "CRM", "url": "https://crm.example.com/hooks/tikker?token=abc", "events": []string{"incoming_sms", "message_status", "incoming_sms"},
	})
	if res.Status != http.StatusCreated {
		t.Fatal(res)
	}
	h := res.obj()
	id := h["id"].(string)
	secret, _ := h["secret"].(string)
	if !strings.HasPrefix(secret, webhook.SecretPrefix) || len(secret) < 40 {
		t.Errorf("generated secret = %q", secret)
	}
	for k, v := range map[string]any{
		"name": "CRM", "url": "https://crm.example.com/hooks/tikker?token=abc", "enabled": true,
		"events": []any{"incoming_sms", "message_status"}, "created_at": "2026-11-19T12:00:00Z",
	} {
		if !reflect.DeepEqual(h[k], v) {
			t.Errorf("%s = %#v, want %#v", k, h[k], v)
		}
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q on a response with a secret", res.Header.Get("Cache-Control"))
	}
	if res.Header.Get("Location") != webhooksPath+"/"+id {
		t.Errorf("Location = %q", res.Header.Get("Location"))
	}

	// The secret is stored encrypted and never returned again.
	stored, err := e.st.Webhooks().Get(e.ctx, domain.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.SecretEnc) == 0 || strings.Contains(string(stored.SecretEnc), secret) {
		t.Errorf("stored secret = %q", stored.SecretEnc)
	}
	for _, r := range []*result{e.get(webhooksPath+"/"+id, e.adminTok), e.get(webhooksPath, e.adminTok)} {
		if r.Status != 200 || strings.Contains(string(r.Body), secret) || strings.Contains(string(r.Body), `"secret"`) {
			t.Errorf("read exposes the secret: %s", r)
		}
	}
	if items := e.get(webhooksPath, e.adminTok).obj()["items"].([]any); len(items) != 1 {
		t.Errorf("list = %v", items)
	}
	e.get(webhooksPath+"/nope", e.adminTok).problem(404, problemNotFound)

	// An own secret is accepted; names are unique.
	own := "my-own-signing-secret"
	res = e.req(http.MethodPost, webhooksPath, e.adminTok, map[string]any{
		"name": "Other", "url": "http://127.0.0.1:9/x", "events": []string{"message_status"}, "secret": own, "enabled": false,
	})
	if res.Status != http.StatusCreated || res.obj()["secret"] != own || res.obj()["enabled"] != false {
		t.Fatal(res)
	}
	e.req(http.MethodPost, webhooksPath, e.adminTok, map[string]any{
		"name": "crm", "url": "https://x.example.com", "events": []string{"incoming_sms"},
	}).problem(409, problemConflict)

	// Patch: merge semantics; secret rotation re-encrypts.
	res = e.req(http.MethodPatch, webhooksPath+"/"+id, e.adminTok, map[string]any{
		"events": []string{"message_status"}, "enabled": false, "secret": "rotated-secret-value",
	})
	if res.Status != 200 {
		t.Fatal(res)
	}
	if o := res.obj(); !reflect.DeepEqual(o["events"], []any{"message_status"}) || o["enabled"] != false ||
		o["name"] != "CRM" || strings.Contains(string(res.Body), "rotated-secret-value") {
		t.Errorf("patched = %s", res.Body)
	}
	after, _ := e.st.Webhooks().Get(e.ctx, domain.ID(id))
	if string(after.SecretEnc) == string(stored.SecretEnc) {
		t.Error("secret was not replaced")
	}
	e.req(http.MethodPatch, webhooksPath+"/"+id, e.adminTok, map[string]any{"name": "CRM 2"})
	if kept, _ := e.st.Webhooks().Get(e.ctx, domain.ID(id)); string(kept.SecretEnc) != string(after.SecretEnc) || kept.Name != "CRM 2" {
		t.Error("secret changed by a patch without secret")
	}
	e.req(http.MethodPatch, webhooksPath+"/"+id, e.adminTok, map[string]any{"name": "Other"}).problem(409, problemConflict)
	e.req(http.MethodPatch, webhooksPath+"/nope", e.adminTok, `{"name":"x"}`).problem(404, problemNotFound)

	if r := e.req(http.MethodDelete, webhooksPath+"/"+id, e.adminTok, nil); r.Status != 204 || len(r.Body) != 0 {
		t.Fatal(r)
	}
	e.req(http.MethodDelete, webhooksPath+"/"+id, e.adminTok, nil).problem(404, problemNotFound)

	// Audited without the secret or the URL path.
	page, _ := e.st.Audit().List(e.ctx, auditFilter(), pageRequest(100))
	var actions []domain.AuditAction
	for _, a := range page.Items {
		for k, v := range a.Meta {
			if strings.Contains(v, "token=abc") || strings.Contains(v, secret) || strings.Contains(v, own) || strings.Contains(v, "rotated") {
				t.Errorf("audit meta %s leaks: %s", k, v)
			}
		}
		if strings.HasPrefix(string(a.Action), "webhook.") {
			actions = append(actions, a.Action)
		}
	}
	want := []domain.AuditAction{webhook.AuditDelete, webhook.AuditUpdate, webhook.AuditUpdate, webhook.AuditCreate, webhook.AuditCreate}
	if !reflect.DeepEqual(actions, want) {
		t.Errorf("audit actions = %v, want %v", actions, want)
	}
}

func TestWebhookValidation(t *testing.T) {
	e := newEnv(t)
	valid := func() map[string]any {
		return map[string]any{"name": "n", "url": "https://x.example.com/h", "events": []string{"incoming_sms"}}
	}
	tests := []struct {
		name   string
		mutate func(m map[string]any)
		field  string
	}{
		{"no name", func(m map[string]any) { delete(m, "name") }, "/name"},
		{"empty name", func(m map[string]any) { m["name"] = "" }, "/name"},
		{"no url", func(m map[string]any) { delete(m, "url") }, "/url"},
		{"relative url", func(m map[string]any) { m["url"] = "/hook" }, "/url"},
		{"ftp", func(m map[string]any) { m["url"] = "ftp://x.example.com" }, "/url"},
		{"credentials", func(m map[string]any) { m["url"] = "https://u:p@x.example.com" }, "/url"},
		{"plain http to a public host", func(m map[string]any) { m["url"] = "http://x.example.com" }, "/url"},
		{"metadata address", func(m map[string]any) { m["url"] = "https://169.254.169.254/latest" }, "/url"},
		{"loopback", func(m map[string]any) { m["url"] = "https://[::1]/x" }, "/url"},
		{"private network", func(m map[string]any) { m["url"] = "https://10.1.2.3/x" }, "/url"},
		{"no events", func(m map[string]any) { delete(m, "events") }, "/events"},
		{"empty events", func(m map[string]any) { m["events"] = []string{} }, "/events"},
		{"unknown event", func(m map[string]any) { m["events"] = []string{"nope"} }, "/events"},
		{"short secret", func(m map[string]any) { m["secret"] = "short" }, "/secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid()
			tt.mutate(m)
			p := e.req(http.MethodPost, webhooksPath, e.adminTok, m).problem(422, problemValidation)
			if !strings.Contains(toJSON(p), tt.field) {
				t.Errorf("problem does not name %s: %v", tt.field, p)
			}
		})
	}
	e.req(http.MethodPost, webhooksPath, e.adminTok, `{"name":"n","bogus":1}`).problem(400, problemMalformed)
	e.req(http.MethodPost, webhooksPath, e.adminTok, ``).problem(400, problemMalformed)

	// Plain http is fine for an allowlisted host.
	m := valid()
	m["url"] = "http://127.0.0.1:8080/hook"
	if r := e.req(http.MethodPost, webhooksPath, e.adminTok, m); r.Status != 201 {
		t.Fatal(r)
	}

	id := e.req(http.MethodPost, webhooksPath, e.adminTok, map[string]any{"name": "p", "url": "https://p.example.com", "events": []string{"incoming_sms"}}).obj()["id"]
	path := webhooksPath + "/" + id.(string)
	for _, body := range []string{`{}`, `{"name": null}`, `{"events": []}`, `{"events": ["x"]}`, `{"url": "ftp://x"}`, `{"secret": "s"}`} {
		e.req(http.MethodPatch, path, e.adminTok, body).problem(422, problemValidation)
	}
	e.req(http.MethodPatch, path, e.adminTok, `{"bogus":1}`).problem(400, problemMalformed)
	e.req(http.MethodPatch, path, e.adminTok, `{"enabled":"yes"}`).problem(400, problemMalformed)
}

func TestWebhooksNeedAdminScope(t *testing.T) {
	e := newEnv(t)
	for _, tok := range []string{e.sendTok, e.readTok} {
		e.get(webhooksPath, tok).problem(403, problemForbidden)
		e.req(http.MethodPost, webhooksPath, tok, map[string]any{}).problem(403, problemForbidden)
		e.req(http.MethodDelete, webhooksPath+"/x", tok, nil).problem(403, problemForbidden)
	}
	e.get(webhooksPath, "").problem(401, problemUnauthorized)
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
