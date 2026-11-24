// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "tkr_abc_secret"

func newTestClient(t *testing.T, h http.HandlerFunc, prefix string) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+prefix, testToken, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNew(t *testing.T) {
	tests := []struct {
		name, url, token string
		wantErr          string
	}{
		{name: "ok", url: "http://127.0.0.1:8080", token: "t"},
		{name: "https with path", url: "https://example.org/sms/", token: "t"},
		{name: "no scheme", url: "127.0.0.1:8080", token: "t", wantErr: "invalid URL"},
		{name: "ftp", url: "ftp://example.org", token: "t", wantErr: "invalid URL"},
		{name: "no host", url: "http://", token: "t", wantErr: "invalid URL"},
		{name: "empty token", url: "http://127.0.0.1", token: "", wantErr: "empty token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.url, tt.token, nil)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewStripsAPIPrefix(t *testing.T) {
	var path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}, "/sms/api/v1/")
	if _, _, err := c.Get(context.Background(), "X"); err != nil {
		t.Fatal(err)
	}
	if path != "/sms/api/v1/messages/X" {
		t.Errorf("path = %q", path)
	}
}

func TestSend(t *testing.T) {
	var gotReq *http.Request
	var gotBody SendRequest
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotReq = r
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"messages":[{"id":"01A","direction":"out","channel_id":null,"peer":"+48600100200","body":"hi","encoding":"gsm7","segments":1,"status":"queued","attempts":0,"possible_duplicate":false,"tags":[],"created_at":"2026-11-24T10:00:00Z"}]}`)
	}, "/base")

	req := SendRequest{To: "+48600100200", Body: "hi", Tags: []string{"a"}, ClientRef: "r1", ExpiresAt: "2026-11-25T09:55:00Z"}
	resp, raw, err := c.Send(context.Background(), req, "key-1")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(resp.Messages) != 1 || resp.Messages[0].ID != "01A" || resp.Messages[0].Status != StatusQueued {
		t.Errorf("resp = %+v", resp)
	}
	if !strings.Contains(string(raw), `"01A"`) {
		t.Errorf("raw = %s", raw)
	}
	if gotReq.Method != http.MethodPost || gotReq.URL.Path != "/base/api/v1/messages" {
		t.Errorf("request = %s %s", gotReq.Method, gotReq.URL.Path)
	}
	if got := gotReq.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q", got)
	}
	if got := gotReq.Header.Get("Idempotency-Key"); got != "key-1" {
		t.Errorf("Idempotency-Key = %q", got)
	}
	if got := gotReq.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if gotBody.To != req.To || gotBody.Body != req.Body || gotBody.ClientRef != "r1" || len(gotBody.Tags) != 1 || gotBody.ExpiresAt != req.ExpiresAt {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestSendOmitsOptionalFieldsAndKey(t *testing.T) {
	var raw map[string]any
	var hasKey bool
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, hasKey = r.Header["Idempotency-Key"]
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[]}`)
	}, "")
	if _, _, err := c.Send(context.Background(), SendRequest{To: "1", Body: "x"}, ""); err != nil {
		t.Fatal(err)
	}
	if hasKey {
		t.Error("Idempotency-Key header sent without a key")
	}
	if len(raw) != 2 {
		t.Errorf("body fields = %v, want only to and body", raw)
	}
}

func TestGetAndCancel(t *testing.T) {
	var method, path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"01A","direction":"out","channel_id":"C1","peer":"+1","body":null,"encoding":"gsm7","segments":1,"status":"cancelled","attempts":0,"last_error":"x","possible_duplicate":false,"tags":[],"created_at":"t"}`)
	}, "")

	m, _, err := c.Get(context.Background(), "01A")
	if err != nil || method != http.MethodGet || path != "/api/v1/messages/01A" {
		t.Fatalf("Get: %v %s %s", err, method, path)
	}
	if m.ChannelID == nil || *m.ChannelID != "C1" || m.Body != nil || m.LastError == nil {
		t.Errorf("message = %+v", m)
	}
	if _, _, err := c.Cancel(context.Background(), "a/b"); err != nil || method != http.MethodDelete || path != "/api/v1/messages/a%2Fb" {
		t.Fatalf("Cancel: %v %s %s", err, method, path)
	}
}

func TestListPagination(t *testing.T) {
	var queries []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = io.WriteString(w, `{"items":[{"id":"02"}],"next_cursor":"CUR"}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"01"}],"next_cursor":null}`)
	}, "")

	p1, _, err := c.List(context.Background(), ListParams{Direction: DirectionIn, Statuses: []string{"queued", "sending"}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p1.NextCursor == nil || *p1.NextCursor != "CUR" || len(p1.Items) != 1 {
		t.Fatalf("page 1 = %+v", p1)
	}
	p2, _, err := c.List(context.Background(), ListParams{Cursor: *p1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if p2.NextCursor != nil || p2.Items[0].ID != "01" {
		t.Errorf("page 2 = %+v", p2)
	}
	want := []string{"direction=in&limit=1&status=queued&status=sending", "cursor=CUR"}
	if strings.Join(queries, "|") != strings.Join(want, "|") {
		t.Errorf("queries = %v, want %v", queries, want)
	}
}

func TestAPIError(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantHas     []string
		wantCode    string
	}{
		{
			name:        "validation problem",
			status:      422,
			contentType: "application/problem+json",
			body:        `{"type":"urn:tikker:problem:validation","title":"Request validation failed","status":422,"detail":"1 error","errors":[{"field":"/to","code":"invalid_phone_number","detail":"bad number"}]}`,
			wantHas:     []string{"API error 422 (validation)", "Request validation failed", "1 error", "/to: invalid_phone_number: bad number"},
			wantCode:    "validation",
		},
		{
			name:        "unauthorized",
			status:      401,
			contentType: "application/problem+json; charset=utf-8",
			body:        `{"type":"urn:tikker:problem:unauthorized","title":"Unauthorized","status":401}`,
			wantHas:     []string{"API error 401 (unauthorized): Unauthorized"},
			wantCode:    "unauthorized",
		},
		{
			name:        "plain text from a proxy",
			status:      502,
			contentType: "text/html",
			body:        "<html>\n  Bad   gateway </html>",
			wantHas:     []string{"API error 502: <html> Bad gateway </html>"},
		},
		{
			name:        "json that is not a problem",
			status:      500,
			contentType: "application/json",
			body:        `{"oops":true}`,
			wantHas:     []string{`API error 500: {"oops":true}`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}, "")
			_, _, err := c.Get(context.Background(), "x")
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if ae.HTTPStatus != tt.status || ae.Problem.Code() != tt.wantCode {
				t.Errorf("status/code = %d/%q", ae.HTTPStatus, ae.Problem.Code())
			}
			for _, s := range tt.wantHas {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not contain %q", err.Error(), s)
				}
			}
			if strings.Contains(err.Error(), testToken) {
				t.Error("error leaks the token")
			}
		})
	}
}

func TestTransportErrorDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := New(url, testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Get(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestBadJSONResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}, "")
	if _, _, err := c.Get(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v", err)
	}
}

func TestIsTerminal(t *testing.T) {
	for status, want := range map[string]bool{
		StatusQueued: false, StatusSending: false, StatusSent: true,
		StatusFailed: true, StatusCancelled: true, StatusReceived: false,
	} {
		if got := IsTerminal(status); got != want {
			t.Errorf("IsTerminal(%q) = %v, want %v", status, got, want)
		}
	}
}
