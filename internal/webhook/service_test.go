// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package webhook

import (
	"errors"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/domain"
)

func (e *env) service(t *testing.T, w Waker) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{
		Store: e.st, Sealer: e.ring, Clock: e.clk, Waker: w,
		AllowHTTP: func(h string) bool { return h == "localhost" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ptr[T any](v T) *T { return &v }

type wakeCounter struct{ n int }

func (w *wakeCounter) Wake() { w.n++ }

func TestServiceCreateValidation(t *testing.T) {
	e := newEnv(t)
	svc := e.service(t, nil)
	long := strings.Repeat("x", MaxURL)
	tests := []struct {
		name  string
		in    Create
		field string
	}{
		{"missing everything", Create{}, "/name"},
		{"long name", Create{Name: ptr(strings.Repeat("n", MaxName+1)), URL: ptr("https://a.example"), Events: []string{"incoming_sms"}}, "/name"},
		{"long url", Create{Name: ptr("n"), URL: ptr("https://a.example/" + long), Events: []string{"incoming_sms"}}, "/url"},
		{"fragment", Create{Name: ptr("n"), URL: ptr("https://a.example/#x"), Events: []string{"incoming_sms"}}, "/url"},
		{"http public", Create{Name: ptr("n"), URL: ptr("http://a.example"), Events: []string{"incoming_sms"}}, "/url"},
		{"unknown event", Create{Name: ptr("n"), URL: ptr("https://a.example"), Events: []string{"x"}}, "/events"},
		{"short secret", Create{Name: ptr("n"), URL: ptr("https://a.example"), Events: []string{"incoming_sms"}, Secret: ptr("x")}, "/secret"},
		{"long secret", Create{Name: ptr("n"), URL: ptr("https://a.example"), Events: []string{"incoming_sms"}, Secret: ptr(strings.Repeat("s", MaxSecret+1))}, "/secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := svc.Create(e.ctx, domain.SystemActor, tt.in)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want ValidationError", err)
			}
			found := false
			for _, f := range ve.Errors {
				found = found || f.Field == tt.field
			}
			if !found {
				t.Errorf("errors %+v do not name %s", ve.Errors, tt.field)
			}
		})
	}
	if _, _, err := svc.Create(e.ctx, domain.SystemActor, Create{Name: ptr("local"), URL: ptr("http://localhost:8080/h"), Events: []string{"incoming_sms"}}); err != nil {
		t.Errorf("allowlisted http rejected: %v", err)
	}
}

func TestServiceSecretRoundTrip(t *testing.T) {
	e := newEnv(t)
	waker := &wakeCounter{}
	svc := e.service(t, waker)
	w, secret, err := svc.Create(e.ctx, domain.SystemActor, Create{Name: ptr("n"), URL: ptr("https://a.example"), Events: []string{"message_status"}})
	if err != nil {
		t.Fatal(err)
	}
	open := func(w domain.Webhook) string {
		b, err := e.ring.Open(SecretPurpose, w.ID.String(), w.SecretEnc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	got, _ := svc.Get(e.ctx, w.ID)
	if open(got) != secret {
		t.Fatal("stored secret does not match the one returned")
	}
	// A secret sealed for one webhook does not open under another ID.
	if _, err := e.ring.Open(SecretPurpose, "01JOTHER0000000000000000AA", got.SecretEnc); err == nil {
		t.Error("secret opens under a different webhook ID")
	}
	up, err := svc.Update(e.ctx, domain.SystemActor, w.ID, Changes{Secret: ptr("brand-new-secret-123"), Enabled: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Get(e.ctx, w.ID)
	if open(got) != "brand-new-secret-123" || got.Enabled || up.Enabled {
		t.Errorf("after update: %+v", got)
	}
	if waker.n == 0 {
		t.Error("sender not woken after an update")
	}
	if _, err := svc.Update(e.ctx, domain.SystemActor, "01JNOPE00000000000000000AA", Changes{Name: ptr("x")}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update of unknown = %v", err)
	}
	if err := svc.Delete(e.ctx, domain.SystemActor, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(e.ctx, domain.SystemActor, w.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second delete = %v", err)
	}
	if _, err := NewService(ServiceDeps{}); err == nil {
		t.Error("NewService without deps accepted")
	}
}

func TestNewSenderValidation(t *testing.T) {
	e := newEnv(t)
	if _, err := NewSender(SenderOptions{}); err == nil {
		t.Error("NewSender without deps accepted")
	}
	bad := DefaultRetryPolicy()
	bad.Multiplier = 0.5
	if _, err := NewSender(SenderOptions{Store: e.st.Webhooks(), Secrets: e.ring, Retry: bad}); err == nil {
		t.Error("invalid retry policy accepted")
	}
}

func TestSignKnownVector(t *testing.T) {
	// HMAC-SHA256("key", "The quick brown fox jumps over the lazy dog").
	got := Sign([]byte("key"), []byte("The quick brown fox jumps over the lazy dog"))
	want := "sha256=f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"
	if got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
	if Verify([]byte("key"), []byte("x"), want) || Verify([]byte("key"), []byte("x"), "") {
		t.Error("Verify accepted a wrong signature")
	}
}
