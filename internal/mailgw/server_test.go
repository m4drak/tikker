// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package mailgw

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/store"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// sendMail delivers one e-mail with net/smtp (STARTTLS when the server has
// TLS) and returns the error of the first failing step.
func (e *env) sendMail(token string, rcpts []string, msg string) error {
	e.t.Helper()
	c, err := smtp.Dial(e.addr)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello("client.test"); err != nil {
		return err
	}
	if e.tls != nil {
		if err := c.StartTLS(e.tls); err != nil {
			return err
		}
	}
	if err := c.Auth(smtp.PlainAuth("", "token", token, "127.0.0.1")); err != nil {
		return err
	}
	if err := c.Mail("monitor@example.net"); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func smtpCode(err error) int {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code
	}
	return 0
}

func rcpt(n string) string { return n + "@" + testDomain }

func TestNewValidation(t *testing.T) {
	ok := Options{Domain: "d.example", Insecure: true, Auth: stubAuth{}, Sender: stubSender{}}
	tests := []struct {
		name string
		mod  func(*Options)
		want string
	}{
		{"valid", func(*Options) {}, ""},
		{"no domain", func(o *Options) { o.Domain = " " }, "Domain"},
		{"no auth", func(o *Options) { o.Auth = nil }, "Auth"},
		{"no tls, not insecure", func(o *Options) { o.Insecure = false }, "TLSConfig"},
		{"tls and insecure", func(o *Options) { o.TLSConfig = serverTLS() }, "exclusive"},
		{"implicit without tls", func(o *Options) { o.ImplicitTLS = true }, "ImplicitTLS"},
		{"bad auth limit", func(o *Options) { o.AuthLimit = Limit{Burst: 0, Every: time.Second} }, "AuthLimit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := ok
			tt.mod(&o)
			_, err := New(o)
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("New() = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestEHLOAdvertisement: AUTH is offered only once the session is
// protected; STARTTLS only before.
func TestEHLOAdvertisement(t *testing.T) {
	tests := []struct {
		name     string
		opts     []envOpt
		upgrade  bool
		want     []string
		dontWant []string
	}{
		{name: "insecure loopback", want: []string{"AUTH PLAIN LOGIN", "SIZE 65536", "8BITMIME"}, dontWant: []string{"STARTTLS", "PIPELINING"}},
		{name: "before STARTTLS", opts: []envOpt{withTLS(false)}, want: []string{"STARTTLS"}, dontWant: []string{"AUTH"}},
		{name: "after STARTTLS", opts: []envOpt{withTLS(false)}, upgrade: true, want: []string{"AUTH PLAIN LOGIN"}, dontWant: []string{"STARTTLS"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.opts...)
			c := e.dial()
			if greet := c.expect(220); !strings.Contains(greet, testDomain) {
				t.Errorf("greeting %q", greet)
			}
			msg := c.cmd(250, "EHLO client.test")
			if tt.upgrade {
				c.startTLS()
				msg = c.cmd(250, "EHLO client.test")
			}
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("EHLO %q misses %q", msg, w)
				}
			}
			for _, w := range tt.dontWant {
				if strings.Contains(msg, w) {
					t.Errorf("EHLO %q has %q", msg, w)
				}
			}
		})
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, withTLS(false))
	expiring := e.token(e.admin.ID, domain.Scopes{domain.ScopeSMSSend}, t0.Add(time.Hour))
	deleted := e.token(e.admin.ID, domain.Scopes{domain.ScopeAdmin}, time.Time{})
	toks, err := e.st.Tokens().List(e.ctx, e.admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if tk.Scopes.Allows(domain.ScopeAdmin) {
			if err := e.auth.DeleteToken(e.ctx, domain.SystemActor, tk.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.clk.Advance(2 * time.Hour) // expiring is now expired
	disabled := true
	if _, err := e.auth.UpdateUser(e.ctx, domain.SystemActor, e.operatorUser.ID, auth.UserChanges{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		tok  string
		code int
	}{
		{"valid sms:send token", e.send, 0},
		{"wrong secret", e.send[:len(e.send)-1] + "x", 535},
		{"not a token", "password", 535},
		{"read-only token", e.read, 535},
		{"expired token", expiring, 535},
		{"deleted token", deleted, 535},
		{"disabled owner", e.operator, 535},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := e.sendMail(tt.tok, []string{rcpt("600100200")}, mailText("", "hi"))
			if got := smtpCode(err); got != tt.code || (tt.code == 0 && err != nil) {
				t.Errorf("sendMail() = %v (code %d), want %d", err, got, tt.code)
			}
		})
	}
	if out := e.logs.String(); strings.Contains(out, e.send) || strings.Contains(out, e.read) {
		t.Error("logs contain a token")
	}
}

func TestAuthProtocol(t *testing.T) {
	tests := []struct {
		name string
		opts []envOpt
		run  func(e *env, c *conn)
	}{
		{name: "AUTH before STARTTLS is refused", opts: []envOpt{withTLS(false)}, run: func(e *env, c *conn) {
			c.cmd(250, "EHLO x")
			c.cmd(538, "AUTH PLAIN %s", plain("token", e.send))
		}},
		{name: "AUTH before EHLO", run: func(e *env, c *conn) {
			c.cmd(503, "AUTH PLAIN %s", plain("token", e.send))
		}},
		{name: "AUTH LOGIN with challenges", run: func(e *env, c *conn) {
			c.cmd(250, "EHLO x")
			c.cmd(334, "AUTH LOGIN")
			c.cmd(334, "%s", b64("token"))
			c.cmd(235, "%s", b64(e.send))
			c.cmd(503, "AUTH PLAIN %s", plain("token", e.send))
		}},
		{name: "AUTH PLAIN with a challenge", run: func(e *env, c *conn) {
			c.cmd(250, "EHLO x")
			c.cmd(334, "AUTH PLAIN")
			c.cmd(235, "%s", plain("anything", e.send))
		}},
		{name: "cancelled, unknown mechanism, bad base64, foreign authzid", run: func(e *env, c *conn) {
			c.cmd(250, "EHLO x")
			c.cmd(334, "AUTH PLAIN")
			c.cmd(501, "*")
			c.cmd(504, "AUTH CRAM-MD5")
			c.cmd(501, "AUTH PLAIN !!!")
			c.cmd(535, "AUTH PLAIN %s", b64("admin\x00token\x00"+e.send))
			c.cmd(501, "AUTH PLAIN %s", b64("only-one-part"))
		}},
		{name: "three failures close the connection", run: func(_ *env, c *conn) {
			c.cmd(250, "EHLO x")
			c.cmd(535, "AUTH PLAIN %s", plain("token", "bad1"))
			c.cmd(535, "AUTH PLAIN %s", plain("token", "bad2"))
			c.cmd(421, "AUTH PLAIN %s", plain("token", "bad3"))
		}},
		{name: "per-address limit", opts: []envOpt{func(o *Options) { o.AuthLimit = Limit{Burst: 2, Every: time.Hour} }}, run: func(e *env, c *conn) {
			c.cmd(250, "EHLO x")
			c.cmd(535, "AUTH PLAIN %s", plain("token", "bad1"))
			c.cmd(535, "AUTH PLAIN %s", plain("token", "bad2"))
			c2 := e.dial()
			c2.expect(220)
			c2.cmd(250, "EHLO x")
			// Even the right token is refused while the address is limited.
			c2.cmd(421, "AUTH PLAIN %s", plain("token", e.send))
		}},
		{name: "valid logins do not use up the limit", opts: []envOpt{func(o *Options) { o.AuthLimit = Limit{Burst: 1, Every: time.Hour} }}, run: func(e *env, _ *conn) {
			for range 3 {
				c := e.dial()
				c.expect(220)
				c.login(e.send)
				c.cmd(221, "QUIT")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.opts...)
			c := e.dial()
			c.expect(220)
			tt.run(e, c)
		})
	}
}

// TestNoOpenRelay: nothing is accepted without authentication, and only
// numbers in the configured domain are recipients.
func TestNoOpenRelay(t *testing.T) {
	tests := []struct {
		name  string
		login bool
		cmds  []struct {
			code int
			line string
		}
	}{
		{name: "MAIL without AUTH", cmds: []struct {
			code int
			line string
		}{{530, "MAIL FROM:<a@example.net>"}, {503, "RCPT TO:<600100200@" + testDomain + ">"}, {503, "DATA"}}},
		{name: "foreign and look-alike domains", login: true, cmds: []struct {
			code int
			line string
		}{
			{250, "MAIL FROM:<a@example.net>"},
			{550, "RCPT TO:<someone@example.com>"},
			{550, "RCPT TO:<600100200@" + testDomain + ".evil.com>"},
			{550, "RCPT TO:<600100200@evil" + testDomain + ">"},
			{550, "RCPT TO:<@" + testDomain + ":victim@example.com>"},
			{553, "RCPT TO:<600100200@example.com@" + testDomain + ">"},
			{553, "RCPT TO:<alice@" + testDomain + ">"},
			{501, `RCPT TO:<"600100200"@` + testDomain + ">"},
			{501, "RCPT TO:600100200@" + testDomain},
			{554, "DATA"},
		}},
		{name: "transaction order", login: true, cmds: []struct {
			code int
			line string
		}{
			{503, "RCPT TO:<600100200@" + testDomain + ">"},
			{250, "MAIL FROM:<>"},
			{503, "MAIL FROM:<a@example.net>"},
			{250, "RCPT TO:<600100200@" + strings.ToUpper(testDomain) + ">"},
			{250, "RSET"},
			{503, "DATA"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			c := e.dial()
			c.expect(220)
			if tt.login {
				c.login(e.send)
			} else {
				c.cmd(250, "EHLO x")
			}
			for _, cmd := range tt.cmds {
				c.cmd(cmd.code, "%s", cmd.line)
			}
			if n := len(e.messages()); n != 0 {
				t.Errorf("%d messages stored", n)
			}
		})
	}
}

func TestSendSingleAndMulti(t *testing.T) {
	e := newEnv(t, withTLS(false))
	if err := e.sendMail(e.send, []string{rcpt("600100200")}, mailText("one@example.net", "Server down: db1\n")); err != nil {
		t.Fatal(err)
	}
	// Three recipients (one given twice) = three messages.
	rcpts := []string{rcpt("600100201"), rcpt("+48600100202"), rcpt("0048600100203"), rcpt("600100201")}
	if err := e.sendMail(e.send, rcpts, mailText("multi@example.net", "Backup OK")); err != nil {
		t.Fatal(err)
	}
	msgs := e.messages()
	if len(msgs) != 4 {
		t.Fatalf("%d messages, want 4", len(msgs))
	}
	want := []struct{ peer, body string }{
		{"+48600100200", "Server down: db1"},
		{"+48600100201", "Backup OK"}, {"+48600100202", "Backup OK"}, {"+48600100203", "Backup OK"},
	}
	for i, w := range want {
		m := msgs[i]
		if m.Peer != w.peer || m.Body != w.body || m.Status != domain.StatusQueued || m.Direction != domain.DirectionOut {
			t.Errorf("message %d = %s %q %s", i, m.Peer, m.Body, m.Status)
		}
		if m.CreatedBy.Type != domain.ActorToken {
			t.Errorf("message %d created by %v", i, m.CreatedBy)
		}
	}
	if msgs[0].ClientRef != "one@example.net" {
		t.Errorf("client_ref = %q", msgs[0].ClientRef)
	}
	audit, err := e.st.Audit().List(e.ctx, store.AuditFilter{Action: domain.AuditMessageSend}, store.PageRequest{Limit: 10})
	if err != nil || len(audit.Items) != 2 || audit.Items[0].Meta["source"] != "smtp" {
		t.Errorf("audit = %+v, %v", audit.Items, err)
	}
	if out := e.logs.String(); strings.Contains(out, "600100200") || strings.Contains(out, "Server down") {
		t.Errorf("logs leak a number or the text:\n%s", out)
	}
}

func TestRecipientLimits(t *testing.T) {
	limited := &stubLimiter{allow: 1}
	e := newEnv(t, func(o *Options) { o.MaxRecipients = 2; o.Limiter = limited })
	c := e.dial()
	c.expect(220)
	c.login(e.send)
	c.cmd(250, "MAIL FROM:<a@example.net>")
	c.cmd(250, "RCPT TO:<%s>", rcpt("600100200"))
	c.cmd(250, "RCPT TO:<%s>", rcpt("600100200")) // duplicate: free
	c.cmd(451, "RCPT TO:<%s>", rcpt("600100201")) // token budget used up
	limited.set(10)
	c.cmd(250, "RCPT TO:<%s>", rcpt("600100201"))
	c.cmd(452, "RCPT TO:<%s>", rcpt("600100202"))
	if got := limited.lastSeen(); got != e.tokenID(e.send) {
		t.Errorf("limiter charged %q", got)
	}
}

func (e *env) tokenID(raw string) domain.ID {
	p, err := e.auth.AuthenticateToken(e.ctx, raw)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Token.ID
}

func TestIdempotentRetry(t *testing.T) {
	e := newEnv(t)
	msg := mailText("retry-1@example.net", "Only once")
	for range 2 {
		if err := e.sendMail(e.send, []string{rcpt("600100200")}, msg); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.messages()); n != 1 {
		t.Fatalf("%d messages after a retry, want 1", n)
	}
	// The second attempt is answered with the original IDs.
	c := e.dial()
	c.expect(220)
	c.login(e.send)
	c.cmd(250, "MAIL FROM:<a@example.net>")
	c.cmd(250, "RCPT TO:<%s>", rcpt("600100200"))
	c.cmd(354, "DATA")
	c.raw(msg + "\r\n.\r\n")
	if got := c.expect(250); !strings.Contains(got, "already queued") || !strings.Contains(got, e.messages()[0].ID.String()) {
		t.Errorf("replay reply %q", got)
	}
	// The same Message-ID with other content is refused.
	err := e.sendMail(e.send, []string{rcpt("600100200")}, mailText("retry-1@example.net", "Different"))
	if smtpCode(err) != 554 || !strings.Contains(err.Error(), "idempotency") {
		t.Errorf("reuse: %v", err)
	}
	// Another token may use the same Message-ID: keys are per token.
	other := e.token(e.admin.ID, domain.Scopes{domain.ScopeSMSSend, domain.ScopeSMSRead}, time.Time{})
	if err := e.sendMail(other, []string{rcpt("600100200")}, msg); err != nil {
		t.Fatal(err)
	}
	// Without Message-ID every submission is new.
	for range 2 {
		if err := e.sendMail(e.send, []string{rcpt("600100200")}, mailText("", "No id")); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.messages()); n != 4 {
		t.Errorf("%d messages, want 4", n)
	}
}

func TestDeliverErrors(t *testing.T) {
	tests := []struct {
		name   string
		opts   []envOpt
		prep   func(e *env)
		msg    string
		code   int
		reason string
	}{
		{name: "html only", msg: "Subject: x\r\nContent-Type: text/html\r\n\r\n<p>hi</p>", code: 554, reason: "html_only"},
		{name: "empty text", msg: "Subject: x\r\n\r\n-- \r\nsig", code: 554, reason: "required"},
		{name: "too long for the channel", msg: mailText("", strings.Repeat("x", 1700)), code: 554, reason: "out_of_range"},
		{name: "no channel", prep: func(e *env) { clear(e.caps) }, msg: mailText("", "hi"), code: 554, reason: "no_matching_channel"},
		{name: "store failure", opts: []envOpt{func(o *Options) { o.Sender = stubSender{err: errors.New("disk I/O error")} }}, msg: mailText("", "hi"), code: 451, reason: "Temporary"},
		{name: "invalid number reported by the sender", opts: []envOpt{func(o *Options) {
			o.Sender = stubSender{ferrs: []sending.FieldError{{Field: "/messages/0/to", Code: sending.CodeInvalidPhone, Detail: "Bad."}}}
		}}, msg: mailText("", "hi"), code: 553, reason: "recipient +48600100200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.opts...)
			if tt.prep != nil {
				tt.prep(e)
			}
			err := e.sendMail(e.send, []string{rcpt("600100200")}, tt.msg)
			if smtpCode(err) != tt.code || !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("sendMail() = %v, want %d %s", err, tt.code, tt.reason)
			}
		})
	}
}

func TestDataLimitsAndDots(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.MaxMessageBytes = 2048 })
	c := e.dial()
	c.expect(220)
	c.login(e.send)

	// SIZE announced too big.
	c.cmd(552, "MAIL FROM:<a@example.net> SIZE=5000")
	c.cmd(555, "MAIL FROM:<a@example.net> SMTPUTF8")
	// Too big while reading: refused, the session goes on.
	c.cmd(250, "MAIL FROM:<a@example.net> SIZE=100 BODY=8BITMIME")
	c.cmd(250, "RCPT TO:<%s>", rcpt("600100200"))
	c.cmd(354, "DATA")
	c.raw(mailText("", strings.Repeat("0123456789\n", 300)) + "\r\n.\r\n")
	c.expect(552)
	c.cmd(250, "NOOP")
	// A line over the limit.
	c.cmd(250, "MAIL FROM:<a@example.net>")
	c.cmd(250, "RCPT TO:<%s>", rcpt("600100200"))
	c.cmd(354, "DATA")
	c.raw(mailText("", strings.Repeat("y", maxLine+10)) + "\r\n.\r\n")
	c.expect(500)
	// Dot-unstuffing; a bare LF before "." does not end the data (the
	// line is unstuffed as any other).
	c.cmd(250, "MAIL FROM:<a@example.net>")
	c.cmd(250, "RCPT TO:<%s>", rcpt("600100200"))
	c.cmd(354, "DATA")
	c.raw(mailText("", "first\n..second\nthird") + "\n.\nfourth\r\n.\r\n")
	c.expect(250)
	msgs := e.messages()
	if len(msgs) != 1 || msgs[0].Body != "first\n.second\nthird\n\nfourth" {
		t.Fatalf("messages = %+v", msgs)
	}
	c.cmd(221, "QUIT")
}

func TestProtocolMisc(t *testing.T) {
	e := newEnv(t, withTLS(false))
	c := e.dial()
	c.expect(220)
	c.cmd(501, "EHLO")
	c.cmd(250, "HELO client")
	c.cmd(250, "NOOP")
	c.cmd(252, "VRFY root")
	c.cmd(214, "HELP")
	c.cmd(500, "BDAT 10 LAST")
	c.cmd(500, "%s", strings.Repeat("A", maxCommandLine+5))
	c.cmd(501, "STARTTLS now")
	c.cmd(530, "MAIL FROM:<a@example.net>")
	// Commands pipelined behind STARTTLS end the connection
	// (CVE-2011-0411).
	c.raw("STARTTLS\r\nNOOP\r\n")
	c.expect(554)
	if _, err := c.tp.ReadLine(); err == nil {
		t.Error("connection still open after pipelined STARTTLS")
	}

	// Too many bad commands.
	c2 := e.dial()
	c2.expect(220)
	for range maxBadCommands {
		c2.cmd(500, "XYZZY")
	}
	c2.cmd(421, "XYZZY")
}

func TestImplicitTLS(t *testing.T) {
	e := newEnv(t, withTLS(true))
	e.tls = nil // net/smtp's Dial cannot do implicit TLS; use tls.Dial
	nc, err := tlsDial(e.addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := smtp.NewClient(nc, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Error("STARTTLS offered inside TLS")
	}
	if err := c.Auth(smtp.PlainAuth("", "token", e.send, "127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	if err := c.Quit(); err != nil {
		t.Fatal(err)
	}
}

func TestInsecureRefusesRemotePeers(t *testing.T) {
	srv, err := New(Options{Domain: testDomain, Insecure: true, Auth: stubAuth{}, Sender: stubSender{}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe() // a pipe address is not loopback
	defer func() { _ = client.Close() }()
	srv.wg.Add(1) // as Serve does
	srv.accept(server)
	srv.wg.Done()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := textproto.NewReader(bufioReader(client)).ReadLine()
	if err != nil || !strings.HasPrefix(line, "554 ") {
		t.Errorf("greeting = %q, %v", line, err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionLimitAndTimeout(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.MaxConnections = 1; o.CommandTimeout = 300 * time.Millisecond })
	c := e.dial()
	c.expect(220)
	c2 := e.dial()
	c2.expect(421) // the server is full
	c.expect(421)  // idle too long
}

func TestShutdown(t *testing.T) {
	e := newEnv(t)
	c := e.dial()
	c.expect(220)
	c.login(e.send)
	c.cmd(250, "MAIL FROM:<a@example.net>")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	c.expect(421)
	if err := <-e.done; !errors.Is(err, ErrServerClosed) {
		t.Errorf("Serve() = %v", err)
	}
	e.done <- ErrServerClosed // for the cleanup
	if _, err := net.DialTimeout("tcp", e.addr, time.Second); err == nil {
		t.Error("listener still open")
	}
	// Serve after Shutdown returns at once.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Serve(ln); !errors.Is(err, ErrServerClosed) {
		t.Errorf("Serve() after Shutdown = %v", err)
	}
}

func TestShutdownTimeoutClosesSessions(t *testing.T) {
	block, entered := make(chan struct{}), make(chan struct{})
	e := newEnv(t, func(o *Options) { o.Sender = blockingSender{entered: entered, block: block} })
	c := e.dial()
	c.expect(220)
	c.login(e.send)
	c.cmd(250, "MAIL FROM:<a@example.net>")
	c.cmd(250, "RCPT TO:<%s>", rcpt("600100200"))
	c.cmd(354, "DATA")
	c.raw(mailText("", "hi") + "\r\n.\r\n")
	<-entered // the session is storing the message
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() {
		<-ctx.Done()
		close(block) // the store call returns after the deadline
	}()
	if err := e.srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown() = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Stubs

type stubAuth struct{}

func (stubAuth) AuthenticateToken(context.Context, string) (auth.Principal, error) {
	return auth.Principal{}, auth.ErrInvalidToken
}

type stubSender struct {
	err   error
	ferrs []sending.FieldError
}

func (s stubSender) SendBatch(context.Context, domain.Actor, []sending.Request, sending.SendOptions) (store.EnqueueResult, []sending.FieldError, error) {
	return store.EnqueueResult{}, s.ferrs, s.err
}

type blockingSender struct{ entered, block chan struct{} }

func (b blockingSender) SendBatch(context.Context, domain.Actor, []sending.Request, sending.SendOptions) (store.EnqueueResult, []sending.FieldError, error) {
	close(b.entered)
	<-b.block
	return store.EnqueueResult{}, nil, errors.New("late")
}

// stubLimiter is called from the session goroutine and changed by the test.
type stubLimiter struct {
	mu    sync.Mutex
	allow int
	seen  domain.ID
}

func (l *stubLimiter) set(n int) {
	l.mu.Lock()
	l.allow = n
	l.mu.Unlock()
}

func (l *stubLimiter) lastSeen() domain.ID {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen
}

func (l *stubLimiter) Allow(id domain.ID) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = id
	if l.allow <= 0 {
		return false, 7 * time.Second
	}
	l.allow--
	return true, 0
}

func tlsDial(addr string) (net.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, clientTLS())
}

func bufioReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }
