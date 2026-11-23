// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"encoding/base64"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	validKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", SecretKeySize)))

	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr []string // substrings that must appear; nil means valid
	}{
		{name: "defaults are valid", mutate: func(*Config) {}},
		{name: "valid secret key", mutate: func(c *Config) { c.SecretKey = Secret(validKey) }},
		{name: "valid base url", mutate: func(c *Config) { c.BaseURL = "https://tikker.example.net" }},
		{name: "valid tls pair", mutate: func(c *Config) {
			c.TLS = TLSConfig{CertFile: existing, KeyFile: existing}
		}},
		{name: "valid admin password", mutate: func(c *Config) { c.AdminPassword = "twelve-chars" }},
		{name: "no secret needed with loopback listen", mutate: func(c *Config) {
			c.Listen = "127.0.0.1:8080"
			c.SetupSecret = ""
		}},
		{name: "secret required when exposed and no admin password", mutate: func(c *Config) {
			c.Listen = ":8080"
			c.AdminPassword = ""
			c.SetupSecret = ""
		}, wantErr: []string{"setup_secret (TIKKER_SETUP_SECRET)", "required"}},
		{name: "setup secret too short", mutate: func(c *Config) {
			c.SetupSecret = "short"
		}, wantErr: []string{"setup_secret (TIKKER_SETUP_SECRET)", "at least 12"}},

		{name: "listen without port", mutate: func(c *Config) { c.Listen = "localhost" }, wantErr: []string{"listen (TIKKER_LISTEN)"}},
		{name: "listen port too large", mutate: func(c *Config) { c.Listen = ":70000" }, wantErr: []string{"port must be between 1 and 65535"}},
		{name: "listen port zero", mutate: func(c *Config) { c.Listen = ":0" }, wantErr: []string{"port must be between 1 and 65535"}},
		{name: "empty data dir", mutate: func(c *Config) { c.DataDir = "  " }, wantErr: []string{"data_dir (TIKKER_DATA_DIR)"}},
		{name: "secret key not base64", mutate: func(c *Config) { c.SecretKey = "not base64!!" }, wantErr: []string{"secret_key (TIKKER_SECRET_KEY)", "standard base64"}},
		{name: "secret key too short", mutate: func(c *Config) {
			c.SecretKey = Secret(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")))
		}, wantErr: []string{"decodes to 16 bytes"}},
		{name: "base url wrong scheme", mutate: func(c *Config) { c.BaseURL = "ftp://tikker.example.net" }, wantErr: []string{"scheme must be http or https"}},
		{name: "base url without host", mutate: func(c *Config) { c.BaseURL = "https://" }, wantErr: []string{"must include a host"}},
		{name: "base url unparsable", mutate: func(c *Config) { c.BaseURL = "http://[::1" }, wantErr: []string{"base_url (TIKKER_BASE_URL)"}},
		{name: "region not a code", mutate: func(c *Config) { c.DefaultRegion = "POL" }, wantErr: []string{"default_region (TIKKER_DEFAULT_REGION)"}},
		{name: "region with digits", mutate: func(c *Config) { c.DefaultRegion = "P1" }, wantErr: []string{"two-letter"}},
		{name: "unknown long message strategy", mutate: func(c *Config) { c.LongMessageStrategy = "drop" }, wantErr: []string{"reject, split or truncate"}},
		{name: "split strategy is valid", mutate: func(c *Config) { c.LongMessageStrategy = LongMessageSplit }},
		{name: "zero inbox poll interval", mutate: func(c *Config) { c.InboxPollInterval = 0 }, wantErr: []string{"inbox_poll_interval (TIKKER_INBOX_POLL_INTERVAL)", "at least 1s"}},
		{name: "negative inbox poll interval", mutate: func(c *Config) { c.InboxPollInterval = -time.Second }, wantErr: []string{"inbox_poll_interval"}},
		{name: "health interval below one second", mutate: func(c *Config) { c.HealthInterval = 500 * time.Millisecond }, wantErr: []string{"health_interval (TIKKER_HEALTH_INTERVAL)"}},
		{name: "negative retention", mutate: func(c *Config) { c.Retention = -time.Hour }, wantErr: []string{"retention (TIKKER_RETENTION)"}},
		{name: "unknown log level", mutate: func(c *Config) { c.LogLevel = "trace" }, wantErr: []string{"log_level (TIKKER_LOG_LEVEL)", "unknown log level"}},
		{name: "unknown log format", mutate: func(c *Config) { c.LogFormat = "xml" }, wantErr: []string{"log_format (TIKKER_LOG_FORMAT)"}},
		{name: "unknown sqlite synchronous", mutate: func(c *Config) { c.SQLiteSynchronous = "MAYBE" }, wantErr: []string{"sqlite_synchronous (TIKKER_SQLITE_SYNCHRONOUS)"}},
		{name: "zero retry attempts", mutate: func(c *Config) { c.Retry.MaxAttempts = 0 }, wantErr: []string{"retry.max_attempts (TIKKER_RETRY_MAX_ATTEMPTS)"}},
		{name: "zero retry base", mutate: func(c *Config) { c.Retry.Base = 0 }, wantErr: []string{"retry.base (TIKKER_RETRY_BASE)"}},
		{name: "factor below one", mutate: func(c *Config) { c.Retry.Factor = 0.5 }, wantErr: []string{"retry.factor (TIKKER_RETRY_FACTOR)"}},
		{name: "factor NaN", mutate: func(c *Config) { c.Retry.Factor = math.NaN() }, wantErr: []string{"retry.factor"}},
		{name: "factor infinite", mutate: func(c *Config) { c.Retry.Factor = math.Inf(1) }, wantErr: []string{"retry.factor"}},
		{name: "max below base", mutate: func(c *Config) { c.Retry.Max = time.Second }, wantErr: []string{"retry.max (TIKKER_RETRY_MAX)"}},
		{name: "jitter one", mutate: func(c *Config) { c.Retry.Jitter = 1 }, wantErr: []string{"retry.jitter (TIKKER_RETRY_JITTER)"}},
		{name: "negative jitter", mutate: func(c *Config) { c.Retry.Jitter = -0.1 }, wantErr: []string{"retry.jitter"}},
		{name: "tls cert without key", mutate: func(c *Config) { c.TLS.CertFile = existing }, wantErr: []string{"must be set together"}},
		{name: "tls key without cert", mutate: func(c *Config) { c.TLS.KeyFile = existing }, wantErr: []string{"must be set together"}},
		{name: "tls file missing", mutate: func(c *Config) {
			c.TLS = TLSConfig{CertFile: "/nonexistent/cert.pem", KeyFile: "/nonexistent/key.pem"}
		}, wantErr: []string{"tls.cert_file (TIKKER_TLS_CERT_FILE)", "not accessible"}},
		{name: "valid trusted proxies", mutate: func(c *Config) { c.TrustedProxies = []string{"10.0.0.1", "192.168.88.0/24", "fd00::/8"} }},
		{name: "trusted proxy not an address", mutate: func(c *Config) { c.TrustedProxies = []string{"proxy.local"} }, wantErr: []string{"trusted_proxies (TIKKER_TRUSTED_PROXIES)"}},
		{name: "trusted proxy bad prefix", mutate: func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/33"} }, wantErr: []string{"trusted_proxies"}},
		{name: "valid trusted origins", mutate: func(c *Config) { c.TrustedOrigins = []string{"https://sms.example.net", "http://192.168.88.1:8080"} }},
		{name: "trusted origin with path", mutate: func(c *Config) { c.TrustedOrigins = []string{"https://sms.example.net/ui"} }, wantErr: []string{"trusted_origins (TIKKER_TRUSTED_ORIGINS)"}},
		{name: "trusted origin without scheme", mutate: func(c *Config) { c.TrustedOrigins = []string{"sms.example.net"} }, wantErr: []string{"trusted_origins"}},
		{name: "session idle too short", mutate: func(c *Config) { c.SessionIdleTimeout = time.Second }, wantErr: []string{"session_idle_timeout (TIKKER_SESSION_IDLE_TIMEOUT)"}},
		{name: "session absolute below idle", mutate: func(c *Config) { c.SessionAbsoluteTimeout = 30 * time.Minute }, wantErr: []string{"session_absolute_timeout (TIKKER_SESSION_ABSOLUTE_TIMEOUT)"}},
		{name: "valid allowed http hosts", mutate: func(c *Config) { c.AllowedHTTPHosts = []string{"172.17.0.1", "172.17.0.0/24", "router.lan", "::1"} }},
		{name: "allowed http host bad prefix", mutate: func(c *Config) { c.AllowedHTTPHosts = []string{"172.17.0.0/99"} }, wantErr: []string{"allowed_http_hosts (TIKKER_ALLOWED_HTTP_HOSTS)"}},
		{name: "allowed http host with scheme", mutate: func(c *Config) { c.AllowedHTTPHosts = []string{"http://router.lan"} }, wantErr: []string{"allowed_http_hosts"}},
		{name: "api rate limit disabled", mutate: func(c *Config) { c.APIRateLimit = 0 }},
		{name: "negative api rate limit", mutate: func(c *Config) { c.APIRateLimit = -1 }, wantErr: []string{"api_rate_limit (TIKKER_API_RATE_LIMIT)"}},
		{name: "shutdown timeout too short", mutate: func(c *Config) { c.ShutdownTimeout = 0 }, wantErr: []string{"shutdown_timeout (TIKKER_SHUTDOWN_TIMEOUT)"}},
		{name: "admin password too short", mutate: func(c *Config) { c.AdminPassword = "short" }, wantErr: []string{"admin_password (TIKKER_ADMIN_PASSWORD)", "at least 12"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			// A setup secret keeps the loopback-exposed default listen
			// valid while the other cases focus on their own setting.
			c.SetupSecret = "test-setup-secret-1234"
			tt.mutate(&c)
			err := Validate(c)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want error containing %q", tt.wantErr)
			}
			for _, sub := range tt.wantErr {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not contain %q", err, sub)
				}
			}
		})
	}
}

func TestValidateAggregatesErrors(t *testing.T) {
	c := Default()
	c.Listen = "nope"
	c.LogLevel = "trace"
	c.Retry.MaxAttempts = 0

	err := Validate(c)
	if err == nil {
		t.Fatal("Validate() error = nil")
	}
	for _, sub := range []string{"listen", "log_level", "retry.max_attempts"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("aggregated error %q misses %q", err, sub)
		}
	}
	if lines := strings.Count(err.Error(), "\n"); lines < 2 {
		t.Errorf("expected one line per problem, got %d line breaks in %q", lines, err)
	}
}

func TestSecretErrorsDoNotEchoValue(t *testing.T) {
	const bad = "super-secret-but-not-base64!!"
	const badPass = "x"

	c := Default()
	c.SecretKey = Secret(bad)
	c.AdminPassword = Secret(badPass)

	err := Validate(c)
	if err == nil {
		t.Fatal("Validate() error = nil")
	}
	if strings.Contains(err.Error(), bad) {
		t.Errorf("error %q echoes the secret key", err)
	}
	if strings.Contains(err.Error(), badPass) {
		t.Errorf("error %q echoes the admin password", err)
	}
}

func TestDecodeSecretKey(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantLen int
		wantErr bool
	}{
		{name: "32 bytes", in: base64.StdEncoding.EncodeToString(make([]byte, 32)), wantLen: 32},
		{name: "surrounding space allowed", in: " " + base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\n", wantLen: 32},
		{name: "31 bytes", in: base64.StdEncoding.EncodeToString(make([]byte, 31)), wantErr: true},
		{name: "33 bytes", in: base64.StdEncoding.EncodeToString(make([]byte, 33)), wantErr: true},
		{name: "not base64", in: "%%%", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := DecodeSecretKey(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("DecodeSecretKey() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(key) != tt.wantLen {
				t.Errorf("len(key) = %d, want %d", len(key), tt.wantLen)
			}
		})
	}
}
