// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
)

func TestFetchInbox(t *testing.T) {
	e := newEnv(t, setup{})
	when := time.Date(2026, 11, 18, 8, 30, 15, 0, time.UTC)
	if _, err := e.srv.InjectSMS(fake.IncomingSMS{From: "+48511222333", Text: "Hello", Time: when}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.InjectSMS(fake.IncomingSMS{From: "+48511222444", Text: "Cześć", Time: when.Add(time.Minute), Class0: true}); err != nil {
		t.Fatal(err)
	}
	got, err := e.conn.FetchInbox(t.Context(), lte1)
	if err != nil {
		t.Fatalf("FetchInbox: %v", err)
	}
	inbox := e.srv.Inbox()
	if len(got) != 2 || len(inbox) != 2 {
		t.Fatalf("got %d entries, inbox %d", len(got), len(inbox))
	}
	for i, m := range got {
		want := inbox[i]
		if m.RemoteID != want.ID || m.Phone != want.Phone || m.PDU != want.PDU || m.Body != want.Message || m.Type != want.Type {
			t.Errorf("entry %d = %+v, want %+v", i, m, want)
		}
		if m.PDU == "" {
			t.Errorf("entry %d has no PDU", i)
		}
		if m.RawTimestamp != fake.TimestampISO(want.Timestamp) || !m.Timestamp.Equal(want.Timestamp) {
			t.Errorf("entry %d timestamp %q / %v, want %v", i, m.RawTimestamp, m.Timestamp, want.Timestamp)
		}
	}
	if got[1].Type != "class-0" {
		t.Errorf("type = %q", got[1].Type)
	}

	// /tool sms receives on lte1 only: lte2 sees an empty inbox.
	other, err := e.conn.FetchInbox(t.Context(), lte2)
	if err != nil || len(other) != 0 {
		t.Fatalf("FetchInbox(lte2) = %v, %v", other, err)
	}
}

func TestFetchInboxEmptyMakesOneCall(t *testing.T) {
	e := newEnv(t, setup{})
	got, err := e.conn.FetchInbox(t.Context(), lte1)
	if err != nil || got != nil {
		t.Fatalf("FetchInbox = %v, %v", got, err)
	}
	if n := e.srv.Requests(fake.EndpointAny); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
}

func TestFetchInboxPortField(t *testing.T) {
	e := newEnv(t, setup{fake: fake.Options{InboxPortField: true}})
	for _, p := range []string{"lte1", "lte2", "lte2"} {
		if _, err := e.srv.InjectSMS(fake.IncomingSMS{Port: p, From: "+48511222333", Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.conn.FetchInbox(t.Context(), lte2)
	if err != nil || len(got) != 2 {
		t.Fatalf("FetchInbox(lte2) = %d, %v", len(got), err)
	}
	if n := e.srv.Requests(fake.EndpointSettings); n != 0 {
		t.Errorf("settings read %d times although entries carry the port", n)
	}
}

func TestFetchInboxTimestamps(t *testing.T) {
	waw, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	when := time.Date(2026, 11, 19, 12, 0, 0, 0, waw)
	tests := []struct {
		name   string
		format func(time.Time) string
		text   string
		want   time.Time
	}{
		{"iso", fake.TimestampISO, "", when},
		{"legacy console", fake.TimestampLegacy, "", when},
		{"unparsable", nil, "yesterday-ish", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, setup{
				fake: fake.Options{FormatTimestamp: tt.format},
				opts: func(o *Options) { o.Location = waw },
			})
			if _, err := e.srv.InjectRaw(fake.RawInbox{Phone: "+48511222333", Timestamp: when, TimestampText: tt.text, Message: "x", PDU: "00"}); err != nil {
				t.Fatal(err)
			}
			got, err := e.conn.FetchInbox(t.Context(), lte1)
			if err != nil || len(got) != 1 {
				t.Fatalf("FetchInbox = %v, %v", got, err)
			}
			if !got[0].Timestamp.Equal(tt.want) {
				t.Errorf("Timestamp = %v (raw %q), want %v", got[0].Timestamp, got[0].RawTimestamp, tt.want)
			}
			if got[0].RawTimestamp == "" {
				t.Error("raw timestamp lost")
			}
		})
	}
}

func TestFetchInboxErrors(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*env)
		kind    domain.ErrorKind
		code    domain.ErrorCode
	}{
		{"503", func(e *env) { e.srv.FailNext(fake.EndpointInbox, fake.HTTPError(503, ""), 1) }, domain.KindTransient, ""},
		{"401", func(e *env) { e.srv.FailNext(fake.EndpointInbox, fake.Unauthorized(), 1) }, domain.KindAuth, domain.CodeAuthFailed},
		{"not a list", func(e *env) { e.srv.FailNext(fake.EndpointInbox, fake.HTTPError(200, ""), 1) }, domain.KindTransient, domain.CodeUnexpectedResponse},
		{"dropped", func(e *env) { e.srv.FailNext(fake.EndpointInbox, fake.DropConnection(), 1) }, domain.KindTransient, domain.CodeUnreachable},
		{"settings fail", func(e *env) {
			_, _ = e.srv.InjectSMS(fake.IncomingSMS{From: "+48511222333", Text: "x"})
			e.srv.FailNext(fake.EndpointSettings, fake.HTTPError(500, ""), 1)
		}, domain.KindTransient, ""},
		{"settings not an object", func(e *env) {
			_, _ = e.srv.InjectSMS(fake.IncomingSMS{From: "+48511222333", Text: "x"})
			e.srv.FailNext(fake.EndpointSettings, fake.HTTPError(200, ""), 1)
		}, domain.KindTransient, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, setup{})
			tt.prepare(e)
			_, err := e.conn.FetchInbox(t.Context(), lte1)
			if tt.name == "settings not an object" {
				// {"error":200,...} decodes as an object without "port": the
				// inbox is then not filtered.
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			wantErr(t, err, tt.kind, false, tt.code)
		})
	}
}

func TestFetchInboxResponseTooLarge(t *testing.T) {
	e := newEnv(t, setup{opts: func(o *Options) { o.MaxResponseBytes = 64 }})
	if _, err := e.srv.InjectSMS(fake.IncomingSMS{From: "+48511222333", Text: "a fairly long message to exceed the limit"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.conn.FetchInbox(t.Context(), lte1)
	wantErr(t, err, domain.KindTransient, false, domain.CodeUnexpectedResponse)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func inject(t *testing.T, e *env, n int) []string {
	t.Helper()
	var ids []string
	for range n {
		got, err := e.srv.InjectSMS(fake.IncomingSMS{From: "+48511222333", Text: "x"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, got...)
	}
	return ids
}

func TestDeleteInbox(t *testing.T) {
	e := newEnv(t, setup{})
	ids := inject(t, e, 3)
	if err := e.conn.DeleteInbox(t.Context(), lte1, []string{ids[0], "", ids[2], "*FF"}); err != nil {
		t.Fatalf("DeleteInbox: %v", err)
	}
	left := e.srv.Inbox()
	if len(left) != 1 || left[0].ID != ids[1] {
		t.Fatalf("inbox after delete = %+v", left)
	}
	if n := e.srv.Requests(fake.EndpointInboxDelete); n != 3 {
		t.Errorf("DELETE calls = %d, want 3", n)
	}
	if n := e.srv.Requests(fake.EndpointInboxRemove); n != 0 {
		t.Errorf("remove calls = %d, want 0", n)
	}
	if got := e.conn.delVariant.Load(); got != deleteByPath {
		t.Errorf("variant = %d", got)
	}
}

func TestDeleteInboxFallbackToRemove(t *testing.T) {
	tests := []struct {
		name  string
		fault fake.Fault
	}{
		{"405", fake.HTTPError(405, "")},
		{"404 without item detail", fake.HTTPError(404, "")},
		{"400 no such command", fake.HTTPError(400, "no such command or directory (*1)")},
		{"501", fake.HTTPError(501, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, setup{})
			e.srv.FailNext(fake.EndpointInboxDelete, tt.fault, -1)
			ids := inject(t, e, 3)
			if err := e.conn.DeleteInbox(t.Context(), lte1, ids[:2]); err != nil {
				t.Fatalf("DeleteInbox: %v", err)
			}
			// The variant is remembered: no further DELETE attempts.
			if err := e.conn.DeleteInbox(t.Context(), lte1, []string{ids[2], "*FF"}); err != nil {
				t.Fatalf("DeleteInbox again: %v", err)
			}
			if left := e.srv.Inbox(); len(left) != 0 {
				t.Fatalf("inbox not emptied: %+v", left)
			}
			if n := e.srv.Requests(fake.EndpointInboxDelete); n != 1 {
				t.Errorf("DELETE calls = %d, want 1", n)
			}
			if n := e.srv.Requests(fake.EndpointInboxRemove); n != 4 {
				t.Errorf("remove calls = %d, want 4", n)
			}
		})
	}
}

func TestDeleteInboxSwitchesAfterPathStopsWorking(t *testing.T) {
	e := newEnv(t, setup{})
	ids := inject(t, e, 2)
	if err := e.conn.DeleteInbox(t.Context(), lte1, ids[:1]); err != nil {
		t.Fatal(err)
	}
	e.srv.FailNext(fake.EndpointInboxDelete, fake.HTTPError(405, ""), -1)
	if err := e.conn.DeleteInbox(t.Context(), lte1, ids[1:]); err != nil {
		t.Fatal(err)
	}
	if got := e.conn.delVariant.Load(); got != deleteByRemove {
		t.Errorf("variant = %d", got)
	}
}

func TestDeleteInboxErrors(t *testing.T) {
	t.Run("partial failure continues", func(t *testing.T) {
		e := newEnv(t, setup{})
		ids := inject(t, e, 2)
		e.srv.FailNext(fake.EndpointInboxDelete, fake.HTTPError(503, ""), 1)
		err := e.conn.DeleteInbox(t.Context(), lte1, ids)
		wantErr(t, err, domain.KindTransient, false, "")
		if !strings.Contains(err.Error(), "HTTP 503") {
			t.Errorf("err = %v", err)
		}
		if left := e.srv.Inbox(); len(left) != 1 || left[0].ID != ids[0] {
			t.Fatalf("inbox = %+v", left)
		}
	})
	t.Run("auth stops", func(t *testing.T) {
		e := newEnv(t, setup{})
		ids := inject(t, e, 2)
		e.srv.FailNext(fake.EndpointInboxDelete, fake.Unauthorized(), -1)
		err := e.conn.DeleteInbox(t.Context(), lte1, ids)
		wantErr(t, err, domain.KindAuth, false, domain.CodeAuthFailed)
		if n := e.srv.Requests(fake.EndpointInboxDelete); n != 1 {
			t.Errorf("DELETE calls = %d, want 1", n)
		}
	})
	t.Run("remove fails", func(t *testing.T) {
		e := newEnv(t, setup{})
		ids := inject(t, e, 1)
		e.srv.FailNext(fake.EndpointInboxDelete, fake.HTTPError(405, ""), -1)
		e.srv.FailNext(fake.EndpointInboxRemove, fake.HTTPError(500, ""), 1)
		wantErr(t, e.conn.DeleteInbox(t.Context(), lte1, ids), domain.KindTransient, false, "")
	})
	t.Run("invalid id", func(t *testing.T) {
		e := newEnv(t, setup{})
		wantErr(t, e.conn.DeleteInbox(t.Context(), lte1, []string{"*1,*2"}), domain.KindPermanent, false, "")
		if n := e.srv.Requests(fake.EndpointAny); n != 0 {
			t.Errorf("router called %d times", n)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		e := newEnv(t, setup{})
		ids := inject(t, e, 2)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		wantErr(t, e.conn.DeleteInbox(ctx, lte1, ids), domain.KindTransient, false, "")
		if len(e.srv.Inbox()) != 2 {
			t.Error("entries deleted after cancellation")
		}
	})
}
