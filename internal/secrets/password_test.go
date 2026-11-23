// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/domain"
)

const plain = "S3cret-router-pass"

func TestPasswordNeverLeaks(t *testing.T) {
	p := Password(plain)
	type wrap struct {
		Name string   `json:"name"`
		Pass Password `json:"password"`
		Ptr  *Password
	}
	w := wrap{"d", p, &p}

	var logs bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&logs, nil), slog.NewJSONHandler(&logs, nil)} {
		l := slog.New(h)
		l.Info("x", "pw", p, "ptr", &p, slog.Any("w", w), slog.Group("g", "pw", p))
	}
	js, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	outs := map[string]string{
		"logs":   logs.String(),
		"json":   string(js),
		"%s":     fmt.Sprintf("%s", p),
		"%v":     fmt.Sprintf("%v", p),
		"%+v":    fmt.Sprintf("%+v %+v", p, w),
		"%#v":    fmt.Sprintf("%#v %#v", p, w),
		"%q":     fmt.Sprintf("%q", p),
		"%x":     fmt.Sprintf("%x", p),
		"%d":     fmt.Sprintf("%d", p),
		"Sprint": fmt.Sprint(p, w),
	}
	for name, out := range outs {
		if strings.Contains(out, plain) {
			t.Errorf("%s leaks password: %s", name, out)
		}
	}
	if !strings.Contains(outs["json"], redacted) {
		t.Errorf("json = %s", outs["json"])
	}
	if Password("").String() != "" || p.Reveal() != plain {
		t.Fatal("String/Reveal contract")
	}
	if b, _ := p.MarshalText(); string(b) != redacted {
		t.Fatalf("MarshalText = %s", b)
	}
}

func TestPasswordUnmarshalJSON(t *testing.T) {
	var in struct {
		Password Password `json:"password"`
	}
	if err := json.Unmarshal([]byte(`{"password":"abc"}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.Password.Reveal() != "abc" {
		t.Fatalf("got %q", in.Password.Reveal())
	}
	if err := json.Unmarshal([]byte(`{"password":5}`), &in); err == nil {
		t.Fatal("non-string accepted")
	}
}

func TestDeviceCredentials(t *testing.T) {
	c := NewDeviceCredentials(newRing(t, 1))
	id1, id2 := domain.ID("01J00000000000000000000001"), domain.ID("01J00000000000000000000002")

	enc, err := c.Encrypt(id1, Password(plain))
	if err != nil {
		t.Fatal(err)
	}
	d1 := domain.Device{ID: id1, Connector: "routeros-rest", BaseURL: "https://r", Username: "api", PasswordEnc: enc, TLSMode: domain.TLSVerify}

	got, err := c.Decrypt(d1)
	if err != nil || got.Reveal() != plain {
		t.Fatalf("Decrypt = %q, %v", got.Reveal(), err)
	}

	cfg, err := c.ConnectorConfig(d1)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != plain || cfg.DeviceID != id1 || cfg.Username != "api" || cfg.BaseURL != "https://r" {
		t.Fatalf("unexpected cfg: %v", cfg)
	}
	if s := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg); strings.Contains(s, plain) {
		t.Fatalf("config leaks password: %s", s)
	}

	// Swapping ciphertexts between devices must fail.
	d2 := d1
	d2.ID = id2
	if _, err := c.Decrypt(d2); err == nil {
		t.Fatal("ciphertext accepted for another device")
	}
	if _, err := c.ConnectorConfig(d2); err == nil {
		t.Fatal("ConnectorConfig accepted swapped ciphertext")
	}
	if err := c.Verify([]domain.Device{d1}); err != nil {
		t.Fatalf("Verify ok case: %v", err)
	}
	if err := c.Verify([]domain.Device{d1, d2}); err == nil {
		t.Fatal("Verify missed swapped ciphertext")
	}
	if err := c.Verify(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Encrypt("", Password("x")); err == nil {
		t.Fatal("empty id accepted")
	}
}

func TestDeviceCredentialsAgentToken(t *testing.T) {
	c := NewDeviceCredentials(newRing(t, 1))
	id1, id2 := domain.ID("01J00000000000000000000001"), domain.ID("01J00000000000000000000002")
	const token = "agent-token-0123456789abcdef0123456789"

	pw, err := c.Encrypt(id1, "")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.EncryptAgentToken(id1, token)
	if err != nil {
		t.Fatal(err)
	}
	d := domain.Device{ID: id1, Connector: domain.ConnectorAgent, PasswordEnc: pw, AgentTokenEnc: tok}

	tests := []struct {
		name    string
		dev     domain.Device
		want    string
		wantErr bool
	}{
		{"round trip", d, token, false},
		{"no token", domain.Device{ID: id1, PasswordEnc: pw}, "", false},
		{"other device", domain.Device{ID: id2, PasswordEnc: pw, AgentTokenEnc: tok}, "", true},
		// A sealed password must not open as a token (different purpose).
		{"password as token", domain.Device{ID: id1, PasswordEnc: pw, AgentTokenEnc: pw}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.DecryptAgentToken(tt.dev)
			if (err != nil) != tt.wantErr || got.Reveal() != tt.want {
				t.Fatalf("DecryptAgentToken = %q, %v", got.Reveal(), err)
			}
			if err := c.Verify([]domain.Device{tt.dev}); (err != nil) != tt.wantErr {
				t.Fatalf("Verify = %v", err)
			}
		})
	}

	cfg, err := c.ConnectorConfig(d)
	if err != nil || cfg.AgentToken != token {
		t.Fatalf("ConnectorConfig = %v, %v", cfg, err)
	}
	if s := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg); strings.Contains(s, token) {
		t.Fatalf("config leaks agent token: %s", s)
	}
	if _, err := c.EncryptAgentToken("", "x"); err == nil {
		t.Fatal("empty id accepted")
	}
}

func TestPasswordGoString(t *testing.T) {
	if got := Password(plain).GoString(); strings.Contains(got, plain) || !strings.Contains(got, redacted) {
		t.Fatalf("GoString = %s", got)
	}
}

func TestEncryptRandFailure(t *testing.T) {
	ring := newRing(t, 1)
	ring.rand = errReader{}
	if _, err := NewDeviceCredentials(ring).Encrypt("01J00000000000000000000001", "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestVerifyDetectsDifferentKey(t *testing.T) {
	id := domain.ID("01J00000000000000000000001")
	enc, err := NewDeviceCredentials(newRing(t, 1)).Encrypt(id, Password(plain))
	if err != nil {
		t.Fatal(err)
	}
	other := NewDeviceCredentials(newRing(t, 2))
	err = other.Verify([]domain.Device{{ID: id, PasswordEnc: enc}})
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), plain) {
		t.Fatal("error leaks password")
	}
}
