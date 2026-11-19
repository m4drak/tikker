// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/domain"
)

// stub is a minimal Connector used to exercise the registry.
type stub struct{ cfg DeviceConfig }

func (s *stub) Capabilities() Capabilities { return Capabilities{} }
func (s *stub) Send(context.Context, ChannelRef, OutboundSMS) (SendResult, error) {
	return SendResult{}, nil
}
func (s *stub) FetchInbox(context.Context, ChannelRef) ([]InboundSMS, error) { return nil, nil }
func (s *stub) DeleteInbox(context.Context, ChannelRef, []string) error      { return nil }
func (s *stub) Health(context.Context, ChannelRef) (ChannelHealth, error) {
	return ChannelHealth{}, nil
}
func (s *stub) Probe(context.Context) (DeviceProbe, error) { return DeviceProbe{}, nil }
func (s *stub) Close() error                               { return nil }

var _ Connector = (*stub)(nil)

func stubFactory(_ context.Context, cfg DeviceConfig) (Connector, error) {
	return &stub{cfg: cfg}, nil
}

func TestRegistry(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry()

	tests := []struct {
		name    string
		kind    domain.ConnectorKind
		f       Factory
		wantErr bool
	}{
		{"routeros", domain.ConnectorRouterOSREST, stubFactory, false},
		{"duplicate", domain.ConnectorRouterOSREST, stubFactory, true},
		{"invalid kind", "smtp", stubFactory, true},
		{"nil factory", domain.ConnectorAgent, nil, true},
		{"agent", domain.ConnectorAgent, stubFactory, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.Register(tt.kind, tt.f)
			if tt.wantErr != (err != nil) {
				t.Fatalf("Register err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	if got := fmt.Sprint(r.Kinds()); got != "[agent routeros-rest]" {
		t.Fatalf("Kinds() = %s", got)
	}

	// Empty kind defaults to routeros-rest.
	c, err := r.New(ctx, DeviceConfig{DeviceID: "D1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.(*stub).cfg.Kind; got != domain.ConnectorRouterOSREST {
		t.Fatalf("default kind = %q", got)
	}

	empty := NewRegistry()
	if _, err := empty.New(ctx, DeviceConfig{Kind: domain.ConnectorAgent}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("New on empty registry: %v", err)
	}
}

func TestDeviceConfigRedaction(t *testing.T) {
	d := domain.Device{
		ID: "D1", Connector: domain.ConnectorRouterOSREST, BaseURL: "https://10.0.0.1",
		Username: "tikker", TLSMode: domain.TLSFingerprint, Fingerprint: "ab",
	}
	cfg := DeviceConfigFromDevice(d, "s3cr3t-pass")
	if cfg.Password != "s3cr3t-pass" || cfg.BaseURL != d.BaseURL || cfg.Fingerprint != "ab" {
		t.Fatalf("DeviceConfigFromDevice = %+v", cfg)
	}

	var logBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&logBuf, nil)).Info("dev", "cfg", cfg)

	outputs := map[string]string{
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg),
		"%#v":  fmt.Sprintf("%#v", cfg),
		"str":  cfg.String(),
		"slog": logBuf.String(),
	}
	for verb, out := range outputs {
		if strings.Contains(out, "s3cr3t") {
			t.Errorf("%s leaks password: %s", verb, out)
		}
		if !strings.Contains(out, "10.0.0.1") {
			t.Errorf("%s lost non-secret fields: %s", verb, out)
		}
	}
	if !strings.Contains(outputs["%v"], redacted) {
		t.Errorf("password not marked redacted: %s", outputs["%v"])
	}
	if cfg.Password != "s3cr3t-pass" {
		t.Error("redaction mutated the original")
	}
	if strings.Contains(DeviceConfig{}.String(), redacted) {
		t.Error("empty password reported as redacted")
	}
}
