// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// envMap returns a LookupFunc over a fixed set of variables.
func envMap(vars map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

// writeYAML writes content to a file in dir and returns its path.
func writeYAML(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testEnv isolates a test from the host: the data directory is a temp dir,
// so the default config.yaml lookup never finds a real file.
func testEnv(dir string, extra map[string]string) map[string]string {
	vars := map[string]string{envDataDir: dir}
	// Default listen ":8080" binds all interfaces (exposed); a setup secret
	// keeps Load() valid for tests that do not set an admin password.
	if _, ok := extra[envSetupSecret]; !ok {
		vars[envSetupSecret] = "test-setup-secret-1234"
	}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

func TestLoadPriority(t *testing.T) {
	// Each row sets one key in some combination of sources and states the
	// value that must win.
	tests := []struct {
		name    string
		yaml    string
		env     map[string]string
		wantRet time.Duration // retention
		wantHea time.Duration // health_interval
		wantPol time.Duration // inbox_poll_interval
	}{
		{
			name:    "defaults only",
			wantRet: 2160 * time.Hour,
			wantHea: 60 * time.Second,
			wantPol: 15 * time.Second,
		},
		{
			name:    "yaml overrides defaults",
			yaml:    "retention: 100h\nhealth_interval: 2m\n",
			wantRet: 100 * time.Hour,
			wantHea: 2 * time.Minute,
			wantPol: 15 * time.Second,
		},
		{
			name:    "env overrides yaml",
			yaml:    "retention: 100h\n",
			env:     map[string]string{envRetention: "50h"},
			wantRet: 50 * time.Hour,
			wantHea: 60 * time.Second,
			wantPol: 15 * time.Second,
		},
		{
			name:    "env overrides defaults without yaml",
			env:     map[string]string{envInboxPollInterval: "5s"},
			wantRet: 2160 * time.Hour,
			wantHea: 60 * time.Second,
			wantPol: 5 * time.Second,
		},
		{
			name:    "all three sources at once",
			yaml:    "retention: 100h\nhealth_interval: 2m\ninbox_poll_interval: 30s\n",
			env:     map[string]string{envRetention: "50h", envHealthInterval: "3m"},
			wantRet: 50 * time.Hour,
			wantHea: 3 * time.Minute,
			wantPol: 30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := ""
			if tt.yaml != "" {
				path = writeYAML(t, dir, tt.yaml)
			}
			cfg, err := Load(Options{Path: path, Lookup: envMap(testEnv(dir, tt.env))})
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Retention != tt.wantRet {
				t.Errorf("Retention = %s, want %s", cfg.Retention, tt.wantRet)
			}
			if cfg.HealthInterval != tt.wantHea {
				t.Errorf("HealthInterval = %s, want %s", cfg.HealthInterval, tt.wantHea)
			}
			if cfg.InboxPollInterval != tt.wantPol {
				t.Errorf("InboxPollInterval = %s, want %s", cfg.InboxPollInterval, tt.wantPol)
			}
		})
	}
}

func TestLoadDefaultsMatchSpec(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(Options{Lookup: envMap(testEnv(dir, nil))})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	rp := domain.DefaultRetryPolicy()
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"listen", cfg.Listen, ":8080"},
		{"default_region", cfg.DefaultRegion, "PL"},
		{"inbox_poll_interval", cfg.InboxPollInterval, 15 * time.Second},
		{"health_interval", cfg.HealthInterval, 60 * time.Second},
		{"retention", cfg.Retention, 2160 * time.Hour},
		{"log_level", cfg.LogLevel, "info"},
		{"log_format", cfg.LogFormat, "json"},
		{"sqlite_synchronous", cfg.SQLiteSynchronous, "NORMAL"},
		{"allow_short_numbers", cfg.AllowShortNumbers, false},
		{"long_message_strategy", cfg.LongMessageStrategy, LongMessageReject},
		{"retry.max_attempts", cfg.Retry.MaxAttempts, rp.MaxAttempts},
		{"retry.base", cfg.Retry.Base, rp.Initial},
		{"retry.factor", cfg.Retry.Factor, rp.Multiplier},
		{"retry.max", cfg.Retry.Max, rp.Max},
		{"retry.jitter", cfg.Retry.Jitter, rp.Jitter},
		{"tls.cert_file", cfg.TLS.CertFile, ""},
		{"admin_password", string(cfg.AdminPassword), ""},
		{"session_idle_timeout", cfg.SessionIdleTimeout, time.Hour},
		{"session_absolute_timeout", cfg.SessionAbsoluteTimeout, 12 * time.Hour},
		{"api_rate_limit", cfg.APIRateLimit, 120},
		{"shutdown_timeout", cfg.ShutdownTimeout, 30 * time.Second},
		{"trusted_proxies", len(cfg.TrustedProxies), 0},
		{"trusted_origins", len(cfg.TrustedOrigins), 0},
		{"allowed_http_hosts", len(cfg.AllowedHTTPHosts), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestLoadYAMLValues(t *testing.T) {
	dir := t.TempDir()
	path := writeYAML(t, dir, `
listen: "127.0.0.1:9000"
default_region: "de"
allow_short_numbers: true
long_message_strategy: split
retention: 720h
log_level: DEBUG
log_format: text
sqlite_synchronous: full
retry:
  max_attempts: 3
  base: 10s
  factor: 2
  max: 5m
  jitter: 0.1
tls:
  cert_file: ""
  key_file: ""
`)
	cfg, err := Load(Options{Path: path, Lookup: envMap(testEnv(dir, nil))})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Listen != "127.0.0.1:9000" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.DefaultRegion != "DE" {
		t.Errorf("DefaultRegion = %q, want normalised DE", cfg.DefaultRegion)
	}
	if !cfg.AllowShortNumbers {
		t.Error("AllowShortNumbers = false, want true")
	}
	if cfg.LongMessageStrategy != LongMessageSplit {
		t.Errorf("LongMessageStrategy = %q", cfg.LongMessageStrategy)
	}
	if cfg.Retention != 720*time.Hour {
		t.Errorf("Retention = %s", cfg.Retention)
	}
	if cfg.LogLevel != "debug" || cfg.LogFormat != "text" || cfg.SQLiteSynchronous != "FULL" {
		t.Errorf("log/sqlite = %q %q %q", cfg.LogLevel, cfg.LogFormat, cfg.SQLiteSynchronous)
	}
	wantRetry := RetryConfig{MaxAttempts: 3, Base: 10 * time.Second, Factor: 2, Max: 5 * time.Minute, Jitter: 0.1}
	if cfg.Retry != wantRetry {
		t.Errorf("Retry = %+v, want %+v", cfg.Retry, wantRetry)
	}
	// A key missing from the file keeps its default.
	if cfg.InboxPollInterval != 15*time.Second {
		t.Errorf("InboxPollInterval = %s, want default 15s", cfg.InboxPollInterval)
	}
}

func TestLoadLists(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  map[string]string
		want []string
	}{
		{name: "default empty", want: nil},
		{name: "yaml list", yaml: "allowed_http_hosts: [\"172.17.0.1\", \" router.lan \"]\n", want: []string{"172.17.0.1", "router.lan"}},
		{name: "env comma separated", env: map[string]string{envAllowedHTTPHosts: " 172.17.0.0/24, ,127.0.0.1 "}, want: []string{"172.17.0.0/24", "127.0.0.1"}},
		{name: "env overrides yaml", yaml: "allowed_http_hosts: [a.lan]\n", env: map[string]string{envAllowedHTTPHosts: "b.lan"}, want: []string{"b.lan"}},
		{name: "empty env clears yaml", yaml: "allowed_http_hosts: [a.lan]\n", env: map[string]string{envAllowedHTTPHosts: ""}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := ""
			if tt.yaml != "" {
				path = writeYAML(t, dir, tt.yaml)
			}
			cfg, err := Load(Options{Path: path, Lookup: envMap(testEnv(dir, tt.env))})
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if fmt.Sprint(cfg.AllowedHTTPHosts) != fmt.Sprint(tt.want) || len(cfg.AllowedHTTPHosts) != len(tt.want) {
				t.Errorf("AllowedHTTPHosts = %q, want %q", cfg.AllowedHTTPHosts, tt.want)
			}
		})
	}
}

func TestLoadServeSettingsFromEnv(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(Options{Lookup: envMap(testEnv(dir, map[string]string{
		envTrustedProxies:         "10.0.0.1,10.1.0.0/16",
		envTrustedOrigins:         "https://sms.example.net",
		envSessionIdleTimeout:     "30m",
		envSessionAbsoluteTimeout: "8h",
		envAPIRateLimit:           "0",
		envShutdownTimeout:        "5s",
	}))})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.TrustedProxies) != 2 || len(cfg.TrustedOrigins) != 1 || cfg.SessionIdleTimeout != 30*time.Minute ||
		cfg.SessionAbsoluteTimeout != 8*time.Hour || cfg.APIRateLimit != 0 || cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("cfg = %+v", cfg)
	}
	prefixes, err := ParsePrefixes(cfg.TrustedProxies)
	if err != nil || len(prefixes) != 2 || prefixes[0].Bits() != 32 || prefixes[1].String() != "10.1.0.0/16" {
		t.Errorf("ParsePrefixes = %v, %v", prefixes, err)
	}
}

func TestLoadMailGatewayFromEnv(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(Options{Lookup: envMap(testEnv(dir, map[string]string{
		envMailGwEnabled:        "true",
		envMailGwListen:         "127.0.0.1:2526",
		envMailGwDomain:         " SMS.Example.NET. ",
		envMailGwHostname:       "mx.example.net",
		envMailGwTLSMode:        "NONE",
		envMailGwMaxMessageSize: "32768",
		envMailGwMaxRecipients:  "3",
		envMailGwMaxConnections: "5",
		envMailGwCommandTimeout: "30s",
		envMailGwDataTimeout:    "2m",
		envMailGwBody:           "Subject_Text",
		envMailGwMaxConnsPerIP:  "2",
		envMailGwMaxSessionDur:  "5m",
		envMailGwMaxMsgsPerSess: "7",
	}))})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	m := cfg.MailGateway
	if !m.Enabled || m.Listen != "127.0.0.1:2526" || m.Domain != "sms.example.net" || m.Hostname != "mx.example.net" ||
		m.TLSMode != MailTLSNone || m.MaxMessageSize != 32768 || m.MaxRecipients != 3 || m.MaxConnections != 5 ||
		m.CommandTimeout != 30*time.Second || m.DataTimeout != 2*time.Minute || m.Body != MailBodySubjectText ||
		m.MaxConnectionsPerIP != 2 || m.MaxSessionDuration != 5*time.Minute || m.MaxMessagesPerSession != 7 {
		t.Errorf("mail gateway = %+v", m)
	}
	if !strings.Contains(cfg.String(), `mail_gateway.domain: "sms.example.net"`) {
		t.Errorf("String() misses the mail gateway:\n%s", cfg)
	}
	if c, k := m.TLSFiles(TLSConfig{CertFile: "a", KeyFile: "b"}); c != "a" || k != "b" {
		t.Errorf("TLSFiles fallback = %q %q", c, k)
	}
	m.TLSCertFile, m.TLSKeyFile = "c", "d"
	if c, k := m.TLSFiles(TLSConfig{CertFile: "a", KeyFile: "b"}); c != "c" || k != "d" {
		t.Errorf("TLSFiles own = %q %q", c, k)
	}
	if !ListenIsLoopback("127.0.0.1:1") || ListenIsLoopback(":1") {
		t.Error("ListenIsLoopback")
	}
}

func TestLoadMailNotifyFromEnv(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(Options{Lookup: envMap(testEnv(dir, map[string]string{
		envMailNotifyEnabled:  "1",
		envMailNotifyHost:     " smtp.example.net ",
		envMailNotifyPort:     "465",
		envMailNotifyTLSMode:  "TLS",
		envMailNotifyUsername: "tikker",
		envMailNotifyPassword: "relay-secret-1",
		envMailNotifyFrom:     "sms@example.net",
		envMailNotifyTo:       "a@example.net, b@example.net",
		envMailNotifyTimeout:  "10s",
	}))})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	m := cfg.MailNotify
	if !m.Enabled || m.SMTPHost != "smtp.example.net" || m.SMTPPort != 465 || m.TLSMode != MailTLSImplicit ||
		m.Username != "tikker" || m.Password != "relay-secret-1" || m.From != "sms@example.net" ||
		len(m.To) != 2 || m.To[1] != "b@example.net" || m.Timeout != 10*time.Second {
		t.Errorf("mail notify = %+v", m)
	}
	if out := cfg.String(); strings.Contains(out, "relay-secret-1") || !strings.Contains(out, "mail_notify.password: \"[REDACTED]\"") {
		t.Errorf("String() = %s", out)
	}
	for _, h := range []struct {
		host string
		want bool
	}{{"localhost", true}, {"127.0.0.1", true}, {"[::1]", true}, {"smtp.example.net", false}, {"10.0.0.1", false}} {
		if got := HostIsLoopback(h.host); got != h.want {
			t.Errorf("HostIsLoopback(%q) = %v", h.host, got)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string // file content; empty means no file
		missing bool   // point Path at a file that does not exist
		env     map[string]string
		wantSub string
	}{
		{
			name:    "unknown yaml key",
			yaml:    "listen: \":8080\"\nlisen_typo: 1\n",
			wantSub: "lisen_typo",
		},
		{
			name:    "bare integer is not a duration",
			yaml:    "retention: 2160\n",
			wantSub: "into time.Duration",
		},
		{
			name:    "malformed yaml",
			yaml:    "listen: [unclosed\n",
			wantSub: "config file",
		},
		{
			name:    "explicit path missing",
			missing: true,
			wantSub: "config file",
		},
		{
			name:    "bool env not parsable",
			env:     map[string]string{envAllowShortNumbers: "maybe"},
			wantSub: envAllowShortNumbers,
		},
		{
			name:    "integer env not parsable",
			env:     map[string]string{envRetryMaxAttempts: "five"},
			wantSub: envRetryMaxAttempts,
		},
		{
			name:    "float env not parsable",
			env:     map[string]string{envRetryFactor: "x"},
			wantSub: envRetryFactor,
		},
		{
			name:    "duration env not parsable",
			env:     map[string]string{envHealthInterval: "1 minute"},
			wantSub: envHealthInterval,
		},
		{
			name:    "several env errors are all reported",
			env:     map[string]string{envRetryMaxAttempts: "x", envRetryBase: "y"},
			wantSub: envRetryBase,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := ""
			switch {
			case tt.missing:
				path = filepath.Join(dir, "absent.yaml")
			case tt.yaml != "":
				path = writeYAML(t, dir, tt.yaml)
			}
			_, err := Load(Options{Path: path, Lookup: envMap(testEnv(dir, tt.env))})
			if err == nil {
				t.Fatal("Load() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

func TestLoadReportsAllEnvErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(Options{Lookup: envMap(testEnv(dir, map[string]string{
		envRetryMaxAttempts: "x",
		envRetryBase:        "y",
	}))})
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	for _, name := range []string{envRetryMaxAttempts, envRetryBase} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}

func TestLoadConfigPathSelection(t *testing.T) {
	tests := []struct {
		name       string
		flagPath   bool   // pass the file through Options.Path
		envPath    bool   // pass the file through TIKKER_CONFIG
		defaultLoc bool   // place the file at <data_dir>/config.yaml
		noFile     bool   // do not create any file
		wantListen string // expected listen value
		wantErr    bool
	}{
		{name: "default location used when present", defaultLoc: true, wantListen: ":7001"},
		{name: "default location absent is fine", noFile: true, wantListen: ":8080"},
		{name: "TIKKER_CONFIG used", envPath: true, wantListen: ":7002"},
		{name: "flag wins over TIKKER_CONFIG", flagPath: true, envPath: true, wantListen: ":7003"},
		{name: "TIKKER_CONFIG missing is an error", envPath: true, noFile: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			vars := testEnv(dir, nil)

			var flagFile string
			switch {
			case tt.defaultLoc:
				writeYAML(t, dir, "listen: \":7001\"\n")
			case tt.flagPath && tt.envPath:
				flagFile = writeYAML(t, t.TempDir(), "listen: \":7003\"\n")
				vars[envConfig] = writeYAML(t, t.TempDir(), "listen: \":7004\"\n")
			case tt.flagPath:
				flagFile = writeYAML(t, t.TempDir(), "listen: \":7003\"\n")
			case tt.envPath:
				vars[envConfig] = writeYAML(t, t.TempDir(), "listen: \":7002\"\n")
				if tt.noFile {
					vars[envConfig] = filepath.Join(dir, "nope.yaml")
				}
			}

			cfg, err := Load(Options{Path: flagFile, Lookup: envMap(vars)})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && cfg.Listen != tt.wantListen {
				t.Errorf("Listen = %q, want %q", cfg.Listen, tt.wantListen)
			}
		})
	}
}

func TestLoadEmptyYAMLKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeYAML(t, dir, "# only a comment\n")
	cfg, err := Load(Options{Path: path, Lookup: envMap(testEnv(dir, nil))})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want default", cfg.Listen)
	}
}

func TestLoadSecretKeyFromEnv(t *testing.T) {
	dir := t.TempDir()
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", SecretKeySize)))
	cfg, err := Load(Options{Lookup: envMap(testEnv(dir, map[string]string{envSecretKey: key}))})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if string(cfg.SecretKey) != key {
		t.Errorf("SecretKey not loaded from env")
	}
}

func TestLoadNilLookupUsesProcessEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envDataDir, dir)
	t.Setenv(envListen, ":9099")
	t.Setenv(envSetupSecret, "test-setup-secret-1234")
	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Listen != ":9099" {
		t.Errorf("Listen = %q, want :9099 from process environment", cfg.Listen)
	}
}

func TestSecretsNeverAppearInOutput(t *testing.T) {
	const (
		secretKey = "c2VjcmV0LWtleS12YWx1ZS10aGF0LWlzLW5vdC1sb2dnZWQ="
		adminPass = "very-secret-admin-password"
	)
	dir := t.TempDir()
	cfg, err := Load(Options{Lookup: envMap(testEnv(dir, map[string]string{
		envSecretKey:     base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", SecretKeySize))),
		envAdminPassword: adminPass,
	}))})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.SecretKey = Secret(secretKey)
	cfg.AdminPassword = Secret(adminPass)

	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("config", slog.Any("cfg", cfg))

	outputs := map[string]string{
		"String()":    cfg.String(),
		"%v":          fmt.Sprintf("%v", cfg),
		"%+v":         fmt.Sprintf("%+v", cfg),
		"%#v":         fmt.Sprintf("%#v", cfg),
		"secret %v":   fmt.Sprintf("%v", cfg.SecretKey),
		"secret %#v":  fmt.Sprintf("%#v", cfg.SecretKey),
		"secret %s":   fmt.Sprintf("%s", cfg.AdminPassword),
		"slog record": logBuf.String(),
	}
	for name, out := range outputs {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(out, secretKey) || strings.Contains(out, adminPass) {
				t.Errorf("output leaks a secret: %q", out)
			}
		})
	}
	if !strings.Contains(cfg.String(), "[REDACTED]") {
		t.Errorf("String() = %q, want redaction marker", cfg.String())
	}
}

func TestRetryPolicyConversion(t *testing.T) {
	cfg := Default()
	cfg.Retry = RetryConfig{MaxAttempts: 7, Base: time.Second, Factor: 3, Max: time.Minute, Jitter: 0.05}
	want := domain.RetryPolicy{MaxAttempts: 7, Initial: time.Second, Multiplier: 3, Max: time.Minute, Jitter: 0.05}
	if got := cfg.Retry.Policy(); got != want {
		t.Errorf("Policy() = %+v, want %+v", got, want)
	}
	if err := cfg.Retry.Policy().Validate(); err != nil {
		t.Errorf("Policy().Validate() = %v", err)
	}
}
