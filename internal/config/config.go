// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package config loads and validates the tikker configuration (SPEC §8).
//
// Sources are applied in this order, later ones win: built-in defaults,
// a YAML file, and TIKKER_* environment variables.
package config

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// LongMessageStrategy selects what happens to a message that does not fit
// into the segments a connector supports (SPEC §4.6).
type LongMessageStrategy string

// Long message strategies.
const (
	LongMessageReject   LongMessageStrategy = "reject"
	LongMessageSplit    LongMessageStrategy = "split"
	LongMessageTruncate LongMessageStrategy = "truncate"
)

// Config is the effective tikker configuration. Use [Load] to obtain one.
type Config struct {
	Listen              string              `yaml:"listen"`
	DataDir             string              `yaml:"data_dir"`
	SecretKey           Secret              `yaml:"secret_key"`
	BaseURL             string              `yaml:"base_url"`
	DefaultRegion       string              `yaml:"default_region"`
	AllowShortNumbers   bool                `yaml:"allow_short_numbers"`
	LongMessageStrategy LongMessageStrategy `yaml:"long_message_strategy"`
	InboxPollInterval   time.Duration       `yaml:"inbox_poll_interval"`
	HealthInterval      time.Duration       `yaml:"health_interval"`
	Retention           time.Duration       `yaml:"retention"`
	LogLevel            string              `yaml:"log_level"`
	LogFormat           string              `yaml:"log_format"`
	SQLiteSynchronous   string              `yaml:"sqlite_synchronous"`
	Retry               RetryConfig         `yaml:"retry"`
	TLS                 TLSConfig           `yaml:"tls"`
	AdminPassword       Secret              `yaml:"admin_password"`

	// TrustedProxies are the reverse proxies (IP addresses or CIDR
	// prefixes) whose X-Forwarded-For header is believed for the client IP.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// TrustedOrigins are extra origins (scheme://host[:port]) allowed to
	// send state-changing requests to the panel, besides base_url.
	TrustedOrigins []string `yaml:"trusted_origins"`
	// SessionIdleTimeout and SessionAbsoluteTimeout bound panel sessions
	// (SPEC §7).
	SessionIdleTimeout     time.Duration `yaml:"session_idle_timeout"`
	SessionAbsoluteTimeout time.Duration `yaml:"session_absolute_timeout"`
	// AllowedHTTPHosts are the device hosts (IP addresses, CIDR prefixes or
	// literal host names) that may be reached over plain http:// (SPEC §3.3,
	// the veth subnet in container mode). Empty: devices need https://.
	AllowedHTTPHosts []string `yaml:"allowed_http_hosts"`
	// APIRateLimit is the per-token request budget per minute of the REST
	// API (SPEC §4.5); 0 disables the limit.
	APIRateLimit int `yaml:"api_rate_limit"`
	// ShutdownTimeout bounds the graceful shutdown: in-flight HTTP
	// requests and sends may finish within it (SPEC §2.2).
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// RetryConfig is the outbound retry schedule (SPEC §4.2).
type RetryConfig struct {
	// MaxAttempts is the total number of attempts, including the first.
	MaxAttempts int `yaml:"max_attempts"`
	// Base is the nominal delay after the first failed attempt.
	Base time.Duration `yaml:"base"`
	// Factor is the growth factor of the nominal delay.
	Factor float64 `yaml:"factor"`
	// Max caps the nominal delay.
	Max time.Duration `yaml:"max"`
	// Jitter is the relative random spread of the delay, in [0, 1).
	Jitter float64 `yaml:"jitter"`
}

// Policy converts the retry settings into the domain retry policy.
func (r RetryConfig) Policy() domain.RetryPolicy {
	return domain.RetryPolicy{
		MaxAttempts: r.MaxAttempts,
		Initial:     r.Base,
		Multiplier:  r.Factor,
		Max:         r.Max,
		Jitter:      r.Jitter,
	}
}

// TLSConfig holds the optional built-in HTTPS certificate. Both fields are
// set together or not at all.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Default returns the configuration with the default values from SPEC §8
// and the retry policy from [domain.DefaultRetryPolicy].
func Default() Config {
	rp := domain.DefaultRetryPolicy()
	return Config{
		Listen:              ":8080",
		DataDir:             "/data",
		DefaultRegion:       "PL",
		LongMessageStrategy: LongMessageReject,
		InboxPollInterval:   15 * time.Second,
		HealthInterval:      60 * time.Second,
		Retention:           2160 * time.Hour,
		LogLevel:            "info",
		LogFormat:           "json",
		SQLiteSynchronous:   "NORMAL",
		// Session timeouts as auth.DefaultSessionIdleTimeout and
		// auth.DefaultSessionAbsoluteTimeout (SPEC §7).
		SessionIdleTimeout:     time.Hour,
		SessionAbsoluteTimeout: 12 * time.Hour,
		APIRateLimit:           120,
		ShutdownTimeout:        30 * time.Second,
		Retry: RetryConfig{
			MaxAttempts: rp.MaxAttempts,
			Base:        rp.Initial,
			Factor:      rp.Multiplier,
			Max:         rp.Max,
			Jitter:      rp.Jitter,
		},
	}
}

// Secret is a sensitive string. Its formatted form is always redacted, so
// it stays out of logs and fmt output. The empty value is shown as empty.
type Secret string

const redacted = "[REDACTED]"

// String returns the redacted form.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}

// Format implements fmt.Formatter, so every verb (including %#v) redacts.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, s.String())
}

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// entries lists the effective settings in a stable order. It is the single
// source for [Config.String], [Config.LogValue] and `--check-config`.
func (c Config) entries() []entry {
	return []entry{
		{"listen", strconv.Quote(c.Listen)},
		{"data_dir", strconv.Quote(c.DataDir)},
		{"secret_key", strconv.Quote(c.SecretKey.String())},
		{"base_url", strconv.Quote(c.BaseURL)},
		{"default_region", strconv.Quote(c.DefaultRegion)},
		{"allow_short_numbers", strconv.FormatBool(c.AllowShortNumbers)},
		{"long_message_strategy", strconv.Quote(string(c.LongMessageStrategy))},
		{"inbox_poll_interval", strconv.Quote(c.InboxPollInterval.String())},
		{"health_interval", strconv.Quote(c.HealthInterval.String())},
		{"retention", strconv.Quote(c.Retention.String())},
		{"log_level", strconv.Quote(c.LogLevel)},
		{"log_format", strconv.Quote(c.LogFormat)},
		{"sqlite_synchronous", strconv.Quote(c.SQLiteSynchronous)},
		{"retry.max_attempts", strconv.Itoa(c.Retry.MaxAttempts)},
		{"retry.base", strconv.Quote(c.Retry.Base.String())},
		{"retry.factor", strconv.FormatFloat(c.Retry.Factor, 'g', -1, 64)},
		{"retry.max", strconv.Quote(c.Retry.Max.String())},
		{"retry.jitter", strconv.FormatFloat(c.Retry.Jitter, 'g', -1, 64)},
		{"tls.cert_file", strconv.Quote(c.TLS.CertFile)},
		{"tls.key_file", strconv.Quote(c.TLS.KeyFile)},
		{"admin_password", strconv.Quote(c.AdminPassword.String())},
		{"trusted_proxies", quoteList(c.TrustedProxies)},
		{"trusted_origins", quoteList(c.TrustedOrigins)},
		{"session_idle_timeout", strconv.Quote(c.SessionIdleTimeout.String())},
		{"session_absolute_timeout", strconv.Quote(c.SessionAbsoluteTimeout.String())},
		{"allowed_http_hosts", quoteList(c.AllowedHTTPHosts)},
		{"api_rate_limit", strconv.Itoa(c.APIRateLimit)},
		{"shutdown_timeout", strconv.Quote(c.ShutdownTimeout.String())},
	}
}

// quoteList renders a list setting as [a, b] with quoted items.
func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, it := range items {
		q[i] = strconv.Quote(it)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

type entry struct {
	key   string
	value string
}

// String returns the effective configuration as "key: value" lines with
// secrets redacted.
func (c Config) String() string {
	var b []byte
	for _, e := range c.entries() {
		b = append(b, e.key...)
		b = append(b, ": "...)
		b = append(b, e.value...)
		b = append(b, '\n')
	}
	return string(b)
}

// LogValue implements slog.LogValuer. Secrets are redacted.
func (c Config) LogValue() slog.Value {
	entries := c.entries()
	attrs := make([]slog.Attr, 0, len(entries))
	for _, e := range entries {
		attrs = append(attrs, slog.String(e.key, e.value))
	}
	return slog.GroupValue(attrs...)
}
