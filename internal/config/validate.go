// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/logging"
)

// SecretKeySize is the length in bytes of the key that encrypts router
// passwords (SPEC §7).
const SecretKeySize = 32

// minAdminPasswordLen is the minimum length of the bootstrap admin password.
const minAdminPasswordLen = 12

// minInterval bounds the polling and health intervals, so that a typo such
// as 1ms does not turn into a busy loop on the router.
const minInterval = time.Second

// setting names a configuration key in messages: its YAML path and the
// environment variable that overrides it.
type setting struct {
	key string
	env string
}

func (s setting) errorf(format string, args ...any) error {
	return fmt.Errorf("%s (%s): %s", s.key, s.env, fmt.Sprintf(format, args...))
}

var (
	sListen              = setting{"listen", envListen}
	sDataDir             = setting{"data_dir", envDataDir}
	sSecretKey           = setting{"secret_key", envSecretKey}
	sBaseURL             = setting{"base_url", envBaseURL}
	sDefaultRegion       = setting{"default_region", envDefaultRegion}
	sLongMessageStrategy = setting{"long_message_strategy", envLongMessageStrat}
	sInboxPollInterval   = setting{"inbox_poll_interval", envInboxPollInterval}
	sHealthInterval      = setting{"health_interval", envHealthInterval}
	sRetention           = setting{"retention", envRetention}
	sLogLevel            = setting{"log_level", envLogLevel}
	sLogFormat           = setting{"log_format", envLogFormat}
	sSQLiteSynchronous   = setting{"sqlite_synchronous", envSQLiteSynchronous}
	sRetryMaxAttempts    = setting{"retry.max_attempts", envRetryMaxAttempts}
	sRetryBase           = setting{"retry.base", envRetryBase}
	sRetryFactor         = setting{"retry.factor", envRetryFactor}
	sRetryMax            = setting{"retry.max", envRetryMax}
	sRetryJitter         = setting{"retry.jitter", envRetryJitter}
	sTLSCertFile         = setting{"tls.cert_file", envTLSCertFile}
	sTLSKeyFile          = setting{"tls.key_file", envTLSKeyFile}
	sAdminPassword       = setting{"admin_password", envAdminPassword}
	sSetupSecret         = setting{"setup_secret", envSetupSecret}
	sTrustedProxies      = setting{"trusted_proxies", envTrustedProxies}
	sTrustedOrigins      = setting{"trusted_origins", envTrustedOrigins}
	sSessionIdle         = setting{"session_idle_timeout", envSessionIdleTimeout}
	sSessionAbsolute     = setting{"session_absolute_timeout", envSessionAbsoluteTimeout}
	sAllowedHTTPHosts    = setting{"allowed_http_hosts", envAllowedHTTPHosts}
	sAPIRateLimit        = setting{"api_rate_limit", envAPIRateLimit}
	sHashConcurrency     = setting{"password_hash_concurrency", envHashConcurrency}
	sShutdownTimeout     = setting{"shutdown_timeout", envShutdownTimeout}
	sAgentSendTimeout    = setting{"agent.send_timeout", envAgentSendTimeout}
	sAgentStaleAfter     = setting{"agent.stale_after", envAgentStaleAfter}
	sAgentMaxSegments    = setting{"agent.max_segments", envAgentMaxSegments}

	sMailGwListen         = setting{"mail_gateway.listen", envMailGwListen}
	sMailGwDomain         = setting{"mail_gateway.domain", envMailGwDomain}
	sMailGwHostname       = setting{"mail_gateway.hostname", envMailGwHostname}
	sMailGwTLSMode        = setting{"mail_gateway.tls_mode", envMailGwTLSMode}
	sMailGwTLSCertFile    = setting{"mail_gateway.tls_cert_file", envMailGwTLSCertFile}
	sMailGwTLSKeyFile     = setting{"mail_gateway.tls_key_file", envMailGwTLSKeyFile}
	sMailGwMaxMessageSize = setting{"mail_gateway.max_message_size", envMailGwMaxMessageSize}
	sMailGwMaxRecipients  = setting{"mail_gateway.max_recipients", envMailGwMaxRecipients}
	sMailGwMaxConnections = setting{"mail_gateway.max_connections", envMailGwMaxConnections}
	sMailGwMaxConnsPerIP  = setting{"mail_gateway.max_connections_per_ip", envMailGwMaxConnsPerIP}
	sMailGwMaxSessionDur  = setting{"mail_gateway.max_session_duration", envMailGwMaxSessionDur}
	sMailGwMaxMsgsPerSess = setting{"mail_gateway.max_messages_per_session", envMailGwMaxMsgsPerSess}
	sMailGwCommandTimeout = setting{"mail_gateway.command_timeout", envMailGwCommandTimeout}
	sMailGwDataTimeout    = setting{"mail_gateway.data_timeout", envMailGwDataTimeout}
	sMailGwBody           = setting{"mail_gateway.body", envMailGwBody}

	sMailNotifyHost         = setting{"mail_notify.smtp_host", envMailNotifyHost}
	sMailNotifyPort         = setting{"mail_notify.smtp_port", envMailNotifyPort}
	sMailNotifyTLSMode      = setting{"mail_notify.tls_mode", envMailNotifyTLSMode}
	sMailNotifyUsername     = setting{"mail_notify.username", envMailNotifyUsername}
	sMailNotifyPassword     = setting{"mail_notify.password", envMailNotifyPassword}
	sMailNotifyPasswordFile = setting{"mail_notify.password_file", envMailNotifyPasswordFile}
	sMailNotifyFrom         = setting{"mail_notify.from", envMailNotifyFrom}
	sMailNotifyTo           = setting{"mail_notify.to", envMailNotifyTo}
	sMailNotifyTimeout      = setting{"mail_notify.timeout", envMailNotifyTimeout}
)

var regionPattern = regexp.MustCompile(`^[A-Z]{2}$`)

// sqliteSynchronousModes are the values accepted by the SQLite pragma.
var sqliteSynchronousModes = map[string]bool{"OFF": true, "NORMAL": true, "FULL": true, "EXTRA": true}

// Validate checks the invariants of a configuration and returns every
// violation, combined with errors.Join. Messages name the setting and its
// environment variable. They never include the value of a secret.
func Validate(c Config) error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	add(validateListen(c.Listen))
	if strings.TrimSpace(c.DataDir) == "" {
		add(sDataDir.errorf("must not be empty"))
	}
	if c.SecretKey != "" {
		if _, err := DecodeSecretKey(string(c.SecretKey)); err != nil {
			add(sSecretKey.errorf("%v", err))
		}
	}
	add(validateBaseURL(c.BaseURL))
	if !regionPattern.MatchString(c.DefaultRegion) {
		add(sDefaultRegion.errorf("must be a two-letter country code such as PL, got %q", c.DefaultRegion))
	}

	switch c.LongMessageStrategy {
	case LongMessageReject, LongMessageSplit, LongMessageTruncate:
	default:
		add(sLongMessageStrategy.errorf("must be reject, split or truncate, got %q", c.LongMessageStrategy))
	}

	add(validateMinDuration(sInboxPollInterval, c.InboxPollInterval, minInterval))
	add(validateMinDuration(sHealthInterval, c.HealthInterval, minInterval))
	if c.Retention <= 0 {
		add(sRetention.errorf("must be positive, got %s", c.Retention))
	}

	if _, err := logging.ParseLevel(c.LogLevel); err != nil {
		add(sLogLevel.errorf("%v", err))
	}
	switch c.LogFormat {
	case logging.FormatJSON, logging.FormatText:
	default:
		add(sLogFormat.errorf("must be json or text, got %q", c.LogFormat))
	}
	if !sqliteSynchronousModes[c.SQLiteSynchronous] {
		add(sSQLiteSynchronous.errorf("must be OFF, NORMAL, FULL or EXTRA, got %q", c.SQLiteSynchronous))
	}

	add(validateRetry(c.Retry))
	add(validateTLS(c.TLS))
	add(validateAgent(c.Agent))
	add(validateMailGateway(c.MailGateway, c.TLS))
	add(validateMailNotify(c.MailNotify))

	if n := utf8.RuneCountInString(string(c.AdminPassword)); n > 0 && n < minAdminPasswordLen {
		add(sAdminPassword.errorf("must be at least %d characters", minAdminPasswordLen))
	}

	// F-01 / SPEC §6: an instance reachable off-loopback must be protected
	// by either a bootstrap admin password (admin is created at startup) or
	// a setup secret (the first-run wizard is gated on it). Default listen
	// ":8080" binds all interfaces, so this closes unauthorized takeover of
	// a network-exposed, uninitialized instance.
	if rc := utf8.RuneCountInString(string(c.SetupSecret)); rc > 0 && rc < minAdminPasswordLen {
		add(sSetupSecret.errorf("must be at least %d characters", minAdminPasswordLen))
	}
	if string(c.AdminPassword) == "" && string(c.SetupSecret) == "" && listenExposed(c.Listen) {
		add(sSetupSecret.errorf("required when TIKKER_ADMIN_PASSWORD is empty and the panel is reachable off-loopback; set TIKKER_SETUP_SECRET to gate the first-run wizard (or set the admin password)"))
	}

	if _, err := ParsePrefixes(c.TrustedProxies); err != nil {
		add(sTrustedProxies.errorf("%v", err))
	}
	for _, o := range c.TrustedOrigins {
		add(validateOrigin(o))
	}
	add(validateMinDuration(sSessionIdle, c.SessionIdleTimeout, time.Minute))
	if c.SessionAbsoluteTimeout < c.SessionIdleTimeout {
		add(sSessionAbsolute.errorf("must not be less than session_idle_timeout (%s), got %s",
			c.SessionIdleTimeout, c.SessionAbsoluteTimeout))
	}
	for _, h := range c.AllowedHTTPHosts {
		add(validateHTTPHost(h))
	}
	if c.APIRateLimit < 0 {
		add(sAPIRateLimit.errorf("must be 0 (no limit) or positive, got %d", c.APIRateLimit))
	}
	if c.HashConcurrency < 0 {
		add(sHashConcurrency.errorf("must be 0 (default) or positive, got %d", c.HashConcurrency))
	}
	add(validateMinDuration(sShutdownTimeout, c.ShutdownTimeout, time.Second))

	return errors.Join(errs...)
}

// DecodeSecretKey decodes the standard base64 form of the encryption key,
// which must be exactly [SecretKeySize] bytes long. The error does not
// contain the input.
func DecodeSecretKey(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("must be standard base64 of 32 bytes")
	}
	if len(key) != SecretKeySize {
		return nil, fmt.Errorf("must be standard base64 of 32 bytes, decodes to %d bytes", len(key))
	}
	return key, nil
}

func validateListen(addr string) error { return validateAddr(sListen, addr) }

func validateAddr(s setting, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return s.errorf("must be host:port, got %q", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return s.errorf("port must be between 1 and 65535, got %q", port)
	}
	return nil
}

// ListenIsLoopback reports whether a listen address binds a loopback
// address only (127.0.0.1, ::1 or localhost).
func ListenIsLoopback(addr string) bool { return !listenExposed(addr) }

// listenExposed reports whether the listen address is reachable outside the
// loopback interface. ":8080", "0.0.0.0:8080" and "::8080" bind all
// interfaces; "127.0.0.1:8080" and "localhost:8080" stay local.
func listenExposed(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true // an invalid address is treated as exposed (warn loudly)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	case "127.0.0.1", "::1", "[::1]", "localhost":
		return false
	}
	// An explicit IP that is not a loopback address.
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return !ip.IsLoopback()
	}
	// A host name we cannot classify is treated as exposed.
	return true
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return sBaseURL.errorf("not a valid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return sBaseURL.errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return sBaseURL.errorf("must include a host")
	}
	return nil
}

func validateMinDuration(s setting, d, least time.Duration) error {
	if d < least {
		return s.errorf("must be at least %s, got %s", least, d)
	}
	return nil
}

func validateRetry(r RetryConfig) error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	if r.MaxAttempts < 1 {
		add(sRetryMaxAttempts.errorf("must be at least 1, got %d", r.MaxAttempts))
	}
	if r.Base <= 0 {
		add(sRetryBase.errorf("must be positive, got %s", r.Base))
	}
	// The negated comparisons also reject NaN.
	if !(r.Factor >= 1) || math.IsInf(r.Factor, 0) {
		add(sRetryFactor.errorf("must be a finite number of at least 1, got %v", r.Factor))
	}
	if r.Max < r.Base {
		add(sRetryMax.errorf("must not be less than retry.base (%s), got %s", r.Base, r.Max))
	}
	if !(r.Jitter >= 0 && r.Jitter < 1) {
		add(sRetryJitter.errorf("must be in [0, 1), got %v", r.Jitter))
	}
	return errors.Join(errs...)
}

func validateTLS(t TLSConfig) error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	if (t.CertFile == "") != (t.KeyFile == "") {
		add(sTLSCertFile.errorf("tls.cert_file and tls.key_file must be set together"))
	}
	for _, f := range []struct {
		s    setting
		path string
	}{{sTLSCertFile, t.CertFile}, {sTLSKeyFile, t.KeyFile}} {
		if f.path == "" {
			continue
		}
		if _, err := os.Stat(f.path); err != nil {
			add(f.s.errorf("file is not accessible: %v", err))
		}
	}
	return errors.Join(errs...)
}

// ParsePrefixes parses IP addresses and CIDR prefixes (trusted_proxies). An
// address is a single-host prefix.
func ParsePrefixes(items []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(items))
	for _, it := range items {
		if strings.Contains(it, "/") {
			p, err := netip.ParsePrefix(it)
			if err != nil {
				return nil, fmt.Errorf("%q is not an IP address or CIDR prefix", it)
			}
			out = append(out, p.Masked())
			continue
		}
		ip, err := netip.ParseAddr(it)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR prefix", it)
		}
		ip = ip.Unmap()
		out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return out, nil
}

// validateOrigin accepts scheme://host[:port] with an http or https scheme
// and nothing else (no path, query or user info).
func validateOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return sTrustedOrigins.errorf("%q is not an origin such as https://sms.example.net", o)
	}
	return nil
}

// hostNamePattern is a DNS host name (letters, digits, hyphens, dots).
var hostNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// validateHTTPHost accepts an IP address, a CIDR prefix or a host name.
func validateHTTPHost(h string) error {
	if strings.Contains(h, "/") {
		if _, err := netip.ParsePrefix(h); err != nil {
			return sAllowedHTTPHosts.errorf("%q is not a valid CIDR prefix", h)
		}
		return nil
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return nil
	}
	if !hostNamePattern.MatchString(h) {
		return sAllowedHTTPHosts.errorf("%q is not an IP address, CIDR prefix or host name", h)
	}
	return nil
}

// Agent connector bounds: a send must leave the agent time to poll; the
// stale limit must exceed a poll cycle.
const (
	minAgentSendTimeout = 5 * time.Second
	maxAgentSendTimeout = 10 * time.Minute
	minAgentStaleAfter  = 10 * time.Second
	maxAgentSegments    = 255
)

func validateAgent(a AgentConfig) error {
	var errs []error
	if a.SendTimeout < minAgentSendTimeout || a.SendTimeout > maxAgentSendTimeout {
		errs = append(errs, sAgentSendTimeout.errorf("must be between %s and %s, got %s", minAgentSendTimeout, maxAgentSendTimeout, a.SendTimeout))
	}
	if a.StaleAfter < minAgentStaleAfter {
		errs = append(errs, sAgentStaleAfter.errorf("must be at least %s, got %s", minAgentStaleAfter, a.StaleAfter))
	}
	if a.MaxSegments < 1 || a.MaxSegments > maxAgentSegments {
		errs = append(errs, sAgentMaxSegments.errorf("must be between 1 and %d, got %d", maxAgentSegments, a.MaxSegments))
	}
	return errors.Join(errs...)
}

// Mail gateway bounds (ADR 0004).
const (
	minMailMessageSize = 1 << 10
	maxMailMessageSize = 10 << 20
	maxMailRecipients  = 100
	maxMailConnections = 1000
	minMailTimeout     = time.Second
	maxMailTimeout     = time.Hour
)

// validateMailGateway checks the SMTP gateway settings. They are checked
// only when the gateway is enabled, so a disabled gateway never blocks a
// start. TLS is mandatory unless the listener is on loopback: the SMTP
// password is an API token.
func validateMailGateway(m MailGatewayConfig, global TLSConfig) error {
	if !m.Enabled {
		return nil
	}
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(validateAddr(sMailGwListen, m.Listen))
	switch {
	case m.Domain == "":
		add(sMailGwDomain.errorf("required when the mail gateway is enabled (recipients are <number>@domain)"))
	case !hostNamePattern.MatchString(m.Domain):
		add(sMailGwDomain.errorf("%q is not a domain name", m.Domain))
	}
	if m.Hostname != "" && !hostNamePattern.MatchString(m.Hostname) {
		add(sMailGwHostname.errorf("%q is not a host name", m.Hostname))
	}
	switch m.TLSMode {
	case MailTLSStartTLS, MailTLSImplicit:
		cert, key := m.TLSFiles(global)
		if cert == "" || key == "" {
			add(sMailGwTLSCertFile.errorf("tls_mode %s needs a certificate: set mail_gateway.tls_cert_file and tls_key_file (or tls.cert_file and tls.key_file)", m.TLSMode))
		}
		if (m.TLSCertFile == "") != (m.TLSKeyFile == "") {
			add(sMailGwTLSKeyFile.errorf("mail_gateway.tls_cert_file and tls_key_file must be set together"))
		}
		for _, f := range []struct {
			s    setting
			path string
		}{{sMailGwTLSCertFile, m.TLSCertFile}, {sMailGwTLSKeyFile, m.TLSKeyFile}} {
			if f.path == "" {
				continue
			}
			if _, err := os.Stat(f.path); err != nil {
				add(f.s.errorf("file is not accessible: %v", err))
			}
		}
	case MailTLSNone:
		if listenExposed(m.Listen) {
			add(sMailGwTLSMode.errorf("none is allowed only on a loopback listen address (127.0.0.1, ::1); the SMTP password is an API token, use starttls or tls"))
		}
	default:
		add(sMailGwTLSMode.errorf("must be starttls, tls or none, got %q", m.TLSMode))
	}
	if m.MaxMessageSize < minMailMessageSize || m.MaxMessageSize > maxMailMessageSize {
		add(sMailGwMaxMessageSize.errorf("must be between %d and %d bytes, got %d", minMailMessageSize, maxMailMessageSize, m.MaxMessageSize))
	}
	if m.MaxRecipients < 1 || m.MaxRecipients > maxMailRecipients {
		add(sMailGwMaxRecipients.errorf("must be between 1 and %d, got %d", maxMailRecipients, m.MaxRecipients))
	}
	if m.MaxConnections < 1 || m.MaxConnections > maxMailConnections {
		add(sMailGwMaxConnections.errorf("must be between 1 and %d, got %d", maxMailConnections, m.MaxConnections))
	}
	if m.MaxConnectionsPerIP < 1 || m.MaxConnectionsPerIP > m.MaxConnections {
		add(sMailGwMaxConnsPerIP.errorf("must be between 1 and max_connections (%d), got %d", m.MaxConnections, m.MaxConnectionsPerIP))
	}
	if m.MaxMessagesPerSession < 1 || m.MaxMessagesPerSession > 10000 {
		add(sMailGwMaxMsgsPerSess.errorf("must be between 1 and 10000, got %d", m.MaxMessagesPerSession))
	}
	if m.MaxSessionDuration < time.Minute || m.MaxSessionDuration > 24*time.Hour {
		add(sMailGwMaxSessionDur.errorf("must be between 1m and 24h, got %s", m.MaxSessionDuration))
	}
	for _, d := range []struct {
		s setting
		v time.Duration
	}{{sMailGwCommandTimeout, m.CommandTimeout}, {sMailGwDataTimeout, m.DataTimeout}} {
		if d.v < minMailTimeout || d.v > maxMailTimeout {
			add(d.s.errorf("must be between %s and %s, got %s", minMailTimeout, maxMailTimeout, d.v))
		}
	}
	switch m.Body {
	case MailBodyText, MailBodySubjectText:
	default:
		add(sMailGwBody.errorf("must be text or subject_text, got %q", m.Body))
	}
	return errors.Join(errs...)
}

// maxMailNotifyRecipients bounds mail_notify.to.
const maxMailNotifyRecipients = 20

// validateMailNotify checks the notification relay settings when enabled.
// The relay must use TLS unless it is on loopback (the e-mails carry SMS
// texts and the relay password).
func validateMailNotify(m MailNotifyConfig) error {
	if !m.Enabled {
		return nil
	}
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	_, ipErr := netip.ParseAddr(m.SMTPHost)
	switch {
	case m.SMTPHost == "":
		add(sMailNotifyHost.errorf("required when mail notifications are enabled"))
	case ipErr != nil && !hostNamePattern.MatchString(m.SMTPHost):
		add(sMailNotifyHost.errorf("%q is not a host name or IP address", m.SMTPHost))
	}
	if m.SMTPPort < 1 || m.SMTPPort > 65535 {
		add(sMailNotifyPort.errorf("must be between 1 and 65535, got %d", m.SMTPPort))
	}
	switch m.TLSMode {
	case MailTLSStartTLS, MailTLSImplicit:
	case MailTLSNone:
		if !HostIsLoopback(m.SMTPHost) {
			add(sMailNotifyTLSMode.errorf("none is allowed only for a loopback relay (127.0.0.1, ::1, localhost); use starttls or tls"))
		}
	default:
		add(sMailNotifyTLSMode.errorf("must be starttls, tls or none, got %q", m.TLSMode))
	}
	hasPassword := m.Password != "" || m.PasswordFile != ""
	switch {
	case m.Password != "" && m.PasswordFile != "":
		add(sMailNotifyPasswordFile.errorf("set either mail_notify.password or mail_notify.password_file, not both"))
	case m.Username == "" && hasPassword:
		add(sMailNotifyUsername.errorf("required when a password is set"))
	case m.Username != "" && !hasPassword:
		add(sMailNotifyPassword.errorf("required when a username is set (or use mail_notify.password_file)"))
	}
	if m.PasswordFile != "" {
		if _, err := os.Stat(m.PasswordFile); err != nil {
			add(sMailNotifyPasswordFile.errorf("file is not accessible: %v", err))
		}
	}
	if m.From == "" {
		add(sMailNotifyFrom.errorf("required when mail notifications are enabled"))
	} else if _, err := mail.ParseAddress(m.From); err != nil {
		add(sMailNotifyFrom.errorf("%q is not an e-mail address", m.From))
	}
	switch {
	case len(m.To) == 0:
		add(sMailNotifyTo.errorf("at least one recipient is required"))
	case len(m.To) > maxMailNotifyRecipients:
		add(sMailNotifyTo.errorf("at most %d recipients, got %d", maxMailNotifyRecipients, len(m.To)))
	}
	for _, to := range m.To {
		if _, err := mail.ParseAddress(to); err != nil {
			add(sMailNotifyTo.errorf("%q is not an e-mail address", to))
		}
	}
	if m.Timeout < time.Second || m.Timeout > 10*time.Minute {
		add(sMailNotifyTimeout.errorf("must be between 1s and 10m, got %s", m.Timeout))
	}
	return errors.Join(errs...)
}

// HostIsLoopback reports whether host is a loopback address or
// "localhost".
func HostIsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && ip.Unmap().IsLoopback()
}
