// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package mailgw

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/logging"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/sms/phone"
)

// Errors of the line reader.
var (
	errLineTooLong = errors.New("line too long")
	errTooMuchData = errors.New("too much data")
	errShutdown    = errors.New("server shutting down")
)

// maxReplyText bounds the free text of a reply.
const maxReplyText = 400

// session is one SMTP connection.
type session struct {
	srv *Server
	id  uint64
	log *slog.Logger

	connMu sync.Mutex // guards conn against interrupt during STARTTLS
	conn   net.Conn
	br     *bufio.Reader
	bw     *bufio.Writer

	closing  atomic.Bool
	tls      bool
	clientIP string
	loopback bool

	helo         string
	principal    *auth.Principal
	token        string // the raw token of principal, to check it again
	authFailures int
	badCommands  int
	preAuthCmds  int // commands before a successful AUTH
	messages     int // e-mails accepted in this session

	// Session limits on the OS clock (they are enforced with socket
	// deadlines, which use it).
	lifeEnd    time.Time
	preAuthEnd time.Time

	inTx  bool     // MAIL FROM accepted
	rcpts []string // E.164 numbers, unique
}

func newSession(s *Server, conn net.Conn) *session {
	id := s.nextID.Add(1)
	now := time.Now()
	return &session{
		lifeEnd: now.Add(s.opts.MaxSessionDuration), preAuthEnd: now.Add(s.opts.PreAuthTimeout),
		srv: s, id: id, conn: conn,
		log:      s.log.With(slog.Uint64("session", id)),
		br:       bufio.NewReaderSize(conn, maxLine),
		bw:       bufio.NewWriter(conn),
		clientIP: clientKey(conn.RemoteAddr()),
		loopback: isLoopback(conn.RemoteAddr()),
	}
}

// interrupt makes the session stop at its next read: the flag first, then
// the deadline, so a read that sets a fresh deadline after this still sees
// the flag (see readLine).
func (c *session) interrupt() {
	c.closing.Store(true)
	c.connMu.Lock()
	_ = c.conn.SetReadDeadline(time.Now())
	c.connMu.Unlock()
}

func (c *session) forceClose() {
	c.closing.Store(true)
	c.connMu.Lock()
	_ = c.conn.Close()
	c.connMu.Unlock()
}

func (c *session) setConn(conn net.Conn) {
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
	c.br = bufio.NewReaderSize(conn, maxLine)
	c.bw = bufio.NewWriter(conn)
}

// serve runs the session until QUIT, an error or shutdown.
func (c *session) serve() {
	defer func() {
		c.connMu.Lock()
		_ = c.conn.Close()
		c.connMu.Unlock()
	}()
	o := &c.srv.opts
	if o.Insecure && !c.loopback {
		// Without TLS the token would cross the network in clear text.
		c.reply("554 5.7.1 This server accepts plain-text SMTP from loopback only")
		c.log.Warn("non-loopback client refused on a server without TLS")
		return
	}
	if o.ImplicitTLS {
		if !c.handshake() {
			return
		}
	}
	c.log.Debug("smtp session started", slog.Bool("tls", c.tls))
	if !c.reply("220 " + o.Hostname + " ESMTP tikker") {
		return
	}
	for {
		line, err := c.readLine(o.CommandTimeout, maxCommandLine)
		switch {
		case errors.Is(err, errShutdown) || (err != nil && c.closing.Load()):
			c.reply("421 4.3.2 Service shutting down")
			return
		case errors.Is(err, errLineTooLong):
			if !c.bad("500 5.5.2 Line too long") {
				return
			}
			continue
		case isTimeout(err):
			c.reply(c.timeoutReply())
			return
		case err != nil:
			return // connection closed or broken
		}
		if !c.command(line) {
			return
		}
	}
}

// timeoutReply explains why a read timed out: the session lifetime, the
// time allowed before AUTH, or an idle client.
func (c *session) timeoutReply() string {
	now := time.Now().Add(50 * time.Millisecond) // deadlines fire on time; allow for scheduling
	switch {
	case !now.Before(c.lifeEnd):
		c.log.Info("smtp session time limit reached")
		return "421 4.7.0 Session time limit reached, closing connection"
	case c.principal == nil && !now.Before(c.preAuthEnd):
		c.log.Info("smtp session closed: no authentication in time", slog.String("client", c.clientIP))
		return "421 4.7.0 Authentication required in time, closing connection"
	}
	return "421 4.4.2 " + c.srv.opts.Hostname + " Timeout, closing connection"
}

// command handles one command line; false ends the session.
func (c *session) command(line string) bool {
	if c.principal == nil {
		// Every command counts before AUTH (NOOP, RSET, HELP, VRFY
		// included), so an unauthenticated client cannot hold a slot.
		c.preAuthCmds++
		if c.preAuthCmds > c.srv.opts.MaxPreAuthCommands {
			c.log.Info("smtp session closed: too many commands without authentication", slog.String("client", c.clientIP))
			c.reply("421 4.7.0 Authentication required, closing connection")
			return false
		}
	}
	verb, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch strings.ToUpper(verb) {
	case "EHLO":
		return c.ehlo(arg, true)
	case "HELO":
		return c.ehlo(arg, false)
	case "STARTTLS":
		return c.startTLS(arg)
	case "AUTH":
		return c.auth(arg)
	case "MAIL":
		return c.mail(arg)
	case "RCPT":
		return c.rcpt(arg)
	case "DATA":
		return c.data(arg)
	case "RSET":
		c.resetTx()
		return c.reply("250 2.0.0 Ok")
	case "NOOP":
		return c.reply("250 2.0.0 Ok")
	case "VRFY", "EXPN":
		return c.reply("252 2.5.0 Cannot verify the user; send the message")
	case "HELP":
		return c.reply("214 2.0.0 See docs/email-gateway.md of tikker")
	case "QUIT":
		c.reply("221 2.0.0 Bye")
		return false
	default:
		return c.bad("500 5.5.2 Command not recognized")
	}
}

// bad replies to a protocol error and ends the session after too many.
func (c *session) bad(reply string) bool {
	c.badCommands++
	if c.badCommands > maxBadCommands {
		c.reply("421 4.7.0 Too many errors, closing connection")
		return false
	}
	return c.reply(reply)
}

func (c *session) resetTx() {
	c.inTx = false
	c.rcpts = nil
}

func (c *session) authAllowed() bool { return c.tls || c.srv.opts.Insecure }

func (c *session) ehlo(arg string, extended bool) bool {
	if arg == "" {
		return c.bad("501 5.5.4 Syntax: EHLO hostname")
	}
	c.helo = cleanLine(arg)
	c.resetTx()
	o := &c.srv.opts
	if !extended {
		return c.reply("250 " + o.Hostname)
	}
	lines := []string{o.Hostname + " Hello", "SIZE " + strconv.Itoa(o.MaxMessageBytes), "8BITMIME", "ENHANCEDSTATUSCODES"}
	if o.TLSConfig != nil && !c.tls {
		lines = append(lines, "STARTTLS")
	}
	if c.authAllowed() {
		lines = append(lines, "AUTH PLAIN LOGIN")
	}
	lines = append(lines, "HELP")
	return c.replyLines(250, lines)
}

func (c *session) startTLS(arg string) bool {
	switch {
	case c.srv.opts.TLSConfig == nil || c.tls:
		return c.bad("502 5.5.1 STARTTLS not available")
	case arg != "":
		return c.bad("501 5.5.4 Syntax: STARTTLS")
	case c.helo == "":
		return c.bad("503 5.5.1 Send EHLO first")
	case c.br.Buffered() > 0:
		// Commands sent together with STARTTLS would be taken as if they
		// came over TLS (CVE-2011-0411).
		c.reply("554 5.5.1 Data after STARTTLS; closing connection")
		c.log.Warn("data pipelined after STARTTLS; connection closed")
		return false
	}
	if !c.reply("220 2.0.0 Ready to start TLS") {
		return false
	}
	if !c.handshake() {
		return false
	}
	// RFC 3207 §4.2: forget everything learned before TLS.
	c.helo = ""
	c.principal, c.token = nil, ""
	c.resetTx()
	return true
}

// handshake wraps the connection in TLS.
func (c *session) handshake() bool {
	tc := tls.Server(c.conn, c.srv.opts.TLSConfig)
	_ = tc.SetDeadline(time.Now().Add(min(c.srv.opts.HandshakeTimeout, c.srv.opts.CommandTimeout)))
	if err := tc.Handshake(); err != nil {
		c.log.Info("tls handshake failed", slog.String("error", err.Error()))
		return false
	}
	_ = tc.SetDeadline(time.Time{})
	c.setConn(tc)
	c.tls = true
	return true
}

func (c *session) auth(arg string) bool {
	o := &c.srv.opts
	switch {
	case c.helo == "":
		return c.bad("503 5.5.1 Send EHLO first")
	case c.principal != nil:
		return c.bad("503 5.5.1 Already authenticated")
	case c.inTx:
		return c.bad("503 5.5.1 AUTH not allowed during a mail transaction")
	case !c.authAllowed():
		return c.bad("538 5.7.11 Encryption required: use STARTTLS first")
	}
	mech, initial, _ := strings.Cut(arg, " ")
	var password string
	var ok bool
	switch strings.ToUpper(mech) {
	case "PLAIN":
		password, ok = c.authPlain(strings.TrimSpace(initial))
	case "LOGIN":
		password, ok = c.authLogin(strings.TrimSpace(initial))
	default:
		return c.bad("504 5.5.4 Unrecognized authentication mechanism; use PLAIN or LOGIN")
	}
	if !ok {
		return !c.closing.Load() && c.badCommands <= maxBadCommands
	}

	now := c.srv.clk.Now()
	if wait, allowed := c.srv.authLim.take(c.clientIP, now); !allowed {
		c.log.Warn("smtp auth rate limited", slog.String("client", c.clientIP))
		c.reply(fmt.Sprintf("421 4.7.0 Too many failed authentications; try again in %d s", int(wait.Seconds())+1))
		return false
	}
	ctx, cancel := context.WithTimeout(o.BaseContext, authTimeout)
	p, err := o.Auth.AuthenticateToken(ctx, password)
	cancel()
	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		c.authFailures++
		c.log.Info("smtp auth failed", slog.String("client", c.clientIP), slog.Int("failures", c.authFailures))
		if c.authFailures >= maxAuthFailures {
			c.reply("421 4.7.0 Too many failed authentications; closing connection")
			return false
		}
		return c.reply("535 5.7.8 Authentication credentials invalid")
	case err != nil:
		c.srv.authLim.refund(c.clientIP, now)
		c.log.Error("smtp auth error", slog.String("error", err.Error()))
		return c.reply("454 4.7.0 Temporary authentication failure")
	}
	c.srv.authLim.refund(c.clientIP, now)
	if !p.Can(domain.ScopeSMSSend) {
		c.log.Info("smtp auth refused: token lacks scope", slog.String("token_id", p.Token.ID.String()))
		return c.reply(`535 5.7.8 Token lacks required scope "sms:send"`)
	}
	c.principal, c.token = &p, password
	c.log.Info("smtp auth succeeded", slog.String("token_id", p.Token.ID.String()), slog.Bool("tls", c.tls))
	return c.reply("235 2.7.0 Authentication successful")
}

// authPlain reads the RFC 4616 response: authzid NUL authcid NUL password.
// The user name is ignored (the token identifies the caller); an authzid
// different from the user (acting as someone else) is refused.
func (c *session) authPlain(initial string) (string, bool) {
	resp, ok := c.authResponse(initial, "")
	if !ok {
		return "", false
	}
	parts := bytes.Split(resp, []byte{0})
	if len(parts) != 3 {
		c.bad("501 5.5.2 Malformed PLAIN response")
		return "", false
	}
	if len(parts[0]) > 0 && !bytes.Equal(parts[0], parts[1]) {
		c.bad("535 5.7.8 Authorization identity not supported")
		return "", false
	}
	return string(parts[2]), true
}

// authLogin runs the LOGIN exchange (user name, then password).
func (c *session) authLogin(initial string) (string, bool) {
	if _, ok := c.authResponse(initial, "VXNlcm5hbWU6"); !ok { // "Username:"
		return "", false
	}
	resp, ok := c.authResponse("", "UGFzc3dvcmQ6") // "Password:"
	if !ok {
		return "", false
	}
	return string(resp), true
}

// authResponse returns the decoded initial response, or asks for one with
// a 334 challenge. "*" cancels (RFC 4954). Failures are answered here.
func (c *session) authResponse(initial, challenge string) ([]byte, bool) {
	line := initial
	if line == "" {
		if !c.reply("334 " + challenge) {
			c.closing.Store(true)
			return nil, false
		}
		var err error
		if line, err = c.readLine(c.srv.opts.CommandTimeout, maxCommandLine); err != nil {
			c.closing.Store(true) // ends the session
			return nil, false
		}
	}
	if line == "*" {
		c.bad("501 5.0.0 Authentication cancelled")
		return nil, false
	}
	if line == "=" { // empty initial response
		return []byte{}, true
	}
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		c.bad("501 5.5.2 Cannot decode the response")
		return nil, false
	}
	return b, true
}

func (c *session) mail(arg string) bool {
	o := &c.srv.opts
	switch {
	case c.helo == "":
		return c.bad("503 5.5.1 Send EHLO first")
	case c.principal == nil:
		// No authentication, no mail: this is never an open relay.
		return c.bad("530 5.7.0 Authentication required")
	case c.inTx:
		return c.bad("503 5.5.1 Nested MAIL command")
	case c.messages >= o.MaxMessagesPerSession:
		c.log.Info("smtp session message limit reached", slog.Int("messages", c.messages))
		c.reply("421 4.7.0 Message limit per session reached, reconnect to continue")
		return false
	}
	switch c.recheck() {
	case checkInvalid:
		return c.reply("535 5.7.8 Authentication credentials no longer valid")
	case checkTemporary:
		return c.reply("454 4.7.0 Temporary authentication failure")
	case checkOK:
	}
	addr, params, ok := parsePath(arg, "FROM:")
	if !ok || len(addr) > 256 {
		return c.bad("501 5.5.4 Syntax: MAIL FROM:<address>")
	}
	for _, p := range params {
		k, v, _ := strings.Cut(p, "=")
		switch strings.ToUpper(k) {
		case "SIZE":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return c.bad("501 5.5.4 Invalid SIZE")
			}
			if n > o.MaxMessageBytes {
				return c.reply(fmt.Sprintf("552 5.3.4 Message too big; the limit is %d bytes", o.MaxMessageBytes))
			}
		case "BODY":
			if u := strings.ToUpper(v); u != "7BIT" && u != "8BITMIME" {
				return c.bad("555 5.5.4 Unsupported BODY type")
			}
		case "AUTH":
			// RFC 4954 §5: accepted and ignored.
		default:
			return c.bad("555 5.5.4 Unsupported MAIL parameter")
		}
	}
	c.inTx = true
	c.rcpts = nil
	return c.reply("250 2.1.0 Ok")
}

func (c *session) rcpt(arg string) bool {
	o := &c.srv.opts
	if !c.inTx {
		return c.bad("503 5.5.1 Send MAIL first")
	}
	addr, params, ok := parsePath(arg, "TO:")
	if !ok || addr == "" {
		return c.bad("501 5.5.4 Syntax: RCPT TO:<number@" + o.Domain + ">")
	}
	if len(params) > 0 {
		return c.bad("555 5.5.4 RCPT parameters are not supported")
	}
	local, dom, found := cutLast(addr, "@")
	if !found || !strings.EqualFold(strings.TrimSuffix(dom, "."), o.Domain) {
		c.log.Info("smtp recipient refused: foreign domain")
		return c.bad("550 5.7.1 Relaying denied; recipients must be <number>@" + o.Domain)
	}
	if local == "" || utf8.RuneCountInString(local) > sending.MaxToLen {
		return c.bad("553 5.1.3 invalid_phone_number: the local part must be a phone number")
	}
	n, err := phone.Normalize(local, c.srv.phone)
	if err != nil {
		return c.bad(fmt.Sprintf("553 5.1.3 invalid_phone_number: not a valid phone number for region %s", c.srv.phone.DefaultRegion))
	}
	for _, r := range c.rcpts {
		if r == n.Value {
			return c.reply("250 2.1.5 Ok (duplicate recipient, sent once)")
		}
	}
	if len(c.rcpts) >= o.MaxRecipients {
		return c.reply(fmt.Sprintf("452 4.5.3 Too many recipients; at most %d per message", o.MaxRecipients))
	}
	// Every recipient is one message: it uses one request of the token's
	// API budget.
	if o.Limiter != nil {
		if ok, wait := o.Limiter.Allow(c.principal.Token.ID); !ok {
			return c.reply(fmt.Sprintf("451 4.7.0 rate_limited: token rate limit exceeded; retry in %d s", int(wait.Seconds())))
		}
	}
	c.rcpts = append(c.rcpts, n.Value)
	return c.reply("250 2.1.5 Ok")
}

func (c *session) data(arg string) bool {
	o := &c.srv.opts
	switch {
	case arg != "":
		return c.bad("501 5.5.4 Syntax: DATA")
	case !c.inTx:
		return c.bad("503 5.5.1 Send MAIL first")
	case len(c.rcpts) == 0:
		return c.bad("554 5.5.1 No valid recipients")
	}
	if !c.reply("354 End data with <CR><LF>.<CR><LF>") {
		return false
	}
	raw, err := c.readData()
	defer c.resetTx()
	switch {
	case errors.Is(err, errTooMuchData):
		c.reply(fmt.Sprintf("552 5.3.4 Message too big; the limit is %d bytes", o.MaxMessageBytes))
		return false // the rest of the data cannot be resynchronised
	case errors.Is(err, errLineTooLong):
		return c.reply(fmt.Sprintf("500 5.5.2 Line too long; the limit is %d bytes", maxLine))
	case errors.Is(err, errShutdown) || (err != nil && c.closing.Load()):
		c.reply("421 4.3.2 Service shutting down")
		return false
	case isTimeout(err):
		c.reply(c.timeoutReply())
		return false
	case err != nil:
		return false
	case raw == nil:
		return c.reply(fmt.Sprintf("552 5.3.4 Message too big; the limit is %d bytes", o.MaxMessageBytes))
	}
	return c.reply(c.deliver(raw))
}

// deliver turns the message into SMS requests and stores them; it returns
// the reply line.
func (c *session) deliver(raw []byte) string {
	o := &c.srv.opts
	m, err := ParseMessage(raw, ParseOptions{SubjectInBody: o.SubjectInBody})
	if err != nil {
		var ce *ContentError
		if errors.As(err, &ce) {
			c.log.Info("smtp message refused", slog.String("code", ce.Code))
			return "554 5.6.0 " + ce.Code + ": " + ce.Detail
		}
		return "451 4.3.0 Temporary failure, try again later"
	}

	// The token may have been revoked, expired or lost its scope (or its
	// owner disabled) since AUTH.
	switch c.recheck() {
	case checkInvalid:
		return "554 5.7.8 Authentication credentials no longer valid"
	case checkTemporary:
		return "451 4.3.0 Temporary failure, try again later"
	case checkOK:
	}

	reqs := make([]sending.Request, len(c.rcpts))
	var clientRef *string
	if m.MessageID != "" && utf8.RuneCountInString(m.MessageID) <= sending.MaxClientRef {
		clientRef = &m.MessageID
	}
	for i := range c.rcpts {
		reqs[i] = sending.Request{To: &c.rcpts[i], Body: &m.Text, ClientRef: clientRef}
	}
	so := sending.SendOptions{Source: "smtp"}
	if m.MessageID != "" {
		// The Message-ID is the client's retry identity; the store scopes
		// the key to the token.
		sum := sha256.Sum256([]byte(m.MessageID))
		so.IdempotencyKey = "smtp:" + hex.EncodeToString(sum[:])
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(o.BaseContext), enqueueTimeout)
	defer cancel()
	res, ferrs, err := o.Sender.SendBatch(ctx, c.principal.Actor(), reqs, so)
	switch {
	case errors.Is(err, domain.ErrIdempotencyMismatch):
		return "554 5.7.0 idempotency_key_mismatch: this Message-ID was already used for a different message"
	case err != nil:
		c.log.Error("smtp enqueue failed", slog.String("error", err.Error()))
		return "451 4.3.0 Temporary failure, try again later"
	case len(ferrs) > 0:
		fe := ferrs[0]
		c.log.Info("smtp message rejected", slog.String("code", fe.Code), slog.String("field", fe.Field))
		where := ""
		if i, ok := recipientIndex(fe.Field); ok && i < len(c.rcpts) {
			where = " (recipient " + c.rcpts[i] + ")"
		}
		status := "554 5.6.0 "
		if fe.Code == sending.CodeInvalidPhone {
			status = "553 5.1.3 "
		}
		return status + fe.Code + ": " + fe.Detail + where
	}

	ids := make([]string, len(res.Messages))
	for i, msg := range res.Messages {
		ids[i] = msg.ID.String()
	}
	attrs := []any{
		slog.String("token_id", c.principal.Token.ID.String()), slog.Int("recipients", len(c.rcpts)),
		slog.Int("messages", len(ids)), slog.Bool("replayed", res.Replayed),
	}
	if len(c.rcpts) == 1 {
		attrs = append(attrs, logging.PhoneAttr("to", c.rcpts[0]))
	}
	c.log.Info("smtp message queued", attrs...)
	c.messages++
	verb := "queued"
	if res.Replayed {
		verb = "already queued (same Message-ID)"
	}
	return fmt.Sprintf("250 2.0.0 Ok: %s as %s", verb, strings.Join(ids, ","))
}

// Outcomes of [session.recheck].
type checkResult int

const (
	checkOK checkResult = iota
	checkInvalid
	checkTemporary
)

// recheck authenticates the session's token again, with the checks of AUTH
// (valid, not expired or revoked, owner enabled, scope sms:send with the
// owner's current role). An invalid token drops the authentication and the
// transaction.
func (c *session) recheck() checkResult {
	ctx, cancel := context.WithTimeout(c.srv.opts.BaseContext, authTimeout)
	defer cancel()
	p, err := c.srv.opts.Auth.AuthenticateToken(ctx, c.token)
	switch {
	case errors.Is(err, auth.ErrInvalidToken) || (err == nil && !p.Can(domain.ScopeSMSSend)):
		c.log.Info("smtp token no longer valid; authentication dropped", slog.String("token_id", c.principal.Token.ID.String()))
		c.principal, c.token = nil, ""
		c.resetTx()
		return checkInvalid
	case err != nil:
		c.log.Error("smtp auth recheck error", slog.String("error", err.Error()))
		return checkTemporary
	}
	c.principal = &p
	return checkOK
}

// recipientIndex reads i from a field pointer "/messages/i/...".
func recipientIndex(field string) (int, bool) {
	rest, ok := strings.CutPrefix(field, "/messages/")
	if !ok {
		return 0, field == "" || strings.HasPrefix(field, "/")
	}
	n, _, _ := strings.Cut(rest, "/")
	i, err := strconv.Atoi(n)
	return i, err == nil
}

// readLine reads one CRLF (or LF) terminated line within timeout.
func (c *session) readLine(timeout time.Duration, limit int) (string, error) {
	c.connMu.Lock()
	_ = c.conn.SetReadDeadline(c.deadline(timeout))
	c.connMu.Unlock()
	if c.closing.Load() {
		return "", errShutdown
	}
	budget := 64 * maxLine
	line, err := readRawLine(c.br, &budget)
	if err != nil {
		return "", err
	}
	if len(line) > limit {
		return "", errLineTooLong
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// deadline is now+timeout, capped by the session lifetime and, before
// AUTH, by the time allowed to authenticate.
func (c *session) deadline(timeout time.Duration) time.Time {
	d := time.Now().Add(timeout)
	if c.lifeEnd.Before(d) {
		d = c.lifeEnd
	}
	if c.principal == nil && c.preAuthEnd.Before(d) {
		d = c.preAuthEnd
	}
	return d
}

// readData reads the message after DATA until a line "." (RFC 5321
// §4.5.2: dot-unstuffing), within the data timeout. The terminator counts
// only after CRLF, so a bare LF cannot end the message early. A message over
// the size limit is read to its end and discarded (nil, nil), unless the
// sender goes on far beyond it (errTooMuchData). Lines are joined with LF.
func (c *session) readData() ([]byte, error) {
	o := &c.srv.opts
	c.connMu.Lock()
	_ = c.conn.SetReadDeadline(c.deadline(o.DataTimeout))
	c.connMu.Unlock()
	if c.closing.Load() {
		return nil, errShutdown
	}
	var buf bytes.Buffer
	budget := 4*o.MaxMessageBytes + 1<<20 // bytes read in total, discarded included
	tooBig, tooLong := false, false
	afterCRLF := true
	for {
		line, err := readRawLine(c.br, &budget)
		if errors.Is(err, errLineTooLong) {
			tooLong, afterCRLF = true, true
			continue
		}
		if err != nil {
			return nil, err
		}
		if afterCRLF && string(line) == ".\r\n" {
			break
		}
		crlf := bytes.HasSuffix(line, []byte("\r\n"))
		content := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(content) > 0 && content[0] == '.' {
			content = content[1:]
		}
		afterCRLF = crlf
		if tooBig || tooLong {
			continue
		}
		if buf.Len()+len(content)+1 > o.MaxMessageBytes {
			tooBig = true
			buf = bytes.Buffer{}
			continue
		}
		buf.Write(content)
		buf.WriteByte('\n')
	}
	if tooLong {
		return nil, errLineTooLong
	}
	if tooBig {
		return nil, nil
	}
	return buf.Bytes(), nil
}

// readRawLine returns the next line including its line end. A line longer
// than the reader's buffer is consumed and reported as errLineTooLong.
// budget is decremented by the bytes read; errTooMuchData when it runs out.
func readRawLine(br *bufio.Reader, budget *int) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	*budget -= len(line)
	if errors.Is(err, bufio.ErrBufferFull) {
		for errors.Is(err, bufio.ErrBufferFull) {
			if *budget < 0 {
				return nil, errTooMuchData
			}
			line, err = br.ReadSlice('\n')
			*budget -= len(line)
		}
		if err != nil {
			return nil, err //nolint:wrapcheck // I/O error of the connection
		}
		return nil, errLineTooLong
	}
	if *budget < 0 {
		return nil, errTooMuchData
	}
	if err != nil {
		return nil, err //nolint:wrapcheck // I/O error of the connection
	}
	return line, nil
}

// reply writes one reply line; false when the connection is gone.
func (c *session) reply(line string) bool {
	return c.write([]string{sanitizeReply(line)})
}

// replyLines writes a multi-line reply with the given code.
func (c *session) replyLines(code int, lines []string) bool {
	out := make([]string, len(lines))
	for i, l := range lines {
		sep := "-"
		if i == len(lines)-1 {
			sep = " "
		}
		out[i] = strconv.Itoa(code) + sep + sanitizeReply(l)
	}
	return c.write(out)
}

func (c *session) write(lines []string) bool {
	c.connMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.srv.opts.CommandTimeout))
	c.connMu.Unlock()
	for _, l := range lines {
		_, _ = c.bw.WriteString(l)
		_, _ = c.bw.WriteString("\r\n")
	}
	return c.bw.Flush() == nil
}

// sanitizeReply keeps a reply on one line and bounded. Trailing spaces
// stay ("334 " is a valid empty challenge).
func sanitizeReply(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxReplyText {
		s = s[:maxReplyText]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// parsePath parses "FROM:<addr> params" (or "TO:"); a space after the
// colon is tolerated. The address may be empty (null reverse path).
func parsePath(arg, prefix string) (addr string, params []string, ok bool) {
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", nil, false
	}
	rest := strings.TrimLeft(arg[len(prefix):], " ")
	if !strings.HasPrefix(rest, "<") {
		return "", nil, false
	}
	end := strings.IndexByte(rest, '>')
	if end < 0 {
		return "", nil, false
	}
	addr = rest[1:end]
	if strings.ContainsAny(addr, " \t<>\"") {
		return "", nil, false
	}
	// A source route (<@a,@b:user@c>) is ignored (RFC 5321 §4.1.2).
	if i := strings.IndexByte(addr, ':'); i >= 0 && strings.HasPrefix(addr, "@") {
		addr = addr[i+1:]
	}
	return addr, strings.Fields(rest[end+1:]), true
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
