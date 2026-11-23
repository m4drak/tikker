// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Environment variables (SPEC §8 and the settings added in T10).
const (
	envConfig            = "TIKKER_CONFIG"
	envListen            = "TIKKER_LISTEN"
	envDataDir           = "TIKKER_DATA_DIR"
	envSecretKey         = "TIKKER_SECRET_KEY" //nolint:gosec // G101: variable name, not a credential
	envBaseURL           = "TIKKER_BASE_URL"
	envDefaultRegion     = "TIKKER_DEFAULT_REGION"
	envAllowShortNumbers = "TIKKER_ALLOW_SHORT_NUMBERS"
	envLongMessageStrat  = "TIKKER_LONG_MESSAGE_STRATEGY"
	envInboxPollInterval = "TIKKER_INBOX_POLL_INTERVAL"
	envHealthInterval    = "TIKKER_HEALTH_INTERVAL"
	envRetention         = "TIKKER_RETENTION"
	envLogLevel          = "TIKKER_LOG_LEVEL"
	envLogFormat         = "TIKKER_LOG_FORMAT"
	envSQLiteSynchronous = "TIKKER_SQLITE_SYNCHRONOUS"
	envRetryMaxAttempts  = "TIKKER_RETRY_MAX_ATTEMPTS"
	envRetryBase         = "TIKKER_RETRY_BASE"
	envRetryFactor       = "TIKKER_RETRY_FACTOR"
	envRetryMax          = "TIKKER_RETRY_MAX"
	envRetryJitter       = "TIKKER_RETRY_JITTER"
	envTLSCertFile       = "TIKKER_TLS_CERT_FILE"
	envTLSKeyFile        = "TIKKER_TLS_KEY_FILE"
	envAdminPassword     = "TIKKER_ADMIN_PASSWORD" //nolint:gosec // G101: variable name, not a credential
	envSetupSecret       = "TIKKER_SETUP_SECRET"   //nolint:gosec // G101: variable name, not a credential

	envTrustedProxies         = "TIKKER_TRUSTED_PROXIES"
	envTrustedOrigins         = "TIKKER_TRUSTED_ORIGINS"
	envSessionIdleTimeout     = "TIKKER_SESSION_IDLE_TIMEOUT"
	envSessionAbsoluteTimeout = "TIKKER_SESSION_ABSOLUTE_TIMEOUT"
	envAllowedHTTPHosts       = "TIKKER_ALLOWED_HTTP_HOSTS"
	envAPIRateLimit           = "TIKKER_API_RATE_LIMIT"
	envShutdownTimeout        = "TIKKER_SHUTDOWN_TIMEOUT"
)

// DefaultConfigFile is the YAML file name looked up in the data directory
// when no path is given explicitly.
const DefaultConfigFile = "config.yaml"

// LookupFunc resolves an environment variable, like [os.LookupEnv].
type LookupFunc func(key string) (string, bool)

// Options controls [Load].
type Options struct {
	// Path is the configuration file given explicitly (--config). When it
	// is empty, TIKKER_CONFIG is used, and then <data_dir>/config.yaml if
	// that file exists. An explicit path that does not exist is an error;
	// the default location is optional.
	//
	// The data directory that locates the default file is taken from
	// TIKKER_DATA_DIR or the built-in default. A data_dir key inside the
	// YAML file does not change where the file is looked up.
	Path string
	// Lookup resolves environment variables. Nil means os.LookupEnv.
	Lookup LookupFunc
}

// Load builds the effective configuration: defaults, then the YAML file,
// then TIKKER_* variables. It returns the configuration together with every
// error found (combined with errors.Join). The caller must not use the
// configuration when the error is non-nil.
func Load(opts Options) (Config, error) {
	lookup := opts.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}

	cfg := Default()
	if v, ok := lookup(envDataDir); ok {
		cfg.DataDir = v
	}

	path, explicit := resolvePath(opts.Path, lookup, cfg.DataDir)

	var errs []error
	if err := loadFile(&cfg, path, explicit); err != nil {
		errs = append(errs, err)
	}
	if err := applyEnv(&cfg, lookup); err != nil {
		errs = append(errs, err)
	}

	normalize(&cfg)
	if err := Validate(cfg); err != nil {
		errs = append(errs, err)
	}
	return cfg, errors.Join(errs...)
}

func resolvePath(flagPath string, lookup LookupFunc, dataDir string) (string, bool) {
	if flagPath != "" {
		return flagPath, true
	}
	if v, ok := lookup(envConfig); ok && v != "" {
		return v, true
	}
	return filepath.Join(dataDir, DefaultConfigFile), false
}

// loadFile decodes the YAML file into cfg. Keys that are not part of the
// configuration are errors. A missing default file is not an error.
func loadFile(cfg *Config, path string, explicit bool) error {
	// The path is chosen by the operator (--config, TIKKER_CONFIG or the data directory).
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config file: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty file
		}
		return fmt.Errorf("config file %s: %w", path, err)
	}
	return nil
}

// applyEnv overrides cfg with the TIKKER_* variables that are set. Every
// unparsable value is reported; values of secrets are never included in
// messages.
func applyEnv(cfg *Config, lookup LookupFunc) error {
	var errs []error

	str := func(name string, dst *string) {
		if v, ok := lookup(name); ok {
			*dst = v
		}
	}
	secret := func(name string, dst *Secret) {
		if v, ok := lookup(name); ok {
			*dst = Secret(v)
		}
	}
	boolean := func(name string, dst *bool) {
		v, ok := lookup(name)
		if !ok {
			return
		}
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: want true or false", name))
			return
		}
		*dst = b
	}
	integer := func(name string, dst *int) {
		v, ok := lookup(name)
		if !ok {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: want an integer", name))
			return
		}
		*dst = n
	}
	float := func(name string, dst *float64) {
		v, ok := lookup(name)
		if !ok {
			return
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: want a number", name))
			return
		}
		*dst = f
	}
	duration := func(name string, dst *time.Duration) {
		v, ok := lookup(name)
		if !ok {
			return
		}
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: want a duration such as 15s or 2160h", name))
			return
		}
		*dst = d
	}

	// list reads a comma-separated list; an empty variable clears it.
	list := func(name string, dst *[]string) {
		if v, ok := lookup(name); ok {
			*dst = splitList(v)
		}
	}

	str(envListen, &cfg.Listen)
	str(envDataDir, &cfg.DataDir)
	secret(envSecretKey, &cfg.SecretKey)
	str(envBaseURL, &cfg.BaseURL)
	str(envDefaultRegion, &cfg.DefaultRegion)
	boolean(envAllowShortNumbers, &cfg.AllowShortNumbers)
	if v, ok := lookup(envLongMessageStrat); ok {
		cfg.LongMessageStrategy = LongMessageStrategy(v)
	}
	duration(envInboxPollInterval, &cfg.InboxPollInterval)
	duration(envHealthInterval, &cfg.HealthInterval)
	duration(envRetention, &cfg.Retention)
	str(envLogLevel, &cfg.LogLevel)
	str(envLogFormat, &cfg.LogFormat)
	str(envSQLiteSynchronous, &cfg.SQLiteSynchronous)
	integer(envRetryMaxAttempts, &cfg.Retry.MaxAttempts)
	duration(envRetryBase, &cfg.Retry.Base)
	float(envRetryFactor, &cfg.Retry.Factor)
	duration(envRetryMax, &cfg.Retry.Max)
	float(envRetryJitter, &cfg.Retry.Jitter)
	str(envTLSCertFile, &cfg.TLS.CertFile)
	str(envTLSKeyFile, &cfg.TLS.KeyFile)
	secret(envAdminPassword, &cfg.AdminPassword)
	secret(envSetupSecret, &cfg.SetupSecret)
	list(envTrustedProxies, &cfg.TrustedProxies)
	list(envTrustedOrigins, &cfg.TrustedOrigins)
	duration(envSessionIdleTimeout, &cfg.SessionIdleTimeout)
	duration(envSessionAbsoluteTimeout, &cfg.SessionAbsoluteTimeout)
	list(envAllowedHTTPHosts, &cfg.AllowedHTTPHosts)
	integer(envAPIRateLimit, &cfg.APIRateLimit)
	duration(envShutdownTimeout, &cfg.ShutdownTimeout)

	return errors.Join(errs...)
}

// normalize canonicalises values that are case-insensitive or padded.
func normalize(cfg *Config) {
	cfg.Listen = strings.TrimSpace(cfg.Listen)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.DefaultRegion = strings.ToUpper(strings.TrimSpace(cfg.DefaultRegion))
	cfg.LongMessageStrategy = LongMessageStrategy(strings.ToLower(strings.TrimSpace(string(cfg.LongMessageStrategy))))
	cfg.LogLevel = strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	cfg.LogFormat = strings.ToLower(strings.TrimSpace(cfg.LogFormat))
	cfg.SQLiteSynchronous = strings.ToUpper(strings.TrimSpace(cfg.SQLiteSynchronous))
	cfg.TrustedProxies = trimList(cfg.TrustedProxies)
	cfg.TrustedOrigins = trimList(cfg.TrustedOrigins)
	cfg.AllowedHTTPHosts = trimList(cfg.AllowedHTTPHosts)
}

// splitList splits a comma-separated environment value.
func splitList(v string) []string {
	return trimList(strings.Split(v, ","))
}

// trimList trims the items and drops empty ones; nil when nothing is left.
func trimList(items []string) []string {
	var out []string
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" {
			out = append(out, it)
		}
	}
	return out
}
